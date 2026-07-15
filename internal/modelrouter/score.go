package modelrouter

import (
	"strings"

	"github.com/Gitlawb/zero/internal/modelregistry"
)

// Scoring weights are deterministic integer points. Higher score ranks higher.
// The ordering intentionally encodes documented priorities: an explicit
// preferred model dominates, then preferred provider, then capability fit,
// then cost as a tie-breaker. None of these encode subjective quality.
const (
	preferredModelBonus    = 1_000_000
	preferredProviderBonus = 10_000
	exactFitBonus          = 1_000
	extraCapPenalty        = 50 // per unnecessary capability
	costPenaltyFactor      = 5  // score -= int(combinedCost * factor)
)

// requiredKnownCapabilities drops any required capability that the registry does
// not recognize, so an unknown value can never silently reject every model.
func requiredKnownCapabilities(task modelregistryCaps) []modelregistry.ModelCapability {
	out := make([]modelregistry.ModelCapability, 0, len(task))
	for _, c := range task {
		if modelregistry.ValidModelCapability(c) {
			out = append(out, c)
		}
	}
	return out
}

// entryInvalid performs a minimal, router-safe validity check. It does NOT call
// modelregistry.ModelEntry.Validate (which requires pricing metadata such as a
// verified source date) because the router must accept custom candidates that
// lack full registry metadata.
func entryInvalid(e modelregistry.ModelEntry) bool {
	if strings.TrimSpace(e.ID) == "" {
		return true
	}
	if e.ContextLimits.ContextWindow <= 0 {
		return true
	}
	if len(e.Capabilities) == 0 {
		return true
	}
	for _, c := range e.Capabilities {
		if !modelregistry.ValidModelCapability(c) {
			return true
		}
	}
	if e.Cost.InputPerMillion < 0 || e.Cost.OutputPerMillion < 0 || e.Cost.CachedInputPerMillion < 0 {
		return true
	}
	if !modelregistry.ValidModelStatus(e.Status) {
		return true
	}
	return false
}

// entryHasKnownPrice reports whether the candidate carries factual base pricing.
// A zero/empty cost is treated as unknown; it is never assumed to be free.
func entryHasKnownPrice(e modelregistry.ModelEntry) bool {
	if strings.TrimSpace(e.Cost.Currency) == "" {
		return false
	}
	if e.Cost.InputPerMillion <= 0 && e.Cost.OutputPerMillion <= 0 && len(e.Cost.Tiers) == 0 {
		return false
	}
	return true
}

// missingCapabilities returns the subset of required capabilities the entry lacks.
func missingCapabilities(e modelregistry.ModelEntry, required []modelregistry.ModelCapability) []modelregistry.ModelCapability {
	var missing []modelregistry.ModelCapability
	for _, c := range required {
		if !e.Supports(c) {
			missing = append(missing, c)
		}
	}
	return missing
}

// exactFit reports whether the entry exposes only required capabilities (no
// unnecessary extras). extraCount is the number of entry capabilities not in the
// required set; it is used as a small tie-breaker.
func capabilityFit(e modelregistry.ModelEntry, required []modelregistry.ModelCapability) (exact bool, extraCount int) {
	reqSet := make(map[modelregistry.ModelCapability]bool, len(required))
	for _, c := range required {
		reqSet[c] = true
	}
	for _, c := range e.Capabilities {
		if !reqSet[c] {
			extraCount++
		}
	}
	return extraCount == 0, extraCount
}

// providerAllowed reports whether the entry is permitted under allowedProviders.
// An empty allow-list means "allow all". Members are matched case-insensitively
// against the primary Provider and any APIProviders.
func providerAllowed(e modelregistry.ModelEntry, allowedProviders []string) bool {
	if len(allowedProviders) == 0 {
		return true
	}
	for _, want := range allowedProviders {
		w := strings.ToLower(strings.TrimSpace(want))
		if w == "" {
			continue
		}
		if strings.ToLower(string(e.Provider)) == w {
			return true
		}
		for _, p := range e.APIProviders {
			if strings.ToLower(string(p)) == w {
				return true
			}
		}
	}
	return false
}

// modelDisallowed reports whether the entry ID or any alias is explicitly blocked.
func modelDisallowed(e modelregistry.ModelEntry, disallowedModels []string) bool {
	for _, blocked := range disallowedModels {
		b := strings.ToLower(strings.TrimSpace(blocked))
		if b == "" {
			continue
		}
		if strings.ToLower(e.ID) == b {
			return true
		}
		for _, a := range e.Aliases {
			if strings.ToLower(a) == b {
				return true
			}
		}
	}
	return false
}

// isPreferredModel reports whether the entry matches an explicit preferred model
// by ID, APIModel, or alias.
func isPreferredModel(e modelregistry.ModelEntry, preferred string) bool {
	p := strings.ToLower(strings.TrimSpace(preferred))
	if p == "" {
		return false
	}
	if strings.ToLower(e.ID) == p || strings.ToLower(e.APIModel) == p {
		return true
	}
	for _, a := range e.Aliases {
		if strings.ToLower(a) == p {
			return true
		}
	}
	return false
}

// isPreferredProvider reports whether the entry is served by the preferred provider.
func isPreferredProvider(e modelregistry.ModelEntry, preferred string) bool {
	p := strings.ToLower(strings.TrimSpace(preferred))
	if p == "" {
		return false
	}
	if strings.ToLower(string(e.Provider)) == p {
		return true
	}
	for _, ap := range e.APIProviders {
		if strings.ToLower(string(ap)) == p {
			return true
		}
	}
	return false
}

// localMatches applies the runtime-supplied local predicate, if any.
func localMatches(e modelregistry.ModelEntry, isLocal func(modelregistry.ModelEntry) bool) bool {
	if isLocal == nil {
		return false
	}
	return isLocal(e)
}

// costExceeds reports whether a priced candidate breaches the supplied limits.
// Unknown price never "exceeds" a limit; it is handled separately by
// RequireKnownPrice.
func costExceeds(e modelregistry.ModelEntry, maxIn, maxOut *float64) (input bool, output bool) {
	if maxIn != nil && e.Cost.InputPerMillion > *maxIn {
		input = true
	}
	if maxOut != nil && e.Cost.OutputPerMillion > *maxOut {
		output = true
	}
	return input, output
}

// modelregistryCaps aliases the slice type to keep helper signatures readable.
type modelregistryCaps = []modelregistry.ModelCapability
