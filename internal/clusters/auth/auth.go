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

// Package auth turns a RemoteCluster's access spec into a rest.Config.
//
// Two rules shape everything here. Credentials are read only from the
// controller's own namespace, never from a namespace named in a CR. And the
// resolved credentials are fingerprinted, so the client cache can notice a
// rotation rather than serving a stale connection until the next restart.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"net/url"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// Options are the controller-wide switches that gate the riskier access types.
type Options struct {
	// CredentialsNamespace is the ONLY namespace Secrets are read from.
	CredentialsNamespace string

	// AllowInsecureTLS must be set for a RemoteCluster to use
	// insecureSkipVerify.
	AllowInsecureTLS bool

	// AllowExecCredentials must be set for the ExecPlugin access type, which is
	// arbitrary code execution driven by a cluster-scoped CR.
	AllowExecCredentials bool

	// ExecCommandAllowlist further restricts which commands may run.
	ExecCommandAllowlist []string
}

// ErrAccessTypeNotImplemented is returned for an access type this build cannot
// service. It is a distinct type so the controller reports
// AccessTypeNotImplemented rather than a generic failure.
type ErrAccessTypeNotImplemented struct {
	Type netv1alpha1.AccessType
}

func (e *ErrAccessTypeNotImplemented) Error() string {
	return fmt.Sprintf("access type %q is not implemented in this build", e.Type)
}

// ErrNotPermitted is returned when a spec asks for something the controller's
// flags forbid.
type ErrNotPermitted struct{ Reason string }

func (e *ErrNotPermitted) Error() string { return e.Reason }

// Result is a built connection.
type Result struct {
	Config *rest.Config

	// Fingerprint identifies the credentials that produced Config. It changes
	// when any of them change, which is what lets the cache detect a rotation.
	Fingerprint string
}

// Builder builds rest.Configs from RemoteCluster specs.
type Builder struct {
	Secrets *Secrets
	Options Options
}

// Build dispatches on the access type.
func (b *Builder) Build(ctx context.Context, rc *netv1alpha1.RemoteCluster) (*Result, error) {
	access := &rc.Spec.Access

	if err := b.checkTLSPermitted(access); err != nil {
		return nil, err
	}

	fp := newFingerprint()
	fp.add("type", string(access.Type))

	var cfg *rest.Config
	var err error

	switch access.Type {
	case netv1alpha1.AccessTypeInCluster:
		cfg, err = buildInCluster()
	case netv1alpha1.AccessTypeToken:
		cfg, err = b.buildToken(ctx, access, fp)
	case netv1alpha1.AccessTypeKubeconfig:
		cfg, err = b.buildKubeconfig(ctx, access, fp)
	case netv1alpha1.AccessTypeGoogleToken:
		cfg, err = b.buildGoogleToken(ctx, access, fp)
	case netv1alpha1.AccessTypeWorkloadIdentity:
		cfg, err = b.buildWorkloadIdentity(ctx, access, fp)
	case netv1alpha1.AccessTypeClientCertificate:
		cfg, err = b.buildClientCertificate(ctx, access, fp)
	case netv1alpha1.AccessTypeGKE:
		cfg, err = b.buildGKE(ctx, access, fp)
	case netv1alpha1.AccessTypeAKS:
		cfg, err = b.buildAKS(ctx, access, fp)
	default:
		return nil, &ErrAccessTypeNotImplemented{Type: access.Type}
	}
	if err != nil {
		return nil, err
	}

	applyClientTuning(cfg, access)
	applyImpersonation(cfg, access)
	if err := b.applyProxy(cfg, access, fp); err != nil {
		return nil, err
	}

	return &Result{Config: cfg, Fingerprint: fp.sum()}, nil
}

func (b *Builder) checkTLSPermitted(access *netv1alpha1.ClusterAccess) error {
	if access.TLS != nil && access.TLS.InsecureSkipVerify && !b.Options.AllowInsecureTLS {
		return &ErrNotPermitted{
			Reason: "insecureSkipVerify requires the controller to run with --allow-insecure-tls",
		}
	}
	return nil
}

// applyClientTuning applies the per-cluster rate limits and timeout.
func applyClientTuning(cfg *rest.Config, access *netv1alpha1.ClusterAccess) {
	if access.Timeout != nil {
		cfg.Timeout = access.Timeout.Duration
	}
	if access.QPS > 0 {
		cfg.QPS = float32(access.QPS)
	}
	if access.Burst > 0 {
		cfg.Burst = int(access.Burst)
	}
}

func applyImpersonation(cfg *rest.Config, access *netv1alpha1.ClusterAccess) {
	if access.Impersonate == nil {
		return
	}
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: access.Impersonate.UserName,
		UID:      access.Impersonate.UID,
		Groups:   access.Impersonate.Groups,
		Extra:    access.Impersonate.Extra,
	}
}

func (b *Builder) applyProxy(cfg *rest.Config, access *netv1alpha1.ClusterAccess, fp *fingerprint) error {
	if access.Proxy == nil {
		return nil
	}
	proxyURL, err := parseProxyURL(access.Proxy.URL)
	if err != nil {
		return err
	}
	fp.add("proxy", access.Proxy.URL)
	cfg.Proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	return nil
}

func parseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	return u, nil
}

// tlsFrom builds the TLS config, preferring a variant's own CA over the shared
// access.tls block.
func (b *Builder) tlsFrom(
	ctx context.Context,
	access *netv1alpha1.ClusterAccess,
	own *netv1alpha1.SecretKeyRef,
	fp *fingerprint,
) (rest.TLSClientConfig, error) {
	out := rest.TLSClientConfig{}

	ref := own
	if ref == nil && access.TLS != nil {
		ref = access.TLS.CASecretRef
	}
	if ref != nil {
		ca, err := b.Secrets.Value(ctx, *ref, "ca.crt")
		if err != nil {
			return out, err
		}
		out.CAData = ca
		fp.addBytes("ca", ca)
	}

	if access.TLS != nil {
		out.ServerName = access.TLS.ServerName
		out.Insecure = access.TLS.InsecureSkipVerify
		fp.add("serverName", access.TLS.ServerName)
	}
	return out, nil
}

// SecretRefs lists every Secret a RemoteCluster reads, so the controller can
// watch exactly those and invalidate on change.
func SecretRefs(rc *netv1alpha1.RemoteCluster) []string {
	access := &rc.Spec.Access
	seen := map[string]bool{}

	add := func(ref *netv1alpha1.SecretKeyRef) {
		if ref != nil && ref.Name != "" {
			seen[ref.Name] = true
		}
	}

	if access.TLS != nil {
		add(access.TLS.CASecretRef)
	}
	if access.Token != nil {
		add(&access.Token.SecretRef)
	}
	if access.Kubeconfig != nil {
		add(&access.Kubeconfig.SecretRef)
	}
	if access.GoogleToken != nil {
		add(access.GoogleToken.CASecretRef)
		if access.GoogleToken.Credentials != nil {
			add(access.GoogleToken.Credentials.ServiceAccountKeySecretRef)
		}
	}
	if access.WorkloadIdentity != nil {
		add(access.WorkloadIdentity.CASecretRef)
	}
	if access.ClientCertificate != nil {
		add(access.ClientCertificate.CASecretRef)
		if access.ClientCertificate.SecretName != "" {
			seen[access.ClientCertificate.SecretName] = true
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Secrets reads Secrets from exactly one namespace.
//
// There is deliberately no way to ask for another. SecretKeyRef has no
// namespace field, and this type takes its namespace from the controller's
// flags, so a cluster-scoped CR cannot reach a Secret it was not given.
type Secrets struct {
	Client    client.Client
	Namespace string
}

// Value reads one key. defaultKeys are tried in order when the ref names none.
func (s *Secrets) Value(ctx context.Context, ref netv1alpha1.SecretKeyRef, defaultKeys ...string) ([]byte, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: s.Namespace, Name: ref.Name}
	if err := s.Client.Get(ctx, key, &secret); err != nil {
		// The namespace is included because it is the controller's own and is
		// useful to see; no secret material is ever included in an error.
		return nil, fmt.Errorf("reading secret %s/%s: %w", s.Namespace, ref.Name, err)
	}

	candidates := defaultKeys
	if ref.Key != "" {
		candidates = []string{ref.Key}
	}
	for _, k := range candidates {
		if v, ok := secret.Data[k]; ok && len(v) > 0 {
			return v, nil
		}
	}
	return nil, fmt.Errorf("secret %s/%s has none of the expected keys %v", s.Namespace, ref.Name, candidates)
}

// fingerprint accumulates a stable hash of the material behind a connection.
type fingerprint struct{ h hash.Hash }

func newFingerprint() *fingerprint { return &fingerprint{h: sha256.New()} }

func (f *fingerprint) add(label, value string) {
	_, _ = f.h.Write([]byte(label))
	_, _ = f.h.Write([]byte{0})
	_, _ = f.h.Write([]byte(value))
	_, _ = f.h.Write([]byte{0})
}

func (f *fingerprint) addBytes(label string, value []byte) {
	_, _ = f.h.Write([]byte(label))
	_, _ = f.h.Write([]byte{0})
	_, _ = f.h.Write(value)
	_, _ = f.h.Write([]byte{0})
}

func (f *fingerprint) sum() string { return hex.EncodeToString(f.h.Sum(nil)) }
