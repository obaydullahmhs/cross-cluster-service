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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

var watchSeq int

// makePod creates a Pod and gives it the status a kubelet would, since envtest
// runs no kubelet of its own.
func makePod(name, ip string, labels map[string]string) *corev1.Pod {
	GinkgoHelper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: watchNamespace, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  labelApp,
				Image: "nginx",
				Ports: []corev1.ContainerPort{{Name: portHTTP, ContainerPort: 8080}},
			}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())

	pod.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		PodIP:      ip,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	return pod
}

var _ = Describe("I14: endpoints are watch-driven, not polled", func() {
	var (
		xsvc *netv1alpha1.CrossService
		key  client.ObjectKey
		app  string
	)

	BeforeEach(func() {
		watchSeq++
		app = fmt.Sprintf("app-%d", watchSeq)

		xsvc = &netv1alpha1.CrossService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("watched-%d", watchSeq),
				Namespace: watchNamespace,
			},
			Spec: netv1alpha1.CrossServiceSpec{
				Ports: []netv1alpha1.CrossServicePort{{Port: 80}},
				Source: netv1alpha1.Source{
					Type: netv1alpha1.SourceTypePods,
					Pods: &netv1alpha1.PodSource{
						Namespace: watchNamespace,
						Selector:  &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: app}},
					},
				},
			},
		}
		key = client.ObjectKeyFromObject(xsvc)
	})

	// addresses returns every address currently written across the slices.
	addresses := func() []string {
		var list discoveryv1.EndpointSliceList
		if err := k8sClient.List(ctx, &list,
			client.InNamespace(watchNamespace),
			client.MatchingLabels{netv1alpha1.CrossServiceNameLabel: xsvc.Name},
		); err != nil {
			return nil
		}
		out := []string{}
		for _, s := range list.Items {
			for _, e := range s.Endpoints {
				out = append(out, e.Addresses...)
			}
		}
		return out
	}

	It("picks up scale up and scale down without any requeue timer", func() {
		Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())

		By("the Service appearing once the CrossService is reconciled")
		Eventually(func() error {
			var svc corev1.Service
			return k8sClient.Get(ctx, key, &svc)
		}, 5*time.Second, 50*time.Millisecond).Should(Succeed())

		By("scaling up to two Pods")
		makePod(app+"-1", "10.244.0.1", map[string]string{labelApp: app})
		makePod(app+"-2", "10.244.0.2", map[string]string{labelApp: app})

		// A short timeout is the assertion. The reconciler returns a zero
		// RequeueAfter for a Pods source, so nothing but a watch event can
		// drive this -- a polling implementation would simply time out here.
		Eventually(addresses, 2*time.Second, 50*time.Millisecond).
			Should(ConsistOf("10.244.0.1", "10.244.0.2"))

		By("scaling up by one more")
		makePod(app+"-3", "10.244.0.3", map[string]string{labelApp: app})
		Eventually(addresses, 2*time.Second, 50*time.Millisecond).
			Should(ConsistOf("10.244.0.1", "10.244.0.2", "10.244.0.3"))

		By("scaling back down")
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: app + "-3", Namespace: watchNamespace}}
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())

		Eventually(addresses, 2*time.Second, 50*time.Millisecond).
			Should(ConsistOf("10.244.0.1", "10.244.0.2"))
	})

	It("reflects a Pod going not-ready", func() {
		Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
		pod := makePod(app+"-1", "10.244.1.1", map[string]string{labelApp: app})

		Eventually(addresses, 2*time.Second, 50*time.Millisecond).Should(ConsistOf("10.244.1.1"))

		By("flipping the Pod's Ready condition to False")
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// The address stays, but stops being routable: removing it outright
		// would be indistinguishable from the Pod disappearing.
		Eventually(func() bool {
			var list discoveryv1.EndpointSliceList
			if err := k8sClient.List(ctx, &list,
				client.InNamespace(watchNamespace),
				client.MatchingLabels{netv1alpha1.CrossServiceNameLabel: xsvc.Name},
			); err != nil || len(list.Items) == 0 {
				return true
			}
			for _, s := range list.Items {
				for _, e := range s.Endpoints {
					if e.Conditions.Ready != nil && *e.Conditions.Ready {
						return true
					}
				}
			}
			return false
		}, 2*time.Second, 50*time.Millisecond).Should(BeFalse(), "the endpoint should have gone not-ready")
	})
})

var _ = Describe("source reference index", func() {
	It("maps each source type to the objects it depends on", func() {
		cases := []struct {
			name string
			src  netv1alpha1.Source
			want []string
		}{
			{
				name: "pods source depends on its namespace",
				src: netv1alpha1.Source{
					Type: netv1alpha1.SourceTypePods,
					Pods: &netv1alpha1.PodSource{Namespace: nsPayments},
				},
				want: []string{"pods|payments"},
			},
			{
				name: "nodes source depends on nodes",
				src:  netv1alpha1.Source{Type: netv1alpha1.SourceTypeNodes, Nodes: &netv1alpha1.NodeSource{}},
				want: []string{keyNodes},
			},
			{
				name: "service via NodePort depends on the Service AND on nodes",
				src: netv1alpha1.Source{
					Type: netv1alpha1.SourceTypeService,
					Service: &netv1alpha1.ServiceSource{
						Namespace: nsPayments, Name: svcAPI, Via: netv1alpha1.ServiceExposureNodePort,
					},
				},
				want: []string{keySvcAPI, keyNodes},
			},
			{
				name: "service via PodIP depends on the Service AND its slices",
				src: netv1alpha1.Source{
					Type: netv1alpha1.SourceTypeService,
					Service: &netv1alpha1.ServiceSource{
						Namespace: nsPayments, Name: svcAPI, Via: netv1alpha1.ServiceExposurePodIP,
					},
				},
				want: []string{keySvcAPI, "slices|payments/api"},
			},
			{
				name: "service via LoadBalancer depends only on the Service",
				src: netv1alpha1.Source{
					Type: netv1alpha1.SourceTypeService,
					Service: &netv1alpha1.ServiceSource{
						Namespace: nsPayments, Name: svcAPI, Via: netv1alpha1.ServiceExposureLoadBalancer,
					},
				},
				want: []string{keySvcAPI},
			},
			{
				name: "static source depends on nothing",
				src: netv1alpha1.Source{
					Type:   netv1alpha1.SourceTypeStatic,
					Static: &netv1alpha1.StaticSource{Addresses: []string{"10.0.0.1"}},
				},
				want: nil,
			},
			{
				// A remote source is driven by that cluster's own informers,
				// so indexing it against local events would be wrong.
				name: "a remote source is not indexed locally",
				src: netv1alpha1.Source{
					Type:       netv1alpha1.SourceTypePods,
					ClusterRef: &netv1alpha1.ClusterRef{Name: "spoke-a"},
					Pods:       &netv1alpha1.PodSource{Namespace: nsPayments},
				},
				want: nil,
			},
		}

		for _, tc := range cases {
			got := sourceRefKeys(&netv1alpha1.CrossService{Spec: netv1alpha1.CrossServiceSpec{Source: tc.src}})
			Expect(got).To(ConsistOf(toAny(tc.want)...), tc.name)
		}
	})
})

func toAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
