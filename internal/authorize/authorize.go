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

// Package authorize implements the Authorize pipeline stage: the
// security boundary between raw ingested routes and everything else. See
// docs/design/architecture.md §1.
package authorize

import (
	"fmt"
	"net"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
)

// Authorize trusts a route only if its prefix falls within a binding's
// allowedPrefixes, and attributes the origin cluster and locality from
// whichever binding matched — never from anything the peer itself
// asserts (RIBRoute.ClusterID and RIBRoute.Locality on the input are
// ignored and overwritten). Routes matching no binding are kept but
// flagged Rejected, not dropped — internal/reconcile.Select already
// excludes any Rejected candidate from real backend selection, but the
// Report stage (AdvertisedBackend) needs to see why a route never made
// it, not just its absence.
//
// Through a route reflector, the advertising peer address is always the
// RR's, so prefix->cluster via allowedPrefixes containment is the only
// trustworthy signal — see the "Attribution note" in
// docs/design/architecture.md §2.1.
func Authorize(routes []pipeline.RIBRoute, bindings []kregv1alpha1.ClusterBinding) ([]pipeline.RIBRoute, error) {
	authorized := make([]pipeline.RIBRoute, 0, len(routes))
	for _, route := range routes {
		binding, err := matchBinding(route.Prefix, bindings)
		if err != nil {
			return nil, fmt.Errorf("authorize %s: %w", route.Prefix, err)
		}
		if binding == nil {
			route.Rejected = true
			route.Reason = fmt.Sprintf("prefix %s not in allowedPrefixes for any cluster", route.Prefix)
			route.ClusterID = ""
			route.Locality = pipeline.Locality{}
			authorized = append(authorized, route)
			continue
		}
		route.ClusterID = binding.ClusterID
		route.Locality = pipeline.Locality{Region: binding.Locality.Region, Zone: binding.Locality.Zone}
		authorized = append(authorized, route)
	}
	return authorized, nil
}

// matchBinding returns the first binding whose allowedPrefixes contains
// prefix, or nil if none do.
func matchBinding(prefix string, bindings []kregv1alpha1.ClusterBinding) (*kregv1alpha1.ClusterBinding, error) {
	ip, _, err := net.ParseCIDR(prefix)
	if err != nil {
		return nil, fmt.Errorf("parse route prefix %q: %w", prefix, err)
	}
	for i := range bindings {
		for _, allowed := range bindings[i].AllowedPrefixes {
			_, network, err := net.ParseCIDR(allowed)
			if err != nil {
				return nil, fmt.Errorf("parse allowedPrefixes %q for cluster %q: %w", allowed, bindings[i].ClusterID, err)
			}
			if network.Contains(ip) {
				return &bindings[i], nil
			}
		}
	}
	return nil, nil
}
