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
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
)

func ep(t *testing.T, address string, ports map[string]int32) resolver.Endpoint {
	t.Helper()
	return resolver.Endpoint{Address: addr(t, address), Ready: true, Serving: true, PortMap: ports}
}

// TestI7_SortBeforeDiffing covers invariant I7.
func TestI7_SortBeforeDiffing(t *testing.T) {
	ports := []netv1alpha1.CrossServicePort{{Port: 80}}
	pm := map[string]int32{"": 80}

	// The same set, in the order DNS happened to return it each time.
	first := []resolver.Endpoint{ep(t, "10.0.0.3", pm), ep(t, addrA, pm), ep(t, "10.0.0.2", pm)}
	second := []resolver.Endpoint{ep(t, "10.0.0.2", pm), ep(t, "10.0.0.3", pm), ep(t, addrA, pm)}

	g1 := GroupEndpoints(first, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
	g2 := GroupEndpoints(second, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))

	if len(g1) != 1 || len(g2) != 1 {
		t.Fatalf("want one group each, got %d and %d", len(g1), len(g2))
	}
	for i := range g1[0].Endpoints {
		if g1[0].Endpoints[i].Address != g2[0].Endpoints[i].Address {
			t.Fatalf("grouping is order-dependent at %d: %v vs %v",
				i, g1[0].Endpoints[i].Address, g2[0].Endpoints[i].Address)
		}
	}
}

// TestI3_AddressTypeIsSingleValued covers invariant I3.
func TestI3_AddressTypeIsSingleValued(t *testing.T) {
	ports := []netv1alpha1.CrossServicePort{{Port: 80}}
	pm := map[string]int32{"": 80}
	in := []resolver.Endpoint{
		ep(t, addrA, pm),
		ep(t, "2001:db8::1", pm),
		ep(t, "10.0.0.2", pm),
		ep(t, "2001:db8::2", pm),
	}

	t.Run("dual stack splits into two groups", func(t *testing.T) {
		groups := GroupEndpoints(in, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyPreferDualStack))
		if len(groups) != 2 {
			t.Fatalf("got %d groups, want 2", len(groups))
		}
		seen := map[discoveryv1.AddressType]int{}
		for _, g := range groups {
			seen[g.AddressType] = len(g.Endpoints)
		}
		if seen[discoveryv1.AddressTypeIPv4] != 2 || seen[discoveryv1.AddressTypeIPv6] != 2 {
			t.Errorf("group sizes = %v, want 2 of each family", seen)
		}
	})

	t.Run("IPv4 policy drops IPv6", func(t *testing.T) {
		groups := GroupEndpoints(in, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
		if len(groups) != 1 || groups[0].AddressType != discoveryv1.AddressTypeIPv4 {
			t.Fatalf("got %d groups (%v), want one IPv4 group", len(groups), groups)
		}
	})

	t.Run("IPv6 policy drops IPv4", func(t *testing.T) {
		groups := GroupEndpoints(in, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv6))
		if len(groups) != 1 || groups[0].AddressType != discoveryv1.AddressTypeIPv6 {
			t.Fatalf("got %d groups (%v), want one IPv6 group", len(groups), groups)
		}
	})
}

// TestI4_NamedPortsFragmentSlices covers invariant I4.
func TestI4_NamedPortsFragmentSlices(t *testing.T) {
	ports := []netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80}}

	// portHTTP resolves to 8080 on some backends and 9090 on others. Because
	// EndpointSlice.ports is per-slice rather than per-endpoint, these cannot
	// share a slice.
	in := []resolver.Endpoint{
		ep(t, addrA, map[string]int32{portHTTP: 8080}),
		ep(t, "10.0.0.2", map[string]int32{portHTTP: 9090}),
		ep(t, "10.0.0.3", map[string]int32{portHTTP: 8080}),
	}

	groups := GroupEndpoints(in, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 -- differing resolved ports must fragment", len(groups))
	}

	sizes := map[string]int{}
	for _, g := range groups {
		sizes[g.Ports.Key()] = len(g.Endpoints)
	}
	if sizes["http/8080/TCP/"] != 2 || sizes["http/9090/TCP/"] != 1 {
		t.Errorf("group sizes = %v, want 2 on 8080 and 1 on 9090", sizes)
	}

	// Distinct indexes keep the two families of slice names from colliding.
	if groups[0].Index == groups[1].Index {
		t.Errorf("both groups have index %d, want distinct indexes", groups[0].Index)
	}
}

func TestGroupEndpointsDropsUnresolvableEndpoints(t *testing.T) {
	ports := []netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80}}
	in := []resolver.Endpoint{
		ep(t, addrA, map[string]int32{portHTTP: 8080}),
		// The named port did not resolve on this backend: it has nothing to be
		// reached on, so it is dropped rather than written with a bogus port.
		ep(t, "10.0.0.2", map[string]int32{}),
	}

	groups := GroupEndpoints(in, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
	if len(groups) != 1 || len(groups[0].Endpoints) != 1 {
		t.Fatalf("got %d groups with %v endpoints, want one group of one", len(groups), groups)
	}
}

func TestDedupePrefersReady(t *testing.T) {
	pm := map[string]int32{"": 80}
	notReady := ep(t, addrA, pm)
	notReady.Ready = false

	out := Dedupe([]resolver.Endpoint{notReady, ep(t, addrA, pm), ep(t, "10.0.0.2", pm)})
	if len(out) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(out))
	}
	for _, e := range out {
		if e.Address.String() == addrA && !e.Ready {
			t.Error("dedupe kept the not-ready duplicate, want ready preferred")
		}
	}
}

// TestI12_SlicePackingIsStable covers invariant I12.
func TestI12_SlicePackingIsStable(t *testing.T) {
	ports := []netv1alpha1.CrossServicePort{{Port: 80}}
	pm := map[string]int32{"": 80}

	mkEndpoints := func(n int) []resolver.Endpoint {
		out := make([]resolver.Endpoint, 0, n)
		for i := range n {
			out = append(out, ep(t, fmt.Sprintf("10.0.%d.%d", i/250, i%250+1), pm))
		}
		return out
	}

	t.Run("250 endpoints pack into 3 slices", func(t *testing.T) {
		groups := GroupEndpoints(mkEndpoints(250), ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
		plans, orphans := planSlices(nil, groups, "api", 100)

		if len(plans) != 3 {
			t.Fatalf("got %d slices, want 3", len(plans))
		}
		if len(orphans) != 0 {
			t.Errorf("got %d orphans on a fresh pack, want 0", len(orphans))
		}
		total := 0
		for _, p := range plans {
			if len(p.Endpoints) > 100 {
				t.Errorf("slice %s holds %d endpoints, want at most 100", p.Name, len(p.Endpoints))
			}
			total += len(p.Endpoints)
		}
		if total != 250 {
			t.Errorf("packed %d endpoints, want 250", total)
		}
	})

	t.Run("adding one endpoint does not repack existing slices", func(t *testing.T) {
		base := mkEndpoints(150)
		groups := GroupEndpoints(base, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
		plans, _ := planSlices(nil, groups, "api", 100)

		existing := make([]discoveryv1.EndpointSlice, 0, len(plans))
		for _, p := range plans {
			s := discoveryv1.EndpointSlice{
				ObjectMeta:  metav1.ObjectMeta{Name: p.Name},
				AddressType: p.AddressType,
			}
			for _, ps := range p.Ports {
				name, port, proto := ps.Name, ps.Port, ps.Protocol
				s.Ports = append(s.Ports, discoveryv1.EndpointPort{Name: &name, Port: &port, Protocol: &proto})
			}
			for _, e := range p.Endpoints {
				s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{Addresses: []string{e.Address.String()}})
			}
			existing = append(existing, s)
		}

		// One new endpoint sorting BEFORE everything already placed. A naive
		// sort-and-chunk packer would shift every endpoint by one and rewrite
		// every slice; each of those writes invalidates every kube-proxy cache.
		grown := append([]resolver.Endpoint{ep(t, "10.0.0.250", pm)}, base...)
		grownGroups := GroupEndpoints(grown, ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
		newPlans, orphans := planSlices(existing, grownGroups, "api", 100)

		if len(orphans) != 0 {
			t.Errorf("got %d orphans, want 0", len(orphans))
		}

		for _, p := range newPlans {
			if p.Existing == nil {
				continue
			}
			before := map[string]bool{}
			for _, e := range p.Existing.Endpoints {
				before[e.Addresses[0]] = true
			}
			for _, e := range p.Endpoints {
				a := e.Address.String()
				if !before[a] && a != "10.0.0.250" {
					t.Errorf("slice %s gained %s, which was already placed elsewhere", p.Name, a)
				}
			}
		}
	})

	t.Run("dropping to 40 endpoints leaves no orphaned slices", func(t *testing.T) {
		groups := GroupEndpoints(mkEndpoints(250), ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
		plans, _ := planSlices(nil, groups, "api", 100)

		existing := make([]discoveryv1.EndpointSlice, 0, len(plans))
		for _, p := range plans {
			s := discoveryv1.EndpointSlice{
				ObjectMeta:  metav1.ObjectMeta{Name: p.Name},
				AddressType: p.AddressType,
			}
			for _, ps := range p.Ports {
				name, port, proto := ps.Name, ps.Port, ps.Protocol
				s.Ports = append(s.Ports, discoveryv1.EndpointPort{Name: &name, Port: &port, Protocol: &proto})
			}
			for _, e := range p.Endpoints {
				s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{Addresses: []string{e.Address.String()}})
			}
			existing = append(existing, s)
		}

		shrunk := GroupEndpoints(mkEndpoints(40), ports, FamilyFilter(netv1alpha1.IPFamilyPolicyIPv4))
		newPlans, orphans := planSlices(existing, shrunk, "api", 100)

		kept := 0
		for _, p := range newPlans {
			kept += len(p.Endpoints)
		}
		if kept != 40 {
			t.Errorf("kept %d endpoints, want 40", kept)
		}
		// Emptied slices must be deleted, not left behind holding nothing.
		if len(newPlans)+len(orphans) != 3 {
			t.Errorf("plans+orphans = %d, want the original 3 slices accounted for", len(newPlans)+len(orphans))
		}
		for _, p := range newPlans {
			if len(p.Endpoints) == 0 {
				t.Errorf("slice %s planned with zero endpoints, want it orphaned instead", p.Name)
			}
		}
	})
}

func TestSliceNamesAreFamilyQualified(t *testing.T) {
	// I3: IPv4 and IPv6 need distinct objects, so their names must not collide.
	used := map[string]bool{}
	v4 := nextSliceName("api", discoveryv1.AddressTypeIPv4, used)
	used[v4] = true
	v6 := nextSliceName("api", discoveryv1.AddressTypeIPv6, used)

	if v4 != "api-ipv4-0" {
		t.Errorf("IPv4 slice name = %q, want api-ipv4-0", v4)
	}
	if v6 != "api-ipv6-0" {
		t.Errorf("IPv6 slice name = %q, want api-ipv6-0", v6)
	}
}
