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
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
	"github.com/obaydullahmhs/cross-cluster-service/internal/endpoints"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
	"github.com/obaydullahmhs/cross-cluster-service/internal/service"
)

// CrossServiceReconciler reconciles a CrossService into a selector-less Service
// plus managed EndpointSlices.
type CrossServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder uses the legacy events API. Migrating to events.EventRecorder
	// changes the call signature and the test fake, so it is deferred to the
	// hardening milestone rather than smuggled in here.
	Recorder record.EventRecorder

	// Resolver dispatches to the per-source-type resolvers. Injected so tests
	// can substitute a deterministic one.
	Resolver resolver.Resolver

	// DefaultAddressPolicy applies to every source, after any policy carried by
	// the source's RemoteCluster.
	DefaultAddressPolicy *netv1alpha1.AddressPolicy

	MaxEndpointsPerSlice int

	// Clusters is the ref-counted remote client cache. Nil for a build with
	// local sources only.
	Clusters *clusters.CachingProvider

	// RemoteEvents carries changes from secondary clusters' informers, which
	// are outside the manager's cache and so cannot be wired with Watches().
	// Nil for a build with local sources only; a remote source will then never
	// see a backend change.
	RemoteEvents <-chan event.TypedGenericEvent[client.Object]

	// Now is injectable so the failure-policy state machine is testable without
	// sleeping.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=crossservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=crossservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=crossservices/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// Credentials are read from exactly one namespace, so this is a namespaced Role.
// A ClusterRole over secrets would make any RemoteCluster a credential-exfiltration
// primitive (9.1).
// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,verbs=get;list;watch

// Reconcile implements the algorithm in the project brief: validate, resolve,
// filter, apply the failure policy, write the Service, write the slices, then
// report.
func (r *CrossServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var xsvc netv1alpha1.CrossService
	if err := r.Get(ctx, req.NamespacedName, &xsvc); err != nil {
		if apierrors.IsNotFound(err) && r.Clusters != nil {
			// Owner references garbage-collect the Service and slices, but the
			// reference on the shared remote client is ours to drop.
			r.Clusters.ReleaseAll(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	status := xsvc.Status.DeepCopy()
	status.ObservedGeneration = xsvc.Generation

	if err := validateSpec(&xsvc); err != nil {
		// A spec problem will not fix itself on a timer, and requeueing would
		// just burn the rate limiter until the user edits the object.
		r.setCondition(status, netv1alpha1.ConditionReady, metav1.ConditionFalse, netv1alpha1.ReasonInvalidSpec, err.Error())
		r.warn(&xsvc, netv1alpha1.ReasonInvalidSpec, err.Error())
		return ctrl.Result{}, r.writeStatus(ctx, &xsvc, status)
	}

	src := &xsvc.Spec.Source
	prevSource := xsvc.Status.Source

	// A source bound to a RemoteCluster has to clear the ops team's grant
	// before anything reads from that cluster (9.2).
	rc, err := r.resolveCluster(ctx, &xsvc)
	if err != nil {
		reason := reasonForClusterError(err)
		r.setCondition(status, netv1alpha1.ConditionSourcesResolved, metav1.ConditionFalse, reason, err.Error())
		r.setCondition(status, netv1alpha1.ConditionReady, metav1.ConditionFalse, reason, err.Error())
		r.warn(&xsvc, reason, err.Error())
		return ctrl.Result{}, r.writeStatus(ctx, &xsvc, status)
	}
	result, resolveErr := r.Resolver.Resolve(ctx, src, xsvc.Spec.Ports)

	// Acquired after resolving, not before: the cached client is created lazily
	// by the resolver's first Get, so acquiring earlier would register against
	// an entry that does not exist yet and silently do nothing -- leaving
	// consumerCount at zero and letting a later Release tear down a client
	// another CrossService is still using.
	if r.Clusters != nil && src.ClusterRef != nil {
		r.Clusters.Acquire(src.ClusterRef.Name, req.NamespacedName)
	}

	var fresh []resolver.Endpoint
	var ttl time.Duration
	if resolveErr == nil {
		fresh = endpoints.Dedupe(result.Endpoints)
		ttl = result.TTL

		// A backend that could not be used is reported, never fatal: one Pod
		// whose named port does not resolve must not fail the whole source (I4).
		for _, w := range result.Warnings {
			logger.Info("backend skipped", "reason", w)
			r.warn(&xsvc, netv1alpha1.ReasonNoEndpointsFound, w)
		}
	}

	kept, rejected, err := r.filterAddresses(rc, fresh)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(rejected) > 0 {
		// Dropped addresses are reported, never fatal: one bad address must not
		// take down an otherwise healthy source.
		msg := fmt.Sprintf("%d address(es) rejected by address policy, first: %s (%s)",
			len(rejected), rejected[0].Address, rejected[0].Reason)
		logger.Info("addresses rejected by policy", "count", len(rejected), "first", rejected[0].Address)
		r.warn(&xsvc, netv1alpha1.ReasonAddressPolicyRejected, msg)
	}

	state := endpoints.ApplyFailurePolicy(xsvc.Spec.FailurePolicy, prevSource, kept, resolveErr, r.now())

	svc, _, err := r.reconcileService(ctx, &xsvc)
	if err != nil {
		r.setCondition(status, netv1alpha1.ConditionServiceReady, metav1.ConditionFalse, "ServiceError", err.Error())
		return ctrl.Result{}, err
	}
	r.setCondition(status, netv1alpha1.ConditionServiceReady, metav1.ConditionTrue, "ServiceReady", "Service reconciled")
	status.ServiceName = svc.Name
	status.ClusterIP = svc.Spec.ClusterIP

	groups := endpoints.GroupEndpoints(state.Endpoints, xsvc.Spec.Ports, endpoints.FamilyFilter(xsvc.Spec.IPFamilyPolicy))

	writer := &endpoints.Writer{Client: r.Client, MaxEndpointsPerSlice: r.MaxEndpointsPerSlice}
	outcome, err := writer.Reconcile(ctx, &xsvc, svc.Name, groups, r.ownerFn(&xsvc))
	if err != nil {
		r.setCondition(status, netv1alpha1.ConditionEndpointsWritten, metav1.ConditionFalse, "WriteFailed", err.Error())
		return ctrl.Result{}, err
	}
	r.setCondition(status, netv1alpha1.ConditionEndpointsWritten, metav1.ConditionTrue, "EndpointsWritten",
		fmt.Sprintf("%d slice(s)", len(outcome.SliceNames())))

	r.applyStatus(status, &xsvc, src, state, kept, resolveErr, outcome, len(rejected))

	if err := r.writeStatus(ctx, &xsvc, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.requeueAfter(src, rc, ttl)}, nil
}

// filterAddresses applies the source cluster's own AddressPolicy, then the
// controller-wide default.
//
// Both, in that order, because a RemoteCluster's policy is the operator's
// statement about one cluster ("only these CIDRs are real backends over there")
// and the default is the deployment-wide floor. Applying only the default would
// leave the per-cluster field accepted by the apiserver and enforcing nothing,
// which is a worse failure than not offering it: the operator believes a
// restriction is in place.
func (r *CrossServiceReconciler) filterAddresses(
	rc *netv1alpha1.RemoteCluster,
	in []resolver.Endpoint,
) ([]resolver.Endpoint, []endpoints.Rejection, error) {
	var rejected []endpoints.Rejection

	if rc != nil && rc.Spec.AddressPolicy != nil {
		clusterPolicy, err := endpoints.NewPolicy(rc.Spec.AddressPolicy)
		if err != nil {
			return nil, nil, fmt.Errorf("compiling address policy for cluster %s: %w", rc.Name, err)
		}
		var dropped []endpoints.Rejection
		in, dropped = clusterPolicy.Filter(in)
		rejected = append(rejected, dropped...)
	}

	defaultPolicy, err := endpoints.NewPolicy(r.DefaultAddressPolicy)
	if err != nil {
		return nil, nil, fmt.Errorf("compiling default address policy: %w", err)
	}
	kept, dropped := defaultPolicy.Filter(in)
	return kept, append(rejected, dropped...), nil
}

// requeueAfter returns the poll interval. DNS is the only source that must
// poll; everything else is driven by watches (I14).
//
// A remote source additionally gets a floor, because its events cross two
// pieces of machinery that can lose one: a watch against another cluster's
// apiserver, and the buffered channel those events are funnelled through,
// which drops rather than blocking an informer's delivery goroutine. Neither
// loss is detectable from here -- a stale EndpointSlice reports Ready and
// serves dead addresses -- so the reconcile is repeated on a timer that costs
// nothing when nothing changed.
//
// A local source needs no floor: its events come from the manager's own cache
// with no channel in between.
func (r *CrossServiceReconciler) requeueAfter(
	src *netv1alpha1.Source,
	rc *netv1alpha1.RemoteCluster,
	ttl time.Duration,
) time.Duration {
	var out time.Duration
	switch {
	case src.Type == netv1alpha1.SourceTypeDNS && src.DNS != nil:
		out = resolver.RequeueAfter(src.DNS, ttl)
	default:
		// A Service source normally needs no timer, but a LoadBalancer whose
		// provider returns a hostname has to be re-resolved, and says so by
		// returning a TTL.
		out = ttl
	}

	if floor := remoteResyncFloor(rc); floor > 0 && (out == 0 || out > floor) {
		out = floor
	}
	return out
}

// remoteResyncFloor is the RemoteCluster's resyncPeriod, or zero for a local
// source or a cluster that has explicitly disabled it.
func remoteResyncFloor(rc *netv1alpha1.RemoteCluster) time.Duration {
	if rc == nil {
		return 0
	}
	if p := rc.Spec.Access.ResyncPeriod; p != nil {
		return p.Duration
	}
	return defaultRemoteResync
}

// defaultRemoteResync matches the CRD default for a spec stored before the
// field existed.
const defaultRemoteResync = 10 * time.Minute

func (r *CrossServiceReconciler) reconcileService(
	ctx context.Context,
	xsvc *netv1alpha1.CrossService,
) (*corev1.Service, bool, error) {
	sr := &service.Reconciler{Client: r.Client}
	return sr.Reconcile(ctx, xsvc, r.ownerFn(xsvc))
}

// ownerFn sets the controller reference. Both the Service and the slices are
// namespaced and live alongside the CrossService, so ownership works normally
// here -- it is only the cluster-scoped RemoteCluster that cannot own them
// (I13).
func (r *CrossServiceReconciler) ownerFn(xsvc *netv1alpha1.CrossService) func(client.Object) error {
	return func(o client.Object) error {
		return ctrl.SetControllerReference(xsvc, o, r.Scheme)
	}
}

func (r *CrossServiceReconciler) applyStatus(
	status *netv1alpha1.CrossServiceStatus,
	xsvc *netv1alpha1.CrossService,
	src *netv1alpha1.Source,
	state endpoints.FailureState,
	fresh []resolver.Endpoint,
	resolveErr error,
	outcome endpoints.Outcome,
	rejectedCount int,
) {
	var ready, total int32
	for _, e := range state.Endpoints {
		total++
		if e.Ready {
			ready++
		}
	}
	status.ReadyEndpoints = ready
	status.TotalEndpoints = total
	status.DroppedAddresses = int32(rejectedCount) // #nosec G115 -- bounded by MaxItems on the source
	status.EndpointSlices = outcome.SliceNames()

	ss := &netv1alpha1.SourceStatus{Type: src.Type}
	if src.ClusterRef != nil {
		ss.ClusterRef = src.ClusterRef.Name
	}
	if src.Service != nil {
		ss.Via = src.Service.Via
		ss.RemoteService = src.Service.Namespace + "/" + src.Service.Name
	}
	if prev := xsvc.Status.Source; prev != nil {
		ss.LastSuccessTime = prev.LastSuccessTime
		ss.LastKnownGood = prev.LastKnownGood
	}

	if resolveErr == nil {
		now := metav1.NewTime(r.now())
		ss.LastSuccessTime = &now
		ss.ConsecutiveErrors = 0
		ss.Endpoints = int32(len(fresh)) // #nosec G115 -- bounded by source MaxItems
		ss.LastKnownGood = endpoints.ToPersisted(fresh)
		ss.Stale = false
	} else {
		ss.LastError = resolveErr.Error()
		ss.ConsecutiveErrors = state.ConsecutiveErrors
		ss.Stale = state.Stale
	}
	status.Source = ss

	switch {
	case resolveErr != nil && state.Degraded:
		r.setCondition(status, netv1alpha1.ConditionSourcesResolved, metav1.ConditionFalse, reasonFor(src), resolveErr.Error())
		r.setCondition(status, netv1alpha1.ConditionDegraded, metav1.ConditionTrue, state.Reason, resolveErr.Error())
		r.setCondition(status, netv1alpha1.ConditionReady, metav1.ConditionFalse, state.Reason, "serving stale endpoints")
	case resolveErr != nil:
		r.setCondition(status, netv1alpha1.ConditionSourcesResolved, metav1.ConditionFalse, reasonFor(src), resolveErr.Error())
		r.setCondition(status, netv1alpha1.ConditionDegraded, metav1.ConditionFalse, netv1alpha1.ReasonPartialFailure, "within failure threshold")
		r.setCondition(status, netv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready", "serving last known good endpoints")
	case total == 0:
		r.setCondition(status, netv1alpha1.ConditionSourcesResolved, metav1.ConditionTrue, "Resolved", "source resolved")
		r.setCondition(status, netv1alpha1.ConditionDegraded, metav1.ConditionFalse, "NotDegraded", "")
		r.setCondition(status, netv1alpha1.ConditionReady, metav1.ConditionFalse, netv1alpha1.ReasonNoEndpointsFound, "source resolved to no endpoints")
	default:
		r.setCondition(status, netv1alpha1.ConditionSourcesResolved, metav1.ConditionTrue, "Resolved", "source resolved")
		r.setCondition(status, netv1alpha1.ConditionDegraded, metav1.ConditionFalse, "NotDegraded", "")
		r.setCondition(status, netv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready",
			fmt.Sprintf("%d/%d endpoints ready", ready, total))
	}
}

func reasonFor(src *netv1alpha1.Source) string {
	if src.Type == netv1alpha1.SourceTypeDNS {
		return netv1alpha1.ReasonDNSResolutionFailed
	}
	return netv1alpha1.ReasonPartialFailure
}

// setCondition updates a condition in place, preserving LastTransitionTime
// unless the state actually changed -- a condition that re-stamps its timestamp
// every reconcile is indistinguishable from one that is genuinely flapping.
func (r *CrossServiceReconciler) setCondition(
	status *netv1alpha1.CrossServiceStatus,
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
		c.Status = state
		c.Reason = reason
		c.Message = message
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

func (r *CrossServiceReconciler) writeStatus(
	ctx context.Context,
	xsvc *netv1alpha1.CrossService,
	status *netv1alpha1.CrossServiceStatus,
) error {
	for i := range status.Conditions {
		status.Conditions[i].ObservedGeneration = xsvc.Generation
	}
	if apiequality.Semantic.DeepEqual(xsvc.Status, *status) {
		return nil
	}
	xsvc.Status = *status
	if err := r.Status().Update(ctx, xsvc); err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("updating status: %w", err)
	}
	return nil
}

// warn emits a Warning event. Everything this controller reports through events
// is a problem worth a human looking at; routine progress belongs in conditions.
func (r *CrossServiceReconciler) warn(xsvc *netv1alpha1.CrossService, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(xsvc, corev1.EventTypeWarning, reason, message)
	}
}

func (r *CrossServiceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager wires the controller. The Service and EndpointSlices are
// owned, so a manual edit to either is corrected immediately rather than at the
// next poll.
func (r *CrossServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&netv1alpha1.CrossService{}).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{})

	b = r.watchLocalSources(b).
		// A RemoteCluster changing is what rebuilds its informers after a
		// credential rotation; see mapRemoteCluster.
		Watches(&netv1alpha1.RemoteCluster{}, handler.EnqueueRequestsFromMapFunc(r.mapRemoteCluster))

	// Secondary clusters' informers live outside the manager's cache, so their
	// events arrive on a channel rather than through Watches().
	if r.RemoteEvents != nil {
		b = b.WatchesRawSource(source.Channel(
			r.RemoteEvents,
			handler.EnqueueRequestsFromMapFunc(r.mapRemote),
		))
	}

	return b.
		Named("crossservice").
		Complete(r)
}
