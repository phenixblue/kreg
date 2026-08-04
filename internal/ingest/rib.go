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

// Package ingest is the Ingest pipeline stage: GoBGP embedded as a
// library, behind a Go interface so tests don't need a daemon. See
// docs/design/architecture.md §1.
package ingest

import (
	"context"
	"fmt"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/apiutil"
	"github.com/osrg/gobgp/v3/pkg/packet/bgp"

	"github.com/phenixblue/kreg/internal/pipeline"
)

// RIB is the ingested BGP routing table. ClusterID and Locality are left
// zero-valued here — Authorize (internal/authorize), not Ingest,
// attributes those, purely from prefix position.
type RIB interface {
	Snapshot(ctx context.Context) ([]pipeline.RIBRoute, error)
}

// decodeDestination turns one api.Destination — a prefix and its known
// paths — into a RIBRoute from its best path, or nil if the destination
// has no usable path.
func decodeDestination(dest *api.Destination) (*pipeline.RIBRoute, error) {
	path := bestPath(dest.Paths)
	if path == nil {
		return nil, nil
	}
	route, err := decodePath(path)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", dest.Prefix, err)
	}
	route.Prefix = dest.Prefix
	return &route, nil
}

func bestPath(paths []*api.Path) *api.Path {
	for _, p := range paths {
		if p.Best {
			return p
		}
	}
	if len(paths) > 0 {
		return paths[0]
	}
	return nil
}

// decodePath decodes a path's attributes into a RIBRoute. Prefix is left
// unset — the caller fills it from the owning Destination.
func decodePath(path *api.Path) (pipeline.RIBRoute, error) {
	route := pipeline.RIBRoute{Peer: path.NeighborIp}

	for _, attr := range path.Pattrs {
		native, err := apiutil.UnmarshalAttribute(attr)
		if err != nil {
			return pipeline.RIBRoute{}, fmt.Errorf("unmarshal attribute: %w", err)
		}
		switch a := native.(type) {
		case *bgp.PathAttributeMultiExitDisc:
			route.MED = a.Value
		case *bgp.PathAttributeAsPath:
			route.ASPath = decodeASPath(a)
		case *bgp.PathAttributeLargeCommunities:
			route.LargeCommunities = decodeLargeCommunities(a)
		}
	}
	return route, nil
}

// decodeASPath flattens AS-PATH segments into a single ASN list. Modern
// sessions (including any two GoBGP speakers, or any peer that
// negotiates the 4-byte ASN capability — the default) carry
// *bgp.As4PathParam segments; *bgp.AsPathParam (2-byte ASNs) is handled
// too, for peers that don't.
func decodeASPath(a *bgp.PathAttributeAsPath) []uint32 {
	var asns []uint32
	for _, param := range a.Value {
		switch p := param.(type) {
		case *bgp.As4PathParam:
			asns = append(asns, p.AS...)
		case *bgp.AsPathParam:
			for _, as16 := range p.AS {
				asns = append(asns, uint32(as16))
			}
		}
	}
	return asns
}

func decodeLargeCommunities(a *bgp.PathAttributeLargeCommunities) []string {
	communities := make([]string, 0, len(a.Values))
	for _, c := range a.Values {
		communities = append(communities, fmt.Sprintf("%d:%d:%d", c.ASN, c.LocalData1, c.LocalData2))
	}
	return communities
}
