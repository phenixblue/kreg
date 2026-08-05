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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/authorize"
	"github.com/phenixblue/kreg/internal/ingest"
)

// PeerManager is the subset of *ingest.Manager this reconciler needs,
// narrowed to an interface so tests don't need a real GoBGP server.
// passwords carries each auth-configured peer's resolved TCP-MD5
// password, keyed by peer Name — resolving the Secret is the
// reconciler's job (it has the k8s client), not Manager's.
type PeerManager interface {
	Reconfigure(ctx context.Context, spec *kregv1alpha1.BGPPeerConfigSpec, passwords map[string]string) error
	Status(ctx context.Context) ([]kregv1alpha1.PeerStatus, error)
}

var _ PeerManager = (*ingest.Manager)(nil)

// statusPollInterval is how often the reconciler re-reads session state.
// BGP sessions change state asynchronously (network events, not spec
// changes), so there's no watch event to key a requeue off; this is a
// plain poll.
const statusPollInterval = 30 * time.Second

// BGPPeerConfigReconciler reconciles a BGPPeerConfig object
type BGPPeerConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Manager owns the live GoBGP session set. Required — unlike
	// BGPBackendPolicyReconciler's Snapshot/Driver, there's no
	// meaningful zero-value stand-in for a running BGP daemon, so this
	// isn't defaulted in SetupWithManager. Construct one *ingest.Manager
	// per controller process (cmd/main.go) and share it with the
	// BGPBackendPolicy snapshot source that reads its RIB.
	Manager PeerManager

	// Namespace is where BGPPeerAuth.tcpMD5SecretRef resolves against.
	// BGPPeerConfig is cluster-scoped, so there's no "same namespace as
	// the CR" to fall back on — secrets must live wherever the controller
	// itself runs (cmd/main.go sets this from the downward-API
	// POD_NAMESPACE env var). Only required if some peer actually sets an
	// auth ref; harmless zero-value otherwise.
	Namespace string

	// Tracker supplies PeerStatus.PrefixesRejected — populated by
	// snapshot.Source's separate reconcile loop, since that's where
	// Authorize actually runs. Optional: nil just leaves
	// PrefixesRejected unreported.
	Tracker *authorize.RejectionTracker
}

// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgppeerconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgppeerconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgppeerconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get,namespace=system

// Reconcile converges the live BGP peer set to spec and reports observed
// session state.
func (r *BGPPeerConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var peerConfig kregv1alpha1.BGPPeerConfig
	if err := r.Get(ctx, req.NamespacedName, &peerConfig); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	passwords, err := r.resolvePasswords(ctx, peerConfig.Spec.Peers)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve peer auth: %w", err)
	}

	if err := r.Manager.Reconfigure(ctx, &peerConfig.Spec, passwords); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconfigure: %w", err)
	}

	statuses, err := r.Manager.Status(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("status: %w", err)
	}
	if r.Tracker != nil {
		for i := range statuses {
			statuses[i].PrefixesRejected = r.Tracker.Get(statuses[i].Name)
		}
	}

	peerConfig.Status.Peers = statuses
	if err := r.Status().Update(ctx, &peerConfig); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	log.Info("reconciled BGPPeerConfig", "peers", len(statuses))
	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// resolvePasswords reads each auth-configured peer's TCP-MD5 password
// from a Secret in r.Namespace, returning a peer-name -> plaintext
// password map (peers with no Auth.TCPMD5SecretRef are simply absent
// from it). A missing Secret or key is a hard error unless the ref is
// marked Optional: a BGP session silently coming up unauthenticated
// because its intended password couldn't be resolved would be a security
// regression, not a degradation worth tolerating quietly.
func (r *BGPPeerConfigReconciler) resolvePasswords(ctx context.Context, peers []kregv1alpha1.BGPPeer) (map[string]string, error) {
	passwords := map[string]string{}
	for _, peer := range peers {
		if peer.Auth == nil || peer.Auth.TCPMD5SecretRef == nil {
			continue
		}
		if r.Namespace == "" {
			return nil, fmt.Errorf("peer %s: tcpMD5SecretRef is set but no namespace is configured to resolve it against (POD_NAMESPACE not set?)", peer.Name)
		}
		ref := peer.Auth.TCPMD5SecretRef
		optional := ref.Optional != nil && *ref.Optional

		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: ref.Name}, &secret); err != nil {
			if apierrors.IsNotFound(err) && optional {
				continue
			}
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("peer %s: secret %s/%s not found", peer.Name, r.Namespace, ref.Name)
			}
			return nil, fmt.Errorf("peer %s: get secret %s/%s: %w", peer.Name, r.Namespace, ref.Name, err)
		}

		password, ok := secret.Data[ref.Key]
		if !ok {
			if optional {
				continue
			}
			return nil, fmt.Errorf("peer %s: key %q not found in secret %s/%s", peer.Name, ref.Key, r.Namespace, ref.Name)
		}
		passwords[peer.Name] = string(password)
	}
	return passwords, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BGPPeerConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// GenerationChangedPredicate: status.peers[].uptime changes on every
	// reconcile (it's elapsed time), so without this the watch would
	// re-trigger on our own status write, forever — spec changes still
	// trigger immediately; status-only refresh stays on
	// statusPollInterval.
	return ctrl.NewControllerManagedBy(mgr).
		For(&kregv1alpha1.BGPPeerConfig{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("bgppeerconfig").
		Complete(r)
}
