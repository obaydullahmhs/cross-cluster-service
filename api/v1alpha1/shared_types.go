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

// SecretKeyRef names a key inside a Secret in the controller's credentials
// namespace.
type SecretKeyRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key within the Secret. Defaults are per-field and documented on each use.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key,omitempty"`

	// NOTE: intentionally no Namespace field. Secrets resolve only from the
	// controller's namespace (--credentials-namespace). A cluster-scoped CR that
	// can name any Secret in any namespace is a credential-exfiltration
	// primitive. See the security requirements in the project brief (S9.1).
}

// ClusterRef selects a RemoteCluster, which is cluster-scoped and therefore
// needs no namespace.
type ClusterRef struct {
	// Name of the RemoteCluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// TLSConfig configures the transport used to reach a remote apiserver.
type TLSConfig struct {
	// CASecretRef holds the CA bundle used to verify the remote apiserver.
	// Default key: ca.crt.
	// +optional
	CASecretRef *SecretKeyRef `json:"caSecretRef,omitempty"`

	// ServerName overrides the SNI / certificate hostname. Needed when
	// connecting to an apiserver by IP.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ServerName string `json:"serverName,omitempty"`

	// InsecureSkipVerify disables verification of the remote apiserver's
	// certificate. Rejected unless the controller runs with
	// --allow-insecure-tls; the CR is failed with a clear condition otherwise.
	// +kubebuilder:default=false
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// ProxyConfig routes apiserver traffic through an HTTP or SOCKS5 proxy. This is
// the escape hatch for secondary cluster apiservers that are only reachable from a bastion
// network.
type ProxyConfig struct {
	// URL of the proxy, e.g. http://proxy.internal:3128 or
	// socks5://bastion.internal:1080.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(http|https|socks5)://`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
}

// ImpersonationConfig is Kubernetes user impersonation, sent as
// Impersonate-User / Impersonate-Group headers on every request. It is distinct
// from GCP service account impersonation, which lives under GCPCredentials.
type ImpersonationConfig struct {
	// UserName to impersonate on the remote cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	UserName string `json:"userName"`

	// UID to impersonate.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	UID string `json:"uid,omitempty"`

	// Groups to impersonate.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Groups []string `json:"groups,omitempty"`

	// Extra impersonation attributes.
	// +optional
	Extra map[string][]string `json:"extra,omitempty"`
}

// AddressPolicy filters resolved addresses before they are ever written to an
// EndpointSlice. It is enforced controller-side: do not assume apiserver
// validation covers EndpointSlice the way it does legacy Endpoints.
type AddressPolicy struct {
	// AllowedCIDRs, when non-empty, restricts endpoints to these ranges.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`

	// DeniedCIDRs are rejected even when they match AllowedCIDRs.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	DeniedCIDRs []string `json:"deniedCIDRs,omitempty"`

	// DenySpecialPurpose blocks loopback, link-local (including the
	// 169.254.169.254 metadata server), unspecified, and multicast addresses.
	// Defaults true: a namespace tenant who can create a CrossService must not
	// be able to point a Service at the node's metadata server.
	// +kubebuilder:default=true
	// +optional
	DenySpecialPurpose *bool `json:"denySpecialPurpose,omitempty"`
}
