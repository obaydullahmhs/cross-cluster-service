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
	"net"
	"net/netip"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// TestI10_FQDNsNeedATrailingDot covers invariant I10.
func TestI10_FQDNsNeedATrailingDot(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare name gets a dot", "db.example.com", fqdnDB},
		{"already qualified is untouched", fqdnDB, fqdnDB},
		{"single label", "db", "db."},
		{"surrounding whitespace is trimmed", "  db.example.com  ", fqdnDB},
		{"empty stays empty", "", ""},
		{"root stays root", ".", "."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FQDN(tc.in); got != tc.want {
				t.Errorf("FQDN(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// fakeLookup is a deterministic LookupClient.
type fakeLookup struct {
	addrs map[string][]netip.Addr
	srvs  map[string][]*net.SRV
	err   error

	// queried records the exact names asked for, so the trailing-dot
	// normalisation can be asserted at the call boundary rather than only on
	// the helper.
	queried []string
}

func (f *fakeLookup) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.queried = append(f.queried, host)
	if f.err != nil {
		return nil, f.err
	}
	return f.addrs[host], nil
}

func (f *fakeLookup) LookupSRV(_ context.Context, name string) ([]*net.SRV, error) {
	f.queried = append(f.queried, name)
	if f.err != nil {
		return nil, f.err
	}
	return f.srvs[name], nil
}

func mustAddrs(t *testing.T, in ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, 0, len(in))
	for _, s := range in {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("bad test address %q: %v", s, err)
		}
		out = append(out, a)
	}
	return out
}

func TestDNSResolveQueriesFullyQualifiedNames(t *testing.T) {
	fake := &fakeLookup{addrs: map[string][]netip.Addr{
		fqdnDB: mustAddrs(t, "10.0.0.1", "10.0.0.2"),
	}}
	d := &DNS{Client: fake}

	src := &netv1alpha1.Source{
		Type: netv1alpha1.SourceTypeDNS,
		// Deliberately written without the trailing dot, as a user would.
		DNS: &netv1alpha1.DNSSource{Names: []string{"db.example.com"}},
	}
	ports := []netv1alpha1.CrossServicePort{{Port: 5432}}

	res, err := d.Resolve(context.Background(), src, ports)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Endpoints) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(res.Endpoints))
	}
	if len(fake.queried) != 1 || fake.queried[0] != fqdnDB {
		t.Errorf("queried %v, want the fully-qualified name", fake.queried)
	}
	if got := res.Endpoints[0].PortMap[""]; got != 5432 {
		t.Errorf("unnamed port resolved to %d, want 5432", got)
	}
}

func TestDNSResolvePropagatesFailure(t *testing.T) {
	// A partial answer must not look like success: serving a subset of the
	// declared names would be indistinguishable from a healthy resolve.
	fake := &fakeLookup{err: context.DeadlineExceeded}
	d := &DNS{Client: fake}

	src := &netv1alpha1.Source{
		Type: netv1alpha1.SourceTypeDNS,
		DNS:  &netv1alpha1.DNSSource{Names: []string{"a.example.com."}},
	}
	if _, err := d.Resolve(context.Background(), src, nil); err == nil {
		t.Fatal("expected an error when the lookup fails")
	}
}

func TestDNSSRVPopulatesTheNamedPort(t *testing.T) {
	fake := &fakeLookup{
		srvs: map[string][]*net.SRV{
			"_pg._tcp.example.com.": {{Target: "db1.example.com.", Port: 15432}},
		},
		addrs: map[string][]netip.Addr{
			"db1.example.com.": mustAddrs(t, "10.0.0.5"),
		},
	}
	d := &DNS{Client: fake}

	src := &netv1alpha1.Source{
		Type: netv1alpha1.SourceTypeDNS,
		DNS: &netv1alpha1.DNSSource{
			Names:      []string{"_pg._tcp.example.com."},
			RecordType: netv1alpha1.DNSRecordTypeSRV,
		},
	}
	ports := []netv1alpha1.CrossServicePort{{Name: "", Port: 5432}}

	res, err := d.Resolve(context.Background(), src, ports)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Endpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(res.Endpoints))
	}
	if got := res.Endpoints[0].PortMap[""]; got != 15432 {
		t.Errorf("SRV port = %d, want the record's 15432", got)
	}
}

func TestRequeueAfterFallsBackToIntervalWithoutTTL(t *testing.T) {
	dur := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

	cases := []struct {
		name string
		cfg  *netv1alpha1.DNSSource
		ttl  time.Duration
		want time.Duration
	}{
		{
			name: "interval when useTTL is off",
			cfg:  &netv1alpha1.DNSSource{DNSResolution: netv1alpha1.DNSResolution{Interval: dur(45 * time.Second)}},
			ttl:  10 * time.Second,
			want: 45 * time.Second,
		},
		{
			// The net.Resolver-backed client never reports a TTL, so useTTL has
			// to degrade rather than requeue instantly.
			name: "interval when useTTL is on but no TTL was reported",
			cfg:  &netv1alpha1.DNSSource{DNSResolution: netv1alpha1.DNSResolution{Interval: dur(45 * time.Second), UseTTL: true}},
			ttl:  0,
			want: 45 * time.Second,
		},
		{
			name: "TTL clamped up to minTTL",
			cfg: &netv1alpha1.DNSSource{DNSResolution: netv1alpha1.DNSResolution{
				UseTTL: true, MinTTL: dur(5 * time.Second), MaxTTL: dur(5 * time.Minute),
			}},
			ttl:  time.Second,
			want: 5 * time.Second,
		},
		{
			name: "TTL clamped down to maxTTL",
			cfg: &netv1alpha1.DNSSource{DNSResolution: netv1alpha1.DNSResolution{
				UseTTL: true, MinTTL: dur(5 * time.Second), MaxTTL: dur(5 * time.Minute),
			}},
			ttl:  24 * time.Hour,
			want: 5 * time.Minute,
		},
		{
			name: "default interval when nothing is set",
			cfg:  &netv1alpha1.DNSSource{},
			want: 30 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequeueAfter(tc.cfg, tc.ttl); got != tc.want {
				t.Errorf("RequeueAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStaticResolveRejectsMalformedAddresses(t *testing.T) {
	s := &Static{}
	src := &netv1alpha1.Source{
		Type:   netv1alpha1.SourceTypeStatic,
		Static: &netv1alpha1.StaticSource{Addresses: []string{"10.0.0.1", "not-an-ip"}},
	}
	if _, err := s.Resolve(context.Background(), src, nil); err == nil {
		t.Fatal("expected an error for a malformed static address")
	}
}

func TestStaticResolveMarksEndpointsReady(t *testing.T) {
	s := &Static{}
	src := &netv1alpha1.Source{
		Type: netv1alpha1.SourceTypeStatic,
		Static: &netv1alpha1.StaticSource{
			Addresses: []string{"10.0.0.1"},
			Zone:      "us-central1-a",
		},
	}
	res, err := s.Resolve(context.Background(), src, []netv1alpha1.CrossServicePort{{Port: 80}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Static endpoints are assumed ready: there is no source to ask, and this
	// controller does no health checking.
	if !res.Endpoints[0].Ready {
		t.Error("static endpoint should be ready")
	}
	if res.Endpoints[0].Zone != "us-central1-a" {
		t.Errorf("zone = %q, want us-central1-a", res.Endpoints[0].Zone)
	}
}
