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
	"time"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/resolver"
)

// FailureState is what the failure policy decided to serve this reconcile.
type FailureState struct {
	// Endpoints to write. May be last-known-good rather than freshly resolved.
	Endpoints []resolver.Endpoint
	// Stale is true when Endpoints are last-known-good rather than fresh.
	Stale bool
	// Degraded is true once the source has crossed its failure threshold.
	Degraded bool
	// ConsecutiveErrors after this reconcile.
	ConsecutiveErrors int32
	// Reason for the Degraded condition, empty when healthy.
	Reason string
}

// FailurePolicyDefaults fills in the documented defaults for a nil or partial
// policy, so callers never branch on nil.
func FailurePolicyDefaults(p *netv1alpha1.FailurePolicy) (threshold int32, stale time.Duration, onStale netv1alpha1.StaleAction) {
	threshold, stale, onStale = 3, 5*time.Minute, netv1alpha1.StaleActionMarkNotReady
	if p == nil {
		return
	}
	if p.FailureThreshold > 0 {
		threshold = p.FailureThreshold
	}
	if p.StaleThreshold != nil {
		stale = p.StaleThreshold.Duration
	}
	if p.OnStale != "" {
		onStale = p.OnStale
	}
	return
}

// ApplyFailurePolicy decides what to serve given a resolution outcome and the
// previous source status.
//
// This is invariant I9. A two-second CoreDNS blip must not black-hole
// production traffic, so a failing source keeps serving its last-known-good
// endpoints -- and even once those go stale the default is to mark them
// not-ready rather than to empty the slice.
func ApplyFailurePolicy(
	policy *netv1alpha1.FailurePolicy,
	prev *netv1alpha1.SourceStatus,
	fresh []resolver.Endpoint,
	resolveErr error,
	now time.Time,
) FailureState {
	threshold, staleAfter, onStale := FailurePolicyDefaults(policy)

	if resolveErr == nil {
		return FailureState{Endpoints: fresh}
	}

	consecutive := int32(1)
	if prev != nil {
		consecutive = prev.ConsecutiveErrors + 1
	}

	lastKnownGood := endpointsFromStatus(prev)

	// Below the threshold this is a blip: keep serving, and do not even report
	// staleness, or every transient DNS hiccup would flap the condition.
	if consecutive < threshold {
		return FailureState{
			Endpoints:         lastKnownGood,
			ConsecutiveErrors: consecutive,
		}
	}

	// Past the threshold but inside the stale window: still serving, now
	// visibly degraded.
	if withinStaleWindow(prev, now, staleAfter) {
		return FailureState{
			Endpoints:         lastKnownGood,
			Stale:             true,
			Degraded:          true,
			ConsecutiveErrors: consecutive,
			Reason:            netv1alpha1.ReasonStaleEndpoints,
		}
	}

	if onStale == netv1alpha1.StaleActionRemove {
		return FailureState{
			Stale:             true,
			Degraded:          true,
			ConsecutiveErrors: consecutive,
			Reason:            netv1alpha1.ReasonStaleEndpoints,
		}
	}

	notReady := make([]resolver.Endpoint, 0, len(lastKnownGood))
	for _, e := range lastKnownGood {
		e.Ready = false
		e.Serving = false
		notReady = append(notReady, e)
	}
	return FailureState{
		Endpoints:         notReady,
		Stale:             true,
		Degraded:          true,
		ConsecutiveErrors: consecutive,
		Reason:            netv1alpha1.ReasonStaleEndpoints,
	}
}

func withinStaleWindow(prev *netv1alpha1.SourceStatus, now time.Time, staleAfter time.Duration) bool {
	if prev == nil || prev.LastSuccessTime == nil {
		// Never succeeded: there is nothing to go stale, so serve nothing
		// rather than pretending a window is open.
		return false
	}
	return now.Sub(prev.LastSuccessTime.Time) <= staleAfter
}

func endpointsFromStatus(prev *netv1alpha1.SourceStatus) []resolver.Endpoint {
	if prev == nil {
		return nil
	}
	out := make([]resolver.Endpoint, 0, len(prev.LastKnownGood))
	for _, pe := range prev.LastKnownGood {
		addr, err := netip.ParseAddr(pe.Address)
		if err != nil {
			continue
		}
		out = append(out, resolver.Endpoint{
			Address:  addr,
			Ready:    pe.Ready,
			Serving:  pe.Ready,
			Zone:     pe.Zone,
			Hostname: pe.Hostname,
			PortMap:  pe.Ports,
		})
	}
	return out
}

// MaxPersistedEndpoints bounds how many endpoints are carried in status. Past
// this the failure policy degrades to whatever is already written in the
// slices, rather than growing the status object without limit.
const MaxPersistedEndpoints = 512

// ToPersisted converts endpoints for storage in status.
func ToPersisted(in []resolver.Endpoint) []netv1alpha1.PersistedEndpoint {
	if len(in) > MaxPersistedEndpoints {
		in = in[:MaxPersistedEndpoints]
	}
	out := make([]netv1alpha1.PersistedEndpoint, 0, len(in))
	for _, e := range in {
		out = append(out, netv1alpha1.PersistedEndpoint{
			Address:  e.Address.String(),
			Ready:    e.Ready,
			Zone:     e.Zone,
			Hostname: e.Hostname,
			Ports:    e.PortMap,
		})
	}
	return out
}
