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

package pipeline

import (
	"fmt"
	"strconv"
	"strings"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
)

// Normalize decodes each route's BGP large communities into a
// BackendCandidate, per cm's rules and fallbacks. It's a pure function:
// the same routes and CommunityMapSpec always produce the same candidates,
// with no I/O of any kind.
func Normalize(routes []RIBRoute, cm *kregv1alpha1.CommunityMapSpec) ([]BackendCandidate, error) {
	candidates := make([]BackendCandidate, 0, len(routes))
	for _, route := range routes {
		candidate, err := normalizeRoute(route, cm)
		if err != nil {
			return nil, fmt.Errorf("normalize %s: %w", route.Prefix, err)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func normalizeRoute(route RIBRoute, cm *kregv1alpha1.CommunityMapSpec) (BackendCandidate, error) {
	candidate := BackendCandidate{
		Prefix:           route.Prefix,
		ClusterID:        route.ClusterID,
		Peer:             route.Peer,
		Locality:         route.Locality,
		MED:              route.MED,
		ASPath:           route.ASPath,
		LargeCommunities: route.LargeCommunities,
	}

	// Already rejected by Authorize: don't decode communities on data
	// that's already untrusted, just carry the rejection through.
	if route.Rejected {
		candidate.Rejected = true
		candidate.Reason = route.Reason
		return candidate, nil
	}

	anyRuleMatched := false
	weightSet := false
	if cm != nil {
		for _, rule := range cm.Rules {
			value, ok := matchLargeCommunity(rule.Match.LargeCommunity, route.LargeCommunities)
			if !ok {
				continue
			}
			anyRuleMatched = true
			if rule.Set.Field == kregv1alpha1.CommunityFieldWeight {
				weightSet = true
			}
			if err := applyFieldSet(&candidate, rule.Set, value); err != nil {
				return BackendCandidate{}, err
			}
		}
	}

	if !weightSet {
		candidate.Weight = fallbackWeight(candidate.MED, cm)
	}

	// "Refuse to guess": a route whose communities matched none of our
	// rules gets flagged per OnUnmappedCommunity rather than silently
	// treated as if it were understood.
	if !anyRuleMatched && cm != nil {
		reason := fmt.Sprintf("no CommunityMap rule matched large communities %v", route.LargeCommunities)
		switch cm.OnUnmappedCommunity {
		case kregv1alpha1.UnmappedCommunityReject:
			candidate.Rejected = true
			candidate.Reason = reason
		case kregv1alpha1.UnmappedCommunityWarn:
			candidate.Reason = reason
		}
	}

	return candidate, nil
}

// matchLargeCommunity matches communities against a
// "<globalAdmin>:<function>:<value>" pattern, where the value segment of
// pattern may be "*" to match any value. Returns the matched value
// segment.
func matchLargeCommunity(pattern string, communities []string) (string, bool) {
	patternParts := strings.SplitN(pattern, ":", 3)
	if len(patternParts) != 3 {
		return "", false
	}
	for _, community := range communities {
		parts := strings.SplitN(community, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] != patternParts[0] || parts[1] != patternParts[1] {
			continue
		}
		if patternParts[2] != "*" && parts[2] != patternParts[2] {
			continue
		}
		return parts[2], true
	}
	return "", false
}

func applyFieldSet(candidate *BackendCandidate, set kregv1alpha1.CommunityFieldSet, communityValue string) error {
	value := communityValue
	if !set.FromCommunityValue && set.Value != nil {
		value = *set.Value
	}

	switch set.Field {
	case kregv1alpha1.CommunityFieldWeight:
		weight, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return fmt.Errorf("set weight from %q: %w", value, err)
		}
		candidate.Weight = int32(weight)
	case kregv1alpha1.CommunityFieldTier:
		candidate.Tier = value
	case kregv1alpha1.CommunityFieldDrain:
		drain, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("set drain from %q: %w", value, err)
		}
		candidate.Drain = drain
	case kregv1alpha1.CommunityFieldServiceTag:
		tag, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return fmt.Errorf("set serviceTag from %q: %w", value, err)
		}
		tag32 := int32(tag)
		candidate.ServiceTag = &tag32
	default:
		return fmt.Errorf("unknown CommunityMap field %q", set.Field)
	}
	return nil
}

// fallbackWeight derives a weight when no rule set one, preferring
// CommunityMapSpec.Fallbacks.WeightFrom and falling back further to
// DefaultWeight, then a hardcoded default matching CommunityMapSpec's own
// +kubebuilder:default.
func fallbackWeight(med uint32, cm *kregv1alpha1.CommunityMapSpec) int32 {
	if cm != nil && cm.Fallbacks != nil {
		if cm.Fallbacks.WeightFrom == kregv1alpha1.WeightFallbackFromMED {
			return invertMED(med)
		}
		if cm.Fallbacks.DefaultWeight != 0 {
			return cm.Fallbacks.DefaultWeight
		}
	}
	return 100
}

// invertMED turns a MED (lower is more preferred) into a weight (higher is
// more preferred), bounded to a sane range.
func invertMED(med uint32) int32 {
	const ceiling = 1000
	if med >= ceiling {
		return 1
	}
	return int32(ceiling - med)
}
