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

// Package ewma implements damp.Damper: an exponentially-decaying
// instability score, decayed against real elapsed wall-clock time
// between evaluations rather than an assumed fixed tick. v1's only
// implementation — see internal/damp's package doc for why the
// algorithm sits behind an interface at all.
package ewma

import (
	"fmt"
	"math"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/damp"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
)

// flapPenalty is added to the score on each detected flap — RFC 2439's
// own classic per-flap default, kept as an internal constant (not a
// DampeningConfig field) so the doc's worked-example suppressThreshold
// (3000) / reuseThreshold (750) numbers stay meaningful without users
// needing to know or tune the underlying algorithm's units.
const flapPenalty = 1000

// Defaults used when a BGPStabilityConfig exists but leaves a field
// unset, or when none exists yet at all — chosen to match
// docs/design/architecture.md's own worked example.
const (
	defaultHalfLife          = 90 * time.Second
	defaultSuppressThreshold = int32(3000)
	defaultReuseThreshold    = int32(750)
	defaultMaxSuppress       = 30 * time.Minute
)

// flapCountHalfLife decays AdvertisedBackendStatus.FlapCount24h — an
// approximation of "flaps in the last 24h", not an exact sliding-window
// count. Independent of DampeningConfig.HalfLife, which decays the
// suppression score.
const flapCountHalfLife = 24 * time.Hour

// Damper implements damp.Damper via EWMA decay.
type Damper struct{}

var _ damp.Damper = Damper{}

// Evaluate implements damp.Damper.
func (Damper) Evaluate(now time.Time, candidates []pipeline.BackendCandidate,
	prior map[string]damp.PriorState, cfg kregv1alpha1.BGPStabilityConfigSpec,
) []pipeline.BackendCandidate {
	withdrawalGrace := durationOrZero(cfg.WithdrawalGrace)
	additionDelay := durationOrZero(cfg.AdditionDelay)

	seen := make(map[string]bool, len(candidates))
	out := make([]pipeline.BackendCandidate, 0, len(candidates))

	for _, c := range candidates {
		if c.Rejected {
			// Authorize/CommunityMap-level rejection is a different axis
			// than route instability — pass through untouched, same as
			// before the Damper existed.
			out = append(out, c)
			continue
		}

		key := reconcile.BackendObjectName(c.Prefix, c.ClusterID)
		seen[key] = true

		p, hadPrior := prior[key]
		candidate := c
		candidate.Damping = evaluatePresent(now, p, hadPrior, additionDelay, cfg.Dampening)
		out = append(out, candidate)
	}

	for key, p := range prior {
		if seen[key] {
			continue
		}
		if synthesized := evaluateAbsent(now, p, withdrawalGrace, cfg.Dampening); synthesized != nil {
			out = append(out, *synthesized)
		}
		// nil means past withdrawalGrace: stop synthesizing, let the
		// caller's normal prune-by-absence handle it.
	}

	return out
}

// evaluatePresent decides Damping for a candidate present in this tick's
// settled snapshot.
func evaluatePresent(now time.Time, p damp.PriorState, hadPrior bool,
	additionDelay time.Duration, dampening *kregv1alpha1.DampeningConfig,
) *pipeline.DampingInfo {
	if !hadPrior {
		return newCandidateDamping(now, additionDelay)
	}

	prev := p.Damping

	// Still settling from a previous tick: not a flap, no score change.
	if prev.State == kregv1alpha1.BackendStatePending {
		pendingSince := prev.PendingSince
		if pendingSince == nil {
			pendingSince = &prev.LastObservedAt
		}
		if now.Sub(*pendingSince) < additionDelay {
			return &pipeline.DampingInfo{
				State:          kregv1alpha1.BackendStatePending,
				Reason:         fmt.Sprintf("settling, additionDelay %s", additionDelay),
				LastObservedAt: now,
				PendingSince:   pendingSince,
			}
		}
		// Past additionDelay: falls through to normal evaluation below,
		// which promotes it (WithdrawnAt is nil, so this never counts as
		// a flap).
	}

	elapsed := now.Sub(prev.LastObservedAt)
	score := decayScore(prev.Score, elapsed, halfLifeOf(dampening))
	flapCount := decayFlapCount(prev.FlapCount24h, elapsed)

	flapped := prev.WithdrawnAt != nil
	if flapped {
		score += flapPenalty
		flapCount++
	}

	state, reason, score, suppressedSince := suppressionState(now, score, prev.SuppressedSince, dampening)

	return &pipeline.DampingInfo{
		State:           state,
		Reason:          reason,
		Score:           score,
		FlapCount24h:    flapCount,
		LastObservedAt:  now,
		SuppressedSince: suppressedSince,
	}
}

// newCandidateDamping is a genuinely new candidate — no prior record at
// all (or its prior record was never evaluated by Damp, e.g. it was
// always Rejected before now).
func newCandidateDamping(now time.Time, additionDelay time.Duration) *pipeline.DampingInfo {
	if additionDelay <= 0 {
		return &pipeline.DampingInfo{State: kregv1alpha1.BackendStateActive, LastObservedAt: now}
	}
	return &pipeline.DampingInfo{
		State:          kregv1alpha1.BackendStatePending,
		Reason:         fmt.Sprintf("settling, additionDelay %s", additionDelay),
		LastObservedAt: now,
		PendingSince:   &now,
	}
}

// evaluateAbsent decides Damping for a candidate that was present before
// but is missing from this tick's settled snapshot. Returns nil once
// withdrawalGrace has elapsed — the signal to the caller to stop
// synthesizing this candidate at all.
func evaluateAbsent(now time.Time, p damp.PriorState, withdrawalGrace time.Duration,
	dampening *kregv1alpha1.DampeningConfig,
) *pipeline.BackendCandidate {
	prev := p.Damping
	if prev.State == kregv1alpha1.BackendStatePending {
		// Never actually served; nothing to hold down.
		return nil
	}

	withdrawnAt := prev.WithdrawnAt
	if withdrawnAt == nil {
		withdrawnAt = &now
	}
	if now.Sub(*withdrawnAt) >= withdrawalGrace {
		return nil
	}

	elapsed := now.Sub(prev.LastObservedAt)
	score := decayScore(prev.Score, elapsed, halfLifeOf(dampening))
	flapCount := decayFlapCount(prev.FlapCount24h, elapsed)

	// A backend that was suppressed before it disappeared must stay
	// Dampened for as long as suppressionState says so (same hysteresis
	// and maxSuppress cap as the present-tick path) -- reconcile.Select
	// only excludes Dampened/Pending, not HoldDown, so falling back to
	// HoldDown here would re-select an actively-flapping backend for the
	// whole withdrawalGrace window on every withdraw, oscillating it back
	// into service and defeating the suppression that's the entire point
	// of this stage.
	state, reason, score, suppressedSince := suppressionState(now, score, prev.SuppressedSince, dampening)
	if state != kregv1alpha1.BackendStateDampened {
		state = kregv1alpha1.BackendStateHoldDown
		reason = fmt.Sprintf("withdrawn %s ago, grace %s", now.Sub(*withdrawnAt).Round(time.Second), withdrawalGrace)
	}

	synthesized := p.Candidate
	synthesized.Damping = &pipeline.DampingInfo{
		State:           state,
		Reason:          reason,
		Score:           score,
		FlapCount24h:    flapCount,
		LastObservedAt:  now,
		WithdrawnAt:     withdrawnAt,
		SuppressedSince: suppressedSince,
	}
	return &synthesized
}

// suppressionState applies hysteresis: only score crossing above
// suppressThreshold enters Dampened; only decaying below reuseThreshold
// (while already Dampened) exits it. Values strictly between the two
// thresholds hold whichever state was already true — that gap is the
// whole point of having two thresholds instead of one, so a route
// sitting near the boundary doesn't oscillate every tick. score is
// returned alongside state rather than mutated by the caller: it's only
// ever reset (to 0) by the maxSuppress cap — every other path must carry
// the real decayed score forward untouched, including a route that's
// simply Active with a small, still-decaying score from a past flap.
func suppressionState(now time.Time, score float64, suppressedSince *time.Time,
	dampening *kregv1alpha1.DampeningConfig,
) (kregv1alpha1.BackendState, string, float64, *time.Time) {
	if dampening == nil || !dampening.Enabled {
		return kregv1alpha1.BackendStateActive, "", score, nil
	}

	suppressThreshold := suppressThresholdOf(dampening)
	reuseThreshold := reuseThresholdOf(dampening)
	maxSuppress := maxSuppressOf(dampening)

	if suppressedSince != nil && now.Sub(*suppressedSince) >= maxSuppress {
		// Capped: force back to Active regardless of score.
		return kregv1alpha1.BackendStateActive, "", 0, nil
	}

	if suppressedSince != nil {
		if score < float64(reuseThreshold) {
			return kregv1alpha1.BackendStateActive, "", score, nil
		}
		reason := fmt.Sprintf("flap-dampened: score %.0f (reuse below %d)", score, reuseThreshold)
		return kregv1alpha1.BackendStateDampened, reason, score, suppressedSince
	}

	if score >= float64(suppressThreshold) {
		reason := fmt.Sprintf("flap-dampened: score %.0f (>= suppressThreshold %d)", score, suppressThreshold)
		return kregv1alpha1.BackendStateDampened, reason, score, &now
	}

	return kregv1alpha1.BackendStateActive, "", score, nil
}

func decayScore(prevScore float64, elapsed, halfLife time.Duration) float64 {
	if elapsed <= 0 || halfLife <= 0 {
		return prevScore
	}
	return prevScore * math.Pow(0.5, elapsed.Seconds()/halfLife.Seconds())
}

func decayFlapCount(prev int32, elapsed time.Duration) int32 {
	if elapsed <= 0 {
		return prev
	}
	decayed := float64(prev) * math.Pow(0.5, elapsed.Seconds()/flapCountHalfLife.Seconds())
	return int32(math.Round(decayed))
}

func durationOrZero(d *metav1.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return d.Duration
}

func halfLifeOf(d *kregv1alpha1.DampeningConfig) time.Duration {
	if d != nil && d.HalfLife != nil {
		return d.HalfLife.Duration
	}
	return defaultHalfLife
}

func suppressThresholdOf(d *kregv1alpha1.DampeningConfig) int32 {
	if d.SuppressThreshold != nil {
		return *d.SuppressThreshold
	}
	return defaultSuppressThreshold
}

func reuseThresholdOf(d *kregv1alpha1.DampeningConfig) int32 {
	if d.ReuseThreshold != nil {
		return *d.ReuseThreshold
	}
	return defaultReuseThreshold
}

func maxSuppressOf(d *kregv1alpha1.DampeningConfig) time.Duration {
	if d.MaxSuppress != nil {
		return d.MaxSuppress.Duration
	}
	return defaultMaxSuppress
}
