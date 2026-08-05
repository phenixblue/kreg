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

package snapshot

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
)

var _ = Describe("needsPriorState", func() {
	It("is false for a zero-value spec (no BGPStabilityConfig yet)", func() {
		Expect(needsPriorState(kregv1alpha1.BGPStabilityConfigSpec{})).To(BeFalse())
	})

	It("is false when dampening is present but not enabled", func() {
		spec := kregv1alpha1.BGPStabilityConfigSpec{
			Dampening: &kregv1alpha1.DampeningConfig{Enabled: false},
		}
		Expect(needsPriorState(spec)).To(BeFalse())
	})

	It("is true when withdrawalGrace is a positive duration", func() {
		spec := kregv1alpha1.BGPStabilityConfigSpec{
			WithdrawalGrace: &metav1.Duration{Duration: 30 * time.Second},
		}
		Expect(needsPriorState(spec)).To(BeTrue())
	})

	It("is true when additionDelay is a positive duration", func() {
		spec := kregv1alpha1.BGPStabilityConfigSpec{
			AdditionDelay: &metav1.Duration{Duration: 10 * time.Second},
		}
		Expect(needsPriorState(spec)).To(BeTrue())
	})

	It("is true when dampening is enabled", func() {
		spec := kregv1alpha1.BGPStabilityConfigSpec{
			Dampening: &kregv1alpha1.DampeningConfig{Enabled: true},
		}
		Expect(needsPriorState(spec)).To(BeTrue())
	})

	It("is false when withdrawalGrace/additionDelay are set but zero", func() {
		spec := kregv1alpha1.BGPStabilityConfigSpec{
			WithdrawalGrace: &metav1.Duration{Duration: 0},
			AdditionDelay:   &metav1.Duration{Duration: 0},
		}
		Expect(needsPriorState(spec)).To(BeFalse())
	})
})
