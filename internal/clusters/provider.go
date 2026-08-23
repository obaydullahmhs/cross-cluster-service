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

// Package clusters resolves a source cluster into a read-only client.
//
// Everything here is read-only by design: this controller never writes to a
// cluster it did not originate in.
package clusters

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// Client is the read-only surface the resolvers need from a source cluster.
//
// GetService and ListEndpointSlices are here in addition to the Pod and Node
// accessors because the Service source derives its ports from the remote
// Service, and its PodIP exposure reads that Service's slices rather than
// re-deriving endpoints from Pods.
type Client interface {
	ListPods(ctx context.Context, namespace string, sel labels.Selector) ([]corev1.Pod, error)
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)

	ListNodes(ctx context.Context, sel labels.Selector) ([]corev1.Node, error)
	GetNode(ctx context.Context, name string) (*corev1.Node, error)

	GetService(ctx context.Context, namespace, name string) (*corev1.Service, error)
	ListEndpointSlices(ctx context.Context, namespace, serviceName string) ([]discoveryv1.EndpointSlice, error)

	// Healthy reports whether this client can currently reach its cluster.
	Healthy() error
}

// Provider hands out clients for named clusters. An empty name means the local
// cluster.
type Provider interface {
	Get(ctx context.Context, name string) (Client, error)
	// Release drops a consumer's reference, so an unused remote client can be
	// torn down along with its informers.
	Release(name string, consumer types.NamespacedName)
	// Invalidate discards a cached client, after a spec or credential change.
	Invalidate(name string)
}

// ErrRemoteNotImplemented is returned for a named cluster by a provider that
// only serves the local one.
type ErrRemoteNotImplemented struct{ Cluster string }

func (e *ErrRemoteNotImplemented) Error() string {
	return fmt.Sprintf("remote cluster %q is not available in this build: remote access lands in a later milestone", e.Cluster)
}

// SourceCluster returns the cluster name a source resolves against; empty means
// local.
func SourceCluster(src *netv1alpha1.Source) string {
	if src.ClusterRef == nil {
		return ""
	}
	return src.ClusterRef.Name
}

// LocalProvider serves the cluster the controller itself runs in.
type LocalProvider struct {
	Client client.Client
}

var _ Provider = (*LocalProvider)(nil)

// Get returns the local client. A named cluster is an error until the remote
// access milestone lands.
func (p *LocalProvider) Get(_ context.Context, name string) (Client, error) {
	if name != "" {
		return nil, &ErrRemoteNotImplemented{Cluster: name}
	}
	return &localClient{c: p.Client}, nil
}

// Release is a no-op: the local client is the manager's own and is never torn
// down.
func (p *LocalProvider) Release(string, types.NamespacedName) {}

// Invalidate is a no-op for the same reason.
func (p *LocalProvider) Invalidate(string) {}

// localClient reads through the manager's cache, so every call is served from
// an informer rather than hitting the apiserver.
type localClient struct {
	c client.Client
}

func (l *localClient) ListPods(ctx context.Context, namespace string, sel labels.Selector) ([]corev1.Pod, error) {
	var list corev1.PodList
	opts := []client.ListOption{client.InNamespace(namespace)}
	if sel != nil {
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	if err := l.c.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing pods in %s: %w", namespace, err)
	}
	return list.Items, nil
}

func (l *localClient) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := l.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod); err != nil {
		return nil, fmt.Errorf("getting pod %s/%s: %w", namespace, name, err)
	}
	return &pod, nil
}

func (l *localClient) ListNodes(ctx context.Context, sel labels.Selector) ([]corev1.Node, error) {
	var list corev1.NodeList
	var opts []client.ListOption
	if sel != nil {
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	if err := l.c.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	return list.Items, nil
}

func (l *localClient) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	var node corev1.Node
	if err := l.c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		return nil, fmt.Errorf("getting node %s: %w", name, err)
	}
	return &node, nil
}

func (l *localClient) GetService(ctx context.Context, namespace, name string) (*corev1.Service, error) {
	var svc corev1.Service
	if err := l.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &svc); err != nil {
		return nil, fmt.Errorf("getting service %s/%s: %w", namespace, name, err)
	}
	return &svc, nil
}

func (l *localClient) ListEndpointSlices(ctx context.Context, namespace, serviceName string) ([]discoveryv1.EndpointSlice, error) {
	var list discoveryv1.EndpointSliceList
	err := l.c.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingLabels{netv1alpha1.ServiceNameLabel: serviceName},
	)
	if err != nil {
		return nil, fmt.Errorf("listing endpointslices for %s/%s: %w", namespace, serviceName, err)
	}
	return list.Items, nil
}

func (l *localClient) Healthy() error { return nil }

// RoutingProvider sends unnamed lookups to the local cluster and named ones to
// the remote cache, so resolvers never branch on whether a source is remote.
type RoutingProvider struct {
	Local  Provider
	Remote Provider
}

var _ Provider = (*RoutingProvider)(nil)

// NewRoutingProvider builds a router over a local and a remote provider.
func NewRoutingProvider(local, remote Provider) *RoutingProvider {
	return &RoutingProvider{Local: local, Remote: remote}
}

// Get routes by whether a cluster was named.
func (p *RoutingProvider) Get(ctx context.Context, name string) (Client, error) {
	if name == "" {
		return p.Local.Get(ctx, "")
	}
	if p.Remote == nil {
		return nil, &ErrRemoteNotImplemented{Cluster: name}
	}
	return p.Remote.Get(ctx, name)
}

// Release forwards to the remote provider; local clients are never torn down.
func (p *RoutingProvider) Release(name string, consumer types.NamespacedName) {
	if p.Remote != nil {
		p.Remote.Release(name, consumer)
	}
}

// Invalidate forwards to the remote provider.
func (p *RoutingProvider) Invalidate(name string) {
	if p.Remote != nil {
		p.Remote.Invalidate(name)
	}
}
