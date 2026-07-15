package cli

import (
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/providers"
)

// syntheticContextWindow is a neutral placeholder used only for routing tiering.
// It is never presented as a factual model capability and only acts as a
// last-resort ordering signal when a configured model has no curated metadata.
const syntheticContextWindow = 128_000

// buildExecutableCandidates derives the orchestrated routing candidate set from
// the user's configured, executable provider profiles — NOT the curated global
// registry. Each configured profile that resolves to a concrete api model and
// provider kind becomes exactly one candidate. When the configured model also
// exists in the curated registry we adopt its richer factual metadata
// (capabilities, context window, pricing); otherwise we synthesize a minimal
// factual candidate from the resolved runtime profile, without inventing
// capabilities or pricing.
//
// It returns the candidate entries plus a map from each candidate's lowercased
// id to the provider profile that produced it, so the orchestrated runner can
// construct the exact provider the router selected instead of mutating the
// active profile in place.
func buildExecutableCandidates(
	resolved config.ResolvedConfig,
	registry *modelregistry.Registry,
	resolveMeta func(config.ProviderProfile, providers.Options) (providers.RuntimeMetadata, error),
) ([]modelregistry.ModelEntry, map[string]config.ProviderProfile) {
	var candidates []modelregistry.ModelEntry
	byID := make(map[string]config.ProviderProfile)
	seen := make(map[string]bool)

	for _, profile := range resolved.Providers {
		if !config.HasProviderProfile(profile) {
			continue
		}
		meta, err := resolveMeta(profile, providers.Options{ModelRegistry: registry})
		if err != nil {
			// Not executable / not resolvable; skip rather than invent a candidate.
			continue
		}
		id := strings.TrimSpace(profile.Model)
		if id == "" {
			id = strings.TrimSpace(meta.APIModel)
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if seen[key] {
			continue
		}

		if registry != nil {
			if entry, ok := registry.Get(id); ok {
				candidates = append(candidates, applyCapabilityOverride(entry, profile))
				byID[key] = profile
				seen[key] = true
				continue
			}
		}
		seen[key] = true
		candidates = append(candidates, applyCapabilityOverride(syntheticCandidate(id, meta), profile))
		byID[key] = profile
	}
	return candidates, byID
}

// applyCapabilityOverride honors the optional, factual provider-profile
// capability override. When the profile declares capabilities, they replace the
// candidate's capability set exactly — the user is asserting these are factual.
// Otherwise the candidate keeps its curated (registry) or synthesized (default)
// capabilities untouched. Zero never invents capabilities from the model name.
func applyCapabilityOverride(entry modelregistry.ModelEntry, profile config.ProviderProfile) modelregistry.ModelEntry {
	if len(profile.Capabilities) == 0 {
		return entry
	}
	caps := make([]modelregistry.ModelCapability, 0, len(profile.Capabilities))
	for _, c := range profile.Capabilities {
		caps = append(caps, modelregistry.ModelCapability(strings.ToLower(strings.TrimSpace(c))))
	}
	entry.Capabilities = caps
	return entry
}

// syntheticCandidate builds a minimal factual ModelEntry for a configured model
// that is absent from the curated registry. It carries only what routing needs
// and never invents capabilities, pricing, or context limits beyond a neutral
// ordering placeholder.
func syntheticCandidate(id string, meta providers.RuntimeMetadata) modelregistry.ModelEntry {
	return modelregistry.ModelEntry{
		ID:       id,
		APIModel: strings.TrimSpace(meta.APIModel),
		Provider: modelregistry.ProviderKind(meta.ProviderKind),
		ContextLimits: modelregistry.ContextLimits{
			ContextWindow: syntheticContextWindow,
		},
		Capabilities: []modelregistry.ModelCapability{
			modelregistry.ModelCapabilityChat,
			modelregistry.ModelCapabilityStreaming,
			modelregistry.ModelCapabilityToolCalling,
			modelregistry.ModelCapabilitySystemPrompt,
		},
		Status: modelregistry.ModelStatusActive,
		// Cost left empty: price is unknown and must never be treated as free.
	}
}

// candidateServesModel reports whether any candidate matches the requested model
// (by id, api model, or alias) and, when an explicit provider is requested, is
// served by that provider.
func modelEntryMatchesIDOrAlias(entry modelregistry.ModelEntry, model string) bool {
	m := strings.ToLower(model)
	if strings.ToLower(entry.ID) == m || strings.ToLower(entry.APIModel) == m {
		return true
	}
	for _, a := range entry.Aliases {
		if strings.ToLower(a) == m {
			return true
		}
	}
	return false
}

func modelEntryMatchesIDOrAliasPtr(selected *modelrouter.Candidate, model string) bool {
	if selected == nil {
		return false
	}
	return modelEntryMatchesIDOrAlias(selected.Model, model)
}
