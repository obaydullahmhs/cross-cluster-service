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
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
	"github.com/obaydullahmhs/cross-cluster-service/internal/clusters/auth"
)

// accessDeniedError reports a namespace that may not use a RemoteCluster.
type accessDeniedError struct {
	Cluster   string
	Namespace string
}

func (e *accessDeniedError) Error() string {
	return fmt.Sprintf("namespace %q is not permitted to reference RemoteCluster %q", e.Namespace, e.Cluster)
}

// checkNamespaceAllowed enforces security requirement 9.2.
//
// It fails closed: a nil or empty allowedNamespaces permits NOTHING. Remote Pod
// IPs are sensitive, and a cluster-scoped object is not a namespace owner's to
// reason about, so an ops team that has not said who may use a spoke has said
// nobody.
func (r *CrossServiceReconciler) checkNamespaceAllowed(
	ctx context.Context,
	rc *netv1alpha1.RemoteCluster,
	namespace string,
) error {
	allowed := rc.Spec.AllowedNamespaces
	if allowed == nil || (len(allowed.MatchNames) == 0 && allowed.Selector == nil) {
		return &accessDeniedError{Cluster: rc.Name, Namespace: namespace}
	}

	if slices.Contains(allowed.MatchNames, namespace) {
		return nil
	}

	if allowed.Selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(allowed.Selector)
		if err != nil {
			return fmt.Errorf("invalid allowedNamespaces selector on RemoteCluster %s: %w", rc.Name, err)
		}
		var ns corev1.Namespace
		if err := r.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
			return fmt.Errorf("reading namespace %s: %w", namespace, err)
		}
		if sel.Matches(labels.Set(ns.Labels)) {
			return nil
		}
	}

	return &accessDeniedError{Cluster: rc.Name, Namespace: namespace}
}

// resolveCluster fetches the RemoteCluster a source binds to and checks that
// this CrossService's namespace may use it. It returns a nil RemoteCluster for
// a local source.
func (r *CrossServiceReconciler) resolveCluster(
	ctx context.Context,
	xsvc *netv1alpha1.CrossService,
) (*netv1alpha1.RemoteCluster, error) {
	ref := xsvc.Spec.Source.ClusterRef
	if ref == nil {
		return nil, nil
	}

	var rc netv1alpha1.RemoteCluster
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &rc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &clusterNotFoundError{Name: ref.Name}
		}
		return nil, err
	}

	if err := r.checkNamespaceAllowed(ctx, &rc, xsvc.Namespace); err != nil {
		return nil, err
	}
	return &rc, nil
}

type clusterNotFoundError struct{ Name string }

func (e *clusterNotFoundError) Error() string {
	return fmt.Sprintf("RemoteCluster %q not found", e.Name)
}

// reasonForClusterError maps a cluster resolution failure to an actionable
// condition reason.
func reasonForClusterError(err error) string {
	var denied *accessDeniedError
	var notFound *clusterNotFoundError
	var notImpl *auth.ErrAccessTypeNotImplemented
	var notPermitted *auth.ErrNotPermitted

	switch {
	case errors.As(err, &denied):
		return netv1alpha1.ReasonClusterAccessDenied
	case errors.As(err, &notFound):
		return netv1alpha1.ReasonClusterNotFound
	case errors.As(err, &notImpl):
		return netv1alpha1.ReasonAccessTypeNotImplemented
	case errors.As(err, &notPermitted):
		return netv1alpha1.ReasonInvalidSpec
	default:
		return netv1alpha1.ReasonAuthenticationFailed
	}
}
