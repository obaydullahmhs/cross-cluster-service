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
	"net/netip"
	"testing"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
)

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("bad test address %q: %v", s, err)
	}
	return a
}

func ptr[T any](v T) *T { return &v }

// TestAddressPolicyBlocksSpecialPurpose covers security requirement 9.3.
func TestAddressPolicyBlocksSpecialPurpose(t *testing.T) {
	p, err := NewPolicy(nil) // nil == the controller-wide default
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	blocked := []string{
		metadataServer, // the cloud metadata server -- the reason this exists
		"169.254.1.1",
		"127.0.0.1",
		"0.0.0.0",
		"224.0.0.1",
		"255.255.255.255",
		"::1",
		"fe80::1",
		"ff02::1",
		"::",
	}
	for _, s := range blocked {
		t.Run("blocks "+s, func(t *testing.T) {
			if ok, reason := p.Allow(addr(t, s)); ok {
				t.Errorf("%s was allowed, want blocked", s)
			} else if reason != RejectSpecialPurpose {
				t.Errorf("%s rejected for %q, want %q", s, reason, RejectSpecialPurpose)
			}
		})
	}

	allowed := []string{addrA, "192.168.1.1", "172.16.0.1", "8.8.8.8", "fd00::1", "2001:db8::1"}
	for _, s := range allowed {
		t.Run("allows "+s, func(t *testing.T) {
			if ok, reason := p.Allow(addr(t, s)); !ok {
				t.Errorf("%s was blocked for %q, want allowed", s, reason)
			}
		})
	}
}

func TestAddressPolicyCIDRMatching(t *testing.T) {
	cases := []struct {
		name       string
		policy     *netv1alpha1.AddressPolicy
		address    string
		wantAllow  bool
		wantReason RejectReason
	}{
		{
			name:      "empty allow list means no restriction",
			policy:    &netv1alpha1.AddressPolicy{},
			address:   "8.8.8.8",
			wantAllow: true,
		},
		{
			name:      "inside the allow list",
			policy:    &netv1alpha1.AddressPolicy{AllowedCIDRs: []string{cidrPrivateA}},
			address:   "10.1.2.3",
			wantAllow: true,
		},
		{
			name:       "outside the allow list",
			policy:     &netv1alpha1.AddressPolicy{AllowedCIDRs: []string{cidrPrivateA}},
			address:    "192.168.1.1",
			wantAllow:  false,
			wantReason: RejectNotAllowed,
		},
		{
			name: "deny wins over allow",
			policy: &netv1alpha1.AddressPolicy{
				AllowedCIDRs: []string{cidrPrivateA},
				DeniedCIDRs:  []string{"10.1.0.0/16"},
			},
			address:    "10.1.2.3",
			wantAllow:  false,
			wantReason: RejectDenied,
		},
		{
			name: "special purpose can be switched off deliberately",
			policy: &netv1alpha1.AddressPolicy{
				DenySpecialPurpose: ptr(false),
			},
			address:   metadataServer,
			wantAllow: true,
		},
		{
			name: "special purpose still wins over an explicit allow list",
			policy: &netv1alpha1.AddressPolicy{
				AllowedCIDRs: []string{"169.254.0.0/16"},
			},
			address:    metadataServer,
			wantAllow:  false,
			wantReason: RejectSpecialPurpose,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPolicy(tc.policy)
			if err != nil {
				t.Fatalf("NewPolicy: %v", err)
			}
			ok, reason := p.Allow(addr(t, tc.address))
			if ok != tc.wantAllow {
				t.Fatalf("Allow(%s) = %v, want %v", tc.address, ok, tc.wantAllow)
			}
			if !ok && reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

func TestAddressPolicyRejectsMalformedCIDR(t *testing.T) {
	if _, err := NewPolicy(&netv1alpha1.AddressPolicy{AllowedCIDRs: []string{"10.0.0.0/33"}}); err == nil {
		t.Fatal("expected an error for a malformed CIDR")
	}
}

func TestPolicyFilterReportsRatherThanFails(t *testing.T) {
	p, _ := NewPolicy(nil)
	in := []resolver.Endpoint{
		{Address: addr(t, addrA)},
		{Address: addr(t, metadataServer)},
		{Address: addr(t, "10.0.0.2")},
	}

	kept, rejected := p.Filter(in)
	if len(kept) != 2 {
		t.Errorf("kept %d, want 2", len(kept))
	}
	// One poisoned address must not take down an otherwise healthy source.
	if len(rejected) != 1 || rejected[0].Reason != RejectSpecialPurpose {
		t.Errorf("rejected = %+v, want one special-purpose rejection", rejected)
	}
}
