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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"k8s.io/client-go/rest"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// defaultGoogleScope is what a GKE apiserver expects. Note that a node's
// default OAuth scopes do NOT include it, which is the usual cause of a 401 on
// the Workload-Identity-disabled path -- and node pool scopes are immutable,
// so the fix is a new node pool or a key file.
const defaultGoogleScope = "https://www.googleapis.com/auth/cloud-platform"

// buildGoogleToken mints Google OAuth2 tokens against an explicitly configured
// server and CA.
//
// Explicit server and CA are the point: this path never calls
// container.googleapis.com, so it needs neither roles/container.clusterViewer
// nor the cloud-platform scope for discovery.
func (b *Builder) buildGoogleToken(
	ctx context.Context,
	access *netv1alpha1.ClusterAccess,
	fp *fingerprint,
) (*rest.Config, error) {
	cfg := access.GoogleToken

	tlsCfg, err := b.tlsFrom(ctx, access, cfg.CASecretRef, fp)
	if err != nil {
		return nil, err
	}

	ts, err := b.googleTokenSource(ctx, cfg.Credentials, fp)
	if err != nil {
		return nil, err
	}

	fp.add("server", cfg.Server)
	return wrapWithTokenSource(cfg.Server, tlsCfg, ts), nil
}

// buildWorkloadIdentity mints tokens through the GKE metadata server.
//
// Functionally this is GoogleToken with ADC, but the type carries no key field
// at all, so the insecure configuration is unrepresentable rather than merely
// discouraged.
func (b *Builder) buildWorkloadIdentity(
	ctx context.Context,
	access *netv1alpha1.ClusterAccess,
	fp *fingerprint,
) (*rest.Config, error) {
	cfg := access.WorkloadIdentity

	tlsCfg, err := b.tlsFrom(ctx, access, cfg.CASecretRef, fp)
	if err != nil {
		return nil, err
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{defaultGoogleScope}
	}

	creds, err := google.FindDefaultCredentials(ctx, scopes...)
	if err != nil {
		return nil, fmt.Errorf("resolving workload identity credentials: %w", err)
	}
	ts := creds.TokenSource

	if cfg.ImpersonateServiceAccount != "" {
		ts = newImpersonatedTokenSource(ts, cfg.ImpersonateServiceAccount, scopes)
		fp.add("impersonate", cfg.ImpersonateServiceAccount)
	}

	fp.add("server", cfg.Server)
	fp.add("wiIdentity", cfg.ServiceAccountEmail)
	return wrapWithTokenSource(cfg.Server, tlsCfg, ts), nil
}

func (b *Builder) googleTokenSource(
	ctx context.Context,
	creds *netv1alpha1.GCPCredentials,
	fp *fingerprint,
) (oauth2.TokenSource, error) {
	scopes := []string{defaultGoogleScope}
	if creds != nil && len(creds.Scopes) > 0 {
		scopes = creds.Scopes
	}

	var base oauth2.TokenSource

	switch {
	case creds != nil && creds.ServiceAccountKeySecretRef != nil:
		// A JSON key bypasses node pool OAuth scopes entirely, which is why it
		// remains supported on the WI-disabled path.
		keyJSON, err := b.Secrets.Value(ctx, *creds.ServiceAccountKeySecretRef, "key.json")
		if err != nil {
			return nil, err
		}
		// nolint:staticcheck // The deprecation warns about credential configs
		// from untrusted sources. This one comes from a Secret in the
		// controller's own namespace, which no CR can redirect (9.1).
		parsed, err := google.CredentialsFromJSON(ctx, keyJSON, scopes...)
		if err != nil {
			return nil, fmt.Errorf("parsing google service account key: %w", err)
		}
		base = parsed.TokenSource
		fp.addBytes("gcpKey", keyJSON)

	default:
		parsed, err := google.FindDefaultCredentials(ctx, scopes...)
		if err != nil {
			return nil, fmt.Errorf("resolving application default credentials: %w", err)
		}
		base = parsed.TokenSource
		fp.add("gcpCreds", "adc")
	}

	if creds != nil && creds.ImpersonateServiceAccount != "" {
		base = newImpersonatedTokenSource(base, creds.ImpersonateServiceAccount, scopes)
		fp.add("impersonate", creds.ImpersonateServiceAccount)
	}
	return base, nil
}

// wrapWithTokenSource attaches a refreshing token source to the transport.
//
// This is invariant I15. Google access tokens live about an hour, so a token
// captured once into rest.Config.BearerToken produces a client that works
// perfectly until it silently stops, an hour after anyone last looked at it.
// oauth2.Transport re-mints on expiry instead.
func wrapWithTokenSource(server string, tlsCfg rest.TLSClientConfig, ts oauth2.TokenSource) *rest.Config {
	cfg := &rest.Config{
		Host:            server,
		TLSClientConfig: tlsCfg,
	}
	cached := oauth2.ReuseTokenSource(nil, ts)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &oauth2.Transport{Source: cached, Base: rt}
	})
	return cfg
}

// impersonatedTokenSource exchanges an ambient token for one belonging to
// another service account, via the IAM Credentials API.
//
// Implemented directly rather than through google.golang.org/api/impersonate to
// avoid taking on that module for one request shape.
type impersonatedTokenSource struct {
	base   oauth2.TokenSource
	target string
	scopes []string
}

func newImpersonatedTokenSource(base oauth2.TokenSource, target string, scopes []string) oauth2.TokenSource {
	return oauth2.ReuseTokenSource(nil, &impersonatedTokenSource{
		base:   base,
		target: target,
		scopes: scopes,
	})
}

func (s *impersonatedTokenSource) Token() (*oauth2.Token, error) {
	ctx := context.Background()

	body, err := json.Marshal(map[string]any{
		"scope":    s.scopes,
		"lifetime": "3600s",
	})
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf(
		"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken", s.target)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := oauth2.NewClient(ctx, s.base)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: %w", s.target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The body is deliberately not included: it can echo request details,
		// and nothing here is worth risking a credential in a log line (9.6).
		return nil, fmt.Errorf("impersonating %s: iamcredentials returned %s", s.target, resp.Status)
	}

	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireTime  string `json:"expireTime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding impersonation response: %w", err)
	}

	expiry, err := time.Parse(time.RFC3339, out.ExpireTime)
	if err != nil {
		expiry = time.Now().Add(55 * time.Minute)
	}
	return &oauth2.Token{AccessToken: out.AccessToken, Expiry: expiry}, nil
}
