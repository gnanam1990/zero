package modelrouter

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/modelregistry"
)

// Decide evaluates the candidates against the task classification and runtime
// constraints, returning a deterministic, explainable ranking. It never calls a
// provider, never executes a model, and never mutates the input.
func Decide(req Request) (Decision, error) {
	if err := validateRequest(req); err != nil {
		return Decision{}, err
	}

	required := requiredKnownCapabilities(req.Task.RequiredCapabilities)

	// hasActiveAlternative is used by the deprecated-model rule: a deprecated
	// model may survive only if it is explicitly preferred or no non-deprecated
	// candidate exists at all.
	hasActiveAlternative := false
	for _, c := range req.Candidates {
		if c.Status == modelregistry.ModelStatusActive || c.Status == modelregistry.ModelStatusPreview {
			hasActiveAlternative = true
			break
		}
	}

	var ranked []Candidate
	var rejected []Rejection
	seen := make(map[string]bool)

	for idx, entry := range req.Candidates {
		id := strings.ToLower(strings.TrimSpace(entry.ID))
		if id == "" {
			id = fmt.Sprintf("@invalid-%d", idx)
		}
		if seen[id] {
			// Duplicate candidate ID: keep the first occurrence only.
			continue
		}
		seen[id] = true

		if rej := evaluate(req, entry, required, hasActiveAlternative); rej != nil {
			rejected = append(rejected, *rej)
			continue
		}
		cand := scoreCandidate(req, entry, required, idx)
		ranked = append(ranked, cand)
	}

	sortRanked(ranked)

	decision := Decision{Rejected: rejected}
	if len(ranked) > 0 {
		top := ranked[0]
		decision.Selected = &top
		decision.Ranked = ranked
	} else {
		decision.NoCompatible = true
	}

	return decision, nil
}

// validateRequest returns an error only for invalid router requests. A normal
// "no compatible model" outcome is NOT an error.
func validateRequest(req Request) error {
	if len(req.Candidates) == 0 {
		return errors.New("modelrouter: no candidate models supplied")
	}
	if req.MaxInputCost != nil && *req.MaxInputCost < 0 {
		return errors.New("modelrouter: MaxInputCost must be non-negative")
	}
	if req.MaxOutputCost != nil && *req.MaxOutputCost < 0 {
		return errors.New("modelrouter: MaxOutputCost must be non-negative")
	}
	if req.LocalOnly && req.IsLocal == nil {
		return errors.New("modelrouter: LocalOnly requested without an IsLocal predicate")
	}
	if req.PreferredProvider != "" && len(req.AllowedProviders) > 0 {
		allowed := false
		for _, p := range req.AllowedProviders {
			if strings.EqualFold(p, req.PreferredProvider) {
				allowed = true
				break
			}
		}
		if !allowed {
			return errors.New("modelrouter: preferred provider not in allowed providers (contradictory constraints)")
		}
	}
	return nil
}

// evaluate applies the hard filters in a fixed order and returns a Rejection if
// the entry is excluded, or nil if it survives. Reasons are appended in a
// deterministic order.
func evaluate(req Request, entry modelregistry.ModelEntry, required []modelregistry.ModelCapability, hasActiveAlternative bool) *Rejection {
	var reasons []Reason

	if entryInvalid(entry) {
		reasons = append(reasons, Reason{
			Signal: "invalid",
			Detail: "model entry is invalid (missing id, capabilities, or invalid status/price)",
		})
		return &Rejection{ModelID: entry.ID, Reasons: reasons}
	}

	if missing := missingCapabilities(entry, required); len(missing) > 0 {
		reasons = append(reasons, Reason{
			Signal: "capability-missing",
			Detail: "missing required capabilities: " + joinCaps(missing),
		})
	}

	if !providerAllowed(entry, req.AllowedProviders) {
		reasons = append(reasons, Reason{
			Signal: "provider-disallowed",
			Detail: "provider not in allowed set: " + providersString(entry),
		})
	}

	if modelDisallowed(entry, req.DisallowedModels) {
		reasons = append(reasons, Reason{
			Signal: "model-disallowed",
			Detail: "model is explicitly disallowed: " + entry.ID,
		})
	}

	if req.LocalOnly && !localMatches(entry, req.IsLocal) {
		reasons = append(reasons, Reason{
			Signal: "local-only",
			Detail: "LocalOnly is set but model is not local",
		})
	}

	if req.RequireKnownPrice && !entryHasKnownPrice(entry) {
		reasons = append(reasons, Reason{
			Signal: "price-missing",
			Detail: "known price required but model has no pricing",
		})
	}

	if entryHasKnownPrice(entry) {
		if in, out := costExceeds(entry, req.MaxInputCost, req.MaxOutputCost); in || out {
			if in {
				reasons = append(reasons, Reason{
					Signal: "cost-input",
					Detail: fmt.Sprintf("input price $%.2f exceeds limit $%.2f", entry.Cost.InputPerMillion, *req.MaxInputCost),
				})
			}
			if out {
				reasons = append(reasons, Reason{
					Signal: "cost-output",
					Detail: fmt.Sprintf("output price $%.2f exceeds limit $%.2f", entry.Cost.OutputPerMillion, *req.MaxOutputCost),
				})
			}
		}
	}

	if entry.Status == modelregistry.ModelStatusDeprecated &&
		!isPreferredModel(entry, req.PreferredModel) && hasActiveAlternative {
		detail := "deprecated model rejected; an active alternative exists"
		if entry.Deprecation != nil && entry.Deprecation.FallbackID != "" {
			detail += " (consider upgrade target " + entry.Deprecation.FallbackID + ")"
		}
		reasons = append(reasons, Reason{Signal: "lifecycle-deprecated", Detail: detail})
	}

	if len(reasons) > 0 {
		return &Rejection{ModelID: entry.ID, Reasons: reasons}
	}
	return nil
}

// scoreCandidate computes the deterministic integer score and explanatory
// reasons for a surviving candidate.
func scoreCandidate(req Request, entry modelregistry.ModelEntry, required []modelregistry.ModelCapability, idx int) Candidate {
	score := 0
	var reasons []Reason

	capDetail := "satisfies required capabilities"
	if len(required) > 0 {
		capDetail += ": " + joinCaps(required)
	} else {
		capDetail += " (none required)"
	}
	reasons = append(reasons, Reason{Signal: "capability", Detail: capDetail})

	exact, extra := capabilityFit(entry, required)
	if exact {
		score += exactFitBonus
		reasons = append(reasons, Reason{Signal: "exact-fit", Detail: "exact capability fit (no unnecessary capabilities)"})
	} else if extra > 0 {
		score -= extraCapPenalty * extra
		reasons = append(reasons, Reason{Signal: "capability-extra", Detail: fmt.Sprintf("includes %d unnecessary capability(ies)", extra)})
	}

	if isPreferredProvider(entry, req.PreferredProvider) {
		score += preferredProviderBonus
		reasons = append(reasons, Reason{Signal: "provider-preferred", Detail: "preferred provider matched: " + req.PreferredProvider})
	}

	if isPreferredModel(entry, req.PreferredModel) {
		score += preferredModelBonus
		reasons = append(reasons, Reason{Signal: "preferred-model", Detail: "explicit preferred model matched: " + req.PreferredModel})
	}

	if req.LocalOnly && localMatches(entry, req.IsLocal) {
		reasons = append(reasons, Reason{Signal: "local", Detail: "local model (LocalOnly satisfied)"})
	}

	if entryHasKnownPrice(entry) {
		reasons = append(reasons, Reason{
			Signal: "cost-known",
			Detail: fmt.Sprintf("known price input=$%.2f output=$%.2f source=%s", entry.Cost.InputPerMillion, entry.Cost.OutputPerMillion, entry.Cost.Source),
		})
		combined := entry.Cost.InputPerMillion + entry.Cost.OutputPerMillion
		score -= int(combined * costPenaltyFactor)
	}

	return Candidate{Model: entry, Score: score, Reasons: reasons}
}

// sortRanked orders by score desc, then original candidate index asc (registry
// order), then model ID asc — a fully deterministic tie-break.
func sortRanked(ranked []Candidate) {
	// Preserve original index via a parallel slice before sorting in place.
	type indexed struct {
		cand Candidate
		idx  int
	}
	// The idx is already embedded by construction order; we re-derive it from a
	// stable counter to avoid relying on slice position.
	ordered := make([]indexed, len(ranked))
	for i, c := range ranked {
		ordered[i] = indexed{cand: c, idx: i}
	}
	sort.Slice(ordered, func(a, b int) bool {
		if ordered[a].cand.Score != ordered[b].cand.Score {
			return ordered[a].cand.Score > ordered[b].cand.Score
		}
		if ordered[a].idx != ordered[b].idx {
			return ordered[a].idx < ordered[b].idx
		}
		return ordered[a].cand.Model.ID < ordered[b].cand.Model.ID
	})
	for i, o := range ordered {
		ranked[i] = o.cand
	}
}

func joinCaps(caps []modelregistry.ModelCapability) string {
	parts := make([]string, len(caps))
	for i, c := range caps {
		parts[i] = string(c)
	}
	return strings.Join(parts, ", ")
}

func providersString(entry modelregistry.ModelEntry) string {
	seen := make(map[string]bool)
	var parts []string
	add := func(p modelregistry.ProviderKind) {
		key := string(p)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		parts = append(parts, key)
	}
	add(entry.Provider)
	for _, p := range entry.APIProviders {
		add(p)
	}
	return strings.Join(parts, ", ")
}
