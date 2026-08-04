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

package reconcile

import (
	"fmt"
	"maps"
	"net"
	"slices"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
)

// Output is everything Render produced for one BGPBackendPolicy.
type Output struct {
	Service        *corev1.Service
	EndpointSlices []*discoveryv1.EndpointSlice
	DriverObjects  []client.Object
}

// Objects returns every generated object as a single slice, in a stable
// order (Service, then EndpointSlices, then driver objects) — convenient
// for applying or for golden-file serialization.
func (o *Output) Objects() []client.Object {
	objs := make([]client.Object, 0, 1+len(o.EndpointSlices)+len(o.DriverObjects))
	objs = append(objs, o.Service)
	for _, s := range o.EndpointSlices {
		objs = append(objs, s)
	}
	objs = append(objs, o.DriverObjects...)
	return objs
}

// Render turns a settled snapshot of BackendCandidates plus a
// BGPBackendPolicy into the desired objects: a portable headless Service
// and per-VIP EndpointSlices, plus whatever driver produces for traffic
// policy. It's a pure function — no cluster access, no I/O.
func Render(policy *kregv1alpha1.BGPBackendPolicy, candidates []pipeline.BackendCandidate, driver Driver) (*Output, error) {
	selected, err := Select(candidates, policy.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("select backends: %w", err)
	}

	svc := renderService(policy)
	endpointSlices, err := renderEndpointSlices(policy, svc.Name, selected)
	if err != nil {
		return nil, fmt.Errorf("render endpoint slices: %w", err)
	}

	host := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, policy.Namespace)
	driverObjects, err := driver.Lower(policy, selected, host)
	if err != nil {
		return nil, fmt.Errorf("lower driver objects: %w", err)
	}

	return &Output{Service: svc, EndpointSlices: endpointSlices, DriverObjects: driverObjects}, nil
}

// Select filters candidates to those matching a BGPBackendPolicy's
// selector, excluding any candidate CommunityMap rejected outright.
func Select(candidates []pipeline.BackendCandidate, sel kregv1alpha1.BackendSelector) ([]pipeline.BackendCandidate, error) {
	var selected []pipeline.BackendCandidate
	for _, c := range candidates {
		if c.Rejected {
			continue
		}
		if len(sel.ClusterIDs) > 0 && !slices.Contains(sel.ClusterIDs, c.ClusterID) {
			continue
		}
		if sel.ServiceTag != nil && (c.ServiceTag == nil || *c.ServiceTag != *sel.ServiceTag) {
			continue
		}
		if len(sel.Prefixes) > 0 {
			within, err := prefixWithinAny(c.Prefix, sel.Prefixes)
			if err != nil {
				return nil, err
			}
			if !within {
				continue
			}
		}
		selected = append(selected, c)
	}
	return selected, nil
}

func prefixWithinAny(prefix string, cidrs []string) (bool, error) {
	ip, _, err := net.ParseCIDR(prefix)
	if err != nil {
		return false, fmt.Errorf("parse candidate prefix %q: %w", prefix, err)
	}
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return false, fmt.Errorf("parse selector prefix %q: %w", cidr, err)
		}
		if network.Contains(ip) {
			return true, nil
		}
	}
	return false, nil
}

// ServiceName is the portable Service Render generates for policy.
// Exported so other consumers of the same candidates (e.g.
// internal/report, computing AdvertisedBackend.status.generatedResources)
// can name-match without drifting from what Render actually produced.
func ServiceName(policy *kregv1alpha1.BGPBackendPolicy) string {
	return policy.Name + "-kreg"
}

// EndpointSliceName is the per-candidate EndpointSlice name Render
// generates, given the Service name it belongs to. See ServiceName.
func EndpointSliceName(serviceName, clusterID string) string {
	return fmt.Sprintf("%s-%s", serviceName, clusterID)
}

func managedByLabels(policy *kregv1alpha1.BGPBackendPolicy, extra map[string]string) map[string]string {
	labels := map[string]string{ManagedByLabel: policy.Name}
	maps.Copy(labels, extra)
	return labels
}

func backendPortName(backend kregv1alpha1.BackendConfig) string {
	if backend.AppProtocol != nil && *backend.AppProtocol != "" {
		return *backend.AppProtocol
	}
	return "backend"
}

func renderService(policy *kregv1alpha1.BGPBackendPolicy) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceName(policy),
			Namespace: policy.Namespace,
			Labels:    managedByLabels(policy, nil),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Ports: []corev1.ServicePort{{
				Name:     backendPortName(policy.Spec.Backend),
				Port:     policy.Spec.Backend.Port,
				Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// renderEndpointSlices builds one EndpointSlice per selected candidate —
// matching AdvertisedBackend's granularity, so "why isn't traffic going to
// atl-2" stays answerable by finding a specific object. weight is decoded
// onto BackendCandidate but deliberately not placed here: it isn't
// expressible on a core EndpointSlice endpoint. See
// docs/design/architecture.md §3.
func renderEndpointSlices(policy *kregv1alpha1.BGPBackendPolicy, serviceName string, candidates []pipeline.BackendCandidate) ([]*discoveryv1.EndpointSlice, error) {
	portName := backendPortName(policy.Spec.Backend)
	endpointSlices := make([]*discoveryv1.EndpointSlice, 0, len(candidates))
	for _, c := range candidates {
		addr, addrType, err := addressAndType(c.Prefix)
		if err != nil {
			return nil, err
		}

		ready := !c.Drain
		serving := true
		endpoint := discoveryv1.Endpoint{
			Addresses: []string{addr},
			Conditions: discoveryv1.EndpointConditions{
				Ready:   &ready,
				Serving: &serving,
			},
		}
		if c.Locality.Zone != "" {
			zone := c.Locality.Zone
			endpoint.Zone = &zone
			endpoint.Hints = &discoveryv1.EndpointHints{
				ForZones: []discoveryv1.ForZone{{Name: zone}},
			}
		}

		endpointSlices = append(endpointSlices, &discoveryv1.EndpointSlice{
			TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSlice"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      EndpointSliceName(serviceName, c.ClusterID),
				Namespace: policy.Namespace,
				Labels: managedByLabels(policy, map[string]string{
					discoveryv1.LabelServiceName: serviceName,
				}),
			},
			AddressType: addrType,
			Ports: []discoveryv1.EndpointPort{{
				Name:     ptr.To(portName),
				Port:     ptr.To(policy.Spec.Backend.Port),
				Protocol: ptr.To(corev1.ProtocolTCP),
			}},
			Endpoints: []discoveryv1.Endpoint{endpoint},
		})
	}
	return endpointSlices, nil
}

func addressAndType(prefix string) (string, discoveryv1.AddressType, error) {
	ip, _, err := net.ParseCIDR(prefix)
	if err != nil {
		return "", "", fmt.Errorf("parse prefix %q: %w", prefix, err)
	}
	if ip.To4() != nil {
		return ip.String(), discoveryv1.AddressTypeIPv4, nil
	}
	return ip.String(), discoveryv1.AddressTypeIPv6, nil
}
