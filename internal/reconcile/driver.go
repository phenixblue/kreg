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

// Package reconcile implements the Reconcile stage's pure core: turning a
// settled snapshot of BackendCandidates plus a BGPBackendPolicy into the
// desired Kubernetes objects. See docs/design/architecture.md §3.
package reconcile

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
)

// ManagedByLabel marks every resource KREG generates, so the reconciler can
// find its own output and never patch a resource it doesn't own.
const ManagedByLabel = "kreg.twr.dev/managed-by"

// Driver lowers a BGPBackendPolicy's traffic-policy fields (load
// balancing, outlier detection, backend TLS) into implementation-specific
// objects. Backend identity — the Service and EndpointSlices Render builds
// directly — is portable Kubernetes and never goes through a Driver.
//
// v1 ships one Driver, Istio (internal/reconcile/istio). A second
// implementation (Envoy Gateway, build-order step 7) plugs in here without
// changing Render.
type Driver interface {
	// Lower produces the traffic-policy object(s) for the backends selected
	// by policy, addressed at host (the portable Service's cluster-local
	// DNS name).
	Lower(policy *kregv1alpha1.BGPBackendPolicy, candidates []pipeline.BackendCandidate, host string) ([]client.Object, error)
}
