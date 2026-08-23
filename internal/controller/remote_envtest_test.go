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
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters/auth"
)

// The spoke apiserver accepts these two tokens. readerToken is bound to a role
// that can read pods and nodes; strangerToken is authenticated but granted
// nothing, which is what makes the rotation assertion meaningful.
const (
	readerToken   = "reader-token-aaaaaaaaaaaaaaaa"
	strangerToken = "stranger-token-bbbbbbbbbbbbbbbb"
)

// startSpoke brings up a second apiserver configured for token authentication,
// standing in for a remote cluster.
//
// This is the highest-value test in the suite because it exercises the real
// client path: credentials out of a Secret, a rest.Config built by the auth
// package, TLS against a CA, and informers over the wire. A fake client would
// prove none of that.
func startSpoke() (*envtest.Environment, *rest.Config, client.Client) {
	GinkgoHelper()

	dir, err := os.MkdirTemp("", "spoke-tokens")
	Expect(err).NotTo(HaveOccurred())

	tokenFile := filepath.Join(dir, "tokens.csv")
	Expect(os.WriteFile(tokenFile, fmt.Appendf(nil,
		"%s,remote-reader,uid-reader,\n%s,stranger,uid-stranger,\n", readerToken, strangerToken), 0o600)).To(Succeed())

	spoke := &envtest.Environment{}
	spoke.ControlPlane.GetAPIServer().Configure().Append("token-auth-file", tokenFile)

	cfg, err := spoke.Start()
	Expect(err).NotTo(HaveOccurred())

	spokeScheme := runtime.NewScheme()
	Expect(scheme.AddToScheme(spokeScheme)).To(Succeed())
	c, err := client.New(cfg, client.Options{Scheme: spokeScheme})
	Expect(err).NotTo(HaveOccurred())

	// Exactly the grant the docs ask a spoke operator for: read-only, on pods
	// and nodes, and nothing else (9.7).
	Expect(c.Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: spokeReaderRole},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods", "nodes", "services"},
			Verbs:     []string{"get", "list", "watch"},
		}, {
			APIGroups: []string{"discovery.k8s.io"},
			Resources: []string{"endpointslices"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	})).To(Succeed())

	Expect(c.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: spokeReaderRole},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: spokeReaderRole,
		},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "remote-reader"}},
	})).To(Succeed())

	return spoke, cfg, c
}

var _ = Describe("M4: a real remote cluster over the Token path", Ordered, func() {
	var (
		spokeEnv    *envtest.Environment
		spokeCfg    *rest.Config
		spokeClient client.Client
		provider    *clusters.CachingProvider
		builder     *auth.Builder
	)

	BeforeAll(func() {
		spokeEnv, spokeCfg, spokeClient = startSpoke()

		By("storing the spoke's credentials as Secrets in the controller's namespace")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: spokeTokenSecret, Namespace: credentialsNS},
			Data:       map[string][]byte{"token": []byte(readerToken)},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "spoke-ca", Namespace: credentialsNS},
			Data:       map[string][]byte{"ca.crt": spokeCfg.TLSClientConfig.CAData},
		})).To(Succeed())

		builder = &auth.Builder{
			Secrets: &auth.Secrets{Client: k8sClient, Namespace: credentialsNS},
			Options: auth.Options{CredentialsNamespace: credentialsNS},
		}

		provider = clusters.NewCachingProvider(ctx, builder, scheme.Scheme,
			func(c context.Context, name string) (*netv1alpha1.RemoteCluster, error) {
				var rc netv1alpha1.RemoteCluster
				if err := k8sClient.Get(c, types.NamespacedName{Name: name}, &rc); err != nil {
					return nil, err
				}
				return &rc, nil
			}, nil)

		By("declaring the spoke as a RemoteCluster")
		Expect(k8sClient.Create(ctx, &netv1alpha1.RemoteCluster{
			ObjectMeta: metav1.ObjectMeta{Name: spokeName},
			Spec: netv1alpha1.RemoteClusterSpec{
				Access: netv1alpha1.ClusterAccess{
					Type: netv1alpha1.AccessTypeToken,
					Token: &netv1alpha1.TokenAccess{
						Server:    spokeCfg.Host,
						SecretRef: netv1alpha1.SecretKeyRef{Name: spokeTokenSecret, Key: "token"},
					},
					TLS: &netv1alpha1.TLSConfig{
						CASecretRef: &netv1alpha1.SecretKeyRef{Name: "spoke-ca", Key: "ca.crt"},
					},
					// Short so the rotation assertion does not sit through the
					// 30s default once the new identity starts being refused.
					Timeout: &metav1.Duration{Duration: 2 * time.Second},
				},
				AllowedNamespaces: &netv1alpha1.NamespaceSelector{MatchNames: []string{nsDefault}},
			},
		})).To(Succeed())

		By("creating a Pod in the spoke for the resolvers to find")
		Expect(spokeClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "remote-app", Namespace: nsDefault, Labels: map[string]string{labelApp: "remote"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Image: "nginx",
				Ports: []corev1.ContainerPort{{Name: portHTTP, ContainerPort: 8080}},
			}}},
		})).To(Succeed())

		pod := &corev1.Pod{}
		Expect(spokeClient.Get(ctx, types.NamespacedName{Namespace: nsDefault, Name: "remote-app"}, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "10.99.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		}
		Expect(spokeClient.Status().Update(ctx, pod)).To(Succeed())
	})

	AfterAll(func() {
		if spokeEnv != nil {
			Expect(spokeEnv.Stop()).To(Succeed())
		}
	})

	It("authenticates and reports the spoke's version", func() {
		var rc netv1alpha1.RemoteCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: spokeName}, &rc)).To(Succeed())

		built, err := builder.Build(ctx, &rc)
		Expect(err).NotTo(HaveOccurred())

		version, err := clusters.Probe(ctx, built.Config)
		Expect(err).NotTo(HaveOccurred())
		Expect(version).To(HavePrefix("v1."))
	})

	It("reads Pods from the spoke through the real client path", func() {
		cl, err := provider.Get(ctx, spokeName)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() ([]corev1.Pod, error) {
			return cl.ListPods(ctx, nsDefault, nil)
		}, 10*time.Second, 200*time.Millisecond).Should(HaveLen(1))

		pods, err := cl.ListPods(ctx, nsDefault, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(pods[0].Status.PodIP).To(Equal("10.99.0.1"))
	})

	It("picks up a token rotation without a restart", func() {
		By("confirming the current credentials work")
		cl, err := provider.Get(ctx, spokeName)
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.ListPods(ctx, nsDefault, nil)).NotTo(BeNil())

		By("rotating the Secret to a token with no permissions")
		var secret corev1.Secret
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: credentialsNS, Name: spokeTokenSecret}, &secret)).To(Succeed())
		secret.Data["token"] = []byte(strangerToken)
		Expect(k8sClient.Update(ctx, &secret)).To(Succeed())

		// The new credentials hash differently, so the next Get must build a
		// fresh connection rather than serving the cached one. If the old
		// client were reused this would keep succeeding -- which is exactly the
		// silent breakage the fingerprint key exists to prevent.
		By("observing that the new identity is actually in use")
		Eventually(func() error {
			rotated, err := provider.Get(ctx, spokeName)
			if err != nil {
				return err
			}
			_, err = rotated.ListPods(ctx, nsDefault, nil)
			return err
		}, 15*time.Second, 250*time.Millisecond).Should(HaveOccurred(),
			"the stranger token has no RBAC, so reads must start failing once the rotation is picked up")
	})

	It("refuses a namespace that was never granted access", func() {
		var rc netv1alpha1.RemoteCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: spokeName}, &rc)).To(Succeed())

		r := &CrossServiceReconciler{Client: k8sClient, Scheme: scheme.Scheme}

		// nsDefault was granted; "kube-system" was not.
		Expect(r.checkNamespaceAllowed(ctx, &rc, nsDefault)).To(Succeed())
		Expect(r.checkNamespaceAllowed(ctx, &rc, "kube-system")).To(HaveOccurred())
	})
})
