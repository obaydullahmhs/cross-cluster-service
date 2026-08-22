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

// Package service reconciles the selector-less Service that fronts a
// CrossService's endpoints.
package service

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// Reconciler owns the generated Service.
type Reconciler struct {
	Client client.Client
}

// NameFor returns the Service name for a CrossService.
func NameFor(xsvc *netv1alpha1.CrossService) string {
	if xsvc.Spec.Service != nil && xsvc.Spec.Service.Name != "" {
		return xsvc.Spec.Service.Name
	}
	return xsvc.Name
}

// Reconcile creates or updates the Service and returns it.
func (r *Reconciler) Reconcile(
	ctx context.Context,
	xsvc *netv1alpha1.CrossService,
	owner func(client.Object) error,
) (*corev1.Service, bool, error) {
	desired := Build(xsvc)
	if err := owner(desired); err != nil {
		return nil, false, fmt.Errorf("setting owner: %w", err)
	}

	var current corev1.Service
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &current)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Client.Create(ctx, desired); err != nil {
			return nil, false, fmt.Errorf("creating service: %w", err)
		}
		return desired, true, nil
	case err != nil:
		return nil, false, fmt.Errorf("getting service: %w", err)
	}

	merged := current.DeepCopy()
	merged.Labels = mergeMap(merged.Labels, desired.Labels)
	merged.Annotations = mergeMap(merged.Annotations, desired.Annotations)
	merged.OwnerReferences = desired.OwnerReferences
	merged.Spec.Ports = mergePorts(current.Spec.Ports, desired.Spec.Ports)

	// Always nil, never merged from the live object. A non-nil selector hands
	// ownership to the built-in EndpointSlice controller, which then deletes
	// our slices in a loop (I1).
	merged.Spec.Selector = nil

	merged.Spec.PublishNotReadyAddresses = desired.Spec.PublishNotReadyAddresses
	// Only override when the template actually asked for something. The
	// apiserver defaults this field, so assigning our nil unconditionally
	// would diff against the live object on every single reconcile.
	if desired.Spec.InternalTrafficPolicy != nil {
		merged.Spec.InternalTrafficPolicy = desired.Spec.InternalTrafficPolicy
	}
	if desired.Spec.SessionAffinity != "" {
		merged.Spec.SessionAffinity = desired.Spec.SessionAffinity
	}

	if apiequality.Semantic.DeepEqual(current.Spec, merged.Spec) &&
		apiequality.Semantic.DeepEqual(current.Labels, merged.Labels) &&
		apiequality.Semantic.DeepEqual(current.Annotations, merged.Annotations) &&
		apiequality.Semantic.DeepEqual(current.OwnerReferences, merged.OwnerReferences) {
		return &current, false, nil
	}

	if err := r.Client.Update(ctx, merged); err != nil {
		return nil, false, fmt.Errorf("updating service: %w", err)
	}
	return merged, true, nil
}

// Build renders the desired Service. It is exported so tests can assert on the
// shape without a live client.
func Build(xsvc *netv1alpha1.CrossService) *corev1.Service {
	tmpl := xsvc.Spec.Service
	if tmpl == nil {
		tmpl = &netv1alpha1.ServiceTemplate{}
	}

	ports := make([]corev1.ServicePort, 0, len(xsvc.Spec.Ports))
	for _, p := range xsvc.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		// TargetPort is deliberately left unset. For a selector-less Service
		// the apiserver ignores it entirely; the backend port lives on the
		// EndpointSlice instead, joined by port NAME (I2). Setting it here
		// would imply a relationship that does not exist.
		ports = append(ports, corev1.ServicePort{
			Name:        p.Name,
			Port:        p.Port,
			Protocol:    proto,
			AppProtocol: p.AppProtocol,
		})
	}

	labels := map[string]string{
		netv1alpha1.CrossServiceNameLabel:      xsvc.Name,
		netv1alpha1.CrossServiceNamespaceLabel: xsvc.Namespace,
	}
	maps.Copy(labels, tmpl.Labels)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        NameFor(xsvc),
			Namespace:   xsvc.Namespace,
			Labels:      labels,
			Annotations: mergeMap(nil, tmpl.Annotations),
		},
		Spec: corev1.ServiceSpec{
			// Selector-less by construction (I1).
			Selector:                 nil,
			Ports:                    ports,
			ClusterIP:                tmpl.ClusterIP,
			PublishNotReadyAddresses: tmpl.PublishNotReadyAddresses,
			InternalTrafficPolicy:    tmpl.InternalTrafficPolicy,
		},
	}
	if tmpl.SessionAffinity != nil {
		svc.Spec.SessionAffinity = *tmpl.SessionAffinity
	}

	// Dual-stack has to be declared on the Service too, or the second family's
	// slices have no matching ipFamily to be routed under.
	if xsvc.Spec.IPFamilyPolicy == netv1alpha1.IPFamilyPolicyPreferDualStack {
		policy := corev1.IPFamilyPolicyPreferDualStack
		svc.Spec.IPFamilyPolicy = &policy
	}
	return svc
}

// mergePorts carries server-defaulted values forward.
//
// We deliberately never set targetPort, because the apiserver ignores it for a
// selector-less Service (I2) -- but it defaults the field anyway. Overwriting
// the live value with our zero would make the object differ from itself on
// every reconcile, producing an endless update loop against the apiserver.
func mergePorts(current, desired []corev1.ServicePort) []corev1.ServicePort {
	byName := make(map[string]corev1.ServicePort, len(current))
	for _, p := range current {
		byName[p.Name] = p
	}

	out := make([]corev1.ServicePort, 0, len(desired))
	for _, d := range desired {
		if c, ok := byName[d.Name]; ok {
			if d.TargetPort.IntValue() == 0 && d.TargetPort.StrVal == "" {
				d.TargetPort = c.TargetPort
			}
			if d.NodePort == 0 {
				d.NodePort = c.NodePort
			}
		}
		out = append(out, d)
	}
	return out
}

func mergeMap(base, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	out := map[string]string{}
	maps.Copy(out, base)
	maps.Copy(out, overlay)
	return out
}
