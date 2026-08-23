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

// Package rbac asserts the shape of the generated RBAC.
//
// The rules here are security properties rather than behaviour, so they are
// checked against the manifests that actually get applied. A stray
// +kubebuilder:rbac marker is a one-line change that silently widens what the
// controller can read, and nothing else in the test suite would notice.
package rbac

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// loadRoles splits the generated role.yaml into its documents.
func loadRoles(t *testing.T) (clusterRoles, roles []rbacv1.PolicyRule) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("reading role.yaml: %v", err)
	}

	for doc := range strings.SplitSeq(string(raw), "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var obj struct {
			Kind  string              `json:"kind"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parsing a role document: %v", err)
		}
		switch obj.Kind {
		case "ClusterRole":
			clusterRoles = append(clusterRoles, obj.Rules...)
		case "Role":
			roles = append(roles, obj.Rules...)
		}
	}
	return clusterRoles, roles
}

func grants(rules []rbacv1.PolicyRule, resource string) bool {
	for _, r := range rules {
		if slices.Contains(r.Resources, resource) {
			return true
		}
	}
	return false
}

// TestSecretsAreNeverGrantedClusterWide covers security requirement 9.1.
func TestSecretsAreNeverGrantedClusterWide(t *testing.T) {
	clusterRules, namespacedRules := loadRoles(t)

	// A ClusterRole over secrets would let the controller read every credential
	// in the cluster. Since a cluster-scoped RemoteCluster names the Secret it
	// wants, that combination is a credential-exfiltration primitive: the whole
	// reason SecretKeyRef has no namespace field.
	if grants(clusterRules, "secrets") {
		t.Error("the ClusterRole grants secrets; credentials must come from a namespaced Role only")
	}

	if !grants(namespacedRules, "secrets") {
		t.Error("the namespaced Role does not grant secrets; the controller cannot read credentials at all")
	}
}

// TestSecondaryClusterAccessStaysReadOnly covers security requirement 9.7.
func TestSecondaryClusterAccessStaysReadOnly(t *testing.T) {
	clusterRules, _ := loadRoles(t)

	// These are the resources read out of other clusters. The documented grant
	// asked of their operators is get/list/watch, so asking for more here would
	// mean the docs understate what the controller actually wants.
	readOnly := map[string]bool{"pods": true, "nodes": true, "namespaces": true}
	writeVerbs := map[string]bool{
		"create": true, "update": true, "patch": true, "delete": true, "deletecollection": true,
	}

	for _, rule := range clusterRules {
		for _, res := range rule.Resources {
			if !readOnly[res] {
				continue
			}
			for _, verb := range rule.Verbs {
				if writeVerbs[verb] || verb == "*" {
					t.Errorf("the ClusterRole grants %q on %q; access to other clusters is read-only, always", verb, res)
				}
			}
		}
	}
}
