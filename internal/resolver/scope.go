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

import "net/netip"

// AddressScope says whether an address is routable on the public internet.
//
// The partition is total and binary on purpose: every address is exactly one of
// the two, so excludePrivateIPs and excludePublicIPs are exact complements. If
// some third "neither" class existed, an address could survive both filters,
// and a user who set one of them would still get answers they had asked to be
// rid of.
type AddressScope string

const (
	// ScopePublic is globally routable unicast.
	ScopePublic AddressScope = "Public"
	// ScopePrivate is everything else: RFC1918, CGNAT, ULA, loopback,
	// link-local, and the unspecified address.
	ScopePrivate AddressScope = "Private"
)

// privateV4 covers the IPv4 ranges that are not routable on the public
// internet. netip's own IsPrivate() is only RFC1918, which is not enough here:
// GKE hands out Pod and Service ranges from 100.64.0.0/10, and EKS uses it for
// secondary VPC CIDRs, so calling a 100.64 address "public" would send traffic
// at a cloud-internal address in the name of keeping it off private networks.
var privateV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918
	netip.MustParsePrefix("100.64.0.0/10"),  // RFC6598 CGNAT
	netip.MustParsePrefix("127.0.0.0/8"),    // loopback
	netip.MustParsePrefix("169.254.0.0/16"), // link-local
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
	netip.MustParsePrefix("198.18.0.0/15"),  // RFC2544 benchmarking
	netip.MustParsePrefix("255.255.255.255/32"),
}

var privateV6 = []netip.Prefix{
	netip.MustParsePrefix("::/128"),    // unspecified
	netip.MustParsePrefix("::1/128"),   // loopback
	netip.MustParsePrefix("fc00::/7"),  // unique local
	netip.MustParsePrefix("fe80::/10"), // link-local unicast
}

// ScopeOf classifies an address. Multicast is reported as private: it is not a
// unicast destination a Service can usefully carry, and the default address
// policy rejects it regardless.
func ScopeOf(addr netip.Addr) AddressScope {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return ScopePrivate
	}
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsPrivate() {
		return ScopePrivate
	}

	prefixes := privateV4
	if addr.Is6() {
		prefixes = privateV6
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return ScopePrivate
		}
	}
	return ScopePublic
}
