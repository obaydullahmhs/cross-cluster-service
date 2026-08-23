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
	"k8s.io/apimachinery/pkg/util/intstr"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
)

// Pods resolves Pod IPs directly, for Pods with no Service in front of them.
type Pods struct {
	Provider clusters.Provider
}

var _ Resolver = (*Pods)(nil)

// Resolve implements Resolver.
func (p *Pods) Resolve(ctx context.Context, src *netv1alpha1.Source, ports []netv1alpha1.CrossServicePort) (*Result, error) {
	if src.Pods == nil {
		return nil, fmt.Errorf("pods source has no pods config")
	}
	cfg := src.Pods

	cl, err := p.Provider.Get(ctx, clusters.SourceCluster(src))
	if err != nil {
		return nil, err
	}

	pods, err := fetchPods(ctx, cl, cfg)
	if err != nil {
		return nil, err
	}

	zones, err := zoneIndex(ctx, cl, boolOrTrue(cfg.PropagateZone))
	if err != nil {
		return nil, err
	}

	local := clusters.SourceCluster(src) == ""
	out := make([]Endpoint, 0, len(pods))
	var warnings []string

	for i := range pods {
		pod := &pods[i]

		if pod.Status.PodIP == "" ||
			pod.Status.Phase == corev1.PodSucceeded ||
			pod.Status.Phase == corev1.PodFailed {
			continue
		}

		terminating := pod.DeletionTimestamp != nil
		if terminating && !cfg.IncludeTerminating {
			continue
		}

		portMap, ok := podPortMap(pod, ports)
		if !ok {
			// I4: a Pod on which a declared named port does not resolve has
			// nothing to be reached on, so it is dropped and reported rather
			// than failing the whole source.
			warnings = append(warnings,
				fmt.Sprintf("pod %s/%s dropped: a declared targetPort does not resolve to a container port", pod.Namespace, pod.Name))
			continue
		}

		addr, err := netip.ParseAddr(pod.Status.PodIP)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("pod %s/%s has an unparseable IP %q", pod.Namespace, pod.Name, pod.Status.PodIP))
			continue
		}

		serving := podReady(pod)
		ep := Endpoint{
			Address:     addr.Unmap(),
			Ready:       serving && !terminating,
			Serving:     serving,
			Terminating: terminating,
			Zone:        zones[pod.Spec.NodeName],
			PortMap:     portMap,
		}

		// targetRef is set for local Pods only. Pointing it at an object in a
		// foreign cluster would dangle (I6).
		if local {
			ep.TargetRef = &corev1.ObjectReference{
				Kind:      "Pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
				UID:       pod.UID,
			}
		}
		out = append(out, ep)
	}

	return &Result{Endpoints: out, Warnings: warnings}, nil
}

func fetchPods(ctx context.Context, cl clusters.Client, cfg *netv1alpha1.PodSource) ([]corev1.Pod, error) {
	if cfg.Selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(cfg.Selector)
		if err != nil {
			return nil, fmt.Errorf("invalid pod selector: %w", err)
		}
		return cl.ListPods(ctx, cfg.Namespace, sel)
	}

	out := make([]corev1.Pod, 0, len(cfg.Names))
	for _, name := range cfg.Names {
		pod, err := cl.GetPod(ctx, cfg.Namespace, name)
		if err != nil {
			// A named Pod that does not exist yet is not a failure of the
			// source; it is simply not an endpoint.
			continue
		}
		out = append(out, *pod)
	}
	return out, nil
}

// podPortMap resolves every declared port against this Pod. The bool reports
// whether all of them resolved; a Pod missing one is dropped by the caller.
func podPortMap(pod *corev1.Pod, ports []netv1alpha1.CrossServicePort) (map[string]int32, bool) {
	out := make(map[string]int32, len(ports))
	for _, p := range ports {
		switch {
		case p.TargetPort == nil:
			out[p.Name] = p.Port
		case p.TargetPort.Type == intstr.String:
			num, ok := containerPort(pod, p.TargetPort.StrVal)
			if !ok {
				return nil, false
			}
			out[p.Name] = num
		default:
			out[p.Name] = int32(p.TargetPort.IntValue()) // #nosec G115 -- CRD bounds this to 1-65535
		}
	}
	return out, true
}

// containerPort finds a named port across the Pod's containers.
func containerPort(pod *corev1.Pod, name string) (int32, bool) {
	for i := range pod.Spec.Containers {
		for _, cp := range pod.Spec.Containers[i].Ports {
			if cp.Name == name {
				return cp.ContainerPort, true
			}
		}
	}
	for i := range pod.Spec.InitContainers {
		for _, cp := range pod.Spec.InitContainers[i].Ports {
			if cp.Name == name {
				return cp.ContainerPort, true
			}
		}
	}
	return 0, false
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// zoneIndex maps node name to zone, so Pod endpoints can carry topology
// without a Get per Pod. Cross-cluster endpoints carry zone rather than
// nodeName (I5).
func zoneIndex(ctx context.Context, cl clusters.Client, wanted bool) (map[string]string, error) {
	if !wanted {
		return nil, nil
	}
	nodes, err := cl.ListNodes(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(nodes))
	for i := range nodes {
		if z := nodes[i].Labels[netv1alpha1.ZoneLabel]; z != "" {
			out[nodes[i].Name] = z
		}
	}
	return out, nil
}

// boolOrTrue reads an optional bool whose documented default is true. Every
// such field in this API defaults on, so the default is not a parameter.
func boolOrTrue(b *bool) bool {
	return b == nil || *b
}
