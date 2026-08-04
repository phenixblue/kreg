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

// Package snapshot chains Ingest -> Authorize -> Normalize into a single
// settled-candidate source, implementing
// controller.BGPBackendPolicyReconciler's SnapshotSource seam with real
// BGP data. See docs/design/architecture.md §1.
package snapshot

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/authorize"
	"github.com/phenixblue/kreg/internal/damp"
	"github.com/phenixblue/kreg/internal/ingest"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
	"github.com/phenixblue/kreg/internal/report"
)

// communityMapName is the conventional name for the (typically singular)
// CommunityMap object, matching docs/design/architecture.md §2.2's
// worked example. A cluster with no CommunityMap yet is a valid,
// unremarkable state — Normalize falls back to defaults, same as it
// does for any route with no matching rule.
const communityMapName = "default"

// stabilityConfigName is BGPStabilityConfig's conventional singular
// name, same convention as communityMapName. A cluster with no
// BGPStabilityConfig yet is equally unremarkable — Damp evaluates
// against a zero-value BGPStabilityConfigSpec, which reproduces
// pre-Damper behavior (no hold-down grace, no addition delay, dampening
// disabled).
const stabilityConfigName = "default"

// +kubebuilder:rbac:groups=kreg.twr.dev,resources=communitymaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgpstabilityconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=advertisedbackends,verbs=get;list;watch

// Source implements controller.SnapshotSource against real BGP data:
// pull the current RIB, authorize it against every BGPPeerConfig's
// clusterBindings, normalize against the CommunityMap, then damp against
// each candidate's prior AdvertisedBackend state.
type Source struct {
	Client client.Client
	RIB    ingest.RIB
	Damper damp.Damper
}

// Snapshot implements controller.SnapshotSource.
func (s *Source) Snapshot(ctx context.Context) ([]pipeline.BackendCandidate, error) {
	routes, err := s.RIB.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingest: %w", err)
	}

	var peerConfigs kregv1alpha1.BGPPeerConfigList
	if err := s.Client.List(ctx, &peerConfigs); err != nil {
		return nil, fmt.Errorf("list BGPPeerConfigs: %w", err)
	}
	var bindings []kregv1alpha1.ClusterBinding
	for _, pc := range peerConfigs.Items {
		bindings = append(bindings, pc.Spec.ClusterBindings...)
	}

	authorized, err := authorize.Authorize(routes, bindings)
	if err != nil {
		return nil, fmt.Errorf("authorize: %w", err)
	}

	var communityMap kregv1alpha1.CommunityMap
	var cm *kregv1alpha1.CommunityMapSpec
	switch err := s.Client.Get(ctx, client.ObjectKey{Name: communityMapName}, &communityMap); {
	case err == nil:
		cm = &communityMap.Spec
	case apierrors.IsNotFound(err):
		// no CommunityMap yet — Normalize handles a nil one.
	default:
		return nil, fmt.Errorf("get CommunityMap %q: %w", communityMapName, err)
	}

	candidates, err := pipeline.Normalize(authorized, cm)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}

	var stabilityConfig kregv1alpha1.BGPStabilityConfig
	switch err := s.Client.Get(ctx, client.ObjectKey{Name: stabilityConfigName}, &stabilityConfig); {
	case err == nil:
	case apierrors.IsNotFound(err):
		// no BGPStabilityConfig yet — Evaluate handles a zero-value spec.
	default:
		return nil, fmt.Errorf("get BGPStabilityConfig %q: %w", stabilityConfigName, err)
	}

	var existing kregv1alpha1.AdvertisedBackendList
	if err := s.Client.List(ctx, &existing,
		client.MatchingLabels{reconcile.ManagedByLabel: report.ManagedByValue}); err != nil {
		return nil, fmt.Errorf("list AdvertisedBackends: %w", err)
	}
	prior := damp.PriorStateFromAdvertisedBackends(existing.Items)

	return s.Damper.Evaluate(time.Now(), candidates, prior, stabilityConfig.Spec), nil
}
