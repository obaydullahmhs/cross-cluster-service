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

package v1alpha1

// ProjectGroup is the API group for every type in this project.
//
// Renaming the project's API group is a three-step change: this constant, the
// +groupName marker in groupversion_info.go, and the group in PROJECT. Nothing
// else should hard-code the group string -- derive from here instead.
const ProjectGroup = "net.obaydullah.dev"

const (
	// ManagedByLabel is written on every EndpointSlice this controller owns.
	ManagedByLabel = "endpointslice.kubernetes.io/managed-by"

	// ManagedByLabelValue claims ownership of a slice (invariant I8). Without
	// it, the built-in EndpointSlice mirroring controller will fight us for
	// ownership of the slices we write.
	ManagedByLabelValue = "crossservice." + ProjectGroup

	// ServiceNameLabel is the required kubernetes.io/service-name label (I8).
	// kube-proxy uses it to associate a slice with its Service.
	ServiceNameLabel = "kubernetes.io/service-name"
)

const (
	// RemoteClusterFinalizer guards a cluster-scoped RemoteCluster while
	// namespaced objects still depend on it. A cluster-scoped object cannot own
	// namespaced objects (I13), so cleanup runs off this finalizer plus the
	// label index below.
	RemoteClusterFinalizer = ProjectGroup + "/remotecluster"

	// CrossServiceFinalizer releases the CrossService's reference on the shared,
	// ref-counted remote client cache before the object goes away.
	CrossServiceFinalizer = ProjectGroup + "/crossservice"
)

const (
	// RemoteClusterLabel indexes generated objects by the RemoteCluster they
	// resolve against, so a RemoteCluster deletion can find them (I13).
	RemoteClusterLabel = ProjectGroup + "/remote-cluster"

	// CrossServiceNameLabel and CrossServiceNamespaceLabel identify the
	// CrossService that generated an object.
	CrossServiceNameLabel      = ProjectGroup + "/crossservice-name"
	CrossServiceNamespaceLabel = ProjectGroup + "/crossservice-namespace"
)

// ZoneLabel is the well-known topology label read from Nodes and propagated
// onto endpoints. Cross-cluster endpoints carry zone instead of nodeName (I5).
const ZoneLabel = "topology.kubernetes.io/zone"
