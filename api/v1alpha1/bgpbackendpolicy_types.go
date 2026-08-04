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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// BGPBackendPolicySpec defines the desired state of BGPBackendPolicy.
//
// It follows the Gateway API policy-attachment convention: it targets a
// Gateway or HTTPRoute via targetRef, selects which BGP-advertised routes
// feed that target, and describes how the reconciler should turn those
// routes into backend config.
type BGPBackendPolicySpec struct {
	// targetRef identifies the Gateway or HTTPRoute this policy attaches to.
	// +required
	TargetRef gatewayv1.LocalPolicyTargetReference `json:"targetRef"`

	// selector picks which advertised routes feed this target.
	// +required
	Selector BackendSelector `json:"selector"`

	// backend describes the port and TLS posture for the selected backends.
	// +required
	Backend BackendConfig `json:"backend"`

	// loadBalancing controls how traffic is distributed across selected
	// backends.
	// +optional
	LoadBalancing LoadBalancingConfig `json:"loadBalancing,omitzero"`

	// stability controls how route churn is smoothed before it reaches
	// Gateway API config.
	// +optional
	Stability StabilityConfig `json:"stability,omitzero"`

	// activeHealth configures HTTP-level health checking of selected
	// backends. BGP is reachability, not application health; both are
	// required before traffic is trusted with production traffic.
	// +optional
	ActiveHealth *ActiveHealthConfig `json:"activeHealth,omitempty"`

	// outlierDetection configures passive ejection of unhealthy backends
	// based on observed traffic.
	// +optional
	OutlierDetection *OutlierDetectionConfig `json:"outlierDetection,omitempty"`
}

// BackendSelector picks which advertised routes feed a BGPBackendPolicy's
// target.
type BackendSelector struct {
	// prefixes restricts selection to advertised routes within these CIDRs.
	// +optional
	Prefixes []string `json:"prefixes,omitempty"`

	// serviceTag restricts selection to routes a CommunityMap rule decoded
	// with this serviceTag.
	// +optional
	ServiceTag *int32 `json:"serviceTag,omitempty"`

	// clusterIDs restricts selection to these origin clusters. Empty means
	// all clusters authorized via BGPPeerConfig.clusterBindings.
	// +optional
	ClusterIDs []string `json:"clusterIDs,omitempty"`
}

// BackendTLSMode selects the TLS posture for the gateway -> cluster hop.
// +kubebuilder:validation:Enum=SIMPLE;Passthrough;Mutual
type BackendTLSMode string

const (
	// BackendTLSModeSimple validates the backend's server cert via
	// credentialRef; no client cert is asserted. The v1 default.
	BackendTLSModeSimple BackendTLSMode = "SIMPLE"
	// BackendTLSModePassthrough routes on SNI without terminating TLS; the
	// gateway never holds key material for this backend.
	BackendTLSModePassthrough BackendTLSMode = "Passthrough"
	// BackendTLSModeMutual additionally asserts a client cert, verified by
	// the backend. Deferred until a deployment's trust-boundary
	// requirements demand it.
	BackendTLSModeMutual BackendTLSMode = "Mutual"
)

// BackendTLSConfig configures TLS for the gateway -> cluster hop.
type BackendTLSConfig struct {
	// mode selects the TLS posture for this hop.
	// +kubebuilder:default=SIMPLE
	// +optional
	Mode BackendTLSMode `json:"mode,omitempty"`

	// sni is the hostname presented during the TLS handshake and, in SIMPLE
	// mode, validated against the backend's server certificate.
	// +required
	SNI string `json:"sni"`

	// credentialRef points at the CA bundle used to validate the backend's
	// server certificate. Unused when mode is Passthrough.
	// +optional
	CredentialRef *corev1.LocalObjectReference `json:"credentialRef,omitempty"`
}

// BackendConfig describes the port and TLS posture for selected backends.
type BackendConfig struct {
	// port is the backend port traffic is forwarded to.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// appProtocol identifies the application protocol spoken by the
	// backend, e.g. "https".
	// +optional
	AppProtocol *string `json:"appProtocol,omitempty"`

	// tls configures the gateway -> cluster hop.
	// +required
	TLS BackendTLSConfig `json:"tls"`
}

// LoadBalancingStrategy selects how traffic is spread across selected
// backends.
// +kubebuilder:validation:Enum=Locality;Weighted;Uniform
type LoadBalancingStrategy string

const (
	LoadBalancingStrategyLocality LoadBalancingStrategy = "Locality"
	LoadBalancingStrategyWeighted LoadBalancingStrategy = "Weighted"
	LoadBalancingStrategyUniform  LoadBalancingStrategy = "Uniform"
)

// LoadBalancingConfig controls how traffic is distributed across selected
// backends.
type LoadBalancingConfig struct {
	// +kubebuilder:default=Locality
	// +optional
	Strategy LoadBalancingStrategy `json:"strategy,omitempty"`

	// +optional
	Locality *LocalityLoadBalancingConfig `json:"locality,omitempty"`
}

// LocalityLoadBalancingConfig configures locality-aware failover.
type LocalityLoadBalancingConfig struct {
	// preference is the ordered locality failover chain.
	// +optional
	Preference []string `json:"preference,omitempty"`

	// failoverThreshold is the percent of local capacity that must be
	// healthy before traffic spills to the next locality in preference.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	FailoverThreshold *int32 `json:"failoverThreshold,omitempty"`
}

// StabilityConfig controls how route churn is smoothed before it reaches
// generated Gateway API / Istio config.
type StabilityConfig struct {
	// withdrawalGrace is the hold-down before removing a backend that
	// stopped being advertised.
	// +optional
	WithdrawalGrace *metav1.Duration `json:"withdrawalGrace,omitempty"`

	// additionDelay is how long a newly-advertised route must be settled
	// before it's added as a backend.
	// +optional
	AdditionDelay *metav1.Duration `json:"additionDelay,omitempty"`

	// +optional
	Dampening *DampeningConfig `json:"dampening,omitempty"`
}

// DampeningConfig configures RFC 2439-style flap dampening.
type DampeningConfig struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// +optional
	HalfLife *metav1.Duration `json:"halfLife,omitempty"`

	// +optional
	SuppressThreshold *int32 `json:"suppressThreshold,omitempty"`

	// +optional
	ReuseThreshold *int32 `json:"reuseThreshold,omitempty"`

	// +optional
	MaxSuppress *metav1.Duration `json:"maxSuppress,omitempty"`
}

// ActiveHealthConfig configures HTTP-level health checking of selected
// backends. BGP reachability alone is not sufficient signal for production
// traffic.
type ActiveHealthConfig struct {
	// +required
	Path string `json:"path"`

	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// +optional
	UnhealthyThreshold *int32 `json:"unhealthyThreshold,omitempty"`

	// +optional
	HealthyThreshold *int32 `json:"healthyThreshold,omitempty"`
}

// OutlierDetectionConfig configures passive ejection of backends based on
// observed traffic (as opposed to active probing).
type OutlierDetectionConfig struct {
	// consecutive5xx is the number of consecutive 5xx responses before a
	// backend is ejected.
	// +optional
	Consecutive5xx *int32 `json:"consecutive5xx,omitempty"`

	// +optional
	BaseEjectionTime *metav1.Duration `json:"baseEjectionTime,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxEjectionPercent *int32 `json:"maxEjectionPercent,omitempty"`
}

// BGPBackendPolicyStatus defines the observed state of BGPBackendPolicy.
type BGPBackendPolicyStatus struct {
	// conditions represent the current state of the BGPBackendPolicy
	// resource. Standard condition types include "Accepted" (the policy is
	// well-formed and attached) and "Programmed" (the generated resources
	// have been written).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// activeBackends is the count of selected backends currently in the
	// Active state.
	// +optional
	ActiveBackends int32 `json:"activeBackends,omitempty"`

	// suppressedBackends is the count of selected backends currently
	// dampened.
	// +optional
	SuppressedBackends int32 `json:"suppressedBackends,omitempty"`

	// generated lists the resources this policy produced, as
	// "Kind/namespace/name".
	// +optional
	Generated []string `json:"generated,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activeBackends`
// +kubebuilder:printcolumn:name="Suppressed",type=integer,JSONPath=`.status.suppressedBackends`

// BGPBackendPolicy is the Schema for the bgpbackendpolicies API
type BGPBackendPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BGPBackendPolicy
	// +required
	Spec BGPBackendPolicySpec `json:"spec"`

	// status defines the observed state of BGPBackendPolicy
	// +optional
	Status BGPBackendPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BGPBackendPolicyList contains a list of BGPBackendPolicy
type BGPBackendPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BGPBackendPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BGPBackendPolicy{}, &BGPBackendPolicyList{})
		return nil
	})
}
