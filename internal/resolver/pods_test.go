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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
)

// fakeCluster is an in-memory clusters.Client.
type fakeCluster struct {
	pods   []corev1.Pod
	nodes  []corev1.Node
	svc    *corev1.Service
	slices []discoveryv1.EndpointSlice
	err    error
}

func (f *fakeCluster) ListPods(_ context.Context, ns string, sel labels.Selector) ([]corev1.Pod, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := []corev1.Pod{}
	for _, p := range f.pods {
		if p.Namespace != ns {
			continue
		}
		if sel != nil && !sel.Matches(labels.Set(p.Labels)) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeCluster) GetPod(_ context.Context, ns, name string) (*corev1.Pod, error) {
	for i := range f.pods {
		if f.pods[i].Namespace == ns && f.pods[i].Name == name {
			return &f.pods[i], nil
		}
	}
	return nil, context.Canceled
}

func (f *fakeCluster) ListNodes(_ context.Context, sel labels.Selector) ([]corev1.Node, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := []corev1.Node{}
	for _, n := range f.nodes {
		if sel != nil && !sel.Matches(labels.Set(n.Labels)) {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeCluster) GetNode(_ context.Context, name string) (*corev1.Node, error) {
	for i := range f.nodes {
		if f.nodes[i].Name == name {
			return &f.nodes[i], nil
		}
	}
	return nil, context.Canceled
}

func (f *fakeCluster) GetService(_ context.Context, _, _ string) (*corev1.Service, error) {
	if f.svc == nil {
		return nil, context.Canceled
	}
	return f.svc, nil
}

func (f *fakeCluster) ListEndpointSlices(_ context.Context, _, _ string) ([]discoveryv1.EndpointSlice, error) {
	return f.slices, nil
}

func (f *fakeCluster) Healthy() error { return nil }

// fakeProvider serves one fakeCluster as the local cluster.
type fakeProvider struct{ c *fakeCluster }

func (p *fakeProvider) Get(_ context.Context, name string) (clusters.Client, error) {
	if name != "" {
		return nil, &clusters.ErrRemoteNotImplemented{Cluster: name}
	}
	return p.c, nil
}
func (p *fakeProvider) Release(string, types.NamespacedName) {}
func (p *fakeProvider) Invalidate(string)                    {}

func readyPod(name, ip string, lbls map[string]string, containerPorts map[string]int32) corev1.Pod {
	ports := make([]corev1.ContainerPort, 0, len(containerPorts))
	for n, v := range containerPorts {
		ports = append(ports, corev1.ContainerPort{Name: n, ContainerPort: v})
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsDefault, Labels: lbls},
		Spec: corev1.PodSpec{
			NodeName:   nodeA,
			Containers: []corev1.Container{{Name: labelApp, Ports: ports}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// TestI4_NamedPortsResolvePerPod covers the resolver half of invariant I4.
func TestI4_NamedPortsResolvePerPod(t *testing.T) {
	fc := &fakeCluster{
		pods: []corev1.Pod{
			readyPod("a", "10.0.0.1", map[string]string{labelApp: appWeb}, map[string]int32{portHTTP: 8080}),
			readyPod("b", "10.0.0.2", map[string]string{labelApp: appWeb}, map[string]int32{portHTTP: 9090}),
		},
		nodes: []corev1.Node{{
			ObjectMeta: metav1.ObjectMeta{Name: nodeA, Labels: map[string]string{netv1alpha1.ZoneLabel: zoneA}},
		}},
	}
	r := &Pods{Provider: &fakeProvider{c: fc}}

	src := &netv1alpha1.Source{
		Type: netv1alpha1.SourceTypePods,
		Pods: &netv1alpha1.PodSource{
			Namespace: nsDefault,
			Selector:  &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: appWeb}},
		},
	}
	tp := intstr.FromString(portHTTP)
	ports := []netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80, TargetPort: &tp}}

	res, err := r.Resolve(context.Background(), src, ports)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Endpoints) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(res.Endpoints))
	}

	// The same port NAME resolves to different numbers per Pod. Grouping turns
	// that into separate slices; the resolver's job is to report it faithfully.
	got := map[string]int32{}
	for _, e := range res.Endpoints {
		got[e.Address.String()] = e.PortMap[portHTTP]
	}
	if got["10.0.0.1"] != 8080 || got["10.0.0.2"] != 9090 {
		t.Errorf("port map = %v, want per-pod resolution 8080/9090", got)
	}
}

func TestPodsDropsPodsWhoseNamedPortIsMissing(t *testing.T) {
	fc := &fakeCluster{
		pods: []corev1.Pod{
			readyPod("a", "10.0.0.1", map[string]string{labelApp: appWeb}, map[string]int32{portHTTP: 8080}),
			// No container port called "http" at all.
			readyPod("b", "10.0.0.2", map[string]string{labelApp: appWeb}, map[string]int32{"metrics": 9100}),
		},
	}
	r := &Pods{Provider: &fakeProvider{c: fc}}

	tp := intstr.FromString(portHTTP)
	res, err := r.Resolve(context.Background(),
		&netv1alpha1.Source{
			Type: netv1alpha1.SourceTypePods,
			Pods: &netv1alpha1.PodSource{
				Namespace: nsDefault,
				Selector:  &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: appWeb}},
			},
		},
		[]netv1alpha1.CrossServicePort{{Name: portHTTP, Port: 80, TargetPort: &tp}},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Dropped with a warning, never an error: one unusable Pod must not fail
	// an otherwise healthy source.
	if len(res.Endpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(res.Endpoints))
	}
	if len(res.Warnings) != 1 {
		t.Errorf("warnings = %v, want exactly one", res.Warnings)
	}
}

func TestPodsReadinessAndTerminating(t *testing.T) {
	notReady := readyPod("nr", "10.0.0.3", nil, nil)
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse

	terminating := readyPod("term", "10.0.0.4", nil, nil)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"keep"}

	fc := &fakeCluster{pods: []corev1.Pod{readyPod("ok", "10.0.0.1", nil, nil), notReady, terminating}}
	r := &Pods{Provider: &fakeProvider{c: fc}}

	base := &netv1alpha1.PodSource{Namespace: nsDefault, Selector: &metav1.LabelSelector{}}
	ports := []netv1alpha1.CrossServicePort{{Port: 80}}

	t.Run("terminating pods are excluded by default", func(t *testing.T) {
		res, err := r.Resolve(context.Background(),
			&netv1alpha1.Source{Type: netv1alpha1.SourceTypePods, Pods: base}, ports)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(res.Endpoints) != 2 {
			t.Fatalf("got %d endpoints, want 2 (terminating excluded)", len(res.Endpoints))
		}
		for _, e := range res.Endpoints {
			if e.Address.String() == "10.0.0.3" && e.Ready {
				t.Error("a not-ready pod produced a ready endpoint")
			}
		}
	})

	t.Run("includeTerminating keeps them as serving but terminating", func(t *testing.T) {
		cfg := *base
		cfg.IncludeTerminating = true
		res, err := r.Resolve(context.Background(),
			&netv1alpha1.Source{Type: netv1alpha1.SourceTypePods, Pods: &cfg}, ports)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(res.Endpoints) != 3 {
			t.Fatalf("got %d endpoints, want 3", len(res.Endpoints))
		}
		for _, e := range res.Endpoints {
			if e.Address.String() != "10.0.0.4" {
				continue
			}
			if e.Ready {
				t.Error("a terminating endpoint must not be ready")
			}
			if !e.Terminating || !e.Serving {
				t.Error("a terminating endpoint should still be serving, for graceful draining")
			}
		}
	})
}

// TestI5_ZoneNotNodeName covers invariant I5 at the resolver boundary.
func TestI5_ZoneNotNodeName(t *testing.T) {
	fc := &fakeCluster{
		pods: []corev1.Pod{readyPod("a", "10.0.0.1", nil, nil)},
		nodes: []corev1.Node{{
			ObjectMeta: metav1.ObjectMeta{Name: nodeA, Labels: map[string]string{netv1alpha1.ZoneLabel: zoneA}},
		}},
	}
	r := &Pods{Provider: &fakeProvider{c: fc}}

	res, err := r.Resolve(context.Background(),
		&netv1alpha1.Source{
			Type: netv1alpha1.SourceTypePods,
			Pods: &netv1alpha1.PodSource{Namespace: nsDefault, Selector: &metav1.LabelSelector{}},
		},
		[]netv1alpha1.CrossServicePort{{Port: 80}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Endpoints[0].Zone != zoneA {
		t.Errorf("zone = %q, want %q propagated from the pod's node", res.Endpoints[0].Zone, zoneA)
	}
}

func TestPodsRejectsRemoteClusterUntilItIsImplemented(t *testing.T) {
	r := &Pods{Provider: &fakeProvider{c: &fakeCluster{}}}
	_, err := r.Resolve(context.Background(),
		&netv1alpha1.Source{
			Type:       netv1alpha1.SourceTypePods,
			ClusterRef: &netv1alpha1.ClusterRef{Name: "secondary-a"},
			Pods:       &netv1alpha1.PodSource{Namespace: nsDefault, Selector: &metav1.LabelSelector{}},
		}, nil)

	var notImpl *clusters.ErrRemoteNotImplemented
	if !errors.As(err, &notImpl) {
		t.Fatalf("err = %v, want ErrRemoteNotImplemented", err)
	}
}
