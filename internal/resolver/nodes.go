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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
)

// excludeFromLBLabel marks a Node that should not receive external traffic.
const excludeFromLBLabel = "node.kubernetes.io/exclude-from-external-load-balancers"

// Nodes resolves Node addresses for a port on the node itself: a DaemonSet
// bound to a hostPort, a node agent, a process outside Kubernetes.
type Nodes struct {
	Provider clusters.Provider
}

var _ Resolver = (*Nodes)(nil)

// Resolve implements Resolver.
func (n *Nodes) Resolve(ctx context.Context, src *netv1alpha1.Source, ports []netv1alpha1.CrossServicePort) (*Result, error) {
	if src.Nodes == nil {
		return nil, fmt.Errorf("nodes source has no nodes config")
	}
	cfg := src.Nodes

	cl, err := n.Provider.Get(ctx, clusters.SourceCluster(src))
	if err != nil {
		return nil, err
	}

	nodes, err := fetchNodes(ctx, cl, cfg.Selector, cfg.Names)
	if err != nil {
		return nil, err
	}

	sel := nodeSelection{
		addressType:          cfg.AddressType,
		requireReady:         boolOrTrue(cfg.RequireReady),
		excludeUnschedulable: boolOrTrue(cfg.ExcludeUnschedulable),
		propagateZone:        boolOrTrue(cfg.PropagateZone),
	}

	// The port comes from the declared targetPort: there is no Service to read
	// one from, which is the whole reason this source type exists.
	portMap := defaultPortMap(ports)

	eps, warnings := nodeEndpoints(nodes, sel, portMap)
	return &Result{Endpoints: eps, Warnings: warnings}, nil
}

// nodeSelection is the filtering shared by the Nodes source and the Service
// source's NodePort exposure.
type nodeSelection struct {
	addressType          netv1alpha1.NodeAddressType
	requireReady         bool
	excludeUnschedulable bool
	propagateZone        bool
}

func fetchNodes(
	ctx context.Context,
	cl clusters.Client,
	selector *metav1.LabelSelector,
	names []string,
) ([]corev1.Node, error) {
	if selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return nil, fmt.Errorf("invalid node selector: %w", err)
		}
		return cl.ListNodes(ctx, sel)
	}
	if len(names) > 0 {
		out := make([]corev1.Node, 0, len(names))
		for _, name := range names {
			node, err := cl.GetNode(ctx, name)
			if err != nil {
				continue
			}
			out = append(out, *node)
		}
		return out, nil
	}
	// Neither selector nor names: every eligible node.
	return cl.ListNodes(ctx, nil)
}

func nodeEndpoints(nodes []corev1.Node, sel nodeSelection, portMap map[string]int32) ([]Endpoint, []string) {
	out := make([]Endpoint, 0, len(nodes))
	var warnings []string

	for i := range nodes {
		node := &nodes[i]

		if sel.excludeUnschedulable && nodeExcluded(node) {
			continue
		}

		addrStr, ok := nodeAddress(node, sel.addressType)
		if !ok {
			warnings = append(warnings,
				fmt.Sprintf("node %s dropped: no %s address", node.Name, addressTypeOrDefault(sel.addressType)))
			continue
		}
		addr, err := netip.ParseAddr(addrStr)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("node %s has an unparseable address %q", node.Name, addrStr))
			continue
		}

		// A not-ready node is emitted as a not-ready endpoint rather than
		// dropped, so the counts in status still describe reality.
		ready := true
		if sel.requireReady {
			ready = nodeReady(node)
		}

		ep := Endpoint{
			Address: addr.Unmap(),
			Ready:   ready,
			Serving: ready,
			PortMap: portMap,
			// nodeName is deliberately never set, even for a local Node
			// source: it is meaningful only for Pod-backed endpoints (I5).
		}
		if sel.propagateZone {
			ep.Zone = node.Labels[netv1alpha1.ZoneLabel]
		}
		out = append(out, ep)
	}
	return out, warnings
}

func nodeExcluded(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return true
	}
	_, excluded := node.Labels[excludeFromLBLabel]
	return excluded
}

func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func addressTypeOrDefault(at netv1alpha1.NodeAddressType) netv1alpha1.NodeAddressType {
	if at == "" {
		return netv1alpha1.NodeAddressTypeInternalIP
	}
	return at
}

// nodeAddress picks the address for the requested type. The Prefer variants
// fall back to the other kind rather than dropping the node, which is what
// makes them useful on mixed public/private node pools.
func nodeAddress(node *corev1.Node, at netv1alpha1.NodeAddressType) (string, bool) {
	internal := firstAddress(node, corev1.NodeInternalIP)
	external := firstAddress(node, corev1.NodeExternalIP)

	switch addressTypeOrDefault(at) {
	case netv1alpha1.NodeAddressTypeExternalIP:
		return external, external != ""
	case netv1alpha1.NodeAddressTypePreferExternal:
		if external != "" {
			return external, true
		}
		return internal, internal != ""
	case netv1alpha1.NodeAddressTypePreferInternal:
		if internal != "" {
			return internal, true
		}
		return external, external != ""
	default: // InternalIP
		return internal, internal != ""
	}
}

func firstAddress(node *corev1.Node, kind corev1.NodeAddressType) string {
	for _, a := range node.Status.Addresses {
		if a.Type == kind {
			return a.Address
		}
	}
	return ""
}
