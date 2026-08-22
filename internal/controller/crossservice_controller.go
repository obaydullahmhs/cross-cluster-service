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
	"sigs.k8s.io/controller-runtime/pkg/log"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/endpoints"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
	"github.com/obaydullahmhs/cross-cluster-service/internal/service"
)

// CrossServiceReconciler reconciles a CrossService into a selector-less Service
// plus managed EndpointSlices.
type CrossServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Resolver dispatches to the per-source-type resolvers. Injected so tests
	// can substitute a deterministic one.
	Resolver resolver.Resolver

	// DefaultAddressPolicy applies to every source, after any policy carried by
	// the source's RemoteCluster.
	DefaultAddressPolicy *netv1alpha1.AddressPolicy

	MaxEndpointsPerSlice int

	// Now is injectable so the failure-policy state machine is testable without
	// sleeping.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=crossservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=crossservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=net.obaydullah.dev,resources=crossservices/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the algorithm in the project brief: validate, resolve,
// filter, apply the failure policy, write the Service, write the slices, then
// report.
func (r *CrossServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var xsvc netv1alpha1.CrossService
	if err := r.Get(ctx, req.NamespacedName, &xsvc); err != nil {
		// Owner references garbage-collect the Service and slices, so a deleted
		// CrossService needs no cleanup of its own.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	status := xsvc.Status.DeepCopy()
	status.ObservedGeneration = xsvc.Generation

	if err := validateSpec(&xsvc); err != nil {
		// A spec problem will not fix itself on a timer, and requeueing would
		// just burn the rate limiter until the user edits the object.
		r.setCondition(status, netv1alpha1.ConditionReady, metav1.ConditionFalse, netv1alpha1.ReasonInvalidSpec, err.Error())
		r.event(&xsvc, corev1.EventTypeWarning, netv1alpha1.ReasonInvalidSpec, err.Error())
		return ctrl.Result{}, r.writeStatus(ctx, &xsvc, status)
	}

	src := &xsvc.Spec.Source
	prevSource := xsvc.Status.Source

	result, resolveErr := r.Resolver.Resolve(ctx, src, xsvc.Spec.Ports)

	var fresh []resolver.Endpoint
	var ttl time.Duration
	if resolveErr == nil {
		fresh = endpoints.Dedupe(result.Endpoints)
		ttl = result.TTL
	}

	policy, err := endpoints.NewPolicy(r.DefaultAddressPolicy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("compiling default address policy: %w", err)
	}

	kept, rejected := policy.Filter(fresh)
	if len(rejected) > 0 {
		// Dropped addresses are reported, never fatal: one bad address must not
		// take down an otherwise healthy source.
		msg := fmt.Sprintf("%d address(es) rejected by address policy, first: %s (%s)",
			len(rejected), rejected[0].Address, rejected[0].Reason)
		logger.Info("addresses rejected by policy", "count", len(rejected), "first", rejected[0].Address)
		r.event(&xsvc, corev1.EventTypeWarning, netv1alpha1.ReasonAddressPolicyRejected, msg)
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

	return ctrl.Result{RequeueAfter: r.requeueAfter(src, ttl)}, nil
}

// requeueAfter returns the poll interval. DNS is the only source that must
// poll; everything else is driven by watches, so returning zero here is
// correct rather than an oversight (I14).
func (r *CrossServiceReconciler) requeueAfter(src *netv1alpha1.Source, ttl time.Duration) time.Duration {
	if src.Type == netv1alpha1.SourceTypeDNS && src.DNS != nil {
		return resolver.RequeueAfter(src.DNS, ttl)
	}
	return 0
}

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

func (r *CrossServiceReconciler) event(xsvc *netv1alpha1.CrossService, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(xsvc, eventType, reason, message)
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1alpha1.CrossService{}).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Named("crossservice").
		Complete(r)
}
