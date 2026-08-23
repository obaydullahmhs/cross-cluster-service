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
	"net/netip"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
)

// fakeResolver returns a scripted answer, so the reconcile path can be
// exercised without any real DNS or remote cluster.
type fakeResolver struct {
	mu        sync.Mutex
	endpoints []resolver.Endpoint
	err       error
	calls     int
}

func (f *fakeResolver) Resolve(_ context.Context, _ *netv1alpha1.Source, _ []netv1alpha1.CrossServicePort) (*resolver.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &resolver.Result{Endpoints: append([]resolver.Endpoint{}, f.endpoints...)}, nil
}

func (f *fakeResolver) set(eps []resolver.Endpoint, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoints, f.err = eps, err
}

// countingClient counts every mutation, so "this reconcile performed zero
// writes" can be asserted directly rather than inferred.
type countingClient struct {
	client.Client
	mu                     sync.Mutex
	creates, updates, dels int
}

func (c *countingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.mu.Lock()
	c.creates++
	c.mu.Unlock()
	return c.Client.Create(ctx, obj, opts...)
}

func (c *countingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.mu.Lock()
	c.updates++
	c.mu.Unlock()
	return c.Client.Update(ctx, obj, opts...)
}

func (c *countingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.mu.Lock()
	c.dels++
	c.mu.Unlock()
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *countingClient) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates + c.updates + c.dels
}

func (c *countingClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates, c.updates, c.dels = 0, 0, 0
}

var xsvcSeq int

func mkEndpoint(address string, ports map[string]int32) resolver.Endpoint {
	return resolver.Endpoint{
		Address: netip.MustParseAddr(address),
		Ready:   true,
		Serving: true,
		PortMap: ports,
	}
}

var _ = Describe("CrossService reconcile", func() {
	var (
		counting *countingClient
		fake     *fakeResolver
		r        *CrossServiceReconciler
		xsvc     *netv1alpha1.CrossService
		key      types.NamespacedName
		now      time.Time
	)

	BeforeEach(func() {
		counting = &countingClient{Client: k8sClient}
		fake = &fakeResolver{}
		now = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

		r = &CrossServiceReconciler{
			Client:   counting,
			Scheme:   scheme.Scheme,
			Recorder: record.NewFakeRecorder(64),
			Resolver: fake,
			Now:      func() time.Time { return now },
		}

		xsvcSeq++
		xsvc = &netv1alpha1.CrossService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("xsvc-%d", xsvcSeq),
				Namespace: "default",
			},
			Spec: netv1alpha1.CrossServiceSpec{
				Ports: []netv1alpha1.CrossServicePort{{Port: 80}},
				Source: netv1alpha1.Source{
					Type:   netv1alpha1.SourceTypeStatic,
					Static: &netv1alpha1.StaticSource{Addresses: []string{"10.0.0.1"}},
				},
			},
		}
		key = client.ObjectKeyFromObject(xsvc)
	})

	reconcile := func() ctrl.Result {
		GinkgoHelper()
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	slicesFor := func() []discoveryv1.EndpointSlice {
		GinkgoHelper()
		var list discoveryv1.EndpointSliceList
		Expect(k8sClient.List(ctx, &list,
			client.InNamespace(xsvc.Namespace),
			client.MatchingLabels{netv1alpha1.CrossServiceNameLabel: xsvc.Name},
		)).To(Succeed())
		return list.Items
	}

	Describe("I1: the Service must be selector-less", func() {
		It("creates a Service with a nil selector", func() {
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{"": 80})}, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())

			// A non-nil selector hands ownership to the built-in EndpointSlice
			// controller, which then deletes our slices in a loop.
			Expect(svc.Spec.Selector).To(BeNil())
		})

		It("strips a selector that was added out of band", func() {
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{"": 80})}, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
			svc.Spec.Selector = map[string]string{"app": "hijacked"}
			Expect(k8sClient.Update(ctx, &svc)).To(Succeed())

			reconcile()

			Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
			Expect(svc.Spec.Selector).To(BeNil())
		})
	})

	Describe("I2: port names are the join key", func() {
		It("gives a single unnamed port an empty name on both sides", func() {
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{"": 8080})}, nil)
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Port: 80, TargetPort: intstrPtr(8080)}}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
			Expect(svc.Spec.Ports[0].Name).To(BeEmpty())

			s := slicesFor()
			Expect(s).To(HaveLen(1))
			Expect(s[0].Ports).To(HaveLen(1))
			Expect(*s[0].Ports[0].Name).To(BeEmpty())
			// targetPort feeds the SLICE port, never the Service's.
			Expect(*s[0].Ports[0].Port).To(Equal(int32(8080)))
		})

		It("matches named ports between the Service and the slice", func() {
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{portHTTP: 8080, "grpc": 9090})}, nil)
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{
				{Name: portHTTP, Port: 80, TargetPort: intstrPtr(8080)},
				{Name: "grpc", Port: 9000, TargetPort: intstrPtr(9090)},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())

			svcNames := make([]string, 0, len(svc.Spec.Ports))
			for _, p := range svc.Spec.Ports {
				svcNames = append(svcNames, p.Name)
			}
			sliceNames := make([]string, 0, len(slicesFor()[0].Ports))
			for _, p := range slicesFor()[0].Ports {
				sliceNames = append(sliceNames, *p.Name)
			}
			Expect(sliceNames).To(ConsistOf(svcNames))
		})
	})

	Describe("I3: IPv4 and IPv6 need separate slices", func() {
		It("writes one slice per address family", func() {
			fake.set([]resolver.Endpoint{
				mkEndpoint("10.0.0.1", map[string]int32{"": 80}),
				mkEndpoint("2001:db8::1", map[string]int32{"": 80}),
			}, nil)
			xsvc.Spec.IPFamilyPolicy = netv1alpha1.IPFamilyPolicyPreferDualStack
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			s := slicesFor()
			Expect(s).To(HaveLen(2))

			types := map[discoveryv1.AddressType]bool{}
			for _, sl := range s {
				types[sl.AddressType] = true
			}
			Expect(types).To(HaveKey(discoveryv1.AddressTypeIPv4))
			Expect(types).To(HaveKey(discoveryv1.AddressTypeIPv6))
		})
	})

	Describe("I7: an unchanged answer in a different order costs zero writes", func() {
		It("performs no apiserver writes on a reordered but identical result", func() {
			eps := []resolver.Endpoint{
				mkEndpoint("10.0.0.1", map[string]int32{"": 80}),
				mkEndpoint("10.0.0.2", map[string]int32{"": 80}),
				mkEndpoint("10.0.0.3", map[string]int32{"": 80}),
			}
			fake.set(eps, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			// Settle any status write from the first pass.
			reconcile()
			counting.reset()

			// Same set, the order DNS happened to return it in this time.
			fake.set([]resolver.Endpoint{eps[2], eps[0], eps[1]}, nil)
			reconcile()

			Expect(counting.writes()).To(Equal(0),
				"a reordered but identical answer must not write: every slice write invalidates every kube-proxy cache")
		})
	})

	Describe("I8: required slice labels", func() {
		It("labels slices so the built-in mirroring controller stays away", func() {
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{"": 80})}, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			s := slicesFor()[0]
			Expect(s.Labels[netv1alpha1.ServiceNameLabel]).To(Equal(xsvc.Name))
			Expect(s.Labels[netv1alpha1.ManagedByLabel]).To(Equal("crossservice.net.obaydullah.dev"))
		})
	})

	Describe("I13: owner references are namespace-local", func() {
		It("sets the CrossService as controller of both the Service and the slices", func() {
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{"": 80})}, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
			Expect(svc.OwnerReferences).To(HaveLen(1))
			Expect(svc.OwnerReferences[0].Kind).To(Equal("CrossService"))
			Expect(*svc.OwnerReferences[0].Controller).To(BeTrue())

			s := slicesFor()[0]
			Expect(s.OwnerReferences).To(HaveLen(1))
			Expect(s.OwnerReferences[0].Kind).To(Equal("CrossService"))
			Expect(s.Namespace).To(Equal(xsvc.Namespace))
		})
	})

	Describe("I5: never set nodeName on endpoints", func() {
		It("carries zone instead of nodeName", func() {
			e := mkEndpoint("10.0.0.1", map[string]int32{"": 80})
			e.Zone = "us-central1-a"
			fake.set([]resolver.Endpoint{e}, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			ep := slicesFor()[0].Endpoints[0]
			// A foreign nodeName breaks internalTrafficPolicy: Local and
			// topology routing.
			Expect(ep.NodeName).To(BeNil())
			Expect(*ep.Zone).To(Equal("us-central1-a"))
		})
	})

	Describe("security 9.3: the metadata server is not a valid backend", func() {
		It("drops 169.254.169.254 and records it", func() {
			fake.set([]resolver.Endpoint{
				mkEndpoint("10.0.0.1", map[string]int32{"": 80}),
				mkEndpoint("169.254.169.254", map[string]int32{"": 80}),
			}, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			addresses := []string{}
			for _, ep := range slicesFor()[0].Endpoints {
				addresses = append(addresses, ep.Addresses...)
			}
			Expect(addresses).To(ConsistOf("10.0.0.1"))

			var got netv1alpha1.CrossService
			Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
			Expect(got.Status.DroppedAddresses).To(Equal(int32(1)))

			events := r.Recorder.(*record.FakeRecorder).Events
			Expect(events).To(Receive(ContainSubstring(netv1alpha1.ReasonAddressPolicyRejected)))
		})
	})

	Describe("I9: a failing source does not black-hole traffic", func() {
		It("keeps serving below the threshold, then applies OnStale", func() {
			fake.set([]resolver.Endpoint{
				mkEndpoint("10.0.0.1", map[string]int32{"": 80}),
				mkEndpoint("10.0.0.2", map[string]int32{"": 80}),
			}, nil)
			xsvc.Spec.FailurePolicy = &netv1alpha1.FailurePolicy{
				FailureThreshold: 2,
				StaleThreshold:   &metav1.Duration{Duration: 5 * time.Minute},
				OnStale:          netv1alpha1.StaleActionMarkNotReady,
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()
			Expect(slicesFor()[0].Endpoints).To(HaveLen(2))

			By("failing once, still under the threshold")
			fake.set(nil, fmt.Errorf("CoreDNS blip"))
			reconcile()

			eps := slicesFor()[0].Endpoints
			Expect(eps).To(HaveLen(2), "a blip must not empty the slice")
			Expect(*eps[0].Conditions.Ready).To(BeTrue())

			By("failing past the threshold and past the stale window")
			now = now.Add(10 * time.Minute)
			reconcile()

			eps = slicesFor()[0].Endpoints
			Expect(eps).To(HaveLen(2), "MarkNotReady keeps the addresses")
			Expect(*eps[0].Conditions.Ready).To(BeFalse(), "but flips them not-ready")

			var got netv1alpha1.CrossService
			Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
			Expect(got.Status.Source.Stale).To(BeTrue())
		})
	})

	Describe("slice packing", func() {
		It("packs 250 endpoints into 3 slices and cleans up when it drops to 40", func() {
			mk := func(n int) []resolver.Endpoint {
				out := make([]resolver.Endpoint, 0, n)
				for i := range n {
					out = append(out, mkEndpoint(fmt.Sprintf("10.1.%d.%d", i/250, i%250+1), map[string]int32{"": 80}))
				}
				return out
			}

			fake.set(mk(250), nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()
			Expect(slicesFor()).To(HaveLen(3))

			fake.set(mk(40), nil)
			reconcile()

			s := slicesFor()
			Expect(s).To(HaveLen(1), "emptied slices must be deleted, not left orphaned")
			Expect(s[0].Endpoints).To(HaveLen(40))
		})
	})

	Describe("requeue behaviour", func() {
		It("does not requeue a Static source", func() {
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{"": 80})}, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())

			// Static is watch-driven; a timer here would be pure waste.
			Expect(reconcile().RequeueAfter).To(BeZero())
		})

		It("requeues a DNS source on its interval", func() {
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{"": 80})}, nil)
			xsvc.Spec.Source = netv1alpha1.Source{
				Type: netv1alpha1.SourceTypeDNS,
				DNS: &netv1alpha1.DNSSource{
					Names:         []string{"db.example.com."},
					DNSResolution: netv1alpha1.DNSResolution{Interval: &metav1.Duration{Duration: 45 * time.Second}},
				},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())

			Expect(reconcile().RequeueAfter).To(Equal(45 * time.Second))
		})
	})

	Describe("status", func() {
		It("reports counts, the service name and observedGeneration", func() {
			notReady := mkEndpoint("10.0.0.2", map[string]int32{"": 80})
			notReady.Ready = false
			fake.set([]resolver.Endpoint{mkEndpoint("10.0.0.1", map[string]int32{"": 80}), notReady}, nil)
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
			reconcile()

			var got netv1alpha1.CrossService
			Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
			Expect(got.Status.ServiceName).To(Equal(xsvc.Name))
			Expect(got.Status.TotalEndpoints).To(Equal(int32(2)))
			Expect(got.Status.ReadyEndpoints).To(Equal(int32(1)))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Status.Source.Type).To(Equal(netv1alpha1.SourceTypeStatic))
		})
	})
})

func intstrPtr(i int32) *intstr.IntOrString {
	v := intstr.FromInt32(i)
	return &v
}
