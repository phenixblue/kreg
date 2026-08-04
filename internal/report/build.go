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
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
)

// unattributedClusterID names an AdvertisedBackend whose route Authorize
// rejected before any cluster could be attributed — ClusterID is empty
// in that case, and an empty name segment isn't a usable object name.
const unattributedClusterID = "unattributed"

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

	return kregv1alpha1.AdvertisedBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name:   objectName(c),
			Labels: map[string]string{reconcile.ManagedByLabel: ManagedByValue},
		},
		Status: kregv1alpha1.AdvertisedBackendStatus{
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
		},
	}
}

func stateAndReason(c pipeline.BackendCandidate) (kregv1alpha1.BackendState, string) {
	if c.Rejected {
		return kregv1alpha1.BackendStateRejected, c.Reason
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

// objectName matches docs/design/architecture.md §2.4's convention:
// "198.51.100.10/32" + "atl-1" -> "198-51-100-10-32-atl-1".
func objectName(c pipeline.BackendCandidate) string {
	clusterID := c.ClusterID
	if clusterID == "" {
		clusterID = unattributedClusterID
	}
	return reconcile.SanitizeForName(c.Prefix) + "-" + clusterID
}
