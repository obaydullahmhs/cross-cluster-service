/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters/auth"
	"github.com/obaydullahmhs/cross-cluster-service/internal/controller"
	"github.com/obaydullahmhs/cross-cluster-service/internal/endpoints"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(netv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	var maxEndpointsPerSlice int
	var credentialsNamespace string
	var allowInsecureTLS bool
	var allowExecCredentials bool
	flag.IntVar(&maxEndpointsPerSlice, "max-endpoints-per-slice", endpoints.DefaultMaxEndpointsPerSlice,
		"Maximum number of endpoints packed into a single EndpointSlice.")
	flag.StringVar(&credentialsNamespace, "credentials-namespace", "",
		"Namespace Secrets are read from. Defaults to the controller's own namespace. "+
			"Credentials are NEVER read from any other namespace.")
	flag.BoolVar(&allowInsecureTLS, "allow-insecure-tls", false,
		"Permit RemoteClusters that set insecureSkipVerify. Off by default.")
	flag.BoolVar(&allowExecCredentials, "allow-exec-credentials", false,
		"Permit the ExecPlugin access type, which is arbitrary code execution "+
			"driven by a cluster-scoped CR. Off by default.")

	flag.Parse()

	if credentialsNamespace == "" {
		// Default to the controller's own namespace, which the Deployment
		// supplies through the downward API. Credentials are never read from
		// anywhere else, so guessing wrong here fails every Secret read with a
		// confusing NotFound -- refuse to start instead.
		credentialsNamespace = os.Getenv("POD_NAMESPACE")
		if credentialsNamespace == "" {
			setupLog.Error(nil, "cannot determine the credentials namespace: "+
				"set --credentials-namespace, or run with POD_NAMESPACE from the downward API")
			os.Exit(1)
		}
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "00193a49.obaydullah.dev",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Established once and shared: the remote client cache's informers must stop
	// when the manager does, and SetupSignalHandler panics if called twice.
	signalCtx := ctrl.SetupSignalHandler()

	if err := controller.SetupIndexes(signalCtx, mgr); err != nil {
		setupLog.Error(err, "unable to set up field indexes")
		os.Exit(1)
	}
	if err := controller.SetupRemoteClusterIndexes(signalCtx, mgr); err != nil {
		setupLog.Error(err, "unable to set up remote cluster indexes")
		os.Exit(1)
	}

	localClusters := &clusters.LocalProvider{Client: mgr.GetClient()}

	authBuilder := &auth.Builder{
		Secrets: &auth.Secrets{Client: mgr.GetClient(), Namespace: credentialsNamespace},
		Options: auth.Options{
			CredentialsNamespace: credentialsNamespace,
			AllowInsecureTLS:     allowInsecureTLS,
			AllowExecCredentials: allowExecCredentials,
		},
	}

	remoteClusters := clusters.NewCachingProvider(
		signalCtx, authBuilder, mgr.GetScheme(),
		func(ctx context.Context, name string) (*netv1alpha1.RemoteCluster, error) {
			var rc netv1alpha1.RemoteCluster
			if err := mgr.GetClient().Get(ctx, types.NamespacedName{Name: name}, &rc); err != nil {
				return nil, err
			}
			return &rc, nil
		}, nil)

	// A source resolves against the local cluster or a named remote one; the
	// resolvers do not care which.
	sourceProvider := clusters.NewRoutingProvider(localClusters, remoteClusters)

	if err := (&controller.CrossServiceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("crossservice"),
		Resolver: resolver.NewRegistry(map[netv1alpha1.SourceType]resolver.Resolver{
			netv1alpha1.SourceTypeStatic:  &resolver.Static{},
			netv1alpha1.SourceTypeDNS:     &resolver.DNS{},
			netv1alpha1.SourceTypePods:    &resolver.Pods{Provider: sourceProvider},
			netv1alpha1.SourceTypeNodes:   &resolver.Nodes{Provider: sourceProvider},
			netv1alpha1.SourceTypeService: &resolver.Service{Provider: sourceProvider},
		}),
		Clusters:             remoteClusters,
		MaxEndpointsPerSlice: maxEndpointsPerSlice,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CrossService")
		os.Exit(1)
	}
	if err := (&controller.RemoteClusterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("remotecluster"),
		Builder:  authBuilder,
		Provider: remoteClusters,
	}).SetupWithManager(mgr, credentialsNamespace); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RemoteCluster")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(signalCtx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
