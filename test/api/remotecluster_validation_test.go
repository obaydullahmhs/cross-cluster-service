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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

const (
	spokeServer    = "https://spoke.example.com"
	spokeTokenName = "spoke-token"
)

var rcCounter int

func newRemoteCluster(access netv1alpha1.ClusterAccess) *netv1alpha1.RemoteCluster {
	rcCounter++
	return &netv1alpha1.RemoteCluster{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("rc-%d", rcCounter)},
		Spec:       netv1alpha1.RemoteClusterSpec{Access: access},
	}
}

var _ = Describe("RemoteCluster CRD validation", func() {
	It("is cluster-scoped", func() {
		rc := newRemoteCluster(netv1alpha1.ClusterAccess{
			Type:      netv1alpha1.AccessTypeInCluster,
			InCluster: &netv1alpha1.InClusterAccess{},
		})
		Expect(k8sClient.Create(ctx, rc)).To(Succeed())
		Expect(rc.Namespace).To(BeEmpty())
	})

	It("applies the documented client defaults", func() {
		rc := newRemoteCluster(netv1alpha1.ClusterAccess{
			Type:      netv1alpha1.AccessTypeInCluster,
			InCluster: &netv1alpha1.InClusterAccess{},
		})
		Expect(k8sClient.Create(ctx, rc)).To(Succeed())

		Expect(rc.Spec.Access.Timeout.Duration.String()).To(Equal("30s"))
		Expect(rc.Spec.Access.QPS).To(Equal(int32(20)))
		Expect(rc.Spec.Access.Burst).To(Equal(int32(30)))
	})

	Describe("access discriminator", func() {
		It("rejects a Token access with no token payload", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{Type: netv1alpha1.AccessTypeToken})
			err := k8sClient.Create(ctx, rc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("token config is required"))
		})

		It("rejects a payload belonging to a different type", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeToken,
				Token: &netv1alpha1.TokenAccess{
					Server:    spokeServer,
					SecretRef: netv1alpha1.SecretKeyRef{Name: spokeTokenName},
				},
				Kubeconfig: &netv1alpha1.KubeconfigAccess{
					SecretRef: netv1alpha1.SecretKeyRef{Name: "spoke-kubeconfig"},
				},
			})
			err := k8sClient.Create(ctx, rc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("kubeconfig config is required if and only if"))
		})

		It("accepts a well-formed Token access", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeToken,
				Token: &netv1alpha1.TokenAccess{
					Server:    spokeServer,
					SecretRef: netv1alpha1.SecretKeyRef{Name: spokeTokenName, Key: "token"},
				},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
		})

		It("accepts a GoogleToken access with an explicit server and CA", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeGoogleToken,
				GoogleToken: &netv1alpha1.GoogleTokenAccess{
					Server:      "https://34.1.2.3",
					CASecretRef: &netv1alpha1.SecretKeyRef{Name: "spoke-ca", Key: "ca.crt"},
					Credentials: &netv1alpha1.GCPCredentials{
						ImpersonateServiceAccount: "reader@proj.iam.gserviceaccount.com",
					},
				},
				TLS: &netv1alpha1.TLSConfig{ServerName: "spoke.example.com"},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
		})
	})

	It("rejects a plaintext http server", func() {
		rc := newRemoteCluster(netv1alpha1.ClusterAccess{
			Type: netv1alpha1.AccessTypeToken,
			Token: &netv1alpha1.TokenAccess{
				Server:    "http://spoke.example.com",
				SecretRef: netv1alpha1.SecretKeyRef{Name: spokeTokenName},
			},
		})
		Expect(k8sClient.Create(ctx, rc)).To(HaveOccurred())
	})

	It("has no namespace field on SecretKeyRef", func() {
		// Secrets resolve only from --credentials-namespace. A namespace field
		// here would let a cluster-scoped CR name any Secret in the cluster,
		// which is a credential-exfiltration primitive.
		rc := newRemoteCluster(netv1alpha1.ClusterAccess{
			Type: netv1alpha1.AccessTypeToken,
			Token: &netv1alpha1.TokenAccess{
				Server:    spokeServer,
				SecretRef: netv1alpha1.SecretKeyRef{Name: spokeTokenName},
			},
		})
		Expect(k8sClient.Create(ctx, rc)).To(Succeed())

		unstructuredRC := unstructuredOf(rc)
		secretRef := digInto(unstructuredRC, "spec", "access", "token", "secretRef")
		Expect(secretRef).To(HaveKey("name"))
		Expect(secretRef).NotTo(HaveKey("namespace"))
	})

	Describe("allowedNamespaces", func() {
		It("accepts names, a selector, or both", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type:      netv1alpha1.AccessTypeInCluster,
				InCluster: &netv1alpha1.InClusterAccess{},
			})
			rc.Spec.AllowedNamespaces = &netv1alpha1.NamespaceSelector{
				MatchNames: []string{"team-a"},
				Selector:   &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			}
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
		})

		It("leaves nil as nil, so the controller fails closed", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type:      netv1alpha1.AccessTypeInCluster,
				InCluster: &netv1alpha1.InClusterAccess{},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			Expect(rc.Spec.AllowedNamespaces).To(BeNil())
		})
	})

	Describe("resyncPeriod", func() {
		It("defaults to 10m", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type:      netv1alpha1.AccessTypeInCluster,
				InCluster: &netv1alpha1.InClusterAccess{},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			Expect(rc.Spec.Access.ResyncPeriod).NotTo(BeNil())
			Expect(rc.Spec.Access.ResyncPeriod.Duration.String()).To(Equal("10m0s"))
		})

		It("accepts 0s to disable resync entirely", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type:         netv1alpha1.AccessTypeInCluster,
				InCluster:    &netv1alpha1.InClusterAccess{},
				ResyncPeriod: &metav1.Duration{},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// An explicit zero must survive round-tripping rather than being
			// silently replaced by the default.
			Expect(rc.Spec.Access.ResyncPeriod).NotTo(BeNil())
			Expect(rc.Spec.Access.ResyncPeriod.Duration).To(BeZero())
		})
	})

	Describe("WorkloadIdentity", func() {
		It("accepts a minimal WI access", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeWorkloadIdentity,
				WorkloadIdentity: &netv1alpha1.WorkloadIdentityAccess{
					Server:      spokeServer,
					CASecretRef: &netv1alpha1.SecretKeyRef{Name: "spoke-ca", Key: "ca.crt"},
				},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
		})

		It("accepts an asserted identity and impersonation", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeWorkloadIdentity,
				WorkloadIdentity: &netv1alpha1.WorkloadIdentityAccess{
					Server:                    spokeServer,
					ServiceAccountEmail:       "hub@my-project.iam.gserviceaccount.com",
					ImpersonateServiceAccount: "reader@my-project.iam.gserviceaccount.com",
				},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
		})

		It("has no service account key field in the schema", func() {
			// The absence is the security property: a static JSON key in a
			// WI-enabled cluster defeats the point of WI, so the type must make
			// that configuration unrepresentable rather than merely discouraged.
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeWorkloadIdentity,
				WorkloadIdentity: &netv1alpha1.WorkloadIdentityAccess{
					Server: spokeServer,
				},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			wi := digInto(unstructuredOf(rc), "spec", "access", "workloadIdentity")
			Expect(wi).To(HaveKey("server"))
			Expect(wi).NotTo(HaveKey("credentials"))
			Expect(wi).NotTo(HaveKey("serviceAccountKeySecretRef"))
		})

		It("rejects a workloadIdentity payload on a GoogleToken type", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeGoogleToken,
				GoogleToken: &netv1alpha1.GoogleTokenAccess{
					Server: spokeServer,
				},
				WorkloadIdentity: &netv1alpha1.WorkloadIdentityAccess{
					Server: spokeServer,
				},
			})
			err := k8sClient.Create(ctx, rc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("workloadIdentity config is required if and only if"))
		})
	})

	Describe("reserved cloud variants", func() {
		It("accepts EKS, which the controller reports as not implemented", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeEKS,
				EKS: &netv1alpha1.EKSAccess{
					Region:  "us-east-1",
					Cluster: "spoke-eks",
					Credentials: &netv1alpha1.AWSCredentials{
						AssumeRoleARN: "arn:aws:iam::123456789012:role/crossservice-reader",
					},
				},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
		})

		It("accepts AKS, which the controller reports as not implemented", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeAKS,
				AKS: &netv1alpha1.AKSAccess{
					SubscriptionID: "00000000-0000-0000-0000-000000000000",
					ResourceGroup:  "spokes",
					Cluster:        "spoke-aks",
				},
			})
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
		})

		It("still rejects a type outside the enum", func() {
			rc := newRemoteCluster(netv1alpha1.ClusterAccess{Type: netv1alpha1.AccessType("OpenShift")})
			Expect(k8sClient.Create(ctx, rc)).To(HaveOccurred())
		})
	})

	It("defaults denySpecialPurpose to true", func() {
		rc := newRemoteCluster(netv1alpha1.ClusterAccess{
			Type:      netv1alpha1.AccessTypeInCluster,
			InCluster: &netv1alpha1.InClusterAccess{},
		})
		rc.Spec.AddressPolicy = &netv1alpha1.AddressPolicy{}
		Expect(k8sClient.Create(ctx, rc)).To(Succeed())

		Expect(rc.Spec.AddressPolicy.DenySpecialPurpose).NotTo(BeNil())
		Expect(*rc.Spec.AddressPolicy.DenySpecialPurpose).To(BeTrue())
	})
})
