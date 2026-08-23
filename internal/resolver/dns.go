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
	"fmt"
	"maps"
	"net"
	"net/netip"
	"strings"
	"time"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// LookupClient is the DNS surface this package needs. It exists so tests can
// substitute a deterministic resolver, and so a TTL-aware implementation
// (miekg/dns, M6) can be dropped in without touching callers.
type LookupClient interface {
	// LookupNetIP returns addresses for host. network is "ip4" or "ip6".
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	// LookupSRV returns SRV records for the fully-qualified name.
	LookupSRV(ctx context.Context, name string) ([]*net.SRV, error)
}

// DNS resolves names to addresses. It is the only source that must poll.
type DNS struct {
	// Client defaults to the process resolver when nil.
	Client LookupClient
}

var _ Resolver = (*DNS)(nil)

// netResolver adapts net.Resolver to LookupClient.
type netResolver struct{ r *net.Resolver }

func (n *netResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return n.r.LookupNetIP(ctx, network, host)
}

func (n *netResolver) LookupSRV(ctx context.Context, name string) ([]*net.SRV, error) {
	// The underscore-prefixed service and proto are already part of name here,
	// so the record is looked up verbatim.
	_, srvs, err := n.r.LookupSRV(ctx, "", "", name)
	return srvs, err
}

// NewSystemLookupClient returns a LookupClient backed by the process resolver,
// optionally directed at explicit nameservers.
func NewSystemLookupClient(nameservers []string) LookupClient {
	if len(nameservers) == 0 {
		return &netResolver{r: net.DefaultResolver}
	}

	// PreferGo is required for the Dial hook to be honoured; cgo's resolver
	// ignores it entirely.
	idx := 0
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			server := nameservers[idx%len(nameservers)]
			idx++
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		},
	}
	return &netResolver{r: r}
}

// FQDN appends a trailing dot when absent (I10).
//
// This is not cosmetic. With ndots:5 in a Pod's resolv.conf, "db.example.com"
// is tried against every search domain first, costing four NXDOMAIN round-trips
// before the real lookup -- per name, per interval, forever.
func FQDN(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}

// scopeFilter drops resolved addresses by routability scope.
//
// It is applied at the DNS layer rather than in the address policy because it
// is a property of the record, not of the cluster: the same address that must
// be excluded from a split-horizon name is perfectly valid arriving from a
// Static or Service source, so a controller-wide CIDR policy is the wrong
// instrument.
type scopeFilter struct {
	exclude AddressScope // "" filters nothing
}

// scopeAll is a filter sentinel, not a classification: ScopeOf never returns
// it, and nothing may be compared against it as though it were a scope.
const scopeAll AddressScope = "Private and Public"

func newScopeFilter(cfg *netv1alpha1.DNSSource) scopeFilter {
	switch {
	// Both true is rejected by CRD validation. Should a CRD predating that rule
	// still be stored, excluding everything is the honest reading, and an empty
	// EndpointSlice is far easier to diagnose than a silently ignored field.
	case cfg.ExcludePrivateIPs && cfg.ExcludePublicIPs:
		return scopeFilter{exclude: scopeAll}
	case cfg.ExcludePrivateIPs:
		return scopeFilter{exclude: ScopePrivate}
	case cfg.ExcludePublicIPs:
		return scopeFilter{exclude: ScopePublic}
	}
	return scopeFilter{}
}

func (f scopeFilter) active() bool { return f.exclude != "" }

// apply returns the surviving addresses and how many were dropped.
func (f scopeFilter) apply(addrs []netip.Addr) ([]netip.Addr, int) {
	if !f.active() {
		return addrs, 0
	}
	kept := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if f.exclude == scopeAll || ScopeOf(a) == f.exclude {
			continue
		}
		kept = append(kept, a)
	}
	return kept, len(addrs) - len(kept)
}

// warn describes a drop for the CrossService's events and status. An address
// removed on purpose still has to be visible: "half my backends vanished" is
// otherwise indistinguishable from a resolver returning less than it used to.
func (f scopeFilter) warn(name string, dropped, total int) string {
	return fmt.Sprintf("dns: %d of %d address(es) for %s excluded as %s", dropped, total, name, f.exclude)
}

// Resolve implements Resolver.
func (d *DNS) Resolve(ctx context.Context, src *netv1alpha1.Source, ports []netv1alpha1.CrossServicePort) (*Result, error) {
	if src.DNS == nil {
		return nil, fmt.Errorf("dns source has no dns config")
	}
	cfg := src.DNS

	client := d.Client
	if client == nil {
		client = NewSystemLookupClient(cfg.Nameservers)
	}

	if cfg.RecordType == netv1alpha1.DNSRecordTypeSRV {
		return d.resolveSRV(ctx, client, cfg, ports)
	}
	return d.resolveAddr(ctx, client, cfg, ports)
}

func (d *DNS) resolveAddr(
	ctx context.Context,
	client LookupClient,
	cfg *netv1alpha1.DNSSource,
	ports []netv1alpha1.CrossServicePort,
) (*Result, error) {
	network := "ip4"
	if cfg.RecordType == netv1alpha1.DNSRecordTypeAAAA {
		network = "ip6"
	}

	portMap := defaultPortMap(ports)
	filter := newScopeFilter(cfg)
	var out []Endpoint
	var errs []error
	var warnings []string

	for _, name := range cfg.Names {
		addrs, err := client.LookupNetIP(ctx, network, FQDN(name))
		if err != nil {
			errs = append(errs, fmt.Errorf("resolving %s: %w", name, err))
			continue
		}

		total := len(addrs)
		addrs, dropped := filter.apply(addrs)
		if dropped > 0 {
			warnings = append(warnings, filter.warn(name, dropped, total))
		}

		for _, a := range addrs {
			out = append(out, Endpoint{
				Address: a.Unmap(),
				Ready:   true,
				Serving: true,
				PortMap: portMap,
			})
		}
	}

	// A partial failure is still a failure: silently serving a subset of the
	// declared names would look identical to success in status.
	if len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	return &Result{Endpoints: out, Warnings: warnings}, nil
}

func (d *DNS) resolveSRV(
	ctx context.Context,
	client LookupClient,
	cfg *netv1alpha1.DNSSource,
	ports []netv1alpha1.CrossServicePort,
) (*Result, error) {
	// An SRV record carries one port, so it can only populate one declared
	// port. Which one is either named explicitly or implied by there being just
	// the one.
	srvPortName := cfg.SRVPortName
	if srvPortName == "" && len(ports) == 1 {
		srvPortName = ports[0].Name
	}

	base := defaultPortMap(ports)
	filter := newScopeFilter(cfg)
	var out []Endpoint
	var errs []error
	var warnings []string

	for _, name := range cfg.Names {
		fqdn := FQDN(name)
		srvs, err := client.LookupSRV(ctx, fqdn)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolving SRV %s: %w", name, err))
			continue
		}

		for _, srv := range srvs {
			addrs, err := client.LookupNetIP(ctx, "ip4", FQDN(srv.Target))
			if err != nil {
				errs = append(errs, fmt.Errorf("resolving SRV target %s: %w", srv.Target, err))
				continue
			}

			// Filtered on the target's addresses, not the SRV record itself: an
			// SRV record carries a name and a port, and the routability that is
			// being selected for belongs to whatever that name resolves to.
			total := len(addrs)
			addrs, dropped := filter.apply(addrs)
			if dropped > 0 {
				warnings = append(warnings, filter.warn(srv.Target, dropped, total))
			}

			// Copied per target: each SRV record carries its own port, so the
			// map cannot be shared across endpoints.
			pm := make(map[string]int32, len(base))
			maps.Copy(pm, base)
			if _, ok := pm[srvPortName]; ok {
				pm[srvPortName] = int32(srv.Port)
			}

			for _, a := range addrs {
				out = append(out, Endpoint{
					Address: a.Unmap(),
					Ready:   true,
					Serving: true,
					PortMap: pm,
				})
			}
		}
	}

	if len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	return &Result{Endpoints: out, Warnings: warnings}, nil
}

func joinErrors(errs []error) error {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

// ClampTTL applies the configured MinTTL/MaxTTL bounds, so a 1-second record
// cannot drive a hot loop and a 24-hour one does not go unrefreshed.
func ClampTTL(ttl time.Duration, cfg *netv1alpha1.DNSSource) time.Duration {
	minTTL, maxTTL := 5*time.Second, 5*time.Minute
	if cfg.MinTTL != nil {
		minTTL = cfg.MinTTL.Duration
	}
	if cfg.MaxTTL != nil {
		maxTTL = cfg.MaxTTL.Duration
	}
	if ttl < minTTL {
		return minTTL
	}
	if ttl > maxTTL {
		return maxTTL
	}
	return ttl
}

// RequeueAfter is how long until this DNS source should be re-resolved.
// UseTTL falls back to Interval when the resolver reported no TTL, which is
// always the case for the net.Resolver-backed client.
func RequeueAfter(cfg *netv1alpha1.DNSSource, ttl time.Duration) time.Duration {
	if cfg.UseTTL && ttl > 0 {
		return ClampTTL(ttl, cfg)
	}
	if cfg.Interval != nil && cfg.Interval.Duration > 0 {
		return cfg.Interval.Duration
	}
	return 30 * time.Second
}
