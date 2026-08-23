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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
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

// sourceRefKeys returns every dependency key for a CrossService.
//
// Only local sources are indexed. A source bound to a RemoteCluster is driven
// by that cluster's own informers, which arrive with remote access.
func sourceRefKeys(xsvc *netv1alpha1.CrossService) []string {
	src := &xsvc.Spec.Source
	if src.ClusterRef != nil {
		return nil
	}

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
