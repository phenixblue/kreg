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

// Package istio implements KREG's v1 reconcile.Driver, lowering
// BGPBackendPolicy traffic policy into Istio's DestinationRule. See
// docs/design/architecture.md §3.
package istio

import (
	"fmt"

	istioapi "istio.io/api/networking/v1alpha3"
	networkingv1 "istio.io/client-go/pkg/apis/networking/v1"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
)

// Driver is KREG's v1 default reconcile.Driver.
type Driver struct{}

var _ reconcile.Driver = Driver{}

// Lower produces the DestinationRule for the backends selected by policy.
// candidates is accepted to satisfy the Driver interface but unused here:
// v1 doesn't express per-backend weight through DestinationRule (weight
// isn't expressible on a core EndpointSlice endpoint either — see
// docs/design/architecture.md §3).
func (Driver) Lower(policy *kregv1alpha1.BGPBackendPolicy, _ []pipeline.BackendCandidate, host string) ([]client.Object, error) {
	tls, err := clientTLSSettings(policy.Spec.Backend.TLS)
	if err != nil {
		return nil, err
	}

	dr := &networkingv1.DestinationRule{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.istio.io/v1", Kind: "DestinationRule"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      policy.Name + "-kreg",
			Namespace: policy.Namespace,
			Labels:    map[string]string{reconcile.ManagedByLabel: policy.Name},
		},
		Spec: istioapi.DestinationRule{
			Host: host,
			TrafficPolicy: &istioapi.TrafficPolicy{
				Tls:              tls,
				LoadBalancer:     loadBalancerSettings(policy.Spec.LoadBalancing),
				OutlierDetection: outlierDetection(policy.Spec.OutlierDetection),
			},
		},
	}
	return []client.Object{dr}, nil
}

// clientTLSSettings only implements BackendTLSModeSimple: Passthrough
// bypasses DestinationRule entirely (it emits a TLSRoute instead — the
// fast-follow noted in BGPBackendPolicy.spec.backend.tls.mode's roadmap),
// and Mutual is deferred. Rather than silently mismodel either, this
// fails loudly until they're actually implemented.
func clientTLSSettings(tls kregv1alpha1.BackendTLSConfig) (*istioapi.ClientTLSSettings, error) {
	switch tls.Mode {
	case kregv1alpha1.BackendTLSModeSimple, "":
		settings := &istioapi.ClientTLSSettings{
			Mode: istioapi.ClientTLSSettings_SIMPLE,
			Sni:  tls.SNI,
		}
		if tls.CredentialRef != nil {
			settings.CredentialName = tls.CredentialRef.Name
		}
		return settings, nil
	default:
		return nil, fmt.Errorf("backend.tls.mode %q is not yet implemented by the Istio driver (only SIMPLE ships in v1; see docs/design/architecture.md §2.3)", tls.Mode)
	}
}

func loadBalancerSettings(lb kregv1alpha1.LoadBalancingConfig) *istioapi.LoadBalancerSettings {
	if lb.Locality == nil || len(lb.Locality.Preference) < 2 {
		return nil
	}
	return &istioapi.LoadBalancerSettings{
		LocalityLbSetting: &istioapi.LocalityLoadBalancerSetting{
			Failover: failoverChain(lb.Locality.Preference),
		},
	}
}

// failoverChain turns an ordered locality preference into consecutive
// failover pairs: [a, b, c] -> a->b, b->c.
func failoverChain(preference []string) []*istioapi.LocalityLoadBalancerSetting_Failover {
	chain := make([]*istioapi.LocalityLoadBalancerSetting_Failover, 0, len(preference)-1)
	for i := 0; i < len(preference)-1; i++ {
		chain = append(chain, &istioapi.LocalityLoadBalancerSetting_Failover{
			From: preference[i],
			To:   preference[i+1],
		})
	}
	return chain
}

func outlierDetection(od *kregv1alpha1.OutlierDetectionConfig) *istioapi.OutlierDetection {
	if od == nil {
		return nil
	}
	out := &istioapi.OutlierDetection{}
	if od.Consecutive5xx != nil {
		out.Consecutive_5XxErrors = wrapperspb.UInt32(uint32(*od.Consecutive5xx))
	}
	if od.BaseEjectionTime != nil {
		out.BaseEjectionTime = durationpb.New(od.BaseEjectionTime.Duration)
	}
	if od.MaxEjectionPercent != nil {
		out.MaxEjectionPercent = *od.MaxEjectionPercent
	}
	return out
}
