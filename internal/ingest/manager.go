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

package ingest

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	api "github.com/osrg/gobgp/v3/api"
	gobgpserver "github.com/osrg/gobgp/v3/pkg/server"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
)

// Manager owns one live GoBGP session set and exposes it as a RIB. One
// Manager is constructed per controller process (cmd/main.go) and shared
// between BGPPeerConfigReconciler, which converges it to spec, and the
// BGPBackendPolicy snapshot source, which reads its current table.
//
// Scope for build-order step 2: RouteReflectorClient sessions, IPv4
// unicast only. Deferred: Passive/BMPCollector modes, RPKI, IPv6.
type Manager struct {
	server  *gobgpserver.BgpServer
	started bool
}

// NewManager constructs a Manager around a fresh, unstarted GoBGP
// server. Call Serve in a goroutine before Reconfigure.
func NewManager() *Manager {
	return &Manager{server: gobgpserver.NewBgpServer()}
}

// Serve runs the server's event loop until Stop is called.
func (m *Manager) Serve() {
	m.server.Serve()
}

// Stop shuts the server down.
func (m *Manager) Stop() {
	m.server.Stop()
}

// Reconfigure converges the live peer set to spec: starts BGP on the
// first call, then adds, updates, or removes peers to match spec.Peers.
// passwords carries each auth-configured peer's resolved TCP-MD5
// password, keyed by peer Name — the caller (BGPPeerConfigReconciler) has
// already resolved any Auth.TCPMD5SecretRef, since that requires a k8s
// client this package deliberately doesn't have.
func (m *Manager) Reconfigure(ctx context.Context, spec *kregv1alpha1.BGPPeerConfigSpec, passwords map[string]string) error {
	if !m.started {
		listenPort := int32(179)
		if spec.ListenPort != nil {
			listenPort = *spec.ListenPort
		}
		if err := m.server.StartBgp(ctx, &api.StartBgpRequest{
			Global: &api.Global{
				Asn:        uint32(spec.LocalASN),
				RouterId:   spec.RouterID,
				ListenPort: listenPort,
			},
		}); err != nil {
			return fmt.Errorf("start bgp: %w", err)
		}
		m.started = true
	}

	existing := map[string]bool{}
	if err := m.server.ListPeer(ctx, &api.ListPeerRequest{}, func(p *api.Peer) {
		existing[p.Conf.NeighborAddress] = true
	}); err != nil {
		return fmt.Errorf("list peers: %w", err)
	}

	desired := map[string]bool{}
	for _, peer := range spec.Peers {
		// existing is keyed by GoBGP's own NeighborAddress, which is
		// always host-only (toAPIPeer strips any ":port" suffix into
		// Transport.RemotePort) — key desired the same way, or a peer
		// configured with the ":port" form (non-standard deployments,
		// loopback tests) would never match its existing entry and get
		// deleted-then-re-added on every reconcile.
		host, _ := splitHostPort(peer.Address)
		desired[host] = true
		apiPeer := toAPIPeer(peer, passwords[peer.Name])
		if existing[host] {
			if _, err := m.server.UpdatePeer(ctx, &api.UpdatePeerRequest{Peer: apiPeer}); err != nil {
				return fmt.Errorf("update peer %s: %w", peer.Name, err)
			}
			continue
		}
		if err := m.server.AddPeer(ctx, &api.AddPeerRequest{Peer: apiPeer}); err != nil {
			return fmt.Errorf("add peer %s: %w", peer.Name, err)
		}
	}

	for addr := range existing {
		if !desired[addr] {
			if err := m.server.DeletePeer(ctx, &api.DeletePeerRequest{Address: addr}); err != nil {
				return fmt.Errorf("delete peer %s: %w", addr, err)
			}
		}
	}

	return nil
}

// toAPIPeer builds GoBGP's peer config from spec. password is the
// resolved TCP-MD5 secret for this peer (empty if peer.Auth is unset —
// resolution happens in BGPPeerConfigReconciler, which has the k8s
// client this package doesn't).
func toAPIPeer(peer kregv1alpha1.BGPPeer, password string) *api.Peer {
	address, remotePort := splitHostPort(peer.Address)
	apiPeer := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: address,
			PeerAsn:         uint32(peer.RemoteASN),
			AuthPassword:    password,
		},
		Transport: &api.Transport{RemotePort: uint32(remotePort)},
		AfiSafis: []*api.AfiSafi{{
			Config: &api.AfiSafiConfig{
				Family:  &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST},
				Enabled: true,
			},
		}},
	}

	if peer.Timers != nil {
		cfg := &api.TimersConfig{}
		if peer.Timers.Hold != nil {
			cfg.HoldTime = uint64(peer.Timers.Hold.Seconds())
		}
		if peer.Timers.Keepalive != nil {
			cfg.KeepaliveInterval = uint64(peer.Timers.Keepalive.Seconds())
		}
		apiPeer.Timers = &api.Timers{Config: cfg}
	}

	if peer.GracefulRestart != nil {
		gr := &api.GracefulRestart{Enabled: peer.GracefulRestart.Enabled}
		if peer.GracefulRestart.StaleRoutesTime != nil {
			gr.StaleRoutesTime = uint32(peer.GracefulRestart.StaleRoutesTime.Seconds())
		}
		apiPeer.GracefulRestart = gr
	}

	return apiPeer
}

// splitHostPort splits an optional ":port" suffix off a peer address,
// defaulting to the standard BGP port when absent. A bare IP (the common
// case — real route reflectors listen on 179) is returned unchanged; the
// ":port" form exists for non-standard deployments and local testing.
func splitHostPort(address string) (string, int32) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return address, 179
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return address, 179
	}
	return host, int32(port)
}

// Status reports the observed state of every configured peer.
func (m *Manager) Status(ctx context.Context) ([]kregv1alpha1.PeerStatus, error) {
	var statuses []kregv1alpha1.PeerStatus
	err := m.server.ListPeer(ctx, &api.ListPeerRequest{}, func(p *api.Peer) {
		statuses = append(statuses, toPeerStatus(p))
	})
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	return statuses, nil
}

func toPeerStatus(p *api.Peer) kregv1alpha1.PeerStatus {
	status := kregv1alpha1.PeerStatus{Name: p.Conf.NeighborAddress}
	if p.State != nil {
		status.SessionState = sessionState(p.State.SessionState)
	}
	if p.Timers != nil && p.Timers.State != nil && p.Timers.State.Uptime != nil {
		status.Uptime = &metav1.Duration{Duration: time.Since(p.Timers.State.Uptime.AsTime())}
	}
	for _, afiSafi := range p.AfiSafis {
		if afiSafi.State == nil {
			continue
		}
		status.PrefixesReceived += int32(afiSafi.State.Received)
		status.PrefixesAccepted += int32(afiSafi.State.Accepted)
	}
	return status
}

func sessionState(s api.PeerState_SessionState) kregv1alpha1.PeerSessionState {
	switch s {
	case api.PeerState_CONNECT:
		return kregv1alpha1.PeerSessionStateConnect
	case api.PeerState_ACTIVE:
		return kregv1alpha1.PeerSessionStateActive
	case api.PeerState_OPENSENT:
		return kregv1alpha1.PeerSessionStateOpenSent
	case api.PeerState_OPENCONFIRM:
		return kregv1alpha1.PeerSessionStateOpenConfirm
	case api.PeerState_ESTABLISHED:
		return kregv1alpha1.PeerSessionStateEstablished
	default:
		return kregv1alpha1.PeerSessionStateIdle
	}
}

// Snapshot implements RIB: the current IPv4 unicast best-path table.
func (m *Manager) Snapshot(ctx context.Context) ([]pipeline.RIBRoute, error) {
	var routes []pipeline.RIBRoute
	var decodeErr error

	err := m.server.ListPath(ctx, &api.ListPathRequest{
		TableType: api.TableType_GLOBAL,
		Family:    &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST},
	}, func(d *api.Destination) {
		if decodeErr != nil {
			return
		}
		route, err := decodeDestination(d)
		if err != nil {
			decodeErr = err
			return
		}
		if route != nil {
			routes = append(routes, *route)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("list path: %w", err)
	}
	if decodeErr != nil {
		return nil, decodeErr
	}
	return routes, nil
}
