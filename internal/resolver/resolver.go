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

// Package resolver turns a Source into a flat list of endpoints. Nothing in
// here knows what an EndpointSlice is: resolution and slice-writing are kept
// strictly separate so that every source type reduces to the same intermediate
// representation before anything is written.
package resolver

import (
	"context"
	"net/netip"
	"time"

	corev1 "k8s.io/api/core/v1"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// Endpoint is one resolved backend, independent of where it came from.
type Endpoint struct {
	Address netip.Addr

	Ready       bool
	Serving     bool
	Terminating bool

	// Zone is topology.kubernetes.io/zone, when known. Cross-cluster endpoints
	// carry zone instead of nodeName, which names a node in the local cluster
	// and would break internalTrafficPolicy: Local (I5).
	Zone string

	// Hostname is only meaningful for a headless Service.
	Hostname string

	// TargetRef is set for local Pod sources only. It is deliberately nil for
	// remote clusters, where it would dangle (I6).
	TargetRef *corev1.ObjectReference

	// PortMap maps a CrossService port name to the resolved backend port. The
	// empty string keys the single unnamed port (I2).
	PortMap map[string]int32
}

// Result is one resolution pass.
type Result struct {
	Endpoints []Endpoint

	// TTL is a requeue hint from the source. Zero means use the configured
	// interval.
	TTL time.Duration

	// Warnings describe backends that were skipped. They surface as Events and
	// in status: a dropped backend is a reporting matter, not an error, because
	// one unusable Pod must not fail an otherwise healthy source.
	Warnings []string
}

// Resolver turns one Source into endpoints.
type Resolver interface {
	Resolve(ctx context.Context, src *netv1alpha1.Source, ports []netv1alpha1.CrossServicePort) (*Result, error)
}

// Registry dispatches to the resolver for a source's type.
type Registry struct {
	byType map[netv1alpha1.SourceType]Resolver
}

// NewRegistry builds a registry over the given resolvers.
func NewRegistry(resolvers map[netv1alpha1.SourceType]Resolver) *Registry {
	return &Registry{byType: resolvers}
}

// Resolve dispatches on src.Type.
func (r *Registry) Resolve(ctx context.Context, src *netv1alpha1.Source, ports []netv1alpha1.CrossServicePort) (*Result, error) {
	res, ok := r.byType[src.Type]
	if !ok {
		return nil, &UnsupportedSourceError{Type: src.Type}
	}
	return res.Resolve(ctx, src, ports)
}

// UnsupportedSourceError is returned for a source type this build cannot
// resolve. It is a distinct type so the controller can report
// AccessTypeNotImplemented rather than a generic failure.
type UnsupportedSourceError struct {
	Type netv1alpha1.SourceType
}

func (e *UnsupportedSourceError) Error() string {
	return "source type " + string(e.Type) + " is not implemented in this build"
}

// defaultPortMap builds the port map shared by sources whose backend port is
// declared rather than discovered -- DNS and Static. A source that resolves
// ports per-endpoint (Pods) builds its own.
func defaultPortMap(ports []netv1alpha1.CrossServicePort) map[string]int32 {
	out := make(map[string]int32, len(ports))
	for _, p := range ports {
		out[p.Name] = resolvedPort(p)
	}
	return out
}

// resolvedPort returns the backend port for a declared port. TargetPort feeds
// the EndpointSlice port, not the Service's (I2); an unset or string TargetPort
// falls back to Port, since a string names a container port that only a Pods
// source can resolve.
func resolvedPort(p netv1alpha1.CrossServicePort) int32 {
	if p.TargetPort == nil {
		return p.Port
	}
	if v := p.TargetPort.IntValue(); v > 0 {
		return int32(v) // #nosec G115 -- CRD validation bounds this to 1-65535
	}
	return p.Port
}
