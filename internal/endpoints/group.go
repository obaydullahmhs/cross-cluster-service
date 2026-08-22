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
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
)

// PortSpec is one resolved port on a slice.
type PortSpec struct {
	Name        string
	Port        int32
	Protocol    corev1.Protocol
	AppProtocol *string
}

// PortTuple is the full set of ports on a slice, canonically ordered.
//
// EndpointSlice.ports is per-slice, not per-endpoint, so two endpoints whose
// ports differ cannot share a slice (I4). The tuple is therefore part of the
// grouping key rather than something written per endpoint.
type PortTuple []PortSpec

// Key is a stable identity for the tuple, used for grouping and slice naming.
func (pt PortTuple) Key() string {
	parts := make([]string, 0, len(pt))
	for _, p := range pt {
		app := ""
		if p.AppProtocol != nil {
			app = *p.AppProtocol
		}
		parts = append(parts, fmt.Sprintf("%s/%d/%s/%s", p.Name, p.Port, p.Protocol, app))
	}
	return strings.Join(parts, ",")
}

// portTupleFor builds the tuple an endpoint belongs to. A declared port the
// endpoint could not resolve is omitted, which is what fragments slices.
func portTupleFor(e resolver.Endpoint, ports []netv1alpha1.CrossServicePort) PortTuple {
	out := make(PortTuple, 0, len(ports))
	for _, p := range ports {
		resolved, ok := e.PortMap[p.Name]
		if !ok {
			continue
		}
		out = append(out, PortSpec{
			Name:        p.Name,
			Port:        resolved,
			Protocol:    protocolOrDefault(p.Protocol),
			AppProtocol: p.AppProtocol,
		})
	}
	slices.SortFunc(out, func(a, b PortSpec) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func protocolOrDefault(p corev1.Protocol) corev1.Protocol {
	if p == "" {
		return corev1.ProtocolTCP
	}
	return p
}

// AddressTypeOf returns the EndpointSlice addressType for an endpoint.
//
// Only IPv4 and IPv6 are ever produced. addressType FQDN exists in the API but
// kube-proxy ignores it, so names are resolved to addresses here instead (I11).
func AddressTypeOf(e resolver.Endpoint) discoveryv1.AddressType {
	if e.Address.Is6() {
		return discoveryv1.AddressTypeIPv6
	}
	return discoveryv1.AddressTypeIPv4
}

// Group is one (addressType, portTuple) bucket, which maps to one family of
// EndpointSlices.
type Group struct {
	AddressType discoveryv1.AddressType
	Ports       PortTuple
	Endpoints   []resolver.Endpoint

	// Index disambiguates multiple port tuples sharing an address family, so
	// their slice names do not collide.
	Index int
}

// GroupKey identifies a group.
type GroupKey struct {
	AddressType discoveryv1.AddressType
	PortKey     string
}

// Group partitions endpoints by (addressType, portTuple).
//
// The result is deterministic: groups are ordered by address type then port
// key, and endpoints within a group are sorted by address. Without this, DNS
// round-robin and unstable List order would produce a write on every reconcile
// (I7).
func GroupEndpoints(in []resolver.Endpoint, ports []netv1alpha1.CrossServicePort, families FamilyFilter) []Group {
	byKey := map[GroupKey]*Group{}

	for _, e := range in {
		at := AddressTypeOf(e)
		if !families.Allows(at) {
			continue
		}

		tuple := portTupleFor(e, ports)
		if len(tuple) == 0 {
			// Every declared port failed to resolve for this endpoint; it has
			// nothing to be reached on.
			continue
		}

		key := GroupKey{AddressType: at, PortKey: tuple.Key()}
		g, ok := byKey[key]
		if !ok {
			g = &Group{AddressType: at, Ports: tuple}
			byKey[key] = g
		}
		g.Endpoints = append(g.Endpoints, e)
	}

	out := make([]Group, 0, len(byKey))
	for _, g := range byKey {
		SortEndpoints(g.Endpoints)
		out = append(out, *g)
	}

	slices.SortFunc(out, func(a, b Group) int {
		if a.AddressType != b.AddressType {
			return strings.Compare(string(a.AddressType), string(b.AddressType))
		}
		return strings.Compare(a.Ports.Key(), b.Ports.Key())
	})

	// Index is assigned per address family, so the common single-tuple case
	// yields index 0 and the shortest possible slice names.
	perFamily := map[discoveryv1.AddressType]int{}
	for i := range out {
		out[i].Index = perFamily[out[i].AddressType]
		perFamily[out[i].AddressType]++
	}
	return out
}

// SortEndpoints canonicalises endpoint order.
//
// DNS round-robins its answers and List order is not stable across informer
// resyncs, so comparing unsorted collections would report a difference on every
// reconcile and produce a write loop against the apiserver (I7).
func SortEndpoints(in []resolver.Endpoint) {
	slices.SortFunc(in, func(a, b resolver.Endpoint) int {
		return a.Address.Compare(b.Address)
	})
}

// FamilyFilter restricts which address families are written. IPv4 and IPv6
// always land in separate slices, because addressType is immutable and
// single-valued (I3).
type FamilyFilter netv1alpha1.IPFamilyPolicy

// Allows reports whether this family should be written.
func (f FamilyFilter) Allows(at discoveryv1.AddressType) bool {
	switch netv1alpha1.IPFamilyPolicy(f) {
	case netv1alpha1.IPFamilyPolicyIPv6:
		return at == discoveryv1.AddressTypeIPv6
	case netv1alpha1.IPFamilyPolicyPreferDualStack:
		return true
	default:
		return at == discoveryv1.AddressTypeIPv4
	}
}

// Dedupe removes endpoints sharing an address, preferring ready over not-ready.
func Dedupe(in []resolver.Endpoint) []resolver.Endpoint {
	seen := map[netip.Addr]int{}
	out := make([]resolver.Endpoint, 0, len(in))

	for _, e := range in {
		idx, ok := seen[e.Address]
		if !ok {
			seen[e.Address] = len(out)
			out = append(out, e)
			continue
		}
		if e.Ready && !out[idx].Ready {
			out[idx] = e
		}
	}
	return out
}
