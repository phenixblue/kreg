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

// Package damp implements the Damp pipeline stage: hold-down, flap
// dampening, and addition debounce. See docs/design/architecture.md §1.
//
// The Damper interface (like internal/reconcile.Driver) exists so the
// specific algorithm is an internal implementation detail, not a
// user-selectable CRD field — v1 ships one implementation
// (internal/damp/ewma), a second is additive later.
package damp

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
)

// Damper evaluates a settled snapshot of BackendCandidates against each
// one's prior state, returning the candidates with Damping decided.
// Candidates absent from this tick but still within their
// withdrawalGrace hold-down are synthesized back into the returned list
// from prior — see PriorState. Pure: no I/O, no cluster access; the
// caller (internal/snapshot.Source) does the listing prior state is
// built from.
type Damper interface {
	Evaluate(now time.Time, candidates []pipeline.BackendCandidate,
		prior map[string]PriorState, cfg kregv1alpha1.BGPStabilityConfigSpec,
	) []pipeline.BackendCandidate
}

// PriorState is one candidate's last-known state, reconstructed from its
// AdvertisedBackend record — the durable, cross-tick memory a Damper
// needs, persisted between reconciles rather than kept in memory so it
// survives a controller restart or leader failover.
type PriorState struct {
	// Candidate is the last-known full candidate (every field Reconcile
	// needs), reconstructed from AdvertisedBackend.status — what a Damper
	// synthesizes back into its output while a withdrawn candidate is
	// still within withdrawalGrace.
	Candidate pipeline.BackendCandidate

	// Damping is the last-known damping verdict for this candidate.
	Damping pipeline.DampingInfo
}

// PriorStateFromAdvertisedBackends reconstructs prior state from the
// current AdvertisedBackend records, keyed by object name (matching
// reconcile.BackendObjectName, since that's how each record was named).
// Pure — no cluster access.
//
// A record whose status.stability.lastObservedAt is unset was never
// evaluated by Damp (e.g. a route that's always been
// Authorize/CommunityMap-rejected, which Evaluate skips outright) and is
// omitted, so it's correctly treated as brand new if it ever becomes
// eligible.
func PriorStateFromAdvertisedBackends(items []kregv1alpha1.AdvertisedBackend) map[string]PriorState {
	prior := make(map[string]PriorState, len(items))
	for _, item := range items {
		stability := item.Status.Stability
		if stability.LastObservedAt == nil {
			continue
		}
		prior[item.Name] = PriorState{
			Candidate: candidateFromStatus(item.Status),
			Damping: pipeline.DampingInfo{
				State:           item.Status.State,
				Reason:          item.Status.Reason,
				Score:           float64(stability.DampeningPenalty),
				FlapCount24h:    stability.FlapCount24h,
				LastObservedAt:  stability.LastObservedAt.Time,
				WithdrawnAt:     timePtr(stability.WithdrawnAt),
				SuppressedSince: timePtr(stability.SuppressedSince),
				PendingSince:    timePtr(stability.PendingSince),
			},
		}
	}
	return prior
}

// candidateFromStatus reverses internal/report.buildOne's mapping: every
// field Reconcile needs to render this candidate again while it's
// synthesized during hold-down.
func candidateFromStatus(status kregv1alpha1.AdvertisedBackendStatus) pipeline.BackendCandidate {
	return pipeline.BackendCandidate{
		Prefix:           status.Prefix,
		ClusterID:        status.ClusterID,
		Peer:             status.Peer,
		Locality:         pipeline.Locality{Region: status.Locality.Region, Zone: status.Locality.Zone},
		MED:              uint32(status.Attributes.MED),
		ASPath:           asPathUint32(status.Attributes.ASPath),
		LargeCommunities: status.Attributes.LargeCommunities,
		Weight:           status.Attributes.Weight,
		Tier:             status.Attributes.Tier,
		Drain:            status.Attributes.Drain,
		ServiceTag:       status.Attributes.ServiceTag,
	}
}

func asPathUint32(asPath []int64) []uint32 {
	if asPath == nil {
		return nil
	}
	out := make([]uint32, len(asPath))
	for i, asn := range asPath {
		out[i] = uint32(asn)
	}
	return out
}

func timePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	return &t.Time
}
