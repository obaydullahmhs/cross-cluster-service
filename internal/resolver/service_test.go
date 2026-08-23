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

package resolver

import (
	"context"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

func node(name, internal, external string, ready bool) corev1.Node {
	addrs := []corev1.NodeAddress{}
	if internal != "" {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: internal})
	}
	if external != "" {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: external})
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{netv1alpha1.ZoneLabel: zoneA}},
		Status: corev1.NodeStatus{
			Addresses:  addrs,
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

func TestNodeAddressSelection(t *testing.T) {
	both := node("both", addrA, addrLB, true)
	onlyInternal := node("int", "10.0.0.2", "", true)
	onlyExternal := node("ext", "", "35.1.1.2", true)

	cases := []struct {
		name string
		node corev1.Node
		at   netv1alpha1.NodeAddressType
		want string
		ok   bool
	}{
		{"InternalIP picks internal", both, netv1alpha1.NodeAddressTypeInternalIP, addrA, true},
		{"ExternalIP picks external", both, netv1alpha1.NodeAddressTypeExternalIP, addrLB, true},
		{"PreferExternal picks external when present", both, netv1alpha1.NodeAddressTypePreferExternal, addrLB, true},
		{"PreferExternal falls back to internal", onlyInternal, netv1alpha1.NodeAddressTypePreferExternal, "10.0.0.2", true},
		{"PreferInternal falls back to external", onlyExternal, netv1alpha1.NodeAddressTypePreferInternal, "35.1.1.2", true},
		{"ExternalIP has nothing to pick", onlyInternal, netv1alpha1.NodeAddressTypeExternalIP, "", false},
		{"empty defaults to InternalIP", both, "", addrA, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nodeAddress(&tc.node, tc.at)
			if ok != tc.ok || got != tc.want {
				t.Errorf("nodeAddress = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNodesFiltersAndReadiness(t *testing.T) {
	cordoned := node("cordoned", "10.0.0.3", "", true)
	cordoned.Spec.Unschedulable = true

	excluded := node("excluded", "10.0.0.4", "", true)
	excluded.Labels[excludeFromLBLabel] = ""

	fc := &fakeCluster{nodes: []corev1.Node{
		node("ok", addrA, "", true),
		node("notready", "10.0.0.2", "", false),
		cordoned,
		excluded,
	}}
	r := &Nodes{Provider: &fakeProvider{c: fc}}

	src := &netv1alpha1.Source{Type: netv1alpha1.SourceTypeNodes, Nodes: &netv1alpha1.NodeSource{}}
	res, err := r.Resolve(context.Background(), src, []netv1alpha1.CrossServicePort{{Port: 9100}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got := map[string]bool{}
	for _, e := range res.Endpoints {
		got[e.Address.String()] = e.Ready
	}
	// Cordoned and excluded nodes are dropped; a not-ready node is emitted as
	// a not-ready endpoint so the counts still describe reality.
	if len(got) != 2 {
		t.Fatalf("endpoints = %v, want 2", got)
	}
	if !got[addrA] {
		t.Error("ready node should be ready")
	}
	if got["10.0.0.2"] {
		t.Error("not-ready node should produce a not-ready endpoint, not a ready one")
	}
}

func TestNodesUsesDeclaredTargetPort(t *testing.T) {
	fc := &fakeCluster{nodes: []corev1.Node{node("n", addrA, "", true)}}
	r := &Nodes{Provider: &fakeProvider{c: fc}}

	tp := intstr.FromInt32(9100)
	res, err := r.Resolve(context.Background(),
		&netv1alpha1.Source{Type: netv1alpha1.SourceTypeNodes, Nodes: &netv1alpha1.NodeSource{}},
		[]netv1alpha1.CrossServicePort{{Port: 80, TargetPort: &tp}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// There is no Service to read a port from -- that is the reason this source
	// type exists separately from Service/NodePort.
	if got := res.Endpoints[0].PortMap[""]; got != 9100 {
		t.Errorf("port = %d, want the declared 9100", got)
	}
	if res.Endpoints[0].Zone != zoneA {
		t.Errorf("zone = %q, want %q", res.Endpoints[0].Zone, zoneA)
	}
}

func svcWithPorts(ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcAPI, Namespace: nsDefault},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: ports},
	}
}

func TestServiceViaNodePortReadsTheAllocatedPort(t *testing.T) {
	fc := &fakeCluster{
		// The nodePort is allocated by the remote cluster. Nothing in the
		// CrossService spec names 31234.
		svc:   svcWithPorts(corev1.ServicePort{Name: portHTTP, Port: 80, NodePort: 31234}),
		nodes: []corev1.Node{node("n1", addrA, "", true), node("n2", "10.0.0.2", "", true)},
	}
	r := &Service{Provider: &fakeProvider{c: fc}}

	res, err := r.Resolve(context.Background(),
		&netv1alpha1.Source{
			Type: netv1alpha1.SourceTypeService,
			Service: &netv1alpha1.ServiceSource{
				Namespace: nsDefault, Name: svcAPI, Via: netv1alpha1.ServiceExposureNodePort,
			},
		},
		[]netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Endpoints) != 2 {
		t.Fatalf("got %d endpoints, want one per node", len(res.Endpoints))
	}
	for _, e := range res.Endpoints {
		if e.PortMap[portHTTP] != 31234 {
			t.Errorf("port = %d, want the allocated nodePort 31234", e.PortMap[portHTTP])
		}
	}
}

func TestServiceViaNodePortFailsClearlyOnAClusterIPService(t *testing.T) {
	svc := svcWithPorts(corev1.ServicePort{Name: portHTTP, Port: 80})
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	fc := &fakeCluster{svc: svc, nodes: []corev1.Node{node("n1", addrA, "", true)}}
	r := &Service{Provider: &fakeProvider{c: fc}}

	_, err := r.Resolve(context.Background(),
		&netv1alpha1.Source{
			Type: netv1alpha1.SourceTypeService,
			Service: &netv1alpha1.ServiceSource{
				Namespace: nsDefault, Name: svcAPI, Via: netv1alpha1.ServiceExposureNodePort,
			},
		},
		[]netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80}})
	if err == nil {
		t.Fatal("expected an error: a ClusterIP Service has no nodePort to read")
	}
}

func TestServicePortBindingByNameAndOverride(t *testing.T) {
	fc := &fakeCluster{
		svc:   svcWithPorts(corev1.ServicePort{Name: "tls", Port: 443, NodePort: 32000}),
		nodes: []corev1.Node{node("n1", addrA, "", true)},
	}
	r := &Service{Provider: &fakeProvider{c: fc}}

	t.Run("remotePort overrides a name mismatch", func(t *testing.T) {
		remote := intstr.FromString("tls")
		res, err := r.Resolve(context.Background(),
			&netv1alpha1.Source{
				Type: netv1alpha1.SourceTypeService,
				Service: &netv1alpha1.ServiceSource{
					Namespace: nsDefault, Name: svcAPI, Via: netv1alpha1.ServiceExposureNodePort,
				},
			},
			// Locally called "https", remotely called "tls".
			[]netv1alpha1.CrossServicePort{{Name: "https", Port: 443, RemotePort: &remote}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got := res.Endpoints[0].PortMap["https"]; got != 32000 {
			t.Errorf("port = %d, want 32000 keyed by the LOCAL name", got)
		}
	})

	t.Run("a name that matches nothing is a clear error", func(t *testing.T) {
		_, err := r.Resolve(context.Background(),
			&netv1alpha1.Source{
				Type: netv1alpha1.SourceTypeService,
				Service: &netv1alpha1.ServiceSource{
					Namespace: nsDefault, Name: svcAPI, Via: netv1alpha1.ServiceExposureNodePort,
				},
			},
			[]netv1alpha1.CrossServicePort{{Name: "nope", Port: 443}})
		if err == nil {
			t.Fatal("expected an error naming the unmatched port")
		}
	})
}

func TestServiceViaLoadBalancerResolvesHostnames(t *testing.T) {
	svc := svcWithPorts(corev1.ServicePort{Name: portHTTP, Port: 80})
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer

	t.Run("an IP ingress is used directly", func(t *testing.T) {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: addrLB}}
		r := &Service{Provider: &fakeProvider{c: &fakeCluster{svc: svc}}}

		res, err := r.Resolve(context.Background(), lbSource(), []netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if res.Endpoints[0].Address.String() != addrLB {
			t.Errorf("address = %s, want the ingress IP", res.Endpoints[0].Address)
		}
		if res.TTL != 0 {
			t.Errorf("TTL = %v, want 0: an IP ingress is purely watch-driven", res.TTL)
		}
	})

	t.Run("a hostname ingress is resolved and requests a requeue", func(t *testing.T) {
		// AWS ELB and NLB return a hostname. addressType FQDN is ignored by
		// kube-proxy (I11), so it has to be resolved here and kept fresh.
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "lb.elb.amazonaws.com"}}
		fake := &fakeLookup{addrs: map[string][]netip.Addr{
			"lb.elb.amazonaws.com.": mustAddrs(t, "52.1.1.1", "52.1.1.2"),
		}}
		r := &Service{Provider: &fakeProvider{c: &fakeCluster{svc: svc}}, Lookup: fake}

		res, err := r.Resolve(context.Background(), lbSource(), []netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(res.Endpoints) != 2 {
			t.Fatalf("got %d endpoints, want 2", len(res.Endpoints))
		}
		if res.TTL == 0 {
			t.Error("a hostname ingress must request a requeue, or it goes stale silently")
		}
		if fake.queried[0] != "lb.elb.amazonaws.com." {
			t.Errorf("queried %q, want the fully-qualified name (I10)", fake.queried[0])
		}
	})
}

func lbSource() *netv1alpha1.Source {
	return &netv1alpha1.Source{
		Type: netv1alpha1.SourceTypeService,
		Service: &netv1alpha1.ServiceSource{
			Namespace: nsDefault, Name: svcAPI, Via: netv1alpha1.ServiceExposureLoadBalancer,
		},
	}
}

func TestServiceViaPodIPReadsRemoteSlices(t *testing.T) {
	name := portHTTP
	port := int32(8080)
	ready := true
	zone := zoneA

	fc := &fakeCluster{
		svc: svcWithPorts(corev1.ServicePort{Name: portHTTP, Port: 80}),
		slices: []discoveryv1.EndpointSlice{{
			ObjectMeta:  metav1.ObjectMeta{Name: "api-abc", Namespace: nsDefault},
			AddressType: discoveryv1.AddressTypeIPv4,
			Ports:       []discoveryv1.EndpointPort{{Name: &name, Port: &port}},
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.244.1.5"},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
				Zone:       &zone,
			}},
		}},
	}
	r := &Service{Provider: &fakeProvider{c: fc}}

	res, err := r.Resolve(context.Background(),
		&netv1alpha1.Source{
			Type: netv1alpha1.SourceTypeService,
			Service: &netv1alpha1.ServiceSource{
				Namespace: nsDefault, Name: svcAPI, Via: netv1alpha1.ServiceExposurePodIP,
			},
		},
		[]netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Endpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(res.Endpoints))
	}

	e := res.Endpoints[0]
	// Readiness, resolved port and zone all come across already computed, which
	// is the reason for reading slices rather than re-deriving from Pods.
	if e.Address.String() != "10.244.1.5" || !e.Ready || e.PortMap[portHTTP] != 8080 || e.Zone != zoneA {
		t.Errorf("endpoint = %+v, want the remote slice's address, readiness, port and zone", e)
	}
	// targetRef would dangle across clusters (I6).
	if e.TargetRef != nil {
		t.Error("targetRef must not be carried across from a remote slice")
	}
}
