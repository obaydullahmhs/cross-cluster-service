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

package endpoints

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
)

// DefaultMaxEndpointsPerSlice matches the upstream EndpointSlice controller's
// default.
const DefaultMaxEndpointsPerSlice = 100

// Writer reconciles the EndpointSlices for one CrossService.
type Writer struct {
	Client               client.Client
	MaxEndpointsPerSlice int
}

// Outcome reports what the writer did, for status and for tests that assert
// an unchanged input produces no apiserver traffic at all (I7).
type Outcome struct {
	Created []string
	Updated []string
	Deleted []string
	// Unchanged are slices that already matched and were not written.
	Unchanged []string
}

// Writes is the total number of apiserver mutations performed.
func (o Outcome) Writes() int { return len(o.Created) + len(o.Updated) + len(o.Deleted) }

// SliceNames is every slice the CrossService now owns.
func (o Outcome) SliceNames() []string {
	out := append([]string{}, o.Created...)
	out = append(out, o.Updated...)
	out = append(out, o.Unchanged...)
	slices.Sort(out)
	return out
}

// Reconcile writes the slices for the given groups and removes anything else
// this CrossService owns.
func (w *Writer) Reconcile(
	ctx context.Context,
	xsvc *netv1alpha1.CrossService,
	serviceName string,
	groups []Group,
	owner func(client.Object) error,
) (Outcome, error) {
	maxPer := w.MaxEndpointsPerSlice
	if maxPer <= 0 {
		maxPer = DefaultMaxEndpointsPerSlice
	}

	existing, err := w.list(ctx, xsvc.Namespace, serviceName)
	if err != nil {
		return Outcome{}, err
	}

	plan, orphans := planSlices(existing, groups, serviceName, maxPer)

	var out Outcome
	for _, p := range plan {
		desired := buildSlice(xsvc, serviceName, p)
		if err := owner(desired); err != nil {
			return out, fmt.Errorf("setting owner on %s: %w", desired.Name, err)
		}

		if p.Existing == nil {
			if err := w.Client.Create(ctx, desired); err != nil {
				return out, fmt.Errorf("creating %s: %w", desired.Name, err)
			}
			out.Created = append(out.Created, desired.Name)
			continue
		}

		if sliceEqual(p.Existing, desired) {
			// The whole point of canonicalising: an unchanged answer arriving
			// in a different order must not cost a write, because every slice
			// write invalidates every kube-proxy's cache.
			out.Unchanged = append(out.Unchanged, desired.Name)
			continue
		}

		merged := p.Existing.DeepCopy()
		merged.Labels = desired.Labels
		merged.OwnerReferences = desired.OwnerReferences
		merged.AddressType = desired.AddressType
		merged.Ports = desired.Ports
		merged.Endpoints = desired.Endpoints

		if err := w.Client.Update(ctx, merged); err != nil {
			return out, fmt.Errorf("updating %s: %w", merged.Name, err)
		}
		out.Updated = append(out.Updated, merged.Name)
	}

	for _, o := range orphans {
		if err := w.Client.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
			return out, fmt.Errorf("deleting %s: %w", o.Name, err)
		}
		out.Deleted = append(out.Deleted, o.Name)
	}

	slices.Sort(out.Created)
	slices.Sort(out.Updated)
	slices.Sort(out.Deleted)
	slices.Sort(out.Unchanged)
	return out, nil
}

func (w *Writer) list(ctx context.Context, namespace, serviceName string) ([]discoveryv1.EndpointSlice, error) {
	var list discoveryv1.EndpointSliceList
	err := w.Client.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingLabels{
			netv1alpha1.ServiceNameLabel: serviceName,
			netv1alpha1.ManagedByLabel:   netv1alpha1.ManagedByLabelValue,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("listing endpointslices: %w", err)
	}

	slices.SortFunc(list.Items, func(a, b discoveryv1.EndpointSlice) int { return strings.Compare(a.Name, b.Name) })
	return list.Items, nil
}

// sliceePlan is one slice to write.
type slicePlan struct {
	Name        string
	AddressType discoveryv1.AddressType
	Ports       PortTuple
	Endpoints   []resolver.Endpoint

	// Existing is nil for a slice that does not exist yet.
	Existing *discoveryv1.EndpointSlice
}

// planSlices assigns endpoints to slices, preferring to keep each endpoint
// where it already lives.
//
// This is invariant I12. The naive approach -- sort everything and chunk it --
// reshuffles every slice whenever a single endpoint is added near the front,
// and each of those writes invalidates every kube-proxy's cache in the cluster.
// So: retain what is already correctly placed, fill spare capacity in name
// order, and only then create.
func planSlices(
	existing []discoveryv1.EndpointSlice,
	groups []Group,
	serviceName string,
	maxPer int,
) ([]slicePlan, []*discoveryv1.EndpointSlice) {
	// Index existing slices by the group they belong to, derived from their own
	// contents rather than from their name, so a rename or a hand-edit cannot
	// desynchronise the mapping.
	byGroup := map[GroupKey][]*discoveryv1.EndpointSlice{}
	var orphans []*discoveryv1.EndpointSlice

	wanted := map[GroupKey]*Group{}
	for i := range groups {
		wanted[GroupKey{AddressType: groups[i].AddressType, PortKey: groups[i].Ports.Key()}] = &groups[i]
	}

	usedNames := map[string]bool{}
	for i := range existing {
		s := &existing[i]
		usedNames[s.Name] = true

		key := GroupKey{AddressType: s.AddressType, PortKey: portTupleOfSlice(s).Key()}
		if _, ok := wanted[key]; !ok {
			orphans = append(orphans, s)
			continue
		}
		byGroup[key] = append(byGroup[key], s)
	}

	var plans []slicePlan

	for i := range groups {
		g := &groups[i]
		key := GroupKey{AddressType: g.AddressType, PortKey: g.Ports.Key()}

		remaining := map[string]resolver.Endpoint{}
		for _, e := range g.Endpoints {
			remaining[e.Address.String()] = e
		}

		owned := byGroup[key]
		slices.SortFunc(owned, func(a, b *discoveryv1.EndpointSlice) int {
			return strings.Compare(a.Name, b.Name)
		})

		assigned := make([][]resolver.Endpoint, len(owned))

		// Pass 1: keep endpoints on the slice that already carries them.
		for si, s := range owned {
			for _, ep := range s.Endpoints {
				for _, addr := range ep.Addresses {
					if e, ok := remaining[addr]; ok && len(assigned[si]) < maxPer {
						assigned[si] = append(assigned[si], e)
						delete(remaining, addr)
					}
				}
			}
		}

		// Pass 2: fill spare capacity in name order before creating anything.
		leftover := sortedRemaining(remaining)
		next := 0
		for si := range owned {
			for len(assigned[si]) < maxPer && next < len(leftover) {
				assigned[si] = append(assigned[si], leftover[next])
				next++
			}
		}

		for si, s := range owned {
			if len(assigned[si]) == 0 {
				// Nothing left for this slice: drop it rather than leaving an
				// empty object behind.
				orphans = append(orphans, s)
				continue
			}
			eps := assigned[si]
			SortEndpoints(eps)
			plans = append(plans, slicePlan{
				Name:        s.Name,
				AddressType: g.AddressType,
				Ports:       g.Ports,
				Endpoints:   eps,
				Existing:    s,
			})
		}

		// Pass 3: whatever is still unplaced needs new slices.
		for next < len(leftover) {
			chunk := leftover[next:]
			if len(chunk) > maxPer {
				chunk = chunk[:maxPer]
			}
			next += len(chunk)

			name := nextSliceName(serviceName, g.AddressType, usedNames)
			usedNames[name] = true

			eps := append([]resolver.Endpoint{}, chunk...)
			SortEndpoints(eps)
			plans = append(plans, slicePlan{
				Name:        name,
				AddressType: g.AddressType,
				Ports:       g.Ports,
				Endpoints:   eps,
			})
		}
	}

	slices.SortFunc(plans, func(a, b slicePlan) int { return strings.Compare(a.Name, b.Name) })
	return plans, orphans
}

func sortedRemaining(m map[string]resolver.Endpoint) []resolver.Endpoint {
	out := make([]resolver.Endpoint, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	SortEndpoints(out)
	return out
}

// nextSliceName returns the lowest unused <service>-<family>-<n>.
func nextSliceName(serviceName string, at discoveryv1.AddressType, used map[string]bool) string {
	family := "ipv4"
	if at == discoveryv1.AddressTypeIPv6 {
		family = "ipv6"
	}
	for n := 0; ; n++ {
		name := fmt.Sprintf("%s-%s-%d", truncateForSuffix(serviceName, len(family)+8), family, n)
		if !used[name] {
			return name
		}
	}
}

// truncateForSuffix keeps the generated name within the 253-character object
// name limit.
func truncateForSuffix(name string, suffixLen int) string {
	maxBase := 253 - suffixLen
	if len(name) <= maxBase {
		return name
	}
	return strings.TrimRight(name[:maxBase], "-")
}

func portTupleOfSlice(s *discoveryv1.EndpointSlice) PortTuple {
	out := make(PortTuple, 0, len(s.Ports))
	for _, p := range s.Ports {
		spec := PortSpec{AppProtocol: p.AppProtocol, Protocol: corev1.ProtocolTCP}
		if p.Name != nil {
			spec.Name = *p.Name
		}
		if p.Port != nil {
			spec.Port = *p.Port
		}
		if p.Protocol != nil {
			spec.Protocol = *p.Protocol
		}
		out = append(out, spec)
	}
	slices.SortFunc(out, func(a, b PortSpec) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// buildSlice renders a plan into its desired object.
func buildSlice(xsvc *netv1alpha1.CrossService, serviceName string, p slicePlan) *discoveryv1.EndpointSlice {
	ports := make([]discoveryv1.EndpointPort, 0, len(p.Ports))
	for _, ps := range p.Ports {
		name, port, proto := ps.Name, ps.Port, ps.Protocol
		ports = append(ports, discoveryv1.EndpointPort{
			Name:        &name,
			Port:        &port,
			Protocol:    &proto,
			AppProtocol: ps.AppProtocol,
		})
	}

	eps := make([]discoveryv1.Endpoint, 0, len(p.Endpoints))
	for _, e := range p.Endpoints {
		ready, serving, terminating := e.Ready, e.Serving, e.Terminating
		ep := discoveryv1.Endpoint{
			Addresses: []string{e.Address.String()},
			Conditions: discoveryv1.EndpointConditions{
				Ready:       &ready,
				Serving:     &serving,
				Terminating: &terminating,
			},
			TargetRef: e.TargetRef,
			// NodeName is deliberately never set. It names a node in the LOCAL
			// cluster, so a foreign value breaks internalTrafficPolicy: Local
			// and topology routing (I5). Zone carries the topology instead.
		}
		if e.Zone != "" {
			zone := e.Zone
			ep.Zone = &zone
		}
		if e.Hostname != "" {
			hostname := e.Hostname
			ep.Hostname = &hostname
		}
		eps = append(eps, ep)
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: xsvc.Namespace,
			Labels: map[string]string{
				// Both labels are required. The first is how kube-proxy finds
				// the slice; the second is what stops the built-in
				// EndpointSlice mirroring controller from fighting us for
				// ownership (I8).
				netv1alpha1.ServiceNameLabel:           serviceName,
				netv1alpha1.ManagedByLabel:             netv1alpha1.ManagedByLabelValue,
				netv1alpha1.CrossServiceNameLabel:      xsvc.Name,
				netv1alpha1.CrossServiceNamespaceLabel: xsvc.Namespace,
			},
		},
		AddressType: p.AddressType,
		Ports:       ports,
		Endpoints:   eps,
	}
}

// sliceEqual compares semantically, after canonicalising both sides.
func sliceEqual(existing, desired *discoveryv1.EndpointSlice) bool {
	if existing.AddressType != desired.AddressType {
		// addressType is immutable, so a mismatch is never an update -- the
		// caller must delete and recreate. Reporting inequality here surfaces
		// it as a failed update rather than silent drift (I3).
		return false
	}
	for k, v := range desired.Labels {
		if existing.Labels[k] != v {
			return false
		}
	}

	a := canonicalSlice(existing)
	b := canonicalSlice(desired)
	return apiequality.Semantic.DeepEqual(a.Ports, b.Ports) &&
		apiequality.Semantic.DeepEqual(a.Endpoints, b.Endpoints)
}

// canonicalSlice returns a copy with ports and endpoints in a stable order.
func canonicalSlice(s *discoveryv1.EndpointSlice) *discoveryv1.EndpointSlice {
	out := s.DeepCopy()

	slices.SortFunc(out.Ports, func(a, b discoveryv1.EndpointPort) int {
		return strings.Compare(derefString(a.Name), derefString(b.Name))
	})
	for i := range out.Endpoints {
		slices.Sort(out.Endpoints[i].Addresses)
	}
	slices.SortFunc(out.Endpoints, func(a, b discoveryv1.Endpoint) int {
		return strings.Compare(strings.Join(a.Addresses, ","), strings.Join(b.Addresses, ","))
	})
	return out
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
