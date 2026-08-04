/*
Copyright 2026 phenixblue.

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
)

// BGPPeerConfigMode selects how the controller acquires routing state.
// Only RouteReflectorClient is implemented; Passive and BMPCollector are
// accepted by the schema but not yet honored by the controller.
// +kubebuilder:validation:Enum=RouteReflectorClient;Passive;BMPCollector
type BGPPeerConfigMode string

const (
	BGPPeerConfigModeRouteReflectorClient BGPPeerConfigMode = "RouteReflectorClient"
	BGPPeerConfigModePassive              BGPPeerConfigMode = "Passive"
	BGPPeerConfigModeBMPCollector         BGPPeerConfigMode = "BMPCollector"
)

// BGPPeerConfigSpec defines the desired state of BGPPeerConfig
type BGPPeerConfigSpec struct {
	// localASN is the controller's own ASN. Use the 4-byte private range
	// (4200000000-4294967294); the 2-byte private range is too small.
	// int64 because 4-byte ASNs overflow int32, the only signed integer
	// format OpenAPI schemas natively support below it.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	LocalASN int64 `json:"localASN"`

	// routerID is the controller's BGP router ID.
	// +required
	RouterID string `json:"routerID"`

	// listenPort is the port the controller listens for BGP sessions on.
	// +kubebuilder:default=179
	// +optional
	ListenPort *int32 `json:"listenPort,omitempty"`

	// mode selects how the controller acquires routing state.
	// +kubebuilder:default=RouteReflectorClient
	// +optional
	Mode BGPPeerConfigMode `json:"mode,omitempty"`

	// peers are the BGP sessions the controller establishes.
	// +optional
	Peers []BGPPeer `json:"peers,omitempty"`

	// clusterBindings is the trust boundary: a peer may only originate
	// prefixes bound to it here. Enforced in-controller, independent of
	// any router-side prefix-list — the origin cluster is identified by
	// which binding's allowedPrefixes contains the route, never by which
	// peer advertised it (through a route reflector, the advertising peer
	// is always the RR).
	// +optional
	ClusterBindings []ClusterBinding `json:"clusterBindings,omitempty"`
}

// BGPPeerAuth configures authentication for a BGP session.
type BGPPeerAuth struct {
	// tcpMD5SecretRef points at the key within a Secret, in the same
	// namespace, holding the TCP MD5 password.
	// +optional
	TCPMD5SecretRef *corev1.SecretKeySelector `json:"tcpMD5SecretRef,omitempty"`
}

// GracefulRestartConfig configures BGP graceful restart for a session.
type GracefulRestartConfig struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// staleRoutesTime bounds how long routes are held after a restart
	// before being treated as withdrawn.
	// +optional
	StaleRoutesTime *metav1.Duration `json:"staleRoutesTime,omitempty"`
}

// BGPTimers configures BGP session timers.
type BGPTimers struct {
	// +optional
	Hold *metav1.Duration `json:"hold,omitempty"`

	// +optional
	Keepalive *metav1.Duration `json:"keepalive,omitempty"`
}

// BGPPeer is one BGP session the controller establishes.
type BGPPeer struct {
	// +required
	Name string `json:"name"`

	// +required
	Address string `json:"address"`

	// remoteASN is the peer's ASN.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	RemoteASN int64 `json:"remoteASN"`

	// +optional
	Auth *BGPPeerAuth `json:"auth,omitempty"`

	// +optional
	GracefulRestart *GracefulRestartConfig `json:"gracefulRestart,omitempty"`

	// +optional
	Timers *BGPTimers `json:"timers,omitempty"`
}

// Locality describes where a cluster in a ClusterBinding lives.
type Locality struct {
	// +required
	Region string `json:"region"`

	// +required
	Zone string `json:"zone"`
}

// ClusterBinding binds a range of prefixes to the cluster authorized to
// originate them, and the locality that cluster lives in. This is the
// security boundary the Authorize pipeline stage enforces.
type ClusterBinding struct {
	// +required
	ClusterID string `json:"clusterID"`

	// allowedPrefixes are the only prefixes this cluster may originate.
	// Routes outside every binding's allowedPrefixes are dropped, not
	// attributed to any cluster.
	// +required
	// +kubebuilder:validation:MinItems=1
	AllowedPrefixes []string `json:"allowedPrefixes"`

	// maxPrefixes tears down the session if the peer exceeds it.
	// +optional
	MaxPrefixes *int32 `json:"maxPrefixes,omitempty"`

	// +required
	Locality Locality `json:"locality"`
}

// PeerSessionState mirrors the BGP FSM, simplified to what operators need.
// +kubebuilder:validation:Enum=Idle;Connect;Active;OpenSent;OpenConfirm;Established
type PeerSessionState string

const (
	PeerSessionStateIdle        PeerSessionState = "Idle"
	PeerSessionStateConnect     PeerSessionState = "Connect"
	PeerSessionStateActive      PeerSessionState = "Active"
	PeerSessionStateOpenSent    PeerSessionState = "OpenSent"
	PeerSessionStateOpenConfirm PeerSessionState = "OpenConfirm"
	PeerSessionStateEstablished PeerSessionState = "Established"
)

// PeerStatus is the observed state of one BGP session.
type PeerStatus struct {
	// +required
	Name string `json:"name"`

	// +optional
	SessionState PeerSessionState `json:"sessionState,omitempty"`

	// +optional
	Uptime *metav1.Duration `json:"uptime,omitempty"`

	// +optional
	PrefixesReceived int32 `json:"prefixesReceived,omitempty"`

	// +optional
	PrefixesAccepted int32 `json:"prefixesAccepted,omitempty"`

	// prefixesRejected is the count of routes dropped by Authorize; see
	// AdvertisedBackend for per-route reasons.
	// +optional
	PrefixesRejected int32 `json:"prefixesRejected,omitempty"`
}

// BGPPeerConfigStatus defines the observed state of BGPPeerConfig.
type BGPPeerConfigStatus struct {
	// +optional
	Peers []PeerStatus `json:"peers,omitempty"`

	// conditions represent the current state of the BGPPeerConfig
	// resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`

// BGPPeerConfig is the Schema for the bgppeerconfigs API
type BGPPeerConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BGPPeerConfig
	// +required
	Spec BGPPeerConfigSpec `json:"spec"`

	// status defines the observed state of BGPPeerConfig
	// +optional
	Status BGPPeerConfigStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BGPPeerConfigList contains a list of BGPPeerConfig
type BGPPeerConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BGPPeerConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BGPPeerConfig{}, &BGPPeerConfigList{})
		return nil
	})
}
