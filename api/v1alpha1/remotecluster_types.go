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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AccessType selects how the controller authenticates to a secondary cluster's apiserver.
type AccessType string

const (
	// AccessTypeInCluster uses the controller's own ServiceAccount against the
	// local apiserver. Phase 1.
	AccessTypeInCluster AccessType = "InCluster"
	// AccessTypeToken uses a static bearer token from a Secret. Phase 1.
	AccessTypeToken AccessType = "Token"
	// AccessTypeKubeconfig uses a kubeconfig from a Secret. Phase 1.
	AccessTypeKubeconfig AccessType = "Kubeconfig"
	// AccessTypeGoogleToken mints Google OAuth2 tokens against an explicit
	// server and CA. Phase 1, and the primary WI-disabled GKE path.
	AccessTypeGoogleToken AccessType = "GoogleToken"
	// AccessTypeWorkloadIdentity mints Google tokens through GKE Workload
	// Identity. Phase 1, and the preferred GKE path when WI is enabled.
	AccessTypeWorkloadIdentity AccessType = "WorkloadIdentity"
	// AccessTypeGKE discovers endpoint and CA through the GKE API. Phase 2.
	AccessTypeGKE AccessType = "GKE"
	// AccessTypeConnectGateway reaches a fleet member through Connect Gateway.
	// Phase 2.
	AccessTypeConnectGateway AccessType = "ConnectGateway"
	// AccessTypeClientCertificate uses an x509 client keypair. Phase 2.
	AccessTypeClientCertificate AccessType = "ClientCertificate"
	// AccessTypeProjectedServiceAccount presents the controller's projected SA
	// token with a cluster-specific audience. Phase 3.
	AccessTypeProjectedServiceAccount AccessType = "ProjectedServiceAccount"
	// AccessTypeExecPlugin shells out to a credential plugin. Phase 3, gated by
	// --allow-exec-credentials plus a command allowlist, because it is arbitrary
	// code execution driven by a cluster-scoped CR.
	AccessTypeExecPlugin AccessType = "ExecPlugin"
	// AccessTypeEKS discovers an EKS endpoint and CA via eks:DescribeCluster and
	// authenticates with a presigned STS GetCallerIdentity token. Not yet
	// implemented; reserved so the name is stable and a CR using it fails with
	// AccessTypeNotImplemented rather than an enum rejection.
	AccessTypeEKS AccessType = "EKS"
	// AccessTypeAKS discovers an AKS endpoint via ARM and authenticates with an
	// Entra ID token. Not yet implemented; reserved as above.
	AccessTypeAKS AccessType = "AKS"
)

// RemoteClusterSpec declares how to reach a secondary cluster, and who may use it.
type RemoteClusterSpec struct {
	// DisplayName is a human-readable label for dashboards and events.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	DisplayName string `json:"displayName,omitempty"`

	// Access is how the controller authenticates to this cluster.
	// +kubebuilder:validation:Required
	Access ClusterAccess `json:"access"`

	// AllowedNamespaces lists the namespaces permitted to reference this
	// cluster. Nil or empty means NONE: this fails closed on purpose, because
	// remote Pod IPs are sensitive and a cluster-scoped CR is not a namespace
	// owner's to reason about.
	// +optional
	AllowedNamespaces *NamespaceSelector `json:"allowedNamespaces,omitempty"`

	// AddressPolicy filters every address resolved from this cluster. It is
	// applied before the controller-wide default policy.
	// +optional
	AddressPolicy *AddressPolicy `json:"addressPolicy,omitempty"`
}

// NamespaceSelector matches namespaces by name, by label, or both. The two are
// a union: a namespace matching either is permitted.
type NamespaceSelector struct {
	// MatchNames lists namespaces verbatim.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	MatchNames []string `json:"matchNames,omitempty"`

	// Selector matches namespaces by label.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// ClusterAccess is a tagged union over the supported authentication methods.
//
// The CEL rules below make the payload required for its own type and rejected
// for every other, so a typo in the discriminator surfaces at admission rather
// than as a confusing runtime condition.
// +kubebuilder:validation:XValidation:rule="self.type == 'InCluster' ? true : !has(self.inCluster)",message="inCluster config is only valid when type is InCluster"
// +kubebuilder:validation:XValidation:rule="self.type == 'Token' ? has(self.token) : !has(self.token)",message="token config is required if and only if type is Token"
// +kubebuilder:validation:XValidation:rule="self.type == 'Kubeconfig' ? has(self.kubeconfig) : !has(self.kubeconfig)",message="kubeconfig config is required if and only if type is Kubeconfig"
// +kubebuilder:validation:XValidation:rule="self.type == 'GoogleToken' ? has(self.googleToken) : !has(self.googleToken)",message="googleToken config is required if and only if type is GoogleToken"
// +kubebuilder:validation:XValidation:rule="self.type == 'GKE' ? has(self.gke) : !has(self.gke)",message="gke config is required if and only if type is GKE"
// +kubebuilder:validation:XValidation:rule="self.type == 'ConnectGateway' ? has(self.connectGateway) : !has(self.connectGateway)",message="connectGateway config is required if and only if type is ConnectGateway"
// +kubebuilder:validation:XValidation:rule="self.type == 'ClientCertificate' ? has(self.clientCertificate) : !has(self.clientCertificate)",message="clientCertificate config is required if and only if type is ClientCertificate"
// +kubebuilder:validation:XValidation:rule="self.type == 'ProjectedServiceAccount' ? has(self.projectedServiceAccount) : !has(self.projectedServiceAccount)",message="projectedServiceAccount config is required if and only if type is ProjectedServiceAccount"
// +kubebuilder:validation:XValidation:rule="self.type == 'ExecPlugin' ? has(self.execPlugin) : !has(self.execPlugin)",message="execPlugin config is required if and only if type is ExecPlugin"
// +kubebuilder:validation:XValidation:rule="self.type == 'WorkloadIdentity' ? has(self.workloadIdentity) : !has(self.workloadIdentity)",message="workloadIdentity config is required if and only if type is WorkloadIdentity"
// +kubebuilder:validation:XValidation:rule="self.type == 'EKS' ? has(self.eks) : !has(self.eks)",message="eks config is required if and only if type is EKS"
// +kubebuilder:validation:XValidation:rule="self.type == 'AKS' ? has(self.aks) : !has(self.aks)",message="aks config is required if and only if type is AKS"
type ClusterAccess struct {
	// Type selects the authentication method and which payload field below is
	// read.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=InCluster;Token;Kubeconfig;GoogleToken;WorkloadIdentity;GKE;ConnectGateway;ClientCertificate;ProjectedServiceAccount;ExecPlugin;EKS;AKS
	Type AccessType `json:"type"`

	// +optional
	InCluster *InClusterAccess `json:"inCluster,omitempty"`
	// +optional
	Token *TokenAccess `json:"token,omitempty"`
	// +optional
	Kubeconfig *KubeconfigAccess `json:"kubeconfig,omitempty"`
	// +optional
	GoogleToken *GoogleTokenAccess `json:"googleToken,omitempty"`
	// +optional
	WorkloadIdentity *WorkloadIdentityAccess `json:"workloadIdentity,omitempty"`
	// +optional
	GKE *GKEAccess `json:"gke,omitempty"`
	// +optional
	ConnectGateway *ConnectGatewayAccess `json:"connectGateway,omitempty"`
	// +optional
	ClientCertificate *ClientCertAccess `json:"clientCertificate,omitempty"`
	// +optional
	ProjectedServiceAccount *ProjectedSAAccess `json:"projectedServiceAccount,omitempty"`
	// +optional
	ExecPlugin *ExecPluginAccess `json:"execPlugin,omitempty"`
	// +optional
	EKS *EKSAccess `json:"eks,omitempty"`
	// +optional
	AKS *AKSAccess `json:"aks,omitempty"`

	// TLS applies to every access type that dials an apiserver directly. Access
	// types carrying their own caSecretRef take precedence over this.
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`

	// Proxy routes apiserver traffic through an HTTP or SOCKS5 proxy.
	// +optional
	Proxy *ProxyConfig `json:"proxy,omitempty"`

	// Impersonate performs Kubernetes user impersonation on the remote cluster.
	// This is distinct from GCP service account impersonation.
	// +optional
	Impersonate *ImpersonationConfig `json:"impersonate,omitempty"`

	// Timeout for individual requests to the remote apiserver.
	// +kubebuilder:default="30s"
	// +optional
	// Pointer so that an unset field is omitted on the wire and the CRD default
	// applies. metav1.Duration is a struct, and encoding/json ignores omitempty
	// on structs, so a non-pointer field is always sent -- as "0s" -- and the
	// apiserver default never fires for a Go client.
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// QPS is the client-side rate limit for this cluster's client.
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	// +optional
	QPS int32 `json:"qps,omitempty"`

	// Burst is the client-side burst allowance for this cluster's client.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	// +optional
	Burst int32 `json:"burst,omitempty"`

	// ResyncPeriod is how often this cluster's informers replay their cache, as
	// a backstop against controller-side drift. Set 0s to disable.
	//
	// This is NOT a polling interval, and lowering it will not make endpoint
	// updates faster: Pod and Node endpoints are watch-driven, and a resync
	// replays the existing cache rather than re-listing the remote apiserver,
	// so it cannot by itself recover an event the watch missed -- watch
	// bookmarks and relist-on-error handle that. Resyncs are cheap because a
	// reconcile over unchanged state is required to produce zero writes.
	//
	// Pointer for the same reason as Timeout: encoding/json ignores omitempty
	// on structs, so a non-pointer metav1.Duration is always sent as "0s" and
	// the CRD default never fires for a Go client.
	// +kubebuilder:default="10m"
	// +optional
	ResyncPeriod *metav1.Duration `json:"resyncPeriod,omitempty"`
}

// InClusterAccess uses the controller's own ServiceAccount against the local
// apiserver. It has no fields by design.
type InClusterAccess struct{}

// TokenAccess presents a static bearer token read from a Secret.
type TokenAccess struct {
	// Server is the apiserver URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	Server string `json:"server"`

	// SecretRef holds the bearer token. Default key: token.
	// +kubebuilder:validation:Required
	SecretRef SecretKeyRef `json:"secretRef"`
}

// KubeconfigAccess reads a full kubeconfig from a Secret.
type KubeconfigAccess struct {
	// SecretRef holds the kubeconfig. Default key: value, then config.
	// +kubebuilder:validation:Required
	SecretRef SecretKeyRef `json:"secretRef"`

	// Context selects a context within the kubeconfig. Defaults to its
	// current-context.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Context string `json:"context,omitempty"`
}

// GoogleTokenAccess mints Google OAuth2 access tokens and presents them to an
// explicitly configured apiserver.
//
// This covers node-SA ADC, service account key files, and impersonation, which
// differ only in how ADC resolves -- a library concern, not an API one. Server
// and CA are explicit, so this path has no dependency on
// container.googleapis.com, roles/container.clusterViewer, or the
// cloud-platform node scope for discovery. That is what makes it work with
// Workload Identity disabled.
type GoogleTokenAccess struct {
	// Server is the apiserver URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	Server string `json:"server"`

	// CASecretRef holds the cluster CA. Falls back to access.tls.caSecretRef.
	// Default key: ca.crt.
	// +optional
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`

	// Credentials selects how Google credentials are obtained. Nil means
	// Application Default Credentials.
	// +optional
	Credentials *GCPCredentials `json:"credentials,omitempty"`
}

// GCPCredentials selects how ambient Google credentials are obtained and
// optionally escalated.
type GCPCredentials struct {
	// ServiceAccountKeySecretRef holds a legacy JSON service account key. It
	// bypasses node pool OAuth scopes entirely, which is why it remains
	// supported. Default key: key.json.
	// +optional
	ServiceAccountKeySecretRef *SecretKeyRef `json:"serviceAccountKeySecretRef,omitempty"`

	// ImpersonateServiceAccount names a GSA to impersonate after obtaining
	// ambient credentials.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ImpersonateServiceAccount string `json:"impersonateServiceAccount,omitempty"`

	// Scopes requested for the token. Defaults to
	// https://www.googleapis.com/auth/cloud-platform.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Scopes []string `json:"scopes,omitempty"`
}

// WorkloadIdentityAccess mints Google tokens through GKE Workload Identity.
//
// This is GoogleTokenAccess with the footgun removed: there is deliberately no
// service account key field, because mounting a static JSON key in a
// WI-enabled cluster defeats the entire point of WI. ADC resolves through the
// GKE metadata server to the GSA bound to this pod's KSA and, unlike the
// node-SA path, the resulting token is not constrained by node pool OAuth
// scopes -- which is the single most common reason the WI-disabled path
// returns 401.
//
// Functionally this overlaps GoogleToken with nil credentials. It is a
// separate type because the narrowing is the point: it states the dependency
// on WI explicitly and makes the insecure configuration unrepresentable.
type WorkloadIdentityAccess struct {
	// Server is the apiserver URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	Server string `json:"server"`

	// CASecretRef holds the cluster CA. Falls back to access.tls.caSecretRef.
	// Default key: ca.crt.
	// +optional
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`

	// ServiceAccountEmail is the GSA this pod is expected to resolve to. When
	// set, the controller asserts the resolved identity and reports a clear
	// condition if the KSA-to-GSA binding is wrong, instead of surfacing an
	// opaque 401 from the secondary cluster.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ServiceAccountEmail string `json:"serviceAccountEmail,omitempty"`

	// ImpersonateServiceAccount names a GSA to impersonate after obtaining the
	// Workload Identity token. This is the supported way to reach a secondary cluster the
	// bound GSA itself must not be granted access to.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ImpersonateServiceAccount string `json:"impersonateServiceAccount,omitempty"`

	// Scopes requested for the token. Defaults to
	// https://www.googleapis.com/auth/cloud-platform.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Scopes []string `json:"scopes,omitempty"`
}

// GKEAccess adds GKE API discovery of the endpoint and CA on top of
// GoogleToken. Phase 2.
type GKEAccess struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Project string `json:"project"`

	// Location is a zone or region.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Location string `json:"location"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Cluster string `json:"cluster"`

	// UsePrivateEndpoint dials the cluster's private endpoint. Requires network
	// reachability from the controller's cluster.
	// +kubebuilder:default=false
	// +optional
	UsePrivateEndpoint bool `json:"usePrivateEndpoint,omitempty"`

	// +optional
	Credentials *GCPCredentials `json:"credentials,omitempty"`
}

// ConnectGatewayAccess reaches a fleet member through Connect Gateway, which
// removes the need for inbound network reachability. Phase 2.
type ConnectGatewayAccess struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Project string `json:"project"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Location string `json:"location"`

	// Membership is the fleet membership name of the secondary cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Membership string `json:"membership"`

	// +optional
	Credentials *GCPCredentials `json:"credentials,omitempty"`
}

// ClientCertAccess authenticates with an x509 client keypair. Phase 2.
//
// SecretName is a whole-Secret reference rather than a SecretKeyRef because a
// keypair is two keys; CertKey and KeyKey name them within that Secret.
type ClientCertAccess struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	Server string `json:"server"`

	// CASecretRef holds the cluster CA. Falls back to access.tls.caSecretRef.
	// +optional
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`

	// SecretName is a Secret in the credentials namespace holding the keypair.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName"`

	// +kubebuilder:default="tls.crt"
	// +optional
	CertKey string `json:"certKey,omitempty"`

	// +kubebuilder:default="tls.key"
	// +optional
	KeyKey string `json:"keyKey,omitempty"`
}

// ProjectedSAAccess presents the controller's own projected ServiceAccount
// token, minted for a cluster-specific audience. It requires the secondary cluster to trust
// the hub's OIDC issuer. Phase 3.
type ProjectedSAAccess struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	Server string `json:"server"`

	// CASecretRef holds the cluster CA. Falls back to access.tls.caSecretRef.
	// +optional
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`

	// Audience the token is minted for, as configured on the secondary cluster's apiserver.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Audience string `json:"audience"`

	// TokenPath is where the projected token is mounted in the controller pod.
	// +kubebuilder:default="/var/run/secrets/tokens/remote"
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	TokenPath string `json:"tokenPath,omitempty"`
}

// ExecPluginAccess shells out to a client-go credential plugin. Phase 3.
//
// This is arbitrary code execution driven by a cluster-scoped CR. It is inert
// unless the controller runs with --allow-exec-credentials and Command appears
// in the configured allowlist.
type ExecPluginAccess struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	Server string `json:"server"`

	// CASecretRef holds the cluster CA. Falls back to access.tls.caSecretRef.
	// +optional
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`

	// Command to execute. Must appear in the controller's allowlist.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=1024
	Command string `json:"command"`

	// +optional
	// +kubebuilder:validation:MaxItems=64
	Args []string `json:"args,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxItems=64
	Env []ExecEnvVar `json:"env,omitempty"`

	// APIVersion of the ExecCredential the plugin speaks.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=client.authentication.k8s.io/v1;client.authentication.k8s.io/v1beta1
	APIVersion string `json:"apiVersion"`

	// InstallHint is surfaced in the Ready condition when Command is missing.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	InstallHint string `json:"installHint,omitempty"`
}

// ExecEnvVar is a literal environment variable passed to a credential plugin.
// Values are literal by design: reading them from Secrets would let a
// cluster-scoped CR launder Secret contents into a subprocess argument.
type ExecEnvVar struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Value string `json:"value,omitempty"`
}

// RemoteClusterStatus reports reachability and authentication health.
type RemoteClusterStatus struct {
	// Conditions are Ready, Authenticated, and Reachable.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// KubernetesVersion reported by the remote apiserver's /version endpoint.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Endpoint is the apiserver URL actually dialed, after any discovery. It
	// never contains credentials.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint,omitempty"`

	// LastProbeTime is when the connection was last verified.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// ConsumerCount is how many CrossServices currently hold a reference on this
	// cluster's cached client.
	// +optional
	ConsumerCount int32 `json:"consumerCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=rc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.access.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.kubernetesVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RemoteCluster declares how to reach a secondary cluster, and which namespaces may
// use it. Access to secondary clusters is read-only, always.
type RemoteCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec RemoteClusterSpec `json:"spec"`

	// +optional
	Status RemoteClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RemoteClusterList contains a list of RemoteCluster.
type RemoteClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RemoteCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &RemoteCluster{}, &RemoteClusterList{})
		return nil
	})
}

// EKSAccess discovers an EKS endpoint and CA via eks:DescribeCluster and
// authenticates with a presigned STS GetCallerIdentity token.
//
// Not yet implemented. The shape is reserved so the enum value is stable and a
// CR using it fails with AccessTypeNotImplemented rather than an enum
// rejection. Note that STS presigned tokens are valid for roughly 15 minutes,
// so this path depends on the same TokenSource-not-static-string handling the
// Google variants require.
type EKSAccess struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Region string `json:"region"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=100
	Cluster string `json:"cluster"`

	// +optional
	Credentials *AWSCredentials `json:"credentials,omitempty"`
}

// AWSCredentials selects how AWS credentials are obtained. Nil means the
// ambient provider chain, which on EKS resolves through IRSA or the node role.
type AWSCredentials struct {
	// AccessKeySecretRef holds static credentials. Default keys within the
	// Secret: access_key_id and secret_access_key.
	// +optional
	AccessKeySecretRef *SecretKeyRef `json:"accessKeySecretRef,omitempty"`

	// AssumeRoleARN is a role to assume after obtaining ambient credentials.
	// This is the AWS analogue of GCP service account impersonation.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	AssumeRoleARN string `json:"assumeRoleARN,omitempty"`
}

// AKSAccess discovers an AKS endpoint via ARM and authenticates with an Entra
// ID token. Not yet implemented; the shape is reserved as for EKSAccess.
type AKSAccess struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=64
	SubscriptionID string `json:"subscriptionID"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=90
	ResourceGroup string `json:"resourceGroup"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Cluster string `json:"cluster"`

	// +optional
	Credentials *AzureCredentials `json:"credentials,omitempty"`
}

// AzureCredentials selects how Entra ID credentials are obtained. Nil means the
// ambient chain, which on AKS resolves through Entra Workload Identity or a
// managed identity.
type AzureCredentials struct {
	// ClientSecretRef holds a client secret for an app registration.
	// +optional
	ClientSecretRef *SecretKeyRef `json:"clientSecretRef,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=64
	ClientID string `json:"clientID,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=64
	TenantID string `json:"tenantID,omitempty"`
}
