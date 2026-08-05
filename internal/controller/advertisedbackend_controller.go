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
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	kregreconcile "github.com/phenixblue/kreg/internal/reconcile"
	"github.com/phenixblue/kreg/internal/report"
)

// AdvertisedBackendReconciler reconciles the materialized RIB view —
// the Report pipeline stage (docs/design/architecture.md §1, §2.4).
// There's no single input CR this reconciles 1:1: AdvertisedBackend is a
// function of the whole settled snapshot plus every BGPBackendPolicy, the
// same situation BGPBackendPolicyReconciler is partially in. It watches
// BGPPeerConfig purely as an anchor to wake up promptly on topology
// changes rather than waiting out the full poll interval; Reconcile
// ignores the request's identity and does a global sweep every time.
type AdvertisedBackendReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Snapshot is required — see BGPBackendPolicyReconciler.Snapshot.
	Snapshot SnapshotSource
}

// +kubebuilder:rbac:groups=kreg.twr.dev,resources=advertisedbackends,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=advertisedbackends/status,verbs=get;update;patch

// Reconcile rebuilds the AdvertisedBackend view from the current settled
// snapshot and every BGPBackendPolicy, applies it, and prunes records for
// routes no longer present at all.
func (r *AdvertisedBackendReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	candidates, err := r.Snapshot.Snapshot(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get snapshot: %w", err)
	}

	var policyList kregv1alpha1.BGPBackendPolicyList
	if err := r.List(ctx, &policyList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list BGPBackendPolicies: %w", err)
	}

	desired := report.BuildAdvertisedBackends(candidates, policyList.Items)

	desiredNames := make(map[string]bool, len(desired))
	for i := range desired {
		if err := r.applyBackend(ctx, &desired[i]); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply %s: %w", desired[i].Name, err)
		}
		desiredNames[desired[i].Name] = true
	}

	if err := r.pruneStaleBackends(ctx, desiredNames); err != nil {
		return ctrl.Result{}, fmt.Errorf("prune: %w", err)
	}

	log.Info("reconciled AdvertisedBackend view", "count", len(desired))
	return ctrl.Result{RequeueAfter: snapshotPollInterval}, nil
}

// applyBackend creates or updates desired, preserving FirstSeen across
// reconciles and only bumping LastChange when the backend's condition
// actually differs from what's already stored.
func (r *AdvertisedBackendReconciler) applyBackend(ctx context.Context, desired *kregv1alpha1.AdvertisedBackend) error {
	var current kregv1alpha1.AdvertisedBackend
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &current)
	switch {
	case apierrors.IsNotFound(err):
		bare := &kregv1alpha1.AdvertisedBackend{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Labels: desired.Labels}}
		if err := r.Create(ctx, bare); err != nil {
			return fmt.Errorf("create: %w", err)
		}
		desired.ObjectMeta = bare.ObjectMeta
		now := metav1.Now()
		desired.Status.FirstSeen = &now
		desired.Status.LastChange = &now
		return r.Status().Update(ctx, desired)
	case err != nil:
		return fmt.Errorf("get: %w", err)
	default:
		desired.ObjectMeta = current.ObjectMeta
		desired.Status.FirstSeen = current.Status.FirstSeen
		if statusChanged(current.Status, desired.Status) {
			now := metav1.Now()
			desired.Status.LastChange = &now
		} else {
			desired.Status.LastChange = current.Status.LastChange
		}
		return r.Status().Update(ctx, desired)
	}
}

// statusChanged reports whether anything but continuously-updating
// bookkeeping differs between old and new. Stability.LastObservedAt
// bumps on every reconcile (Damp re-evaluates every candidate each
// tick); Stability.DampeningPenalty and .FlapCount24h decay a little on
// every tick too, even with zero flaps, as long as any residual score
// hasn't fully decayed to zero.
//
// Reason is excluded only for the specific states whose Reason text
// itself embeds a continuously-drifting value — HoldDown's "withdrawn
// Xs ago" (X grows every tick) and Dampened's "score X" (X decays every
// tick) — via reasonDrifts. Every other state's Reason stays in the
// comparison: Pending's is a fixed function of additionDelay (same text
// every tick unless the config itself changes, which should bump
// LastChange), and Rejected's describes a stable condition (e.g. which
// allowedPrefixes rule excluded it) that can genuinely change — e.g. a
// clusterBindings edit — while the candidate stays Rejected throughout,
// which is exactly the kind of change an operator needs reflected.
// WithdrawnAt/SuppressedSince/PendingSince stay in the comparison too:
// those only change at real transition points.
func statusChanged(old, latest kregv1alpha1.AdvertisedBackendStatus) bool {
	old.FirstSeen, old.LastChange = nil, nil
	if reasonDrifts(old.State) {
		old.Reason = ""
	}
	old.Stability.LastObservedAt, old.Stability.DampeningPenalty, old.Stability.FlapCount24h = nil, 0, 0

	latest.FirstSeen, latest.LastChange = nil, nil
	if reasonDrifts(latest.State) {
		latest.Reason = ""
	}
	latest.Stability.LastObservedAt, latest.Stability.DampeningPenalty, latest.Stability.FlapCount24h = nil, 0, 0

	return !reflect.DeepEqual(old, latest)
}

// reasonDrifts reports whether state's own Reason text (see
// internal/damp/ewma) embeds a value that changes on essentially every
// tick regardless of whether anything semantic changed.
func reasonDrifts(state kregv1alpha1.BackendState) bool {
	switch state {
	case kregv1alpha1.BackendStateHoldDown, kregv1alpha1.BackendStateDampened:
		return true
	default:
		return false
	}
}

// pruneStaleBackends deletes AdvertisedBackend records for routes no
// longer in the settled snapshot at all — not merely rejected, which
// still produces a record (state: Rejected), but genuinely gone. Listing
// is scoped to report.ManagedByValue, the label every AdvertisedBackend
// this reconciler creates carries, so an object of this kind this
// reconciler didn't create is never a deletion candidate.
func (r *AdvertisedBackendReconciler) pruneStaleBackends(ctx context.Context, desiredNames map[string]bool) error {
	var existing kregv1alpha1.AdvertisedBackendList
	if err := r.List(ctx, &existing, client.MatchingLabels{kregreconcile.ManagedByLabel: report.ManagedByValue}); err != nil {
		return fmt.Errorf("list: %w", err)
	}
	for i := range existing.Items {
		backend := &existing.Items[i]
		if desiredNames[backend.Name] {
			continue
		}
		if err := r.Delete(ctx, backend); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale advertisedbackend %s: %w", backend.Name, err)
		}
	}
	return nil
}

// enqueueGlobalSweep triggers a reconcile regardless of which object
// changed — Reconcile ignores request identity and always recomputes the
// whole view, so any request value works as the trigger.
func enqueueGlobalSweep(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *AdvertisedBackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kregv1alpha1.BGPPeerConfig{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// BoundPolicies/GeneratedResources depend on every BGPBackendPolicy,
		// not just BGPPeerConfig — without this, a policy create/update/delete
		// wouldn't be reflected until the next snapshotPollInterval sweep.
		Watches(&kregv1alpha1.BGPBackendPolicy{}, handler.EnqueueRequestsFromMapFunc(enqueueGlobalSweep),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("advertisedbackend").
		Complete(r)
}
