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
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
)

// Service resolves a Service in the source cluster, deriving both addresses
// and ports from it.
//
// Deriving rather than declaring is the point: a nodePort is allocated out of
// 30000-32767 and changes if the Service is recreated, and a LoadBalancer
// address is assigned by the cloud provider. Either one, written by hand,
// needs hand-syncing forever.
type Service struct {
	Provider clusters.Provider

	// Lookup resolves LoadBalancer hostnames. Defaults to the process resolver.
	Lookup LookupClient
}

var _ Resolver = (*Service)(nil)

// portBinding ties a declared port to the remote Service port it maps to.
type portBinding struct {
	local  netv1alpha1.CrossServicePort
	remote corev1.ServicePort
}

// Resolve implements Resolver.
func (s *Service) Resolve(ctx context.Context, src *netv1alpha1.Source, ports []netv1alpha1.CrossServicePort) (*Result, error) {
	if src.Service == nil {
		return nil, fmt.Errorf("service source has no service config")
	}
	cfg := src.Service

	cl, err := s.Provider.Get(ctx, clusters.SourceCluster(src))
	if err != nil {
		return nil, err
	}

	svc, err := cl.GetService(ctx, cfg.Namespace, cfg.Name)
	if err != nil {
		return nil, err
	}

	bindings, err := bindPorts(ports, svc)
	if err != nil {
		return nil, err
	}

	switch cfg.Via {
	case netv1alpha1.ServiceExposureNodePort:
		return s.viaNodePort(ctx, cl, cfg, svc, bindings)
	case netv1alpha1.ServiceExposureLoadBalancer:
		return s.viaLoadBalancer(ctx, cfg, svc, bindings)
	case netv1alpha1.ServiceExposurePodIP:
		return s.viaPodIP(ctx, cl, cfg, bindings)
	default:
		return nil, fmt.Errorf("unsupported service exposure %q", cfg.Via)
	}
}

// bindPorts matches every declared port to a port on the remote Service.
//
// The join is by NAME by default, which keeps the key consistent with how
// kube-proxy joins a Service to its slices (I2). remotePort overrides it when
// the two sides disagree on naming.
func bindPorts(ports []netv1alpha1.CrossServicePort, svc *corev1.Service) ([]portBinding, error) {
	out := make([]portBinding, 0, len(ports))

	for _, p := range ports {
		match, err := matchRemotePort(p, svc)
		if err != nil {
			return nil, err
		}
		out = append(out, portBinding{local: p, remote: *match})
	}
	return out, nil
}

func matchRemotePort(p netv1alpha1.CrossServicePort, svc *corev1.Service) (*corev1.ServicePort, error) {
	// A single port on both sides matches implicitly, so the common
	// single-unnamed-port case needs no configuration at all.
	if p.RemotePort == nil && p.Name == "" && len(svc.Spec.Ports) == 1 {
		return &svc.Spec.Ports[0], nil
	}

	if p.RemotePort != nil && p.RemotePort.Type == intstr.Int {
		want := int32(p.RemotePort.IntValue()) // #nosec G115 -- CRD bounds this to 1-65535
		for i := range svc.Spec.Ports {
			if svc.Spec.Ports[i].Port == want {
				return &svc.Spec.Ports[i], nil
			}
		}
		return nil, fmt.Errorf("service %s/%s has no port numbered %d", svc.Namespace, svc.Name, want)
	}

	want := p.Name
	if p.RemotePort != nil {
		want = p.RemotePort.StrVal
	}
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Name == want {
			return &svc.Spec.Ports[i], nil
		}
	}
	return nil, fmt.Errorf("service %s/%s has no port named %q", svc.Namespace, svc.Name, want)
}

func (s *Service) viaNodePort(
	ctx context.Context,
	cl clusters.Client,
	cfg *netv1alpha1.ServiceSource,
	svc *corev1.Service,
	bindings []portBinding,
) (*Result, error) {
	portMap := make(map[string]int32, len(bindings))
	for _, b := range bindings {
		if b.remote.NodePort == 0 {
			return nil, fmt.Errorf("service %s/%s port %q has no allocated nodePort: it is of type %s",
				svc.Namespace, svc.Name, b.remote.Name, svc.Spec.Type)
		}
		portMap[b.local.Name] = b.remote.NodePort
	}

	exposure := cfg.NodePort
	if exposure == nil {
		exposure = &netv1alpha1.NodePortExposure{}
	}

	nodes, err := fetchNodes(ctx, cl, exposure.Selector, exposure.Names)
	if err != nil {
		return nil, err
	}

	eps, warnings := nodeEndpoints(nodes, nodeSelection{
		addressType:          exposure.AddressType,
		requireReady:         boolOrTrue(exposure.RequireReady),
		excludeUnschedulable: boolOrTrue(exposure.ExcludeUnschedulable),
		propagateZone:        boolOrTrue(exposure.PropagateZone),
	}, portMap)

	return &Result{Endpoints: eps, Warnings: warnings}, nil
}

func (s *Service) viaLoadBalancer(
	ctx context.Context,
	cfg *netv1alpha1.ServiceSource,
	svc *corev1.Service,
	bindings []portBinding,
) (*Result, error) {
	portMap := make(map[string]int32, len(bindings))
	for _, b := range bindings {
		portMap[b.local.Name] = b.remote.Port
	}

	ingress := svc.Status.LoadBalancer.Ingress
	if len(ingress) == 0 {
		return nil, fmt.Errorf("service %s/%s has no loadBalancer ingress yet", svc.Namespace, svc.Name)
	}

	var out []Endpoint
	var warnings []string
	var ttl time.Duration

	for _, in := range ingress {
		switch {
		case in.IP != "":
			addr, err := netip.ParseAddr(in.IP)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("loadBalancer ingress IP %q is unparseable", in.IP))
				continue
			}
			out = append(out, Endpoint{Address: addr.Unmap(), Ready: true, Serving: true, PortMap: portMap})

		case in.Hostname != "":
			// AWS ELB and NLB hand back a hostname. kube-proxy ignores
			// addressType FQDN (I11), so the name has to be resolved here and
			// kept fresh -- which is what makes a hostname LoadBalancer a
			// polling source despite the Service itself being watched.
			resolution := &netv1alpha1.DNSResolution{}
			if cfg.LoadBalancer != nil && cfg.LoadBalancer.HostnameResolution != nil {
				resolution = cfg.LoadBalancer.HostnameResolution
			}
			ttl = resolutionInterval(resolution)

			client := s.Lookup
			if client == nil {
				client = NewSystemLookupClient(resolution.Nameservers)
			}
			addrs, err := client.LookupNetIP(ctx, "ip4", FQDN(in.Hostname))
			if err != nil {
				return nil, fmt.Errorf("resolving loadBalancer hostname %s: %w", in.Hostname, err)
			}
			for _, a := range addrs {
				out = append(out, Endpoint{Address: a.Unmap(), Ready: true, Serving: true, PortMap: portMap})
			}
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("service %s/%s loadBalancer ingress resolved to no addresses", svc.Namespace, svc.Name)
	}
	return &Result{Endpoints: out, Warnings: warnings, TTL: ttl}, nil
}

func resolutionInterval(r *netv1alpha1.DNSResolution) time.Duration {
	if r.Interval != nil && r.Interval.Duration > 0 {
		return r.Interval.Duration
	}
	return 30 * time.Second
}

func (s *Service) viaPodIP(
	ctx context.Context,
	cl clusters.Client,
	cfg *netv1alpha1.ServiceSource,
	bindings []portBinding,
) (*Result, error) {
	slices, err := cl.ListEndpointSlices(ctx, cfg.Namespace, cfg.Name)
	if err != nil {
		return nil, err
	}

	exposure := cfg.PodIP
	if exposure == nil {
		exposure = &netv1alpha1.PodIPExposure{}
	}
	propagateZone := boolOrTrue(exposure.PropagateZone)

	var out []Endpoint
	var warnings []string

	for i := range slices {
		slice := &slices[i]

		// The remote slice already carries per-endpoint resolved ports, so the
		// named-port fragmentation of I4 has been dealt with on that side.
		// Re-deriving it from Pods here would reproduce the problem for no
		// benefit.
		portMap, ok := slicePortMap(slice, bindings)
		if !ok {
			warnings = append(warnings,
				fmt.Sprintf("endpointslice %s/%s skipped: it does not carry every mapped port", slice.Namespace, slice.Name))
			continue
		}

		for _, ep := range slice.Endpoints {
			ready := ep.Conditions.Ready == nil || *ep.Conditions.Ready
			serving := ep.Conditions.Serving == nil || *ep.Conditions.Serving
			terminating := ep.Conditions.Terminating != nil && *ep.Conditions.Terminating

			if terminating && !exposure.IncludeTerminating {
				continue
			}

			for _, raw := range ep.Addresses {
				addr, err := netip.ParseAddr(raw)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("endpoint address %q is unparseable", raw))
					continue
				}
				e := Endpoint{
					Address:     addr.Unmap(),
					Ready:       ready && !terminating,
					Serving:     serving,
					Terminating: terminating,
					PortMap:     portMap,
					// targetRef is deliberately not carried across: it names a
					// Pod in the source cluster and would dangle here (I6).
				}
				if propagateZone && ep.Zone != nil {
					e.Zone = *ep.Zone
				}
				out = append(out, e)
			}
		}
	}

	return &Result{Endpoints: out, Warnings: warnings}, nil
}

// slicePortMap translates the remote slice's ports into our declared port
// names. The bool reports whether every mapped port was present.
func slicePortMap(slice *discoveryv1.EndpointSlice, bindings []portBinding) (map[string]int32, bool) {
	byName := make(map[string]int32, len(slice.Ports))
	for _, p := range slice.Ports {
		if p.Port == nil {
			continue
		}
		name := ""
		if p.Name != nil {
			name = *p.Name
		}
		byName[name] = *p.Port
	}

	out := make(map[string]int32, len(bindings))
	for _, b := range bindings {
		num, ok := byName[b.remote.Name]
		if !ok {
			return nil, false
		}
		out[b.local.Name] = num
	}
	return out, true
}
