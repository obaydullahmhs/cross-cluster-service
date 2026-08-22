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

// Package endpoints turns resolved endpoints into EndpointSlices: filtering,
// grouping, packing, diffing and writing. Nothing here knows where an endpoint
// came from.
package endpoints

import (
	"fmt"
	"net/netip"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
)

// Policy enforces an AddressPolicy against resolved addresses.
//
// This is enforced controller-side deliberately: apiserver validation does not
// cover EndpointSlice the way it does legacy Endpoints, so a namespace tenant
// who can create a CrossService would otherwise be able to point a Service at
// the node's metadata server.
type Policy struct {
	allowed []netip.Prefix
	denied  []netip.Prefix

	denySpecialPurpose bool
}

// NewPolicy compiles an AddressPolicy. A nil policy denies special-purpose
// addresses and nothing else, which is the controller-wide default.
func NewPolicy(p *netv1alpha1.AddressPolicy) (*Policy, error) {
	out := &Policy{denySpecialPurpose: true}
	if p == nil {
		return out, nil
	}

	if p.DenySpecialPurpose != nil {
		out.denySpecialPurpose = *p.DenySpecialPurpose
	}

	var err error
	if out.allowed, err = parsePrefixes(p.AllowedCIDRs); err != nil {
		return nil, fmt.Errorf("allowedCIDRs: %w", err)
	}
	if out.denied, err = parsePrefixes(p.DeniedCIDRs); err != nil {
		return nil, fmt.Errorf("deniedCIDRs: %w", err)
	}
	return out, nil
}

func parsePrefixes(raw []string) ([]netip.Prefix, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", s, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// linkLocalIPv6Multicast is not covered by netip's IsMulticast for the
// ff00::/8 range check below; the stdlib predicates are used where they exist.
var deniedSpecialV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), // includes the 169.254.169.254 metadata server
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

var deniedSpecialV6 = []netip.Prefix{
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// RejectReason explains why an address was dropped, for the Event and status.
type RejectReason string

const (
	RejectSpecialPurpose RejectReason = "SpecialPurpose"
	RejectNotAllowed     RejectReason = "NotInAllowedCIDRs"
	RejectDenied         RejectReason = "InDeniedCIDRs"
)

// Allow reports whether addr may be written, and why not when it may not.
func (p *Policy) Allow(addr netip.Addr) (bool, RejectReason) {
	addr = addr.Unmap()

	if p.denySpecialPurpose && isSpecialPurpose(addr) {
		return false, RejectSpecialPurpose
	}
	for _, d := range p.denied {
		if d.Contains(addr) {
			return false, RejectDenied
		}
	}
	// An empty allow list means "no restriction", not "allow nothing" -- unlike
	// allowedNamespaces, which fails closed. The asymmetry is deliberate: an
	// empty CIDR list is the common default, whereas an absent namespace grant
	// is a missing decision.
	if len(p.allowed) > 0 {
		for _, a := range p.allowed {
			if a.Contains(addr) {
				return true, ""
			}
		}
		return false, RejectNotAllowed
	}
	return true, ""
}

func isSpecialPurpose(addr netip.Addr) bool {
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() {
		return true
	}
	prefixes := deniedSpecialV4
	if addr.Is6() {
		prefixes = deniedSpecialV6
	}
	for _, pfx := range prefixes {
		if pfx.Contains(addr) {
			return true
		}
	}
	return false
}

// Rejection records one dropped address.
type Rejection struct {
	Address netip.Addr
	Reason  RejectReason
}

// Filter partitions endpoints into those that may be written and those that
// were rejected. Rejections are reported, never returned as an error: one bad
// address must not fail an otherwise healthy source.
func (p *Policy) Filter(in []resolver.Endpoint) (kept []resolver.Endpoint, rejected []Rejection) {
	for _, e := range in {
		if ok, reason := p.Allow(e.Address); !ok {
			rejected = append(rejected, Rejection{Address: e.Address, Reason: reason})
			continue
		}
		kept = append(kept, e)
	}
	return kept, rejected
}
