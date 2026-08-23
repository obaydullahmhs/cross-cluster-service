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

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

func TestScopeOfClassifiesEveryAddress(t *testing.T) {
	cases := []struct {
		addr string
		want AddressScope
	}{
		{"10.0.0.1", ScopePrivate},
		{"172.16.0.1", ScopePrivate},
		{"172.32.0.1", ScopePublic}, // just outside 172.16/12
		{"192.168.1.1", ScopePrivate},
		{"100.64.0.1", ScopePrivate}, // CGNAT: GKE and EKS allocate from here
		{"100.128.0.1", ScopePublic}, // just outside 100.64/10
		{"127.0.0.1", ScopePrivate},
		{"169.254.169.254", ScopePrivate}, // metadata server
		{"0.0.0.0", ScopePrivate},
		{"198.18.0.1", ScopePrivate},
		{"8.8.8.8", ScopePublic},
		{"35.1.1.1", ScopePublic},
		{"::1", ScopePrivate},
		{"fd00::1", ScopePrivate}, // ULA
		{"fe80::1", ScopePrivate},
		{"2001:4860:4860::8888", ScopePublic},
		{"::ffff:10.0.0.1", ScopePrivate}, // v4-mapped is unmapped first
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := ScopeOf(netip.MustParseAddr(tc.addr)); got != tc.want {
				t.Errorf("ScopeOf(%s) = %s, want %s", tc.addr, got, tc.want)
			}
		})
	}
}

// TestScopesArePreciseComplements is the property the two exclude knobs rest
// on: every address is exactly one scope, so excluding one keeps the other.
// Were a third class to appear, an address could survive both filters.
func TestScopesArePreciseComplements(t *testing.T) {
	for _, s := range []string{"1.1.1.1", "10.0.0.1", "::1", "2606:4700::1111", "224.0.0.1"} {
		got := ScopeOf(netip.MustParseAddr(s))
		if got != ScopePublic && got != ScopePrivate {
			t.Errorf("ScopeOf(%s) = %q, which is neither scope", s, got)
		}
	}
}

func TestDNSExcludesByScope(t *testing.T) {
	const public, private = "203.0.113.9", "10.20.0.5"

	cases := []struct {
		name           string
		excludePrivate bool
		excludePublic  bool
		want           []string
		wantWarnings   int
	}{
		{"no filter keeps both", false, false, []string{public, private}, 0},
		{"excludePrivateIPs keeps the public answer", true, false, []string{public}, 1},
		{"excludePublicIPs keeps the private answer", false, true, []string{private}, 1},
		// Rejected by CRD validation; asserted so a stored object predating the
		// rule fails visibly rather than quietly ignoring both fields.
		{"both set excludes everything", true, true, nil, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One name, both views -- what a split-horizon zone looks like
			// from a resolver that can see the internal and external answers.
			d := &DNS{Client: &fakeLookup{addrs: map[string][]netip.Addr{
				fqdnDB: mustAddrs(t, public, private),
			}}}
			src := &netv1alpha1.Source{
				Type: netv1alpha1.SourceTypeDNS,
				DNS: &netv1alpha1.DNSSource{
					Names:             []string{fqdnDB},
					RecordType:        netv1alpha1.DNSRecordTypeA,
					ExcludePrivateIPs: tc.excludePrivate,
					ExcludePublicIPs:  tc.excludePublic,
				},
			}
			res, err := d.Resolve(context.Background(), src, []netv1alpha1.CrossServicePort{{Port: 80}})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			got := make([]string, 0, len(res.Endpoints))
			for _, e := range res.Endpoints {
				got = append(got, e.Address.String())
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("endpoint %d = %s, want %s", i, got[i], tc.want[i])
				}
			}

			// A deliberate drop still has to be reported: otherwise it is
			// indistinguishable from the record having shrunk on its own.
			if len(res.Warnings) != tc.wantWarnings {
				t.Errorf("warnings = %v, want %d", res.Warnings, tc.wantWarnings)
			}
		})
	}
}

// TestDNSScopeFilterDoesNotWarnWhenNothingDropped keeps the event stream quiet
// for the overwhelmingly common case: a name that only ever answers with the
// kind of address that was asked for.
func TestDNSScopeFilterDoesNotWarnWhenNothingDropped(t *testing.T) {
	d := &DNS{Client: &fakeLookup{addrs: map[string][]netip.Addr{
		fqdnDB: mustAddrs(t, "10.0.0.1", "10.0.0.2"),
	}}}
	src := &netv1alpha1.Source{
		Type: netv1alpha1.SourceTypeDNS,
		DNS: &netv1alpha1.DNSSource{
			Names:            []string{fqdnDB},
			RecordType:       netv1alpha1.DNSRecordTypeA,
			ExcludePublicIPs: true,
		},
	}
	res, err := d.Resolve(context.Background(), src, []netv1alpha1.CrossServicePort{{Port: 80}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Endpoints) != 2 {
		t.Errorf("got %d endpoints, want 2", len(res.Endpoints))
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", res.Warnings)
	}
}
