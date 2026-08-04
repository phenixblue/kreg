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

package ewma_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/damp"
	"github.com/phenixblue/kreg/internal/damp/ewma"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func testCandidate() pipeline.BackendCandidate {
	return pipeline.BackendCandidate{
		Prefix:    "198.51.100.10/32",
		ClusterID: "atl-1",
		Peer:      "rr-atl-a",
		Weight:    80,
	}
}

func key(c pipeline.BackendCandidate) string {
	return reconcile.BackendObjectName(c.Prefix, c.ClusterID)
}

// priorMap builds a one-entry prior map from a previously-evaluated
// candidate, the way internal/snapshot.Source would after reading it
// back from AdvertisedBackend on the next tick.
func priorMap(c pipeline.BackendCandidate) map[string]damp.PriorState {
	bare := c
	damping := *c.Damping
	bare.Damping = nil
	return map[string]damp.PriorState{key(c): {Candidate: bare, Damping: damping}}
}

// baseConfig: withdrawalGrace 10s, no additionDelay, dampening enabled
// with halfLife 10s / suppressThreshold 3000 / reuseThreshold 750 /
// maxSuppress 1m — matching docs/design/architecture.md's worked
// example, scaled down to seconds for fast, deterministic tests.
func baseConfig() kregv1alpha1.BGPStabilityConfigSpec {
	grace := metav1.Duration{Duration: 10 * time.Second}
	halfLife := metav1.Duration{Duration: 10 * time.Second}
	suppress := int32(3000)
	reuse := int32(750)
	maxSuppress := metav1.Duration{Duration: time.Minute}
	return kregv1alpha1.BGPStabilityConfigSpec{
		WithdrawalGrace: &grace,
		Dampening: &kregv1alpha1.DampeningConfig{
			Enabled:           true,
			HalfLife:          &halfLife,
			SuppressThreshold: &suppress,
			ReuseThreshold:    &reuse,
			MaxSuppress:       &maxSuppress,
		},
	}
}

var _ = Describe("Damper.Evaluate", func() {
	It("marks a brand new candidate Active immediately when additionDelay is unset", func() {
		cfg := kregv1alpha1.BGPStabilityConfigSpec{} // zero-value: no grace, no delay, dampening disabled
		out := (ewma.Damper{}).Evaluate(t0, []pipeline.BackendCandidate{testCandidate()}, map[string]damp.PriorState{}, cfg)

		Expect(out).To(HaveLen(1))
		Expect(out[0].Damping.State).To(Equal(kregv1alpha1.BackendStateActive))
		Expect(out[0].Damping.LastObservedAt).To(Equal(t0))
	})

	It("holds a brand new candidate Pending until additionDelay elapses, then promotes it", func() {
		delay := metav1.Duration{Duration: 5 * time.Second}
		cfg := baseConfig()
		cfg.AdditionDelay = &delay
		c := testCandidate()

		out1 := (ewma.Damper{}).Evaluate(t0, []pipeline.BackendCandidate{c}, map[string]damp.PriorState{}, cfg)
		Expect(out1[0].Damping.State).To(Equal(kregv1alpha1.BackendStatePending))
		Expect(out1[0].Damping.PendingSince).To(gstruct.PointTo(Equal(t0)))

		t1 := t0.Add(3 * time.Second) // still within additionDelay
		out2 := (ewma.Damper{}).Evaluate(t1, []pipeline.BackendCandidate{c}, priorMap(out1[0]), cfg)
		Expect(out2[0].Damping.State).To(Equal(kregv1alpha1.BackendStatePending))

		t2 := t0.Add(6 * time.Second) // past additionDelay
		out3 := (ewma.Damper{}).Evaluate(t2, []pipeline.BackendCandidate{c}, priorMap(out2[0]), cfg)
		Expect(out3[0].Damping.State).To(Equal(kregv1alpha1.BackendStateActive))
	})

	It("holds a withdrawn candidate as HoldDown within grace, then stops synthesizing it once grace expires", func() {
		cfg := baseConfig()
		c := testCandidate()

		active := (ewma.Damper{}).Evaluate(t0, []pipeline.BackendCandidate{c}, map[string]damp.PriorState{}, cfg)
		Expect(active[0].Damping.State).To(Equal(kregv1alpha1.BackendStateActive))

		t1 := t0.Add(4 * time.Second) // withdrawn, well within the 10s grace
		withdrawn := (ewma.Damper{}).Evaluate(t1, nil, priorMap(active[0]), cfg)
		Expect(withdrawn).To(HaveLen(1))
		Expect(withdrawn[0].Prefix).To(Equal(c.Prefix))
		Expect(withdrawn[0].Damping.State).To(Equal(kregv1alpha1.BackendStateHoldDown))
		Expect(withdrawn[0].Damping.WithdrawnAt).To(gstruct.PointTo(Equal(t1)))

		t2 := t1.Add(11 * time.Second) // still absent, now past the 10s grace since t1
		expired := (ewma.Damper{}).Evaluate(t2, nil, priorMap(withdrawn[0]), cfg)
		Expect(expired).To(BeEmpty())
	})

	It("suppresses a candidate once repeated flaps cross suppressThreshold", func() {
		cfg := baseConfig()
		c := testCandidate()

		active := (ewma.Damper{}).Evaluate(t0, []pipeline.BackendCandidate{c}, map[string]damp.PriorState{}, cfg)
		last := active[0]
		Expect(last.Damping.State).To(Equal(kregv1alpha1.BackendStateActive))

		tick := t0
		for i := 0; i < 5 && last.Damping.State != kregv1alpha1.BackendStateDampened; i++ {
			tick = tick.Add(time.Second)
			absent := (ewma.Damper{}).Evaluate(tick, nil, priorMap(last), cfg)
			Expect(absent).To(HaveLen(1))
			Expect(absent[0].Damping.State).To(Equal(kregv1alpha1.BackendStateHoldDown))

			tick = tick.Add(time.Second)
			present := (ewma.Damper{}).Evaluate(tick, []pipeline.BackendCandidate{c}, priorMap(absent[0]), cfg)
			Expect(present).To(HaveLen(1))
			last = present[0]
		}

		Expect(last.Damping.State).To(Equal(kregv1alpha1.BackendStateDampened))
		Expect(last.Damping.Score).To(BeNumerically(">=", 3000))
		Expect(last.Damping.SuppressedSince).NotTo(BeNil())
	})

	It("returns a suppressed candidate to Active once its score decays below reuseThreshold", func() {
		cfg := baseConfig()
		c := testCandidate()
		suppressedSince := t0
		prior := map[string]damp.PriorState{
			key(c): {
				Candidate: c,
				Damping: pipeline.DampingInfo{
					State:           kregv1alpha1.BackendStateDampened,
					Score:           4000,
					LastObservedAt:  t0,
					SuppressedSince: &suppressedSince,
				},
			},
		}

		// Several half-lives (10s) later, but comfortably within maxSuppress (1m)
		// -- isolates decay-driven reuse from the maxSuppress cap.
		later := t0.Add(45 * time.Second)
		out := (ewma.Damper{}).Evaluate(later, []pipeline.BackendCandidate{c}, prior, cfg)

		Expect(out).To(HaveLen(1))
		Expect(out[0].Damping.State).To(Equal(kregv1alpha1.BackendStateActive))
		Expect(out[0].Damping.Score).To(BeNumerically("<", 750))
		Expect(out[0].Damping.SuppressedSince).To(BeNil())
	})

	It("forces a suppressed candidate back to Active once maxSuppress elapses, regardless of score", func() {
		cfg := baseConfig()
		cfg.Dampening.MaxSuppress = &metav1.Duration{Duration: 5 * time.Second}
		cfg.Dampening.HalfLife = &metav1.Duration{Duration: 5 * time.Minute} // slow decay: isolates the cap from reuse
		c := testCandidate()
		suppressedSince := t0
		prior := map[string]damp.PriorState{
			key(c): {
				Candidate: c,
				Damping: pipeline.DampingInfo{
					State:           kregv1alpha1.BackendStateDampened,
					Score:           10000,
					LastObservedAt:  t0,
					SuppressedSince: &suppressedSince,
				},
			},
		}

		later := t0.Add(6 * time.Second) // past the 5s maxSuppress; barely any decay at this halfLife
		out := (ewma.Damper{}).Evaluate(later, []pipeline.BackendCandidate{c}, prior, cfg)

		Expect(out).To(HaveLen(1))
		Expect(out[0].Damping.State).To(Equal(kregv1alpha1.BackendStateActive))
		Expect(out[0].Damping.Score).To(Equal(float64(0)))
		Expect(out[0].Damping.SuppressedSince).To(BeNil())
	})

	It("passes a Rejected candidate through untouched, never setting Damping", func() {
		cfg := baseConfig()
		rejected := pipeline.BackendCandidate{
			Prefix:   "203.0.113.5/32",
			Rejected: true,
			Reason:   "prefix 203.0.113.5/32 not in allowedPrefixes for any cluster",
		}

		out := (ewma.Damper{}).Evaluate(t0, []pipeline.BackendCandidate{rejected}, map[string]damp.PriorState{}, cfg)

		Expect(out).To(HaveLen(1))
		Expect(out[0].Damping).To(BeNil())
		Expect(out[0].Rejected).To(BeTrue())
	})
})
