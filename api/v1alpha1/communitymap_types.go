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

// CommunityField is a BackendCandidate field a CommunityRule can set.
// +kubebuilder:validation:Enum=weight;tier;drain;serviceTag
type CommunityField string

const (
	CommunityFieldWeight     CommunityField = "weight"
	CommunityFieldTier       CommunityField = "tier"
	CommunityFieldDrain      CommunityField = "drain"
	CommunityFieldServiceTag CommunityField = "serviceTag"
)

// UnmappedCommunityPolicy controls what happens when a route's large
// communities don't match any CommunityMap rule at all.
// +kubebuilder:validation:Enum=Ignore;Reject;Warn
type UnmappedCommunityPolicy string

const (
	// UnmappedCommunityIgnore proceeds with fallback/default values.
	UnmappedCommunityIgnore UnmappedCommunityPolicy = "Ignore"
	// UnmappedCommunityReject excludes the route from the settled snapshot.
	UnmappedCommunityReject UnmappedCommunityPolicy = "Reject"
	// UnmappedCommunityWarn proceeds like Ignore but flags the candidate for
	// visibility (e.g. via AdvertisedBackend.status.reason).
	UnmappedCommunityWarn UnmappedCommunityPolicy = "Warn"
)

// CommunityMatch matches a BGP large community. LargeCommunity is a
// "<globalAdmin>:<function>:<value>" pattern; the value segment may be "*"
// to match any value and capture it for FromCommunityValue.
type CommunityMatch struct {
	// +required
	LargeCommunity string `json:"largeCommunity"`
}

// CommunityFieldSet sets one BackendCandidate field when a CommunityRule
// matches.
type CommunityFieldSet struct {
	// field is the BackendCandidate field this rule sets.
	// +required
	Field CommunityField `json:"field"`

	// value is a literal to set the field to.
	// +optional
	Value *string `json:"value,omitempty"`

	// fromCommunityValue sets the field from the community's captured value
	// segment. Only valid when the match pattern's value segment is "*".
	// +optional
	FromCommunityValue bool `json:"fromCommunityValue,omitempty"`
}

// CommunityRule decodes one BGP large community pattern into a
// BackendCandidate field.
type CommunityRule struct {
	// +required
	Match CommunityMatch `json:"match"`

	// +required
	Set CommunityFieldSet `json:"set"`
}

// WeightFallbackSource selects the BGP attribute used to derive weight when
// no rule sets it.
// +kubebuilder:validation:Enum=MED
type WeightFallbackSource string

const WeightFallbackFromMED WeightFallbackSource = "MED"

// PreferenceFallbackSource selects the BGP attribute used to derive
// preference when no rule sets it.
// +kubebuilder:validation:Enum=ASPathLength
type PreferenceFallbackSource string

const PreferenceFallbackFromASPathLength PreferenceFallbackSource = "ASPathLength"

// CommunityFallbacks are used when no rule matches a candidate's
// communities.
type CommunityFallbacks struct {
	// weightFrom derives weight from a BGP attribute when no rule sets it.
	// MED is inverted: lower MED means higher weight.
	// +optional
	WeightFrom WeightFallbackSource `json:"weightFrom,omitempty"`

	// +optional
	PreferenceFrom PreferenceFallbackSource `json:"preferenceFrom,omitempty"`

	// +kubebuilder:default=100
	// +optional
	DefaultWeight int32 `json:"defaultWeight,omitempty"`
}

// CommunityMapSpec defines the desired state of CommunityMap
type CommunityMapSpec struct {
	// rules decode BGP large communities into BackendCandidate fields, in
	// order. Multiple rules may match a single route, each setting a
	// different field.
	// +optional
	Rules []CommunityRule `json:"rules,omitempty"`

	// fallbacks are used when none of rules match a candidate's
	// communities at all.
	// +optional
	Fallbacks *CommunityFallbacks `json:"fallbacks,omitempty"`

	// onUnmappedCommunity controls what happens when a route's large
	// communities don't match any rule.
	// +kubebuilder:default=Ignore
	// +optional
	OnUnmappedCommunity UnmappedCommunityPolicy `json:"onUnmappedCommunity,omitempty"`
}

// CommunityMapStatus defines the observed state of CommunityMap.
type CommunityMapStatus struct {
	// conditions represent the current state of the CommunityMap resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// CommunityMap is the Schema for the communitymaps API
type CommunityMap struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CommunityMap
	// +required
	Spec CommunityMapSpec `json:"spec"`

	// status defines the observed state of CommunityMap
	// +optional
	Status CommunityMapStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CommunityMapList contains a list of CommunityMap
type CommunityMapList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CommunityMap `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CommunityMap{}, &CommunityMapList{})
		return nil
	})
}
