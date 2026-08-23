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

package clusters

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters/auth"
)

// SpecFetcher returns the current RemoteCluster spec by name.
type SpecFetcher func(ctx context.Context, name string) (*netv1alpha1.RemoteCluster, error)

// OnStart is called once per newly started remote cluster, so the controller
// can attach watches to its informers.
type OnStart func(name string, c cluster.Cluster) error

// CachingProvider hands out remote clients, keyed by credential fingerprint and
// reference-counted by consumer.
//
// Keying on the fingerprint rather than only the cluster name is what makes a
// rotation take effect: new credentials hash differently, so the next Get
// builds a fresh connection instead of returning the stale one. Reference
// counting is what lets the old one's informers actually stop.
type CachingProvider struct {
	Builder *auth.Builder
	Fetch   SpecFetcher
	Scheme  *runtime.Scheme
	OnStart OnStart
	BaseCtx context.Context

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	fingerprint string
	cluster     cluster.Cluster
	cancel      context.CancelFunc
	consumers   map[types.NamespacedName]struct{}

	// timeout bounds every read against this cluster.
	timeout time.Duration
}

var _ Provider = (*CachingProvider)(nil)

// NewCachingProvider constructs a provider.
func NewCachingProvider(ctx context.Context, b *auth.Builder, scheme *runtime.Scheme, fetch SpecFetcher, onStart OnStart) *CachingProvider {
	return &CachingProvider{
		Builder: b,
		Fetch:   fetch,
		Scheme:  scheme,
		OnStart: onStart,
		BaseCtx: ctx,
		entries: map[string]*entry{},
	}
}

// Get returns a client for the named cluster, starting its informers on first
// use.
func (p *CachingProvider) Get(ctx context.Context, name string) (Client, error) {
	if name == "" {
		return nil, fmt.Errorf("caching provider requires a cluster name; the local cluster is served separately")
	}

	rc, err := p.Fetch(ctx, name)
	if err != nil {
		return nil, err
	}

	built, err := p.Builder.Build(ctx, rc)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.entries[name]; ok {
		if e.fingerprint == built.Fingerprint {
			return &remoteClient{c: e.cluster, timeout: e.timeout}, nil
		}
		// The credentials changed underneath us. Tear the old connection down
		// rather than letting it run until it starts failing.
		e.cancel()
		delete(p.entries, name)
	}

	e, err := p.start(name, built)
	if err != nil {
		return nil, err
	}
	p.entries[name] = e
	return &remoteClient{c: e.cluster, timeout: e.timeout}, nil
}

// start builds and runs a cluster's informers.
func (p *CachingProvider) start(name string, built *auth.Result) (*entry, error) {
	cl, err := cluster.New(built.Config, func(o *cluster.Options) {
		o.Scheme = p.Scheme
	})
	if err != nil {
		return nil, fmt.Errorf("building client for cluster %s: %w", name, err)
	}

	ctx, cancel := context.WithCancel(p.BaseCtx)
	if p.OnStart != nil {
		if err := p.OnStart(name, cl); err != nil {
			cancel()
			return nil, fmt.Errorf("attaching watches for cluster %s: %w", name, err)
		}
	}

	go func() {
		// A failure here is reported through Healthy and the RemoteCluster's
		// conditions rather than crashing the controller: one unreachable spoke
		// must not take down every other CrossService.
		_ = cl.Start(ctx)
	}()

	if !cl.GetCache().WaitForCacheSync(ctx) {
		cancel()
		return nil, fmt.Errorf("cache for cluster %s did not sync", name)
	}

	return &entry{
		fingerprint: built.Fingerprint,
		cluster:     cl,
		cancel:      cancel,
		consumers:   map[types.NamespacedName]struct{}{},
		timeout:     readTimeout(built.Config.Timeout),
	}, nil
}

// Acquire records that a CrossService depends on a cluster.
func (p *CachingProvider) Acquire(name string, consumer types.NamespacedName) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[name]; ok {
		e.consumers[consumer] = struct{}{}
	}
}

// Release drops a consumer's reference and stops the cluster's informers once
// nothing is using it.
func (p *CachingProvider) Release(name string, consumer types.NamespacedName) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.entries[name]
	if !ok {
		return
	}
	delete(e.consumers, consumer)
	if len(e.consumers) == 0 {
		e.cancel()
		delete(p.entries, name)
	}
}

// Invalidate discards a cached connection, after a spec or credential change.
func (p *CachingProvider) Invalidate(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[name]; ok {
		e.cancel()
		delete(p.entries, name)
	}
}

// Consumers returns how many CrossServices hold a reference, for status.
func (p *CachingProvider) Consumers(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[name]; ok {
		return len(e.consumers)
	}
	return 0
}

// defaultReadTimeout bounds a read against a remote cluster.
const defaultReadTimeout = 30 * time.Second

func readTimeout(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return defaultReadTimeout
}

// remoteClient reads through a remote cluster's informers.
//
// Every read is bounded. A cached client whose credentials have lost permission
// keeps an informer that can never sync, and a cache-backed read against it
// blocks indefinitely rather than returning the RBAC error -- which would wedge
// a reconcile worker rather than surfacing as a condition. The timeout turns
// that into an error the caller can report.
type remoteClient struct {
	c       cluster.Cluster
	timeout time.Duration
}

// bounded derives a deadline for one read.
func (r *remoteClient) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, readTimeout(r.timeout))
}

func (r *remoteClient) ListPods(ctx context.Context, namespace string, sel labels.Selector) ([]corev1.Pod, error) {
	c, cancel := r.bounded(ctx)
	defer cancel()
	return (&localClient{c: r.c.GetClient()}).ListPods(c, namespace, sel)
}

func (r *remoteClient) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	c, cancel := r.bounded(ctx)
	defer cancel()
	return (&localClient{c: r.c.GetClient()}).GetPod(c, namespace, name)
}

func (r *remoteClient) ListNodes(ctx context.Context, sel labels.Selector) ([]corev1.Node, error) {
	c, cancel := r.bounded(ctx)
	defer cancel()
	return (&localClient{c: r.c.GetClient()}).ListNodes(c, sel)
}

func (r *remoteClient) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	c, cancel := r.bounded(ctx)
	defer cancel()
	return (&localClient{c: r.c.GetClient()}).GetNode(c, name)
}

func (r *remoteClient) GetService(ctx context.Context, namespace, name string) (*corev1.Service, error) {
	c, cancel := r.bounded(ctx)
	defer cancel()
	return (&localClient{c: r.c.GetClient()}).GetService(c, namespace, name)
}

func (r *remoteClient) ListEndpointSlices(ctx context.Context, namespace, serviceName string) ([]discoveryv1.EndpointSlice, error) {
	c, cancel := r.bounded(ctx)
	defer cancel()
	return (&localClient{c: r.c.GetClient()}).ListEndpointSlices(c, namespace, serviceName)
}

func (r *remoteClient) Healthy() error { return nil }

// Probe verifies a connection works, without caching anything. It is used by
// the RemoteCluster controller to report Reachable and Authenticated.
func Probe(ctx context.Context, cfg *rest.Config) (string, error) {
	dc, err := discoveryClient(cfg)
	if err != nil {
		return "", err
	}
	v, err := dc.ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

func discoveryClient(cfg *rest.Config) (*discovery.DiscoveryClient, error) {
	return discovery.NewDiscoveryClientForConfig(cfg)
}

// ReleaseAll drops a consumer's reference from every cluster, for a
// CrossService that has been deleted.
func (p *CachingProvider) ReleaseAll(consumer types.NamespacedName) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for name, e := range p.entries {
		delete(e.consumers, consumer)
		if len(e.consumers) == 0 {
			e.cancel()
			delete(p.entries, name)
		}
	}
}
