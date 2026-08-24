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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters"
)

// SourceRefIndex indexes each CrossService by the objects its source depends
// on, so an incoming event can be turned into the affected CrossServices with
// a single indexed lookup rather than a full scan.
const SourceRefIndex = "spec.source.refs"

// nodesIndexKey is shared by the Nodes source and the Service source's
// NodePort exposure, both of which take their addresses from Nodes.
const nodesIndexKey = "nodes|"

// Index key constructors. The prefix keeps the namespaces of the different
// dependency kinds from colliding.
func podsKey(namespace string) string   { return "pods|" + namespace }
func nodesKey() string                  { return nodesIndexKey }
func serviceKey(ns, name string) string { return "service|" + ns + "/" + name }
func slicesKey(ns, name string) string  { return "slices|" + ns + "/" + name }

// remotePrefix scopes an index key to the cluster it refers to, so a Pod event
// from one secondary cluster cannot enqueue a CrossService watching the same
// namespace in another.
func remotePrefix(clusterName string) string { return "remote|" + clusterName + "|" }

// sourceRefKeys returns every dependency key for a CrossService.
//
// Remote sources are indexed under a cluster-scoped prefix and driven by that
// cluster's own informers, which arrive with remote access; local sources are
// indexed bare and driven by the manager's cache. Both end up in the same index
// so one lookup serves either.
func sourceRefKeys(xsvc *netv1alpha1.CrossService) []string {
	src := &xsvc.Spec.Source

	var prefix string
	if src.ClusterRef != nil {
		prefix = remotePrefix(src.ClusterRef.Name)
	}
	keys := localSourceRefKeys(src)
	for i := range keys {
		keys[i] = prefix + keys[i]
	}
	return keys
}

// localSourceRefKeys returns the unprefixed dependency keys for a source.
func localSourceRefKeys(src *netv1alpha1.Source) []string {
	switch src.Type {
	case netv1alpha1.SourceTypePods:
		if src.Pods != nil {
			return []string{podsKey(src.Pods.Namespace)}
		}
	case netv1alpha1.SourceTypeNodes:
		return []string{nodesKey()}
	case netv1alpha1.SourceTypeService:
		if src.Service == nil {
			return nil
		}
		keys := []string{serviceKey(src.Service.Namespace, src.Service.Name)}
		switch src.Service.Via {
		case netv1alpha1.ServiceExposureNodePort:
			// The addresses come from Nodes, so node churn matters as much as
			// the Service itself.
			keys = append(keys, nodesKey())
		case netv1alpha1.ServiceExposurePodIP:
			keys = append(keys, slicesKey(src.Service.Namespace, src.Service.Name))
		}
		return keys
	}
	return nil
}

// SetupIndexes registers the reverse index. It must run before the controller
// starts.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(ctx, &netv1alpha1.CrossService{}, SourceRefIndex,
		func(o client.Object) []string {
			xsvc, ok := o.(*netv1alpha1.CrossService)
			if !ok {
				return nil
			}
			return sourceRefKeys(xsvc)
		})
}

// enqueueForKey returns every CrossService registered against an index key.
func (r *CrossServiceReconciler) enqueueForKey(ctx context.Context, key string) []reconcile.Request {
	var list netv1alpha1.CrossServiceList
	if err := r.List(ctx, &list, client.MatchingFields{SourceRefIndex: key}); err != nil {
		return nil
	}

	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			},
		})
	}
	return out
}

func (r *CrossServiceReconciler) mapPod(ctx context.Context, o client.Object) []reconcile.Request {
	return r.enqueueForKey(ctx, podsKey(o.GetNamespace()))
}

func (r *CrossServiceReconciler) mapNode(ctx context.Context, _ client.Object) []reconcile.Request {
	return r.enqueueForKey(ctx, nodesKey())
}

func (r *CrossServiceReconciler) mapService(ctx context.Context, o client.Object) []reconcile.Request {
	return r.enqueueForKey(ctx, serviceKey(o.GetNamespace(), o.GetName()))
}

// mapEndpointSlice routes a slice event to the CrossServices consuming the
// Service it belongs to.
func (r *CrossServiceReconciler) mapEndpointSlice(ctx context.Context, o client.Object) []reconcile.Request {
	labels := o.GetLabels()

	// Slices this controller wrote are already covered by Owns(). Feeding them
	// back in here would let a CrossService that consumes its own output spin
	// forever.
	if labels[netv1alpha1.ManagedByLabel] == netv1alpha1.ManagedByLabelValue {
		return nil
	}

	svcName := labels[netv1alpha1.ServiceNameLabel]
	if svcName == "" {
		return nil
	}
	return r.enqueueForKey(ctx, slicesKey(o.GetNamespace(), svcName))
}

// watchLocalSources wires the event sources that make endpoint updates
// immediate rather than periodic (I14). Pod IPs churn, and a 30-second timer
// would mean 30 seconds of black-holed traffic per Pod restart.
func (r *CrossServiceReconciler) watchLocalSources(b *builder.Builder) *builder.Builder {
	return b.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPod)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNode)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.mapService)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.mapEndpointSlice))
}

// RemoteEvent carries one change from a secondary cluster's informer.
//
// It exists because a remote object cannot say which apiserver produced it, and
// the cluster name is exactly what turns the event into an index lookup. The
// object itself is never needed past its namespace and name, so only those are
// carried -- keeping a reference to a live informer's object across a channel
// would risk mutating cache-owned memory.
type RemoteEvent struct {
	metav1.ObjectMeta

	// Cluster is the RemoteCluster's name.
	Cluster string
	// Key is the unprefixed index key for the object that changed.
	Key string
}

// GetObjectKind implements runtime.Object. A RemoteEvent never round-trips
// through the apiserver, so an empty kind is correct rather than merely
// tolerated.
func (e *RemoteEvent) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

// DeepCopyObject implements runtime.Object.
func (e *RemoteEvent) DeepCopyObject() runtime.Object {
	out := *e
	e.DeepCopyInto(&out.ObjectMeta)
	return &out
}

// mapRemote turns a remote cluster's event into the CrossServices that depend
// on it.
func (r *CrossServiceReconciler) mapRemote(ctx context.Context, o client.Object) []reconcile.Request {
	ev, ok := o.(*RemoteEvent)
	if !ok {
		return nil
	}
	return r.enqueueForKey(ctx, remotePrefix(ev.Cluster)+ev.Key)
}

// RemoteWatcher returns the callback that attaches event handlers to a newly
// started secondary cluster's informers.
//
// Without this, a secondary cluster's informers run and keep their cache warm
// but nothing ever enqueues a reconcile, so a remote Pod restart changes the
// remote cache and is never written through to the local EndpointSlice. The
// endpoints then point at addresses that no longer exist, indefinitely --
// there is no timer to fall back on for remote sources.
//
// Events are funnelled into one channel rather than each cluster getting its
// own watch registration, because clusters come and go as credentials rotate
// and the controller's watches are fixed at startup.
func RemoteWatcher(ch chan<- event.TypedGenericEvent[client.Object]) clusters.OnStart {
	return func(name string, c cluster.Cluster) error {
		reg := []struct {
			obj client.Object
			key func(o client.Object) string
		}{
			{&corev1.Pod{}, func(o client.Object) string { return podsKey(o.GetNamespace()) }},
			{&corev1.Node{}, func(client.Object) string { return nodesKey() }},
			{&corev1.Service{}, func(o client.Object) string { return serviceKey(o.GetNamespace(), o.GetName()) }},
			{&discoveryv1.EndpointSlice{}, func(o client.Object) string {
				svc := o.GetLabels()[netv1alpha1.ServiceNameLabel]
				if svc == "" {
					return ""
				}
				return slicesKey(o.GetNamespace(), svc)
			}},
		}

		for _, entry := range reg {
			inf, err := c.GetCache().GetInformer(context.Background(), entry.obj)
			if err != nil {
				return fmt.Errorf("informer for %T on cluster %s: %w", entry.obj, name, err)
			}
			if _, err := inf.AddEventHandler(remoteHandler(name, entry.key, ch)); err != nil {
				return fmt.Errorf("watching %T on cluster %s: %w", entry.obj, name, err)
			}
		}
		return nil
	}
}

// remoteHandler forwards add/update/delete to the channel. Update is not
// filtered on resourceVersion: the reconciler diffs before writing, so a
// redundant reconcile is cheap, whereas a missed one leaves dead addresses in
// a live EndpointSlice.
func remoteHandler(
	clusterName string,
	key func(client.Object) string,
	ch chan<- event.TypedGenericEvent[client.Object],
) toolscache.ResourceEventHandler {
	send := func(obj any) {
		o, ok := obj.(client.Object)
		if !ok {
			// A tombstone from a missed delete. The next resync corrects the
			// cache, and dropping it here is better than guessing a key.
			return
		}
		k := key(o)
		if k == "" {
			return
		}
		ev := &RemoteEvent{
			ObjectMeta: metav1.ObjectMeta{Namespace: o.GetNamespace(), Name: o.GetName()},
			Cluster:    clusterName,
			Key:        k,
		}
		select {
		case ch <- event.TypedGenericEvent[client.Object]{Object: ev}:
		default:
			// Dropping beats blocking an informer's shared delivery goroutine,
			// which would stall every watcher on this cluster. The periodic
			// resync is the backstop.
		}
	}

	return toolscache.ResourceEventHandlerFuncs{
		AddFunc:    send,
		UpdateFunc: func(_, newObj any) { send(newObj) },
		DeleteFunc: send,
	}
}
