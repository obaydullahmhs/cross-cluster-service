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

package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

const (
	credsNS       = "crossservice-system"
	keyToken      = "token"
	testServerURL = "https://example.invalid"
)

func newBuilder(t *testing.T, opts Options, secrets ...*corev1.Secret) *Builder {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]runtime.Object, 0, len(secrets))
	for _, s := range secrets {
		objs = append(objs, s)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	if opts.CredentialsNamespace == "" {
		opts.CredentialsNamespace = credsNS
	}
	return &Builder{
		Secrets: &Secrets{Client: c, Namespace: opts.CredentialsNamespace},
		Options: opts,
	}
}

func secret(ns, name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
}

// TestSecretsResolveOnlyFromTheControllerNamespace covers security 9.1.
func TestSecretsResolveOnlyFromTheControllerNamespace(t *testing.T) {
	// The same Secret name exists in two namespaces with different contents.
	// Nothing in the API can select the one outside the controller's namespace,
	// because SecretKeyRef has no namespace field at all.
	b := newBuilder(t, Options{},
		secret(credsNS, "creds", map[string][]byte{keyToken: []byte("correct")}),
		secret("attacker", "creds", map[string][]byte{keyToken: []byte("stolen")}),
	)

	got, err := b.Secrets.Value(context.Background(),
		netv1alpha1.SecretKeyRef{Name: "creds", Key: keyToken})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if string(got) != "correct" {
		t.Errorf("read %q, want the controller namespace's copy", got)
	}
}

func TestSecretsFallBackThroughDefaultKeys(t *testing.T) {
	b := newBuilder(t, Options{}, secret(credsNS, "kc", map[string][]byte{"config": []byte("data")}))

	got, err := b.Secrets.Value(context.Background(),
		netv1alpha1.SecretKeyRef{Name: "kc"}, "value", "config")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if string(got) != "data" {
		t.Errorf("read %q, want the second default key to be tried", got)
	}
}

func TestSecretErrorsNeverLeakMaterial(t *testing.T) {
	// A missing key must not tempt anyone into dumping what the Secret does
	// contain (9.6).
	b := newBuilder(t, Options{}, secret(credsNS, "creds", map[string][]byte{keyToken: []byte("canary-value-xyz")}))

	_, err := b.Secrets.Value(context.Background(), netv1alpha1.SecretKeyRef{Name: "creds", Key: "absent"})
	if err == nil {
		t.Fatal("expected an error for a missing key")
	}
	if strings.Contains(err.Error(), "canary-value-xyz") {
		t.Errorf("error leaked secret material: %v", err)
	}
}

// TestInsecureTLSRequiresTheFlag covers security 9.4.
func TestInsecureTLSRequiresTheFlag(t *testing.T) {
	rc := &netv1alpha1.RemoteCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: netv1alpha1.RemoteClusterSpec{
			Access: netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeToken,
				Token: &netv1alpha1.TokenAccess{
					Server:    testServerURL,
					SecretRef: netv1alpha1.SecretKeyRef{Name: "tok"},
				},
				TLS: &netv1alpha1.TLSConfig{InsecureSkipVerify: true},
			},
		},
	}
	tok := secret(credsNS, "tok", map[string][]byte{keyToken: []byte("t")})

	t.Run("rejected without the flag", func(t *testing.T) {
		b := newBuilder(t, Options{}, tok)
		_, err := b.Build(context.Background(), rc)
		var notPermitted *ErrNotPermitted
		if !errors.As(err, &notPermitted) {
			t.Fatalf("err = %v, want ErrNotPermitted", err)
		}
	})

	t.Run("permitted with the flag", func(t *testing.T) {
		b := newBuilder(t, Options{AllowInsecureTLS: true}, tok)
		if _, err := b.Build(context.Background(), rc); err != nil {
			t.Fatalf("Build: %v", err)
		}
	})
}

func TestFingerprintChangesWithCredentials(t *testing.T) {
	rc := &netv1alpha1.RemoteCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: netv1alpha1.RemoteClusterSpec{
			Access: netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeToken,
				Token: &netv1alpha1.TokenAccess{
					Server:    testServerURL,
					SecretRef: netv1alpha1.SecretKeyRef{Name: "tok"},
				},
			},
		},
	}

	first := newBuilder(t, Options{}, secret(credsNS, "tok", map[string][]byte{keyToken: []byte("aaa")}))
	a, err := first.Build(context.Background(), rc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	second := newBuilder(t, Options{}, secret(credsNS, "tok", map[string][]byte{keyToken: []byte("bbb")}))
	bb, err := second.Build(context.Background(), rc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// This is the whole rotation mechanism: if the fingerprint did not move,
	// the cache would keep serving the old connection until the old credential
	// expired and everything failed at once.
	if a.Fingerprint == bb.Fingerprint {
		t.Error("fingerprint did not change when the token did")
	}
}

func TestNotImplementedAccessTypesReportThemselves(t *testing.T) {
	for _, tc := range []struct {
		name   string
		access netv1alpha1.ClusterAccess
	}{
		{"ConnectGateway", netv1alpha1.ClusterAccess{
			Type:           netv1alpha1.AccessTypeConnectGateway,
			ConnectGateway: &netv1alpha1.ConnectGatewayAccess{Project: "p", Location: "l", Membership: "m"},
		}},
		{"EKS", netv1alpha1.ClusterAccess{
			Type: netv1alpha1.AccessTypeEKS,
			EKS:  &netv1alpha1.EKSAccess{Region: "us-east-1", Cluster: "c"},
		}},
		{"ProjectedServiceAccount", netv1alpha1.ClusterAccess{
			Type: netv1alpha1.AccessTypeProjectedServiceAccount,
			ProjectedServiceAccount: &netv1alpha1.ProjectedSAAccess{
				Server: testServerURL, Audience: "a",
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuilder(t, Options{})
			_, err := b.Build(context.Background(),
				&netv1alpha1.RemoteCluster{Spec: netv1alpha1.RemoteClusterSpec{Access: tc.access}})

			var notImpl *ErrAccessTypeNotImplemented
			if !errors.As(err, &notImpl) {
				t.Fatalf("err = %v, want ErrAccessTypeNotImplemented", err)
			}
		})
	}
}

func TestClientCertificateBuildsATLSKeypair(t *testing.T) {
	b := newBuilder(t, Options{}, secret(credsNS, "keypair", map[string][]byte{
		"tls.crt": []byte("CERT"),
		"tls.key": []byte("KEY"),
	}))

	res, err := b.Build(context.Background(), &netv1alpha1.RemoteCluster{
		Spec: netv1alpha1.RemoteClusterSpec{
			Access: netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeClientCertificate,
				ClientCertificate: &netv1alpha1.ClientCertAccess{
					Server:     testServerURL,
					SecretName: "keypair",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if string(res.Config.CertData) != "CERT" ||
		string(res.Config.KeyData) != "KEY" {
		t.Error("client keypair was not attached to the TLS config")
	}
}

func TestSecretRefsListsEverythingWatched(t *testing.T) {
	rc := &netv1alpha1.RemoteCluster{
		Spec: netv1alpha1.RemoteClusterSpec{
			Access: netv1alpha1.ClusterAccess{
				Type: netv1alpha1.AccessTypeGoogleToken,
				GoogleToken: &netv1alpha1.GoogleTokenAccess{
					Server:      testServerURL,
					CASecretRef: &netv1alpha1.SecretKeyRef{Name: "ca"},
					Credentials: &netv1alpha1.GCPCredentials{
						ServiceAccountKeySecretRef: &netv1alpha1.SecretKeyRef{Name: "gcp-key"},
					},
				},
				TLS: &netv1alpha1.TLSConfig{CASecretRef: &netv1alpha1.SecretKeyRef{Name: "shared-ca"}},
			},
		},
	}

	// Every Secret a cluster reads has to be watched, or a rotation of one of
	// them is silently missed.
	got := SecretRefs(rc)
	want := []string{"ca", "gcp-key", "shared-ca"}
	if len(got) != len(want) {
		t.Fatalf("SecretRefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SecretRefs = %v, want %v", got, want)
		}
	}
}
