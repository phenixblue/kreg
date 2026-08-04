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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BGPStabilityConfigSpec controls how route churn is smoothed before it
// reaches generated Gateway API / Istio config. Cluster-scoped and
// singular (conventionally named "default", mirroring CommunityMap)
// rather than attached per-BGPBackendPolicy: a backend's hold-down and
// dampening state (AdvertisedBackend.status) is one value per
// prefix+clusterID, independent of which policy or policies currently
// select it, so there's no principled per-policy owner for this config.
type BGPStabilityConfigSpec struct {
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

// DampeningConfig configures flap dampening: an exponentially-decaying
// instability score accumulates on each flap and decays with halfLife;
// crossing suppressThreshold suppresses the backend until the score
// decays back below reuseThreshold, capped by maxSuppress. The specific
// algorithm sits behind an internal interface (internal/damp.Damper) and
// may change; these fields describe its tunable behavior, not its
// implementation.
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

// BGPStabilityConfigStatus defines the observed state of
// BGPStabilityConfig.
type BGPStabilityConfigStatus struct {
	// conditions represent the current state of the BGPStabilityConfig
	// resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// BGPStabilityConfig is the Schema for the bgpstabilityconfigs API
type BGPStabilityConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BGPStabilityConfig
	// +required
	Spec BGPStabilityConfigSpec `json:"spec"`

	// status defines the observed state of BGPStabilityConfig
	// +optional
	Status BGPStabilityConfigStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BGPStabilityConfigList contains a list of BGPStabilityConfig
type BGPStabilityConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BGPStabilityConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BGPStabilityConfig{}, &BGPStabilityConfigList{})
		return nil
	})
}
