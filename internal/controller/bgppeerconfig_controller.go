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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/ingest"
)

// PeerManager is the subset of *ingest.Manager this reconciler needs,
// narrowed to an interface so tests don't need a real GoBGP server.
type PeerManager interface {
	Reconfigure(ctx context.Context, spec *kregv1alpha1.BGPPeerConfigSpec) error
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
}

// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgppeerconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgppeerconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgppeerconfigs/finalizers,verbs=update

// Reconcile converges the live BGP peer set to spec and reports observed
// session state.
func (r *BGPPeerConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var peerConfig kregv1alpha1.BGPPeerConfig
	if err := r.Get(ctx, req.NamespacedName, &peerConfig); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.Manager.Reconfigure(ctx, &peerConfig.Spec); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconfigure: %w", err)
	}

	statuses, err := r.Manager.Status(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("status: %w", err)
	}

	peerConfig.Status.Peers = statuses
	if err := r.Status().Update(ctx, &peerConfig); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	log.Info("reconciled BGPPeerConfig", "peers", len(statuses))
	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BGPPeerConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kregv1alpha1.BGPPeerConfig{}).
		Named("bgppeerconfig").
		Complete(r)
}
