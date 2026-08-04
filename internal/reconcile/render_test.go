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

package reconcile_test

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
	istiodriver "github.com/phenixblue/kreg/internal/reconcile/istio"
)

func mustParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

func gatewayTargetRef(kind, name string) gatewayv1.LocalPolicyTargetReference {
	return gatewayv1.LocalPolicyTargetReference{
		Group: "gateway.networking.k8s.io",
		Kind:  gatewayv1.Kind(kind),
		Name:  gatewayv1.ObjectName(name),
	}
}

// docPolicy mirrors the BGPBackendPolicy worked example in
// docs/design/architecture.md §2.3.
func docPolicy() *kregv1alpha1.BGPBackendPolicy {
	appProtocol := "https"
	serviceTag := int32(80)
	failoverThreshold := int32(50)
	consecutive5xx := int32(5)
	baseEjectionTime := metav1.Duration{Duration: mustParseDuration("30s")}
	maxEjectionPercent := int32(50)

	return &kregv1alpha1.BGPBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-web", Namespace: "gateways"},
		Spec: kregv1alpha1.BGPBackendPolicySpec{
			TargetRef: gatewayTargetRef("Gateway", "prod-web"),
			Selector: kregv1alpha1.BackendSelector{
				Prefixes:   []string{"198.51.100.0/24"},
				ServiceTag: &serviceTag,
			},
			Backend: kregv1alpha1.BackendConfig{
				Port:        8443,
				AppProtocol: &appProtocol,
				TLS: kregv1alpha1.BackendTLSConfig{
					Mode:          kregv1alpha1.BackendTLSModeSimple,
					SNI:           "prod-web.internal",
					CredentialRef: &corev1.LocalObjectReference{Name: "upstream-ca"},
				},
			},
			LoadBalancing: kregv1alpha1.LoadBalancingConfig{
				Strategy: kregv1alpha1.LoadBalancingStrategyLocality,
				Locality: &kregv1alpha1.LocalityLoadBalancingConfig{
					Preference:        []string{"us-east", "us-west", "eu-west"},
					FailoverThreshold: &failoverThreshold,
				},
			},
			OutlierDetection: &kregv1alpha1.OutlierDetectionConfig{
				Consecutive5xx:     &consecutive5xx,
				BaseEjectionTime:   &baseEjectionTime,
				MaxEjectionPercent: &maxEjectionPercent,
			},
		},
	}
}

// docCandidate mirrors the AdvertisedBackend worked example (atl-1) in
// docs/design/architecture.md §2.4.
func docCandidate() pipeline.BackendCandidate {
	serviceTag := int32(80)
	return pipeline.BackendCandidate{
		Prefix:           "198.51.100.10/32",
		ClusterID:        "atl-1",
		Peer:             "rr-atl-a",
		Locality:         pipeline.Locality{Region: "us-east", Zone: "us-east-atl-a"},
		MED:              100,
		ASPath:           []uint32{4200000101},
		LargeCommunities: []string{"4200000000:1:80", "4200000000:4:80"},
		Weight:           80,
		Tier:             "stable",
		Drain:            false,
		ServiceTag:       &serviceTag,
	}
}

var _ = Describe("Render", func() {
	It("matches the docs' worked example", func() {
		output, err := reconcile.Render(docPolicy(), []pipeline.BackendCandidate{docCandidate()}, istiodriver.Driver{})
		Expect(err).NotTo(HaveOccurred())

		Expect(output.Service.Name).To(Equal("prod-web-kreg"))
		Expect(output.EndpointSlices).To(HaveLen(1))
		Expect(output.EndpointSlices[0].Name).To(Equal("prod-web-kreg-atl-1"))
		Expect(output.DriverObjects).To(HaveLen(1))

		compareGolden("service.yaml", output.Service)
		compareGolden("endpointslice-atl-1.yaml", output.EndpointSlices[0])
		compareGolden("destinationrule.yaml", output.DriverObjects[0])
	})

	It("excludes candidates outside the selector's prefixes", func() {
		policy := docPolicy()
		candidate := docCandidate()
		candidate.Prefix = "203.0.113.5/32" // outside 198.51.100.0/24

		output, err := reconcile.Render(policy, []pipeline.BackendCandidate{candidate}, istiodriver.Driver{})
		Expect(err).NotTo(HaveOccurred())
		Expect(output.EndpointSlices).To(BeEmpty())
	})

	It("excludes candidates with a non-matching serviceTag", func() {
		policy := docPolicy()
		candidate := docCandidate()
		otherTag := int32(81)
		candidate.ServiceTag = &otherTag

		output, err := reconcile.Render(policy, []pipeline.BackendCandidate{candidate}, istiodriver.Driver{})
		Expect(err).NotTo(HaveOccurred())
		Expect(output.EndpointSlices).To(BeEmpty())
	})

	It("excludes candidates outside the selector's clusterIDs when set", func() {
		policy := docPolicy()
		policy.Spec.Selector.ClusterIDs = []string{"atl-2"}

		output, err := reconcile.Render(policy, []pipeline.BackendCandidate{docCandidate()}, istiodriver.Driver{})
		Expect(err).NotTo(HaveOccurred())
		Expect(output.EndpointSlices).To(BeEmpty())
	})

	It("excludes candidates the normalizer already rejected", func() {
		policy := docPolicy()
		candidate := docCandidate()
		candidate.Rejected = true

		output, err := reconcile.Render(policy, []pipeline.BackendCandidate{candidate}, istiodriver.Driver{})
		Expect(err).NotTo(HaveOccurred())
		Expect(output.EndpointSlices).To(BeEmpty())
	})

	It("marks a draining candidate not-ready without removing it", func() {
		policy := docPolicy()
		candidate := docCandidate()
		candidate.Drain = true

		output, err := reconcile.Render(policy, []pipeline.BackendCandidate{candidate}, istiodriver.Driver{})
		Expect(err).NotTo(HaveOccurred())
		Expect(output.EndpointSlices).To(HaveLen(1))
		Expect(*output.EndpointSlices[0].Endpoints[0].Conditions.Ready).To(BeFalse())
		Expect(*output.EndpointSlices[0].Endpoints[0].Conditions.Serving).To(BeTrue())
	})

	It("produces one EndpointSlice per candidate cluster", func() {
		policy := docPolicy()
		candidate := docCandidate()
		second := docCandidate()
		second.ClusterID = "atl-2"
		second.Prefix = "198.51.100.74/32"

		output, err := reconcile.Render(policy, []pipeline.BackendCandidate{candidate, second}, istiodriver.Driver{})
		Expect(err).NotTo(HaveOccurred())
		Expect(output.EndpointSlices).To(HaveLen(2))
		names := []string{output.EndpointSlices[0].Name, output.EndpointSlices[1].Name}
		Expect(names).To(ConsistOf("prod-web-kreg-atl-1", "prod-web-kreg-atl-2"))
	})

	It("propagates a driver error", func() {
		policy := docPolicy()
		policy.Spec.Backend.TLS.Mode = kregv1alpha1.BackendTLSModeMutual // not yet implemented by the Istio driver

		_, err := reconcile.Render(policy, []pipeline.BackendCandidate{docCandidate()}, istiodriver.Driver{})
		Expect(err).To(HaveOccurred())
	})

	It("returns every generated object from Objects in a stable order", func() {
		output, err := reconcile.Render(docPolicy(), []pipeline.BackendCandidate{docCandidate()}, istiodriver.Driver{})
		Expect(err).NotTo(HaveOccurred())

		objs := output.Objects()
		Expect(objs).To(HaveLen(3))
		Expect(objs[0].GetObjectKind().GroupVersionKind().Kind).To(Equal("Service"))
		Expect(objs[1].GetObjectKind().GroupVersionKind().Kind).To(Equal("EndpointSlice"))
		Expect(objs[2].GetObjectKind().GroupVersionKind().Kind).To(Equal("DestinationRule"))
	})
})

// compareGolden marshals obj to YAML and compares it against
// testdata/golden/<name>. Run with UPDATE_GOLDEN=1 to (re)write the golden
// file from the current output.
func compareGolden(name string, obj client.Object) {
	GinkgoHelper()

	actual, err := yaml.Marshal(obj)
	Expect(err).NotTo(HaveOccurred())

	path := filepath.Join("testdata", "golden", name)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		Expect(os.WriteFile(path, actual, 0o644)).To(Succeed())
	}

	want, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("read golden file %s (run with UPDATE_GOLDEN=1 to create it)", path))
	Expect(string(actual)).To(Equal(string(want)), fmt.Sprintf("%s does not match golden output; run with UPDATE_GOLDEN=1 to update", path))
}
