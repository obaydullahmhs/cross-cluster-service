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

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// SourceType selects what the endpoints describe.
//
// The distinction that matters: Service targets a remote Service and lets the
// controller derive addresses AND ports from it, because NodePorts and
// LoadBalancer addresses are allocated by the remote cluster and cannot be
// hardcoded without hand-syncing them forever. Pods and Nodes are lower-level
// escape hatches for backends that have no Service in front of them.
type SourceType string

const (
	// SourceTypeService targets a remote Service. This is the path to use for
	// anything fronted by a Service; see ServiceExposure for how it is reached.
	SourceTypeService SourceType = "Service"
	// SourceTypePods resolves Pod IPs directly, for Pods with no Service in
	// front of them. If a Service exists, prefer SourceTypeService with
	// ServiceExposurePodIP, which reuses the readiness and port resolution the
	// remote cluster has already done.
	SourceTypePods SourceType = "Pods"
	// SourceTypeNodes resolves Node addresses for a port on the node itself: a
	// DaemonSet bound to a hostPort, a node agent, a non-Kubernetes process.
	// For a NodePort Service use SourceTypeService with ServiceExposureNodePort
	// instead, which tracks the allocated nodePort automatically.
	SourceTypeNodes SourceType = "Nodes"
	// SourceTypeDNS resolves A, AAAA, or SRV records. It is the only source
	// that must poll.
	SourceTypeDNS SourceType = "DNS"
	// SourceTypeStatic uses literal addresses.
	SourceTypeStatic SourceType = "Static"
)

// ServiceExposure selects how a remote Service is reached.
type ServiceExposure string

const (
	// ServiceExposureNodePort pairs node addresses with the Service's allocated
	// nodePort, which the controller reads rather than the user hardcoding.
	ServiceExposureNodePort ServiceExposure = "NodePort"
	// ServiceExposureLoadBalancer uses status.loadBalancer.ingress.
	ServiceExposureLoadBalancer ServiceExposure = "LoadBalancer"
	// ServiceExposurePodIP reads the remote Service's EndpointSlices. Requires
	// a flat or peered network between the clusters.
	ServiceExposurePodIP ServiceExposure = "PodIP"
)

// DNSRecordType is the record class queried by a DNS source.
type DNSRecordType string

const (
	DNSRecordTypeA    DNSRecordType = "A"
	DNSRecordTypeAAAA DNSRecordType = "AAAA"
	DNSRecordTypeSRV  DNSRecordType = "SRV"
)

// NodeAddressType selects which Node address becomes the endpoint.
type NodeAddressType string

const (
	NodeAddressTypeInternalIP     NodeAddressType = "InternalIP"
	NodeAddressTypeExternalIP     NodeAddressType = "ExternalIP"
	NodeAddressTypePreferExternal NodeAddressType = "PreferExternal"
	NodeAddressTypePreferInternal NodeAddressType = "PreferInternal"
)

// IPFamilyPolicy selects which address families produce EndpointSlices.
// Each family needs its own slice: addressType is immutable and single-valued
// (I3).
type IPFamilyPolicy string

const (
	IPFamilyPolicyIPv4            IPFamilyPolicy = "IPv4"
	IPFamilyPolicyIPv6            IPFamilyPolicy = "IPv6"
	IPFamilyPolicyPreferDualStack IPFamilyPolicy = "PreferDualStack"
)

// StaleAction is what happens to endpoints once a failing source exceeds its
// stale threshold.
type StaleAction string

const (
	// StaleActionRemove drops the stale endpoints from the slice.
	StaleActionRemove StaleAction = "Remove"
	// StaleActionMarkNotReady keeps them but flips ready to false, so a
	// headless consumer can still see them and kube-proxy stops routing.
	StaleActionMarkNotReady StaleAction = "MarkNotReady"
)

// DNSResolution controls re-resolution timing for anything that resolves names
// to addresses. It is shared by the DNS source and by LoadBalancer exposures
// whose provider hands back a hostname rather than an IP.
type DNSResolution struct {
	// Interval between re-resolutions.
	// +kubebuilder:default="30s"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// UseTTL requeues on the record's TTL instead of Interval. Requires a
	// TTL-aware resolver; falls back to Interval otherwise, and says so in the
	// source status.
	// +kubebuilder:default=false
	// +optional
	UseTTL bool `json:"useTTL,omitempty"`

	// MinTTL clamps a short TTL, so a 1-second record cannot drive a hot loop.
	// +kubebuilder:default="5s"
	// +optional
	MinTTL *metav1.Duration `json:"minTTL,omitempty"`

	// MaxTTL clamps a long TTL, so a 24-hour record does not go unrefreshed.
	// +kubebuilder:default="5m"
	// +optional
	MaxTTL *metav1.Duration `json:"maxTTL,omitempty"`

	// Nameservers to query as host:port, instead of the pod's resolv.conf.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=253
	Nameservers []string `json:"nameservers,omitempty"`
}

// CrossServicePort is one port on the generated Service.
//
// Name is the join key between the Service and its EndpointSlices (I2): for a
// selector-less Service, kube-proxy matches Service.spec.ports[].name against
// EndpointSlice.ports[].name, and Service.spec.ports[].targetPort is ignored
// entirely. For a Service source it is also the join key against the REMOTE
// Service's port names.
// +kubebuilder:validation:XValidation:rule="!has(self.name) || (self.name.matches('[a-z]') && !self.name.contains('--'))",message="port name must be a valid IANA_SVC_NAME: at least one letter, no adjacent hyphens"
// +kubebuilder:validation:XValidation:rule="!(has(self.targetPort) && has(self.remotePort))",message="targetPort and remotePort are mutually exclusive: targetPort is for Pods, Nodes, DNS and Static sources, remotePort for Service sources"
type CrossServicePort struct {
	// Name of the port. Required when more than one port is defined. Must be a
	// valid IANA_SVC_NAME: at most 15 characters, lowercase alphanumeric and
	// hyphens, at least one letter, no leading, trailing, or adjacent hyphens.
	// +optional
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name,omitempty"`

	// Port is the port exposed on the Service's ClusterIP.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Protocol of the port.
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`

	// AppProtocol is a hint copied to both the Service and the EndpointSlice.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	AppProtocol *string `json:"appProtocol,omitempty"`

	// TargetPort is the port on the backend, for Pods, Nodes, DNS and Static
	// sources. It feeds EndpointSlice.ports[].port -- NOT
	// Service.spec.ports[].targetPort, which the apiserver ignores for a
	// selector-less Service (I2). Defaults to Port.
	//
	// The string form names a container port and is only valid for a Pods
	// source. Be aware that a named port resolving to different numbers across
	// Pods fragments the endpoints into separate slices, because ports are
	// per-slice, not per-endpoint (I4).
	//
	// Rejected for Service sources, where the backend port is derived from the
	// remote Service rather than declared here.
	// +optional
	TargetPort *intstr.IntOrString `json:"targetPort,omitempty"`

	// RemotePort selects which port of the remote Service this maps to, for
	// Service sources only. Name or number.
	//
	// Defaults to matching Name against the remote Service's port names, which
	// keeps the join key consistent with I2. Set this only when the local and
	// remote names differ. A single unnamed port on both sides matches
	// implicitly.
	// +optional
	RemotePort *intstr.IntOrString `json:"remotePort,omitempty"`
}

// Source is a tagged union over the resolution backends.
//
// The CEL rules make the payload required for its own type and rejected for
// every other. ClusterRef is rejected outright for DNS and Static rather than
// silently ignored: a clusterRef that quietly does nothing is exactly the class
// of silent breakage this API is trying to avoid.
// +kubebuilder:validation:XValidation:rule="self.type == 'Service' ? has(self.service) : !has(self.service)",message="service config is required if and only if type is Service"
// +kubebuilder:validation:XValidation:rule="self.type == 'DNS' ? has(self.dns) : !has(self.dns)",message="dns config is required if and only if type is DNS"
// +kubebuilder:validation:XValidation:rule="self.type == 'Pods' ? has(self.pods) : !has(self.pods)",message="pods config is required if and only if type is Pods"
// +kubebuilder:validation:XValidation:rule="self.type == 'Nodes' ? has(self.nodes) : !has(self.nodes)",message="nodes config is required if and only if type is Nodes"
// +kubebuilder:validation:XValidation:rule="self.type == 'Static' ? has(self.static) : !has(self.static)",message="static config is required if and only if type is Static"
// +kubebuilder:validation:XValidation:rule="(self.type == 'Service' || self.type == 'Pods' || self.type == 'Nodes') || !has(self.clusterRef)",message="clusterRef is only valid for Service, Pods and Nodes sources"
type Source struct {
	// Type selects the backend and which payload field below is read.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Service;Pods;Nodes;DNS;Static
	Type SourceType `json:"type"`

	// ClusterRef selects the RemoteCluster to resolve against. Omit for the
	// local cluster. Only valid for Service, Pods and Nodes.
	// +optional
	ClusterRef *ClusterRef `json:"clusterRef,omitempty"`

	// +optional
	Service *ServiceSource `json:"service,omitempty"`
	// +optional
	Pods *PodSource `json:"pods,omitempty"`
	// +optional
	Nodes *NodeSource `json:"nodes,omitempty"`
	// +optional
	DNS *DNSSource `json:"dns,omitempty"`
	// +optional
	Static *StaticSource `json:"static,omitempty"`
}

// ServiceSource targets a Service in the local or a remote cluster, and derives
// both addresses and ports from it.
//
// This exists because the interesting numbers are allocated by the cluster that
// owns the Service: a nodePort comes from the 30000-32767 range and changes if
// the Service is recreated, and a LoadBalancer address is assigned by the cloud
// provider. Declaring either by hand means hand-syncing it forever.
// +kubebuilder:validation:XValidation:rule="self.via == 'NodePort' || !has(self.nodePort)",message="nodePort config is only valid when via is NodePort"
// +kubebuilder:validation:XValidation:rule="self.via == 'LoadBalancer' || !has(self.loadBalancer)",message="loadBalancer config is only valid when via is LoadBalancer"
// +kubebuilder:validation:XValidation:rule="self.via == 'PodIP' || !has(self.podIP)",message="podIP config is only valid when via is PodIP"
type ServiceSource struct {
	// Namespace of the Service in the source cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace"`

	// Name of the Service in the source cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Via selects how the Service is reached. This determines both the
	// addresses and the backend ports.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=NodePort;LoadBalancer;PodIP
	Via ServiceExposure `json:"via"`

	// +optional
	NodePort *NodePortExposure `json:"nodePort,omitempty"`
	// +optional
	LoadBalancer *LoadBalancerExposure `json:"loadBalancer,omitempty"`
	// +optional
	PodIP *PodIPExposure `json:"podIP,omitempty"`
}

// NodePortExposure selects which nodes provide the addresses that are paired
// with the remote Service's allocated nodePort. Omit it entirely to use every
// eligible node.
// +kubebuilder:validation:XValidation:rule="!(has(self.selector) && has(self.names))",message="selector and names are mutually exclusive; omit both to use every eligible node"
type NodePortExposure struct {
	// Selector matches nodes by label.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// Names lists nodes verbatim.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=253
	Names []string `json:"names,omitempty"`

	// AddressType selects which node address to use. The Prefer variants fall
	// back to the other kind when the preferred one is absent.
	// +kubebuilder:validation:Enum=InternalIP;ExternalIP;PreferExternal;PreferInternal
	// +kubebuilder:default=InternalIP
	// +optional
	AddressType NodeAddressType `json:"addressType,omitempty"`

	// RequireReady emits nodes whose Ready condition is not True as not-ready
	// endpoints rather than dropping them silently.
	// +kubebuilder:default=true
	// +optional
	RequireReady *bool `json:"requireReady,omitempty"`

	// ExcludeUnschedulable drops nodes that are cordoned or labelled
	// node.kubernetes.io/exclude-from-external-load-balancers.
	// +kubebuilder:default=true
	// +optional
	ExcludeUnschedulable *bool `json:"excludeUnschedulable,omitempty"`

	// PropagateZone copies topology.kubernetes.io/zone from the node (I5).
	// +kubebuilder:default=true
	// +optional
	PropagateZone *bool `json:"propagateZone,omitempty"`
}

// LoadBalancerExposure configures use of status.loadBalancer.ingress.
type LoadBalancerExposure struct {
	// HostnameResolution controls re-resolution when the provider returns a
	// hostname rather than an IP -- AWS ELB and NLB do, GCP does not.
	//
	// kube-proxy ignores addressType FQDN (I11), so a hostname ingress cannot
	// be written through as-is; it has to be resolved to addresses and kept
	// fresh on an interval. That makes a hostname LoadBalancer a polling source
	// despite being watch-driven for the Service object itself.
	// +optional
	HostnameResolution *DNSResolution `json:"hostnameResolution,omitempty"`
}

// PodIPExposure configures reading the remote Service's EndpointSlices.
//
// Reading slices rather than re-deriving from Pods is deliberate: the remote
// cluster has already computed readiness, resolved every named port per Pod,
// and recorded zone. Re-deriving all of that from a duplicated selector would
// reproduce I4's named-port fragmentation for no benefit.
type PodIPExposure struct {
	// IncludeTerminating keeps endpoints that are serving but terminating, for
	// graceful draining.
	// +kubebuilder:default=false
	// +optional
	IncludeTerminating bool `json:"includeTerminating,omitempty"`

	// PropagateZone copies the zone recorded on the remote endpoint (I5).
	// +kubebuilder:default=true
	// +optional
	PropagateZone *bool `json:"propagateZone,omitempty"`
}

// PodSource resolves Pod IPs directly, for Pods with no Service in front.
// +kubebuilder:validation:XValidation:rule="has(self.selector) != has(self.names)",message="exactly one of selector or names must be set"
type PodSource struct {
	// Namespace in the source cluster. Required: an empty namespace would mean
	// every namespace, which is not a grant this API should make implicitly.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace"`

	// Selector matches Pods by label. Exactly one of selector or names -- there
	// is deliberately no "all Pods in the namespace" form.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// Names lists Pods verbatim. Exactly one of selector or names.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=253
	Names []string `json:"names,omitempty"`

	// IncludeTerminating keeps Pods with a deletionTimestamp as
	// serving-but-terminating endpoints, for graceful draining.
	// +kubebuilder:default=false
	// +optional
	IncludeTerminating bool `json:"includeTerminating,omitempty"`

	// PropagateZone copies topology.kubernetes.io/zone from the Pod's Node onto
	// the endpoint. Cross-cluster endpoints carry zone rather than nodeName,
	// which names a node in the local cluster and would break
	// internalTrafficPolicy: Local (I5). Requires nodes get/list in the source
	// cluster.
	// +kubebuilder:default=true
	// +optional
	PropagateZone *bool `json:"propagateZone,omitempty"`
}

// NodeSource resolves Node addresses for a port on the node itself: a DaemonSet
// bound to a hostPort, a node agent, a process outside Kubernetes.
//
// For a NodePort Service, use a Service source with via: NodePort instead. This
// type is for the case where there is no Service to read, so the port comes
// from spec.ports[].targetPort.
// +kubebuilder:validation:XValidation:rule="!(has(self.selector) && has(self.names))",message="selector and names are mutually exclusive; omit both to use every eligible node"
type NodeSource struct {
	// Selector matches Nodes by label.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// Names lists Nodes verbatim.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=253
	Names []string `json:"names,omitempty"`

	// AddressType selects which Node address to use.
	// +kubebuilder:validation:Enum=InternalIP;ExternalIP;PreferExternal;PreferInternal
	// +kubebuilder:default=InternalIP
	// +optional
	AddressType NodeAddressType `json:"addressType,omitempty"`

	// RequireReady emits Nodes whose Ready condition is not True as not-ready
	// endpoints rather than dropping them silently.
	// +kubebuilder:default=true
	// +optional
	RequireReady *bool `json:"requireReady,omitempty"`

	// ExcludeUnschedulable drops Nodes that are cordoned or labelled
	// node.kubernetes.io/exclude-from-external-load-balancers.
	// +kubebuilder:default=true
	// +optional
	ExcludeUnschedulable *bool `json:"excludeUnschedulable,omitempty"`

	// PropagateZone copies topology.kubernetes.io/zone from the Node (I5).
	// +kubebuilder:default=true
	// +optional
	PropagateZone *bool `json:"propagateZone,omitempty"`
}

// DNSSource resolves names on an interval or on the record's TTL. This is the
// only source that polls by design.
// +kubebuilder:validation:XValidation:rule="!(has(self.excludePrivateIPs) && self.excludePrivateIPs && has(self.excludePublicIPs) && self.excludePublicIPs)",message="excludePrivateIPs and excludePublicIPs cannot both be true; that would exclude every address"
type DNSSource struct {
	// Names to resolve. A trailing dot is appended if absent (I10): without it,
	// ndots:5 in a Pod's resolv.conf costs four NXDOMAIN round-trips per name
	// per interval, forever.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=253
	Names []string `json:"names"`

	// RecordType queried. Note that the resolved addresses are always written as
	// IPv4 or IPv6 EndpointSlices; addressType FQDN is never used, because
	// kube-proxy ignores it (I11).
	// +kubebuilder:validation:Enum=A;AAAA;SRV
	// +kubebuilder:default=A
	// +optional
	RecordType DNSRecordType `json:"recordType,omitempty"`

	// SRVPortName names the port in spec.ports that an SRV record's port
	// populates. Defaults to the single port when only one is defined. Ignored
	// unless recordType is SRV.
	// +optional
	// +kubebuilder:validation:MaxLength=15
	SRVPortName string `json:"srvPortName,omitempty"`

	// ExcludePrivateIPs drops privately-routable answers, keeping only publicly
	// routable ones.
	//
	// This exists for split-horizon DNS, where one name answers with both an
	// internal and an external address depending on who asks -- and where a
	// resolver that can see both would otherwise write a mix of the two into a
	// single EndpointSlice, making roughly half of all connections take a path
	// the caller cannot reach. Which half is unreachable depends on where the
	// consuming cluster sits, so the choice has to be declared, not guessed.
	//
	// See AddressScope for exactly which ranges count as private; notably
	// 100.64.0.0/10 does, because CNIs allocate from it and it is not routable
	// on the internet.
	// +kubebuilder:default=false
	// +optional
	ExcludePrivateIPs bool `json:"excludePrivateIPs,omitempty"`

	// ExcludePublicIPs drops publicly-routable answers, keeping only privately
	// routable ones. The counterpart to ExcludePrivateIPs, and the one to reach
	// for when peered/VPN connectivity exists and traffic must not leave it.
	//
	// Setting both is rejected: it would exclude every address.
	// +kubebuilder:default=false
	// +optional
	ExcludePublicIPs bool `json:"excludePublicIPs,omitempty"`

	// DNSResolution is inlined so the timing knobs read as dns.interval rather
	// than dns.resolution.interval, while staying the same type a hostname
	// LoadBalancer uses.
	DNSResolution `json:",inline"`
}

// StaticSource is a literal list of addresses. Static endpoints are assumed
// ready: this controller does no health checking.
type StaticSource struct {
	// Addresses are literal IPv4 or IPv6 addresses. Validated controller-side
	// with netip.ParseAddr rather than by CEL, because the isIP() CEL extension
	// is not available on the minimum supported apiserver (1.27).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=45
	Addresses []string `json:"addresses"`

	// Zone applied to every address, for topology-aware routing.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Zone string `json:"zone,omitempty"`
}

// ServiceTemplate customises the generated Service. spec.selector is always nil
// regardless of what is set here (I1): a non-nil selector hands ownership to
// the built-in EndpointSlice controller, which then deletes our slices in a
// loop.
type ServiceTemplate struct {
	// Name of the generated Service. Defaults to the CrossService name.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`

	// Labels merged onto the generated Service.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations merged onto the generated Service.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// ClusterIP: set to None for a headless Service, which is also the only
	// configuration in which endpoint hostnames are meaningful.
	// +kubebuilder:validation:Enum="";None
	// +optional
	ClusterIP string `json:"clusterIP,omitempty"`

	// InternalTrafficPolicy on the generated Service. Local is near-useless with
	// cross-cluster endpoints, since those endpoints carry no local nodeName by
	// design (I5).
	// +kubebuilder:validation:Enum=Cluster;Local
	// +optional
	InternalTrafficPolicy *corev1.ServiceInternalTrafficPolicy `json:"internalTrafficPolicy,omitempty"`

	// SessionAffinity on the generated Service.
	// +kubebuilder:validation:Enum=None;ClientIP
	// +optional
	SessionAffinity *corev1.ServiceAffinity `json:"sessionAffinity,omitempty"`

	// PublishNotReadyAddresses routes to not-ready endpoints too. Mostly useful
	// with a headless Service.
	// +kubebuilder:default=false
	// +optional
	PublishNotReadyAddresses bool `json:"publishNotReadyAddresses,omitempty"`

	// TopologyHints emits hints on the generated EndpointSlices. Off by default:
	// hints derived from a foreign cluster's topology are misleading at best
	// (I5).
	// +kubebuilder:default=false
	// +optional
	TopologyHints bool `json:"topologyHints,omitempty"`
}

// FailurePolicy governs what happens while the source is failing to resolve.
//
// The defaults exist so that a two-second CoreDNS blip cannot black-hole
// production traffic (I9): last-known-good endpoints keep serving until the
// stale threshold, and even then the default is to mark them not-ready rather
// than to empty the slice.
type FailurePolicy struct {
	// FailureThreshold is how many consecutive resolution failures mark the
	// source as failing.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// StaleThreshold is how long last-known-good endpoints keep serving after
	// the source starts failing, before OnStale applies.
	// +kubebuilder:default="5m"
	// +optional
	// Pointer so that an unset field is omitted on the wire and the CRD default
	// applies. metav1.Duration is a struct, and encoding/json ignores omitempty
	// on structs, so a non-pointer field is always sent -- as "0s" -- and the
	// apiserver default never fires for a Go client.
	StaleThreshold *metav1.Duration `json:"staleThreshold,omitempty"`

	// OnStale is what to do once StaleThreshold is exceeded.
	// +kubebuilder:validation:Enum=Remove;MarkNotReady
	// +kubebuilder:default=MarkNotReady
	// +optional
	OnStale StaleAction `json:"onStale,omitempty"`
}

// CrossServiceSpec declares the ports to expose and where the backends come
// from. It contains no credentials: those live on the RemoteCluster, which is
// an ops-team object.
//
// The port-name rules are enforced in CEL rather than controller-side because
// CRD validation is both cheaper and visible at kubectl apply time.
// +kubebuilder:validation:XValidation:rule="size(self.ports) == 1 || self.ports.all(p, has(p.name))",message="port name is required when more than one port is defined"
// +kubebuilder:validation:XValidation:rule="size(self.ports) == 1 || self.ports.all(p, self.ports.exists_one(q, q.name == p.name))",message="port names must be unique"
// +kubebuilder:validation:XValidation:rule="self.source.type == 'Pods' || self.ports.all(p, !has(p.targetPort) || type(p.targetPort) == int)",message="a string targetPort names a container port and is only valid for a Pods source"
// +kubebuilder:validation:XValidation:rule="self.source.type == 'Service' || self.ports.all(p, !has(p.remotePort))",message="remotePort is only valid for a Service source"
// +kubebuilder:validation:XValidation:rule="self.source.type != 'Service' || self.ports.all(p, !has(p.targetPort))",message="targetPort is not valid for a Service source: the backend port is derived from the remote Service, so use remotePort to select which one"
type CrossServiceSpec struct {
	// Ports exposed on the generated Service.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Ports []CrossServicePort `json:"ports"`

	// Source is where the backends are resolved from.
	// +kubebuilder:validation:Required
	Source Source `json:"source"`

	// Service customises the generated Service.
	// +optional
	Service *ServiceTemplate `json:"service,omitempty"`

	// IPFamilyPolicy selects which address families are written. IPv4 and IPv6
	// always produce separate EndpointSlices (I3).
	// +kubebuilder:validation:Enum=IPv4;IPv6;PreferDualStack
	// +kubebuilder:default=IPv4
	// +optional
	IPFamilyPolicy IPFamilyPolicy `json:"ipFamilyPolicy,omitempty"`

	// FailurePolicy governs behaviour while the source is failing. Defaults
	// apply when unset.
	// +optional
	FailurePolicy *FailurePolicy `json:"failurePolicy,omitempty"`
}

// PersistedEndpoint is one last-known-good endpoint, carried in status so that
// a controller restart during a source outage does not black-hole traffic (I9).
type PersistedEndpoint struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=45
	Address string `json:"address"`

	// +optional
	Ready bool `json:"ready,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=253
	Zone string `json:"zone,omitempty"`

	// Hostname is only meaningful for a headless Service.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Hostname string `json:"hostname,omitempty"`

	// Ports maps port name to the resolved backend port. The empty string keys
	// the single unnamed port (I2).
	// +optional
	Ports map[string]int32 `json:"ports,omitempty"`
}

// SourceStatus is the per-source detail that makes kubectl describe tell the
// whole story without log-diving.
type SourceStatus struct {
	// Type of the source, echoed for readability.
	// +optional
	Type SourceType `json:"type,omitempty"`

	// Via is the exposure used, echoed for Service sources.
	// +optional
	Via ServiceExposure `json:"via,omitempty"`

	// RemoteService is the Service that was read, as namespace/name, for
	// Service sources.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	RemoteService string `json:"remoteService,omitempty"`

	// ClusterRef is the RemoteCluster resolved against, if any.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ClusterRef string `json:"clusterRef,omitempty"`

	// LastSuccessTime is the last time resolution succeeded.
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`

	// LastError is the most recent resolution error, scrubbed of credentials.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	LastError string `json:"lastError,omitempty"`

	// ConsecutiveErrors since the last success. Compared against
	// FailurePolicy.FailureThreshold.
	// +optional
	ConsecutiveErrors int32 `json:"consecutiveErrors,omitempty"`

	// Stale is true once the endpoints being served are last-known-good rather
	// than freshly resolved.
	// +optional
	Stale bool `json:"stale,omitempty"`

	// Endpoints is the count resolved on the last success.
	// +optional
	Endpoints int32 `json:"endpoints,omitempty"`

	// LastKnownGood carries the endpoints served during a failure window. The
	// cap bounds status object growth; beyond it, FailurePolicy degrades to
	// whatever is already written in the EndpointSlices.
	// +optional
	// +kubebuilder:validation:MaxItems=512
	LastKnownGood []PersistedEndpoint `json:"lastKnownGood,omitempty"`
}

// CrossServiceStatus reports what was generated and how healthy it is.
type CrossServiceStatus struct {
	// Conditions are Ready, SourcesResolved, ServiceReady, EndpointsWritten, and
	// Degraded.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ServiceName is the generated Service.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ServiceName string `json:"serviceName,omitempty"`

	// ClusterIP allocated to the generated Service, or None if headless.
	// +optional
	// +kubebuilder:validation:MaxLength=45
	ClusterIP string `json:"clusterIP,omitempty"`

	// ReadyEndpoints currently written across all slices.
	// +optional
	ReadyEndpoints int32 `json:"readyEndpoints,omitempty"`

	// TotalEndpoints currently written across all slices, ready or not.
	// +optional
	TotalEndpoints int32 `json:"totalEndpoints,omitempty"`

	// DroppedAddresses is how many resolved addresses the AddressPolicy
	// rejected on the last reconcile. Non-zero deserves a look at the Events.
	// +optional
	DroppedAddresses int32 `json:"droppedAddresses,omitempty"`

	// EndpointSlices names the slices this CrossService manages.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MaxLength=253
	EndpointSlices []string `json:"endpointSlices,omitempty"`

	// Source is the detail for this CrossService's single source.
	// +optional
	Source *SourceStatus `json:"source,omitempty"`
}

// Condition types set on a CrossService.
const (
	ConditionReady            = "Ready"
	ConditionSourcesResolved  = "SourcesResolved"
	ConditionServiceReady     = "ServiceReady"
	ConditionEndpointsWritten = "EndpointsWritten"
	ConditionDegraded         = "Degraded"
)

// Condition types set on a RemoteCluster. ConditionReady is shared.
const (
	ConditionAuthenticated = "Authenticated"
	ConditionReachable     = "Reachable"
)

// Condition reasons. These are deliberately specific: a reason should tell the
// reader what to go fix, not merely that something is wrong.
const (
	ReasonInvalidSpec              = "InvalidSpec"
	ReasonClusterNotFound          = "ClusterNotFound"
	ReasonClusterAccessDenied      = "ClusterAccessDenied"
	ReasonAuthenticationFailed     = "AuthenticationFailed"
	ReasonNetworkUnreachable       = "NetworkUnreachable"
	ReasonDNSResolutionFailed      = "DNSResolutionFailed"
	ReasonNoEndpointsFound         = "NoEndpointsFound"
	ReasonAddressPolicyRejected    = "AddressPolicyRejected"
	ReasonStaleEndpoints           = "StaleEndpoints"
	ReasonPartialFailure           = "PartialFailure"
	ReasonAccessTypeNotImplemented = "AccessTypeNotImplemented"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=xsvc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.status.serviceName`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyEndpoints`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalEndpoints`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CrossService generates a selector-less Service plus managed EndpointSlices
// whose backends come from outside the local Service selector mechanism.
type CrossService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec CrossServiceSpec `json:"spec"`

	// +optional
	Status CrossServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CrossServiceList contains a list of CrossService.
type CrossServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CrossService `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &CrossService{}, &CrossServiceList{})
		return nil
	})
}
