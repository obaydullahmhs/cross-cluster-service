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

package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"k8s.io/client-go/rest"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// buildClientCertificate authenticates with an x509 client keypair.
//
// This is the one Phase 2 access type that depends on no cloud at all, which
// makes it the answer for on-prem and for any cluster whose CA you manage
// yourself.
func (b *Builder) buildClientCertificate(
	ctx context.Context,
	access *netv1alpha1.ClusterAccess,
	fp *fingerprint,
) (*rest.Config, error) {
	cfg := access.ClientCertificate

	certKey, keyKey := cfg.CertKey, cfg.KeyKey
	if certKey == "" {
		certKey = "tls.crt"
	}
	if keyKey == "" {
		keyKey = "tls.key"
	}

	// A keypair is two keys in one Secret, which is why this variant names the
	// Secret rather than taking a SecretKeyRef.
	certPEM, err := b.Secrets.Value(ctx, netv1alpha1.SecretKeyRef{Name: cfg.SecretName, Key: certKey}, certKey)
	if err != nil {
		return nil, err
	}
	keyPEM, err := b.Secrets.Value(ctx, netv1alpha1.SecretKeyRef{Name: cfg.SecretName, Key: keyKey}, keyKey)
	if err != nil {
		return nil, err
	}

	tlsCfg, err := b.tlsFrom(ctx, access, cfg.CASecretRef, fp)
	if err != nil {
		return nil, err
	}
	tlsCfg.CertData = certPEM
	tlsCfg.KeyData = keyPEM

	fp.add("server", cfg.Server)
	fp.addBytes("clientCert", certPEM)
	fp.addBytes("clientKey", keyPEM)

	return &rest.Config{Host: cfg.Server, TLSClientConfig: tlsCfg}, nil
}

// gkeClusterEndpoint is the GKE API resource describing one cluster.
const gkeAPIBase = "https://container.googleapis.com/v1"

// buildGKE discovers the endpoint and CA through the GKE API, then
// authenticates the same way GoogleToken does.
//
// The tradeoff against GoogleToken is explicit: discovery removes the need to
// copy an endpoint and CA per cluster by hand, which matters once there are
// many of them, but it costs a container.clusters.get permission
// (roles/container.clusterViewer) and reachability to container.googleapis.com.
// GoogleToken needs neither, which is why it stays the recommended path when
// Workload Identity is unavailable.
func (b *Builder) buildGKE(
	ctx context.Context,
	access *netv1alpha1.ClusterAccess,
	fp *fingerprint,
) (*rest.Config, error) {
	cfg := access.GKE

	ts, err := b.googleTokenSource(ctx, cfg.Credentials, fp)
	if err != nil {
		return nil, err
	}

	endpoint, caPEM, err := discoverGKECluster(ctx, ts, cfg)
	if err != nil {
		return nil, err
	}

	tlsCfg, err := b.tlsFrom(ctx, access, nil, fp)
	if err != nil {
		return nil, err
	}
	if len(caPEM) > 0 {
		tlsCfg.CAData = caPEM
	}

	fp.add("gkeCluster", fmt.Sprintf("%s/%s/%s", cfg.Project, cfg.Location, cfg.Cluster))
	fp.add("gkePrivate", fmt.Sprintf("%t", cfg.UsePrivateEndpoint))

	return wrapWithTokenSource("https://"+endpoint, tlsCfg, ts), nil
}

// discoverGKECluster reads the endpoint and CA from the GKE API.
//
// Called directly rather than through the generated container/v1 client, which
// would pull in google.golang.org/api for a single GET.
func discoverGKECluster(
	ctx context.Context,
	ts oauth2.TokenSource,
	cfg *netv1alpha1.GKEAccess,
) (endpoint string, caPEM []byte, err error) {
	url := fmt.Sprintf("%s/projects/%s/locations/%s/clusters/%s",
		gkeAPIBase, cfg.Project, cfg.Location, cfg.Cluster)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, err
	}

	resp, err := oauth2.NewClient(ctx, ts).Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("querying the GKE API for %s: %w", cfg.Cluster, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it can restate request details, and nothing
		// in it is worth risking in a log line (9.6).
		return "", nil, fmt.Errorf(
			"querying the GKE API for %s: %s (does the identity hold roles/container.clusterViewer?)",
			cfg.Cluster, resp.Status)
	}

	var out struct {
		Endpoint             string `json:"endpoint"`
		PrivateClusterConfig struct {
			PrivateEndpoint string `json:"privateEndpoint"`
		} `json:"privateClusterConfig"`
		MasterAuth struct {
			ClusterCaCertificate string `json:"clusterCaCertificate"`
		} `json:"masterAuth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, fmt.Errorf("decoding the GKE API response: %w", err)
	}

	endpoint = out.Endpoint
	if cfg.UsePrivateEndpoint {
		if out.PrivateClusterConfig.PrivateEndpoint == "" {
			return "", nil, fmt.Errorf("cluster %s has no private endpoint", cfg.Cluster)
		}
		endpoint = out.PrivateClusterConfig.PrivateEndpoint
	}
	if endpoint == "" {
		return "", nil, fmt.Errorf("cluster %s reported no endpoint", cfg.Cluster)
	}

	if enc := out.MasterAuth.ClusterCaCertificate; enc != "" {
		caPEM, err = decodeBase64(enc)
		if err != nil {
			return "", nil, fmt.Errorf("decoding the cluster CA for %s: %w", cfg.Cluster, err)
		}
	}
	return endpoint, caPEM, nil
}

// aksServerApplicationID is the well-known audience an Entra-enabled AKS
// apiserver validates tokens against.
const aksServerApplicationID = "6dae42f8-4368-4678-94ff-3960e28e3630"

// buildAKS authenticates to an AKS apiserver with an Entra ID token.
//
// The token is minted through an OAuth2 client-credentials exchange, so this
// needs nothing beyond golang.org/x/oauth2. Managed identity and Entra Workload
// Identity are the better production answer and resolve ambiently, but they
// require the Azure identity SDK and are not implemented here.
func (b *Builder) buildAKS(
	ctx context.Context,
	access *netv1alpha1.ClusterAccess,
	fp *fingerprint,
) (*rest.Config, error) {
	cfg := access.AKS

	if cfg.Credentials == nil || cfg.Credentials.ClientSecretRef == nil {
		return nil, &ErrAccessTypeNotImplemented{Type: netv1alpha1.AccessTypeAKS}
	}
	creds := cfg.Credentials

	secret, err := b.Secrets.Value(ctx, *creds.ClientSecretRef, "clientSecret")
	if err != nil {
		return nil, err
	}

	audience := creds.ServerApplicationID
	if audience == "" {
		audience = aksServerApplicationID
	}

	// The ".default" suffix asks Entra for whatever the application was already
	// consented to, which is what the AKS server application expects.
	conf := &clientcredentials.Config{
		ClientID:     creds.ClientID,
		ClientSecret: string(secret),
		TokenURL: fmt.Sprintf(
			"https://login.microsoftonline.com/%s/oauth2/v2.0/token", creds.TenantID),
		Scopes: []string{audience + "/.default"},
	}

	tlsCfg, err := b.tlsFrom(ctx, access, cfg.CASecretRef, fp)
	if err != nil {
		return nil, err
	}

	fp.add("server", cfg.Server)
	fp.add("azureClient", creds.ClientID)
	fp.add("azureTenant", creds.TenantID)
	fp.addBytes("azureSecret", secret)

	return wrapWithTokenSource(cfg.Server, tlsCfg, conf.TokenSource(ctx)), nil
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
