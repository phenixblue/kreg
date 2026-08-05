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

package authorize

import (
	"sync"

	"github.com/phenixblue/kreg/internal/pipeline"
)

// RejectedCountsByPeer groups Authorize's output by the peer a route was
// learned from, counting only the ones flagged Rejected — the
// authorization-level counterpart to PeerStatus.PrefixesReceived/
// PrefixesAccepted, which come straight from GoBGP's own session state
// and know nothing about ClusterBindings.
func RejectedCountsByPeer(routes []pipeline.RIBRoute) map[string]int32 {
	counts := map[string]int32{}
	for _, route := range routes {
		if route.Rejected {
			counts[route.Peer]++
		}
	}
	return counts
}

// RejectionTracker hands Authorize's per-peer rejection counts from the
// BGPBackendPolicy reconcile loop (where Authorize actually runs, inside
// Source.Snapshot) to the separate BGPPeerConfig reconcile loop, which
// owns PeerStatus.PrefixesRejected but never calls Authorize itself. Like
// PrefixesReceived/PrefixesAccepted, this is a live gauge, not durable
// state — a restart just resets it to empty until the next Snapshot
// tick, which is fine for something that's observability, not behavior.
type RejectionTracker struct {
	mu     sync.Mutex
	counts map[string]int32
}

// Set replaces the tracked counts with the latest snapshot's.
func (t *RejectionTracker) Set(counts map[string]int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts = counts
}

// Get returns the most recently tracked rejection count for peer, or 0
// if none has been recorded yet.
func (t *RejectionTracker) Get(peer string) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[peer]
}
