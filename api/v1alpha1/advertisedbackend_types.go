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

// AdvertisedBackendSpec is empty: AdvertisedBackend is entirely
// controller-written, the materialized view of the RIB — there is no
// user-configurable desired state. See docs/design/architecture.md §2.4.
type AdvertisedBackendSpec struct{}

// BackendAttributes are the semantic attributes decoded from a route's
// BGP large communities (or their fallbacks), mirroring
// pipeline.BackendCandidate. MED and ASPath are int64, not uint32: 4-byte
// ASNs and this design's own worked example (asPath: [4200000101])
// already exceed int32's range, the only signed format OpenAPI schemas
// natively support below it — same reasoning as BGPPeerConfig's ASN
// fields.
type BackendAttributes struct {
	// +optional
	Weight int32 `json:"weight,omitempty"`

	// +optional
	Tier string `json:"tier,omitempty"`

	// +optional
	Drain bool `json:"drain,omitempty"`

	// +optional
	ServiceTag *int32 `json:"serviceTag,omitempty"`

	// +optional
	MED int64 `json:"med,omitempty"`

	// +optional
	ASPath []int64 `json:"asPath,omitempty"`

	// +optional
	LargeCommunities []string `json:"largeCommunities,omitempty"`
}

// BackendState mirrors where a route currently sits in the pipeline.
// HoldDown, Dampened, and Pending are set by the Damper stage
// (internal/damp): HoldDown while a withdrawn route is within its
// withdrawalGrace, Dampened while its flap score is above
// suppressThreshold, Pending while a newly-seen route is within its
// additionDelay.
// +kubebuilder:validation:Enum=Active;HoldDown;Draining;Dampened;Pending;Rejected
type BackendState string

const (
	BackendStateActive   BackendState = "Active"
	BackendStateHoldDown BackendState = "HoldDown"
	BackendStateDraining BackendState = "Draining"
	BackendStateDampened BackendState = "Dampened"
	BackendStatePending  BackendState = "Pending"
	BackendStateRejected BackendState = "Rejected"
)

// StabilityStatus is the Damper's own bookkeeping for one backend —
// separated from AdvertisedBackendStatus's other fields (which track the
// record's general lifecycle, not damping specifically) so the full
// hold-down/flap/suppression picture reads at a glance.
type StabilityStatus struct {
	// flapCount24h approximates flaps in roughly the last 24h: an
	// exponentially-decaying counter (24h half-life), not an exact
	// sliding-window count.
	// +optional
	FlapCount24h int32 `json:"flapCount24h,omitempty"`

	// dampeningPenalty is the current flap-dampening score. Crossing
	// BGPStabilityConfig's suppressThreshold moves state to Dampened;
	// decaying back below reuseThreshold moves it back out.
	// +optional
	DampeningPenalty int32 `json:"dampeningPenalty,omitempty"`

	// lastObservedAt is the wall-clock time the Damper last evaluated
	// this backend. Distinct from AdvertisedBackendStatus.firstSeen (set
	// once) and .lastChange (only bumps on a semantic change) — this
	// bumps every reconcile, and is used to compute real elapsed time for
	// EWMA decay across restarts.
	// +optional
	LastObservedAt *metav1.Time `json:"lastObservedAt,omitempty"`

	// withdrawnAt is when this backend was first observed absent from
	// the settled snapshot; nil while present. Drives withdrawalGrace
	// expiry.
	// +optional
	WithdrawnAt *metav1.Time `json:"withdrawnAt,omitempty"`

	// suppressedSince is when this backend most recently entered
	// Dampened; nil otherwise. Drives the maxSuppress cap.
	// +optional
	SuppressedSince *metav1.Time `json:"suppressedSince,omitempty"`

	// pendingSince is when this backend was first observed (never seen
	// before); nil once past additionDelay or if additionDelay is unset.
	// +optional
	PendingSince *metav1.Time `json:"pendingSince,omitempty"`
}

// AdvertisedBackendStatus defines the observed state of AdvertisedBackend.
type AdvertisedBackendStatus struct {
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// peer is the BGP peer address this route was learned from.
	// +optional
	Peer string `json:"peer,omitempty"`

	// +optional
	Locality Locality `json:"locality,omitzero"`

	// +optional
	Attributes BackendAttributes `json:"attributes,omitzero"`

	// +optional
	State BackendState `json:"state,omitempty"`

	// reason explains a non-Active state, e.g. "prefix 203.0.113.5/32 not
	// in allowedPrefixes for any cluster".
	// +optional
	Reason string `json:"reason,omitempty"`

	// +optional
	FirstSeen *metav1.Time `json:"firstSeen,omitempty"`

	// +optional
	LastChange *metav1.Time `json:"lastChange,omitempty"`

	// stability is the Damper's hold-down/flap/suppression bookkeeping
	// for this backend.
	// +optional
	Stability StabilityStatus `json:"stability,omitzero"`

	// boundPolicies are the BGPBackendPolicies (as "namespace/name") whose
	// selector currently matches this backend.
	// +optional
	BoundPolicies []string `json:"boundPolicies,omitempty"`

	// generatedResources lists what the Reconcile stage produced for this
	// backend, as "Kind/namespace/name".
	// +optional
	GeneratedResources []string `json:"generatedResources,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.status.prefix`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.clusterID`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`

// AdvertisedBackend is the Schema for the advertisedbackends API
type AdvertisedBackend struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AdvertisedBackend
	// +optional
	Spec AdvertisedBackendSpec `json:"spec,omitzero"`

	// status defines the observed state of AdvertisedBackend
	// +optional
	Status AdvertisedBackendStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AdvertisedBackendList contains a list of AdvertisedBackend
type AdvertisedBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AdvertisedBackend `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AdvertisedBackend{}, &AdvertisedBackendList{})
		return nil
	})
}
