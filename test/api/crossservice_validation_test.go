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

package api

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

const (
	testNamespace   = "default"
	portNameHTTP    = "http"
	remoteNamespace = "payments"
	remoteSvcName   = "api"
	nodePoolEdge    = "edge"
	nodePoolLabel   = "pool"
)

var xsvcCounter int

// newCrossService returns a minimal valid CrossService with a unique name, so
// specs never collide on the shared apiserver.
func newCrossService() *netv1alpha1.CrossService {
	xsvcCounter++
	return &netv1alpha1.CrossService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("xsvc-%d", xsvcCounter),
			Namespace: testNamespace,
		},
		Spec: netv1alpha1.CrossServiceSpec{
			Ports: []netv1alpha1.CrossServicePort{{Port: 80}},
			Source: netv1alpha1.Source{
				Type:   netv1alpha1.SourceTypeStatic,
				Static: &netv1alpha1.StaticSource{Addresses: []string{"10.0.0.1"}},
			},
		},
	}
}

func intOrStr(s string) *intstr.IntOrString {
	v := intstr.FromString(s)
	return &v
}

func intOrNum(i int32) *intstr.IntOrString {
	v := intstr.FromInt32(i)
	return &v
}

var _ = Describe("CrossService CRD validation", func() {
	It("accepts a minimal single-unnamed-port Static source", func() {
		Expect(k8sClient.Create(ctx, newCrossService())).To(Succeed())
	})

	It("applies the documented defaults", func() {
		xsvc := newCrossService()
		Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())

		Expect(xsvc.Spec.IPFamilyPolicy).To(Equal(netv1alpha1.IPFamilyPolicyIPv4))
		Expect(xsvc.Spec.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
	})

	Describe("port names", func() {
		It("rejects a multi-port spec with an unnamed port", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{
				{Name: portNameHTTP, Port: 80},
				{Port: 443},
			}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("port name is required when more than one port is defined"))
		})

		It("rejects duplicate port names", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{
				{Name: portNameHTTP, Port: 80},
				{Name: portNameHTTP, Port: 8080},
			}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("port names must be unique"))
		})

		It("accepts distinct names on a multi-port spec", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{
				{Name: portNameHTTP, Port: 80},
				{Name: "https", Port: 443},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
		})

		It("rejects a port name that is not a valid IANA_SVC_NAME", func() {
			for _, bad := range []string{"HTTP", "-http", "http-", "ht--tp", "1234", "waytoolongportname"} {
				xsvc := newCrossService()
				xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Name: bad, Port: 80}}
				Expect(k8sClient.Create(ctx, xsvc)).To(HaveOccurred(), "port name %q should be rejected", bad)
			}
		})
	})

	Describe("targetPort", func() {
		It("rejects a string targetPort on a non-Pods source", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Port: 80, TargetPort: intOrStr("http")}}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("only valid for a Pods source"))
		})

		It("accepts a numeric targetPort on a non-Pods source", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Port: 80, TargetPort: intOrNum(8080)}}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
		})

		It("accepts a string targetPort on a Pods source", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Port: 80, TargetPort: intOrStr("http")}}
			xsvc.Spec.Source = netv1alpha1.Source{
				Type: netv1alpha1.SourceTypePods,
				Pods: &netv1alpha1.PodSource{
					Namespace: testNamespace,
					Selector:  &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
		})
	})

	Describe("source discriminator", func() {
		It("rejects a payload that does not match the type", func() {
			xsvc := newCrossService()
			xsvc.Spec.Source = netv1alpha1.Source{
				Type:   netv1alpha1.SourceTypeDNS,
				Static: &netv1alpha1.StaticSource{Addresses: []string{"10.0.0.1"}},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(HaveOccurred())
		})

		It("rejects a type with no payload", func() {
			xsvc := newCrossService()
			xsvc.Spec.Source = netv1alpha1.Source{Type: netv1alpha1.SourceTypeDNS}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("dns config is required"))
		})

		It("rejects clusterRef on a DNS source", func() {
			xsvc := newCrossService()
			xsvc.Spec.Source = netv1alpha1.Source{
				Type:       netv1alpha1.SourceTypeDNS,
				ClusterRef: &netv1alpha1.ClusterRef{Name: "spoke"},
				DNS:        &netv1alpha1.DNSSource{Names: []string{"db.example.com."}},
			}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("clusterRef is only valid for Service, Pods and Nodes"))
		})

		It("accepts clusterRef on a Nodes source", func() {
			xsvc := newCrossService()
			xsvc.Spec.Source = netv1alpha1.Source{
				Type:       netv1alpha1.SourceTypeNodes,
				ClusterRef: &netv1alpha1.ClusterRef{Name: "spoke"},
				Nodes: &netv1alpha1.NodeSource{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nodePoolLabel: nodePoolEdge}},
				},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
		})
	})

	Describe("selector versus names", func() {
		It("rejects a Pods source with neither", func() {
			xsvc := newCrossService()
			xsvc.Spec.Source = netv1alpha1.Source{
				Type: netv1alpha1.SourceTypePods,
				Pods: &netv1alpha1.PodSource{Namespace: testNamespace},
			}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of selector or names"))
		})

		It("accepts a Nodes source with neither, meaning every eligible node", func() {
			xsvc := newCrossService()
			xsvc.Spec.Source = netv1alpha1.Source{
				Type:  netv1alpha1.SourceTypeNodes,
				Nodes: &netv1alpha1.NodeSource{},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
		})

		It("rejects a Nodes source with both", func() {
			xsvc := newCrossService()
			xsvc.Spec.Source = netv1alpha1.Source{
				Type: netv1alpha1.SourceTypeNodes,
				Nodes: &netv1alpha1.NodeSource{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nodePoolLabel: nodePoolEdge}},
					Names:    []string{"node-1"},
				},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(HaveOccurred())
		})
	})

	Describe("Service source", func() {
		newServiceSource := func(via netv1alpha1.ServiceExposure) *netv1alpha1.CrossService {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Name: portNameHTTP, Port: 80}}
			xsvc.Spec.Source = netv1alpha1.Source{
				Type:       netv1alpha1.SourceTypeService,
				ClusterRef: &netv1alpha1.ClusterRef{Name: "spoke-a"},
				Service: &netv1alpha1.ServiceSource{
					Namespace: remoteNamespace,
					Name:      remoteSvcName,
					Via:       via,
				},
			}
			return xsvc
		}

		It("accepts each exposure mode", func() {
			for _, via := range []netv1alpha1.ServiceExposure{
				netv1alpha1.ServiceExposureNodePort,
				netv1alpha1.ServiceExposureLoadBalancer,
				netv1alpha1.ServiceExposurePodIP,
			} {
				Expect(k8sClient.Create(ctx, newServiceSource(via))).To(Succeed(), "via %s", via)
			}
		})

		It("rejects an exposure payload belonging to a different via", func() {
			xsvc := newServiceSource(netv1alpha1.ServiceExposurePodIP)
			xsvc.Spec.Source.Service.NodePort = &netv1alpha1.NodePortExposure{}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("nodePort config is only valid when via is NodePort"))
		})

		It("rejects ClusterIP, which is deliberately not an exposure mode", func() {
			Expect(k8sClient.Create(ctx, newServiceSource("ClusterIP"))).To(HaveOccurred())
		})

		It("defaults the nodePort exposure to InternalIP over every eligible node", func() {
			xsvc := newServiceSource(netv1alpha1.ServiceExposureNodePort)
			xsvc.Spec.Source.Service.NodePort = &netv1alpha1.NodePortExposure{}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())

			np := xsvc.Spec.Source.Service.NodePort
			Expect(np.AddressType).To(Equal(netv1alpha1.NodeAddressTypeInternalIP))
			Expect(np.Selector).To(BeNil())
			Expect(np.Names).To(BeEmpty())
			Expect(np.RequireReady).NotTo(BeNil())
			Expect(*np.RequireReady).To(BeTrue())
		})

		It("rejects selector and names together on a nodePort exposure", func() {
			xsvc := newServiceSource(netv1alpha1.ServiceExposureNodePort)
			xsvc.Spec.Source.Service.NodePort = &netv1alpha1.NodePortExposure{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nodePoolLabel: nodePoolEdge}},
				Names:    []string{"node-1"},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(HaveOccurred())
		})

		It("defaults hostnameResolution for a LoadBalancer that returns a hostname", func() {
			xsvc := newServiceSource(netv1alpha1.ServiceExposureLoadBalancer)
			xsvc.Spec.Source.Service.LoadBalancer = &netv1alpha1.LoadBalancerExposure{
				HostnameResolution: &netv1alpha1.DNSResolution{},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())

			res := xsvc.Spec.Source.Service.LoadBalancer.HostnameResolution
			Expect(res.Interval.Duration.String()).To(Equal("30s"))
			Expect(res.MinTTL.Duration.String()).To(Equal("5s"))
		})
	})

	Describe("Service source port mapping", func() {
		It("rejects targetPort, whose backend port is derived from the remote Service", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Name: portNameHTTP, Port: 80, TargetPort: intOrNum(8080)}}
			xsvc.Spec.Source = netv1alpha1.Source{
				Type:    netv1alpha1.SourceTypeService,
				Service: &netv1alpha1.ServiceSource{Namespace: remoteNamespace, Name: remoteSvcName, Via: netv1alpha1.ServiceExposureNodePort},
			}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("targetPort is not valid for a Service source"))
		})

		It("accepts remotePort as the name override", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Name: "https", Port: 443, RemotePort: intOrStr("tls")}}
			xsvc.Spec.Source = netv1alpha1.Source{
				Type:    netv1alpha1.SourceTypeService,
				Service: &netv1alpha1.ServiceSource{Namespace: remoteNamespace, Name: remoteSvcName, Via: netv1alpha1.ServiceExposureLoadBalancer},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())
		})

		It("rejects remotePort on a non-Service source", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Port: 80, RemotePort: intOrNum(8080)}}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("remotePort is only valid for a Service source"))
		})

		It("rejects targetPort and remotePort together", func() {
			xsvc := newCrossService()
			xsvc.Spec.Ports = []netv1alpha1.CrossServicePort{{Port: 80, TargetPort: intOrNum(8080), RemotePort: intOrNum(9090)}}
			err := k8sClient.Create(ctx, xsvc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
		})
	})

	Describe("DNS source defaults", func() {
		It("defaults recordType, interval, and the TTL clamps", func() {
			xsvc := newCrossService()
			xsvc.Spec.Source = netv1alpha1.Source{
				Type: netv1alpha1.SourceTypeDNS,
				DNS:  &netv1alpha1.DNSSource{Names: []string{"db.example.com."}},
			}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())

			dns := xsvc.Spec.Source.DNS
			Expect(dns.RecordType).To(Equal(netv1alpha1.DNSRecordTypeA))
			Expect(dns.Interval.Duration.String()).To(Equal("30s"))
			Expect(dns.MinTTL.Duration.String()).To(Equal("5s"))
			Expect(dns.MaxTTL.Duration.String()).To(Equal("5m0s"))
			Expect(dns.UseTTL).To(BeFalse())
		})
	})

	Describe("failure policy defaults", func() {
		It("defaults to MarkNotReady, never immediate Remove", func() {
			xsvc := newCrossService()
			xsvc.Spec.FailurePolicy = &netv1alpha1.FailurePolicy{}
			Expect(k8sClient.Create(ctx, xsvc)).To(Succeed())

			fp := xsvc.Spec.FailurePolicy
			Expect(fp.OnStale).To(Equal(netv1alpha1.StaleActionMarkNotReady))
			Expect(fp.FailureThreshold).To(Equal(int32(3)))
			Expect(fp.StaleThreshold.Duration.String()).To(Equal("5m0s"))
		})
	})
})
