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

// Package pipeline implements the Ingest-independent stages of KREG's BGP
// -> Gateway API pipeline: Normalize decodes raw route data into semantic
// BackendCandidates. Both are pure functions over plain Go data, so they
// need neither a BGP daemon nor a Kubernetes API server to test.
package pipeline

import (
	"time"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
)

// RIBRoute is one route advertisement as it would arrive from BGP ingest.
// GoBGP ingest (build-order step 2) will produce these from a live RIB;
// until then they come from hand-written fixtures. ClusterID and Locality
// are assumed already resolved via BGPPeerConfig.clusterBindings — that
// prefix -> cluster trust boundary is an Authorize-stage concern, not
// Normalize's.
type RIBRoute struct {
	// Prefix is the advertised route, e.g. "198.51.100.10/32".
	Prefix string

	// ClusterID is the origin cluster.
	ClusterID string

	// Peer is the BGP peer (route reflector) this route was learned from.
	Peer string

	Locality Locality

	// MED is the route's multi-exit discriminator.
	MED uint32

	// ASPath is the route's AS path, in order.
	ASPath []uint32

	// LargeCommunities are the route's BGP large communities, each
	// formatted as "<globalAdmin>:<function>:<value>".
	LargeCommunities []string

	// Rejected is set by Authorize when this route's prefix falls within
	// no clusterBinding's allowedPrefixes. Rejected routes are kept, not
	// dropped, so the Report stage (AdvertisedBackend) can show why a
	// route never made it into service — see the "Attribution note" in
	// docs/design/architecture.md §2.1.
	Rejected bool
	Reason   string
}

// Locality describes where a route's advertising cluster lives.
type Locality struct {
	Region string
	Zone   string
}

// BackendCandidate is Normalize's output: a route decoded into the
// semantic attributes the reconciler acts on. Mirrors
// AdvertisedBackend.status.
type BackendCandidate struct {
	Prefix           string
	ClusterID        string
	Peer             string
	Locality         Locality
	MED              uint32
	ASPath           []uint32
	LargeCommunities []string

	// Weight, Tier, Drain, ServiceTag are decoded from LargeCommunities via
	// CommunityMap rules, falling back to CommunityMapSpec.Fallbacks when
	// no rule sets a given field.
	Weight     int32
	Tier       string
	Drain      bool
	ServiceTag *int32

	// Rejected is set when CommunityMapSpec.OnUnmappedCommunity is Reject
	// and no rule matched this route's communities at all. Rejected
	// candidates carry a Reason and should be excluded from
	// reconciliation.
	Rejected bool
	Reason   string

	// Damping is set by the Damp stage (internal/damp); nil until Damp
	// runs, meaning "no opinion, behave as before" everywhere it's
	// checked. See DampingInfo.
	Damping *DampingInfo
}

// DampingInfo is the Damp stage's verdict on one BackendCandidate,
// carried through to AdvertisedBackend.status. Score, LastObservedAt, and
// the *Since/*At timestamps are the durable, cross-tick state a Damper
// implementation needs — persisted via AdvertisedBackend.status between
// reconciles rather than kept in memory, so it survives a controller
// restart or leader failover.
type DampingInfo struct {
	// State is one of Active, Pending, HoldDown, or Dampened — Rejected
	// and Draining are decided elsewhere and never appear here.
	State  kregv1alpha1.BackendState
	Reason string

	// Score is the current flap-dampening score: decays continuously
	// against real elapsed time, bumped by a fixed penalty on each
	// detected flap.
	Score float64

	// FlapCount24h approximates flaps in roughly the last 24h via the
	// same decay mechanism as Score, with a fixed 24h half-life.
	FlapCount24h int32

	// LastObservedAt is when this candidate was last evaluated by Damp —
	// used to compute real elapsed time for decay on the next tick.
	LastObservedAt time.Time

	// WithdrawnAt is when this candidate was first observed absent from
	// the settled snapshot; nil while present. Drives withdrawalGrace
	// expiry.
	WithdrawnAt *time.Time

	// SuppressedSince is when this candidate most recently entered
	// Dampened; nil otherwise. Drives the maxSuppress cap.
	SuppressedSince *time.Time

	// PendingSince is when this candidate was first observed at all;
	// nil once past additionDelay or if additionDelay is unset.
	PendingSince *time.Time
}
