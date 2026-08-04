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

	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
	istiodriver "github.com/phenixblue/kreg/internal/reconcile/istio"
)

// snapshotPollInterval bounds how stale generated resources can get.
// BGP routes change independent of the BGPBackendPolicy object itself —
// there's no watch event for "the RIB changed" — so this periodic
// requeue is what actually keeps Service/EndpointSlice/DestinationRule
// in sync with reality, not just with edits to the policy.
const snapshotPollInterval = 30 * time.Second

// SnapshotSource provides the settled BackendCandidate snapshot the
// reconciler renders against. internal/snapshot.Source is the real
// implementation, chaining Ingest -> Authorize -> Normalize.
type SnapshotSource interface {
	Snapshot(ctx context.Context) ([]pipeline.BackendCandidate, error)
}

// BGPBackendPolicyReconciler reconciles a BGPBackendPolicy object
type BGPBackendPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Snapshot is required — unlike Driver, there's no meaningful
	// zero-value stand-in for real BGP data, so it isn't defaulted in
	// SetupWithManager. Construct one *internal/snapshot.Source per
	// controller process (cmd/main.go), sharing the same *ingest.Manager
	// as BGPPeerConfigReconciler.
	Snapshot SnapshotSource

	// Driver defaults to the Istio driver in SetupWithManager when unset.
	Driver reconcile.Driver
}

// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgpbackendpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgpbackendpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kreg.twr.dev,resources=bgpbackendpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=destinationrules,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders a BGPBackendPolicy's selected backends into
// Kubernetes/Istio objects and applies them. It follows the pipeline
// described in docs/design/architecture.md §1: pull the settled
// candidate snapshot, Render it against the policy, then apply each
// generated object, refusing to overwrite anything this policy doesn't
// already own.
func (r *BGPBackendPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy kregv1alpha1.BGPBackendPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	candidates, err := r.Snapshot.Snapshot(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get snapshot: %w", err)
	}

	output, err := reconcile.Render(&policy, candidates, r.Driver)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render: %w", err)
	}

	generated := make([]string, 0, len(output.Objects()))
	for _, obj := range output.Objects() {
		// Capture Kind before applying: a typed client.Create/Update
		// clears TypeMeta on the object it was given once the call
		// returns, so reading GroupVersionKind() after apply sees a
		// zero-value Kind.
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		if err := r.applyOwned(ctx, policy.Name, obj); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply %s: %w", kind, err)
		}
		generated = append(generated, fmt.Sprintf("%s/%s/%s", kind, obj.GetNamespace(), obj.GetName()))
	}

	if err := r.pruneStaleEndpointSlices(ctx, &policy, output.Service.Name, output.EndpointSlices); err != nil {
		return ctrl.Result{}, fmt.Errorf("prune: %w", err)
	}

	policy.Status.Generated = generated
	policy.Status.ActiveBackends = int32(len(output.EndpointSlices))
	if err := r.Status().Update(ctx, &policy); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	log.Info("reconciled BGPBackendPolicy", "generated", len(generated))
	return ctrl.Result{RequeueAfter: snapshotPollInterval}, nil
}

// applyOwned creates or updates desired, refusing to overwrite a resource
// this policy doesn't already own — see "Rules for the reconciler" in
// docs/design/architecture.md §3.
func (r *BGPBackendPolicyReconciler) applyOwned(ctx context.Context, policyName string, desired client.Object) error {
	current := desired.DeepCopyObject().(client.Object) //nolint:forcetypeassert // desired is always a client.Object
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if owner := current.GetLabels()[reconcile.ManagedByLabel]; owner != "" && owner != policyName {
		return fmt.Errorf("%s/%s is managed by %q, not %q — refusing to overwrite",
			desired.GetObjectKind().GroupVersionKind().Kind, desired.GetName(), owner, policyName)
	}
	desired.SetResourceVersion(current.GetResourceVersion())
	return r.Update(ctx, desired)
}

// pruneStaleEndpointSlices deletes EndpointSlices this policy previously
// generated for clusters no longer selected — e.g. after a route is
// withdrawn. EndpointSlices are the only generated objects whose count
// varies with the candidate set (Service and the driver's objects are
// always exactly one per policy, deterministically named), so nothing
// else needs this treatment today.
func (r *BGPBackendPolicyReconciler) pruneStaleEndpointSlices(ctx context.Context, policy *kregv1alpha1.BGPBackendPolicy, serviceName string, desired []*discoveryv1.EndpointSlice) error {
	desiredNames := make(map[string]bool, len(desired))
	for _, slice := range desired {
		desiredNames[slice.Name] = true
	}

	var existing discoveryv1.EndpointSliceList
	if err := r.List(ctx, &existing,
		client.InNamespace(policy.Namespace),
		client.MatchingLabels{
			reconcile.ManagedByLabel:     policy.Name,
			discoveryv1.LabelServiceName: serviceName,
		},
	); err != nil {
		return fmt.Errorf("list endpoint slices: %w", err)
	}

	for i := range existing.Items {
		slice := &existing.Items[i]
		if desiredNames[slice.Name] {
			continue
		}
		if err := r.Delete(ctx, slice); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale endpoint slice %s: %w", slice.Name, err)
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BGPBackendPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Driver == nil {
		r.Driver = istiodriver.Driver{}
	}
	// GenerationChangedPredicate: our own status write shouldn't
	// re-trigger the watch (redundant with snapshotPollInterval); spec
	// changes still trigger immediately.
	return ctrl.NewControllerManagedBy(mgr).
		For(&kregv1alpha1.BGPBackendPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("bgpbackendpolicy").
		Complete(r)
}
