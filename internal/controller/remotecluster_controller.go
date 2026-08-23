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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters/auth"
)

// SecretRefIndex indexes RemoteClusters by the Secrets they read, so a
// credential change can be turned into the clusters it affects.
const SecretRefIndex = "spec.access.secretRefs"

// ClusterRefIndex indexes CrossServices by the RemoteCluster they bind to.
const ClusterRefIndex = "spec.source.clusterRef"

// RemoteClusterReconciler validates a secondary cluster's access configuration and reports
// whether it is reachable.
type RemoteClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Builder  *auth.Builder
	Provider *clusters.CachingProvider

	// ProbeInterval is how often a healthy cluster is re-probed.
	ProbeInterval time.Duration

	Now func() time.Time
}

// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=remoteclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=remoteclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=remoteclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile builds the cluster's connection, probes it, and reports the result.
func (r *RemoteClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rc netv1alpha1.RemoteCluster
	if err := r.Get(ctx, req.NamespacedName, &rc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !rc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &rc)
	}

	if !controllerutil.ContainsFinalizer(&rc, netv1alpha1.RemoteClusterFinalizer) {
		// A cluster-scoped object cannot own namespaced ones (I13), so cleanup
		// runs off this finalizer rather than garbage collection.
		controllerutil.AddFinalizer(&rc, netv1alpha1.RemoteClusterFinalizer)
		if err := r.Update(ctx, &rc); err != nil {
			return ctrl.Result{}, err
		}
	}

	status := rc.Status.DeepCopy()
	status.ObservedGeneration = rc.Generation
	status.ConsumerCount = int32(r.Provider.Consumers(rc.Name)) // #nosec G115 -- bounded by CrossService count

	built, err := r.Builder.Build(ctx, &rc)
	if err != nil {
		reason := reasonForClusterError(err)
		r.set(status, netv1alpha1.ConditionAuthenticated, metav1.ConditionFalse, reason, err.Error())
		r.set(status, netv1alpha1.ConditionReady, metav1.ConditionFalse, reason, err.Error())
		// A build failure is a spec or credential problem. Requeueing on a
		// timer would burn the rate limiter; the Secret and spec watches will
		// retrigger when something actually changes.
		return ctrl.Result{}, r.writeStatus(ctx, &rc, status)
	}
	r.set(status, netv1alpha1.ConditionAuthenticated, metav1.ConditionTrue, "Authenticated", "credentials resolved")
	status.Endpoint = built.Config.Host

	version, err := clusters.Probe(ctx, built.Config)
	if err != nil {
		r.set(status, netv1alpha1.ConditionReachable, metav1.ConditionFalse, netv1alpha1.ReasonNetworkUnreachable, err.Error())
		r.set(status, netv1alpha1.ConditionReady, metav1.ConditionFalse, netv1alpha1.ReasonNetworkUnreachable, err.Error())
		return ctrl.Result{RequeueAfter: r.probeInterval()}, r.writeStatus(ctx, &rc, status)
	}

	now := metav1.NewTime(r.now())
	status.KubernetesVersion = version
	status.LastProbeTime = &now
	r.set(status, netv1alpha1.ConditionReachable, metav1.ConditionTrue, "Reachable", "apiserver responded")
	r.set(status, netv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready", "cluster is reachable and authenticated")

	return ctrl.Result{RequeueAfter: r.probeInterval()}, r.writeStatus(ctx, &rc, status)
}

// finalize tears the cached connection down and releases the finalizer.
func (r *RemoteClusterReconciler) finalize(ctx context.Context, rc *netv1alpha1.RemoteCluster) error {
	r.Provider.Invalidate(rc.Name)

	if controllerutil.RemoveFinalizer(rc, netv1alpha1.RemoteClusterFinalizer) {
		return r.Update(ctx, rc)
	}
	return nil
}

func (r *RemoteClusterReconciler) probeInterval() time.Duration {
	if r.ProbeInterval > 0 {
		return r.ProbeInterval
	}
	return 5 * time.Minute
}

func (r *RemoteClusterReconciler) set(
	status *netv1alpha1.RemoteClusterStatus,
	condType string,
	state metav1.ConditionStatus,
	reason, message string,
) {
	for i := range status.Conditions {
		c := &status.Conditions[i]
		if c.Type != condType {
			continue
		}
		if c.Status == state && c.Reason == reason {
			c.Message = message
			return
		}
		c.Status, c.Reason, c.Message = state, reason, message
		c.LastTransitionTime = metav1.NewTime(r.now())
		return
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             state,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(r.now()),
	})
}

func (r *RemoteClusterReconciler) writeStatus(
	ctx context.Context,
	rc *netv1alpha1.RemoteCluster,
	status *netv1alpha1.RemoteClusterStatus,
) error {
	for i := range status.Conditions {
		status.Conditions[i].ObservedGeneration = rc.Generation
	}
	if apiequality.Semantic.DeepEqual(rc.Status, *status) {
		return nil
	}
	rc.Status = *status
	if err := r.Status().Update(ctx, rc); err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("updating RemoteCluster status: %w", err)
	}
	return nil
}

func (r *RemoteClusterReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// mapSecretToClusters turns a credential change into the RemoteClusters that
// read it, and invalidates their cached connections.
//
// Without this a token rotation is invisible: the cached client keeps working
// until the old credential expires, and then everything breaks at once, an hour
// after anybody touched the system.
func (r *RemoteClusterReconciler) mapSecretToClusters(ctx context.Context, o client.Object) []reconcile.Request {
	var list netv1alpha1.RemoteClusterList
	if err := r.List(ctx, &list, client.MatchingFields{SecretRefIndex: o.GetName()}); err != nil {
		return nil
	}

	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		r.Provider.Invalidate(list.Items[i].Name)
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
	}
	return out
}

// SetupRemoteClusterIndexes registers the reverse indexes this controller and
// the CrossService controller need.
func SetupRemoteClusterIndexes(ctx context.Context, mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(ctx, &netv1alpha1.RemoteCluster{}, SecretRefIndex,
		func(o client.Object) []string {
			rc, ok := o.(*netv1alpha1.RemoteCluster)
			if !ok {
				return nil
			}
			return auth.SecretRefs(rc)
		})
	if err != nil {
		return err
	}

	return mgr.GetFieldIndexer().IndexField(ctx, &netv1alpha1.CrossService{}, ClusterRefIndex,
		func(o client.Object) []string {
			xsvc, ok := o.(*netv1alpha1.CrossService)
			if !ok || xsvc.Spec.Source.ClusterRef == nil {
				return nil
			}
			return []string{xsvc.Spec.Source.ClusterRef.Name}
		})
}

// SetupWithManager wires the RemoteCluster controller, including the Secret
// watch that makes credential rotation take effect without a restart.
func (r *RemoteClusterReconciler) SetupWithManager(mgr ctrl.Manager, credentialsNamespace string) error {
	if r.Now == nil {
		r.Now = time.Now
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1alpha1.RemoteCluster{}).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToClusters),
			builder.WithPredicates(inNamespace(credentialsNamespace)),
		).
		Named("remotecluster").
		Complete(r)
}

// inNamespace restricts a watch to the credentials namespace. Secrets are only
// ever read from there (9.1), so watching any other namespace would be both
// useless and a needless cache of other people's secrets.
func inNamespace(ns string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == ns
	})
}
