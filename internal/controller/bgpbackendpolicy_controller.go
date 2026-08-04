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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
	istiodriver "github.com/phenixblue/kreg/internal/reconcile/istio"
)

// SnapshotSource provides the settled BackendCandidate snapshot the
// reconciler renders against. Build-order step 2 (GoBGP ingest) replaces
// the static stub this package defaults to with a real implementation
// backed by a live RIB; nothing else about the reconciler changes.
type SnapshotSource interface {
	Snapshot(ctx context.Context) ([]pipeline.BackendCandidate, error)
}

// staticSnapshotSource is the step-1 stand-in: no BGP ingest exists yet,
// so every policy reconciles against an empty settled snapshot.
type staticSnapshotSource struct{}

func (staticSnapshotSource) Snapshot(context.Context) ([]pipeline.BackendCandidate, error) {
	return nil, nil
}

// BGPBackendPolicyReconciler reconciles a BGPBackendPolicy object
type BGPBackendPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Snapshot and Driver default to the step-1 stand-ins (no candidates,
	// the Istio driver) in SetupWithManager when unset.
	Snapshot SnapshotSource
	Driver   reconcile.Driver
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

	policy.Status.Generated = generated
	policy.Status.ActiveBackends = int32(len(output.EndpointSlices))
	if err := r.Status().Update(ctx, &policy); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	log.Info("reconciled BGPBackendPolicy", "generated", len(generated))
	return ctrl.Result{}, nil
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

// SetupWithManager sets up the controller with the Manager.
func (r *BGPBackendPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Snapshot == nil {
		r.Snapshot = staticSnapshotSource{}
	}
	if r.Driver == nil {
		r.Driver = istiodriver.Driver{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kregv1alpha1.BGPBackendPolicy{}).
		Named("bgpbackendpolicy").
		Complete(r)
}
