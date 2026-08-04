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

// Package report implements the Report pipeline stage: turning a settled
// snapshot into AdvertisedBackend objects, the materialized view of the
// RIB. See docs/design/architecture.md §1 and §2.4.
package report

import (
	"fmt"
	"math"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
)

// ManagedByValue is the reconcile.ManagedByLabel value stamped on every
// AdvertisedBackend this package builds. AdvertisedBackendReconciler's
// prune step lists by this label rather than every object of the kind
// cluster-wide, so it can never delete an AdvertisedBackend it didn't
// create — see docs/design/architecture.md §3's "never patch a resource
// it doesn't own" rule, applied here to deletes.
const ManagedByValue = "advertisedbackend"

// BuildAdvertisedBackends turns a settled snapshot into the desired
// AdvertisedBackend objects: one per candidate, whether authorized or
// rejected. Pure function — no cluster access — so it's tested the same
// way Render is.
func BuildAdvertisedBackends(candidates []pipeline.BackendCandidate, policies []kregv1alpha1.BGPBackendPolicy) []kregv1alpha1.AdvertisedBackend {
	backends := make([]kregv1alpha1.AdvertisedBackend, 0, len(candidates))
	for _, c := range candidates {
		backends = append(backends, buildOne(c, policies))
	}
	return backends
}

func buildOne(c pipeline.BackendCandidate, policies []kregv1alpha1.BGPBackendPolicy) kregv1alpha1.AdvertisedBackend {
	state, reason := stateAndReason(c)
	boundPolicies, generatedResources := bindings(c, policies)

	status := kregv1alpha1.AdvertisedBackendStatus{
		Prefix:    c.Prefix,
		ClusterID: c.ClusterID,
		Peer:      c.Peer,
		Locality:  kregv1alpha1.Locality{Region: c.Locality.Region, Zone: c.Locality.Zone},
		Attributes: kregv1alpha1.BackendAttributes{
			Weight:           c.Weight,
			Tier:             c.Tier,
			Drain:            c.Drain,
			ServiceTag:       c.ServiceTag,
			MED:              int64(c.MED),
			ASPath:           asPathInt64(c.ASPath),
			LargeCommunities: c.LargeCommunities,
		},
		State:              state,
		Reason:             reason,
		BoundPolicies:      boundPolicies,
		GeneratedResources: generatedResources,
	}
	// Damping is nil for a Rejected candidate (Damp never evaluates one)
	// — status.stability simply stays zero-valued, same as before the
	// Damper existed.
	if c.Damping != nil {
		status.Stability = kregv1alpha1.StabilityStatus{
			FlapCount24h:     c.Damping.FlapCount24h,
			DampeningPenalty: clampToInt32(c.Damping.Score),
			LastObservedAt:   metaTime(c.Damping.LastObservedAt),
			WithdrawnAt:      metaTimeOrNil(c.Damping.WithdrawnAt),
			SuppressedSince:  metaTimeOrNil(c.Damping.SuppressedSince),
			PendingSince:     metaTimeOrNil(c.Damping.PendingSince),
		}
	}

	return kregv1alpha1.AdvertisedBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name:   objectName(c),
			Labels: map[string]string{reconcile.ManagedByLabel: ManagedByValue},
		},
		Status: status,
	}
}

// metaTime and metaTimeOrNil convert internal/pipeline.DampingInfo's
// plain time.Time fields to the *metav1.Time AdvertisedBackendStatus
// needs. metaTime treats the zero value as unset (nil) rather than a
// literal year-0001 timestamp: PriorStateFromAdvertisedBackends uses
// LastObservedAt being nil as its sentinel for "never evaluated by
// Damp", so a bogus non-nil zero-time would incorrectly look like a
// real evaluation happened.
func metaTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}

func metaTimeOrNil(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	return metaTime(*t)
}

// clampToInt32 rounds and clamps a damping score into int32's range
// before it's persisted as DampeningPenalty. Go's float->int conversion
// is implementation-specific once the value is out of the target type's
// range (or NaN, which compares false against every bound and so would
// otherwise slip past the range checks below untouched) — a corrupted
// penalty would then feed back into the next tick's Score via
// PriorStateFromAdvertisedBackends, so this must never silently wrap or
// produce garbage, however unlikely an actual overflow or NaN score is
// in practice given maxSuppress already forces a periodic reset.
func clampToInt32(f float64) int32 {
	if math.IsNaN(f) {
		return 0
	}
	rounded := math.Round(f)
	switch {
	case rounded > math.MaxInt32:
		return math.MaxInt32
	case rounded < math.MinInt32:
		return math.MinInt32
	default:
		return int32(rounded)
	}
}

// stateAndReason applies precedence: Rejected (Authorize/CommunityMap)
// outranks anything Damp decided, which in turn outranks Drain (a
// deliberate, community-driven signal, not instability) — Active is the
// default once none of those apply.
func stateAndReason(c pipeline.BackendCandidate) (kregv1alpha1.BackendState, string) {
	if c.Rejected {
		return kregv1alpha1.BackendStateRejected, c.Reason
	}
	if c.Damping != nil {
		switch c.Damping.State {
		case kregv1alpha1.BackendStatePending, kregv1alpha1.BackendStateHoldDown, kregv1alpha1.BackendStateDampened:
			return c.Damping.State, c.Damping.Reason
		}
	}
	if c.Drain {
		return kregv1alpha1.BackendStateDraining, ""
	}
	return kregv1alpha1.BackendStateActive, ""
}

// bindings computes which policies currently select c, and what Render
// would generate for it under each — reusing reconcile.Select and
// Render's own naming helpers so this can never drift from what's
// actually applied to the cluster. A Select error (a malformed prefix
// somewhere) is treated as "doesn't match" rather than propagated: this
// is a best-effort debuggability surface, not the source of truth for
// routing decisions — that's Render's job, which already surfaces the
// same errors properly.
//
// Both results are sorted before returning: policies comes from a
// Kubernetes List, whose ordering isn't guaranteed across calls, and an
// unstable order here would otherwise make AdvertisedBackend status flap
// (bumping LastChange) even when the underlying bindings haven't changed.
func bindings(c pipeline.BackendCandidate, policies []kregv1alpha1.BGPBackendPolicy) ([]string, []string) {
	if c.Rejected {
		return nil, nil
	}

	var boundPolicies, generatedResources []string
	for _, policy := range policies {
		selected, err := reconcile.Select([]pipeline.BackendCandidate{c}, policy.Spec.Selector)
		if err != nil || len(selected) == 0 {
			continue
		}
		boundPolicies = append(boundPolicies, fmt.Sprintf("%s/%s", policy.Namespace, policy.Name))
		serviceName := reconcile.ServiceName(&policy)
		generatedResources = append(generatedResources,
			fmt.Sprintf("EndpointSlice/%s/%s", policy.Namespace, reconcile.EndpointSliceName(serviceName, c.ClusterID, c.Prefix)))
	}
	slices.Sort(boundPolicies)
	slices.Sort(generatedResources)
	return boundPolicies, generatedResources
}

func asPathInt64(asPath []uint32) []int64 {
	if asPath == nil {
		return nil
	}
	out := make([]int64, len(asPath))
	for i, asn := range asPath {
		out[i] = int64(asn)
	}
	return out
}

// objectName delegates to reconcile.BackendObjectName so this can never
// drift from the name internal/damp uses to key prior-tick state.
func objectName(c pipeline.BackendCandidate) string {
	return reconcile.BackendObjectName(c.Prefix, c.ClusterID)
}
