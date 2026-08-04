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

package snapshot_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/damp/ewma"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/snapshot"
)

// atl1Address is the prefix used across these tests as a route inside
// docPeerConfig's atl-1 allowedPrefixes.
const atl1Address = "198.51.100.10/32"

// fakeRIB stands in for a real *ingest.Manager, so these tests exercise
// Source's own orchestration (list bindings, authorize, normalize)
// without needing a live BGP session.
type fakeRIB struct {
	routes []pipeline.RIBRoute
	err    error
}

func (f fakeRIB) Snapshot(context.Context) ([]pipeline.RIBRoute, error) {
	return f.routes, f.err
}

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(kregv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func docPeerConfig() *kregv1alpha1.BGPPeerConfig {
	return &kregv1alpha1.BGPPeerConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "atl-reflectors"},
		Spec: kregv1alpha1.BGPPeerConfigSpec{
			LocalASN: 4200000000,
			RouterID: "10.0.0.1",
			ClusterBindings: []kregv1alpha1.ClusterBinding{{
				ClusterID:       "atl-1",
				AllowedPrefixes: []string{"198.51.100.0/26"},
				Locality:        kregv1alpha1.Locality{Region: "us-east", Zone: "us-east-atl-a"},
			}},
		},
	}
}

var _ = Describe("Source", func() {
	It("chains ingest, authorize, and normalize end to end", func() {
		communityMap := &kregv1alpha1.CommunityMap{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: kregv1alpha1.CommunityMapSpec{
				Rules: []kregv1alpha1.CommunityRule{{
					Match: kregv1alpha1.CommunityMatch{LargeCommunity: "4200000000:1:*"},
					Set:   kregv1alpha1.CommunityFieldSet{Field: kregv1alpha1.CommunityFieldWeight, FromCommunityValue: true},
				}},
			},
		}

		src := &snapshot.Source{
			Client: newFakeClient(docPeerConfig(), communityMap),
			RIB: fakeRIB{routes: []pipeline.RIBRoute{
				{Prefix: atl1Address, LargeCommunities: []string{"4200000000:1:80"}},
				{Prefix: "203.0.113.5/32"}, // outside every binding -> kept, flagged Rejected
			}},
			Damper: ewma.Damper{},
		}

		candidates, err := src.Snapshot(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(2))
		Expect(candidates[0].Prefix).To(Equal(atl1Address))
		Expect(candidates[0].ClusterID).To(Equal("atl-1"))
		Expect(candidates[0].Locality).To(Equal(pipeline.Locality{Region: "us-east", Zone: "us-east-atl-a"}))
		Expect(candidates[0].Weight).To(Equal(int32(80)))
		Expect(candidates[0].Rejected).To(BeFalse())

		Expect(candidates[1].Prefix).To(Equal("203.0.113.5/32"))
		Expect(candidates[1].Rejected).To(BeTrue())
		Expect(candidates[1].Reason).NotTo(BeEmpty())
	})

	It("proceeds with default weight when no CommunityMap exists yet", func() {
		src := &snapshot.Source{
			Client: newFakeClient(docPeerConfig()),
			RIB:    fakeRIB{routes: []pipeline.RIBRoute{{Prefix: atl1Address}}},
			Damper: ewma.Damper{},
		}

		candidates, err := src.Snapshot(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))
		Expect(candidates[0].Weight).To(Equal(int32(100)))
	})

	It("rejects every route when there are no BGPPeerConfigs at all", func() {
		src := &snapshot.Source{
			Client: newFakeClient(),
			RIB:    fakeRIB{routes: []pipeline.RIBRoute{{Prefix: atl1Address}}},
			Damper: ewma.Damper{},
		}

		candidates, err := src.Snapshot(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))
		Expect(candidates[0].Rejected).To(BeTrue())
	})

	It("propagates an ingest error", func() {
		src := &snapshot.Source{
			Client: newFakeClient(),
			RIB:    fakeRIB{err: errors.New("bgp session down")},
			Damper: ewma.Damper{},
		}

		_, err := src.Snapshot(context.Background())
		Expect(err).To(HaveOccurred())
	})
})
