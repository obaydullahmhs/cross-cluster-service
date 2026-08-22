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
	"net/netip"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// Static resolves literal addresses. Static endpoints are assumed ready: this
// controller does no health checking, so readiness has to come from the source
// and a literal address has no source to ask.
type Static struct{}

var _ Resolver = (*Static)(nil)

// Resolve implements Resolver.
func (s *Static) Resolve(_ context.Context, src *netv1alpha1.Source, ports []netv1alpha1.CrossServicePort) (*Result, error) {
	if src.Static == nil {
		return nil, fmt.Errorf("static source has no static config")
	}

	portMap := defaultPortMap(ports)
	out := make([]Endpoint, 0, len(src.Static.Addresses))

	for _, raw := range src.Static.Addresses {
		// Parsed here rather than in CEL: the isIP() extension is not available
		// on the minimum supported apiserver (1.27).
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid static address %q: %w", raw, err)
		}

		out = append(out, Endpoint{
			Address: addr.Unmap(),
			Ready:   true,
			Serving: true,
			Zone:    src.Static.Zone,
			PortMap: portMap,
		})
	}

	return &Result{Endpoints: out}, nil
}
