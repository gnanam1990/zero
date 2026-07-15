package modelregistry

import (
	"strings"
	"testing"
)

// makeModel builds a minimal, structurally valid ModelEntry for tests.
func makeModel(id, display, apiModel string, provider ProviderKind, caps ...ModelCapability) ModelEntry {
	return ModelEntry{
		ID:           id,
		DisplayName:  display,
		APIModel:     apiModel,
		Provider:     provider,
		Capabilities: caps,
		ContextLimits: ContextLimits{
			ContextWindow:   128_000,
			MaxOutputTokens: 4_096,
		},
		Cost: ModelCost{
			Currency:              "USD",
			Unit:                  "per_1m_tokens",
			InputPerMillion:       1,
			OutputPerMillion:      2,
			CachedInputPerMillion: 0.5,
			Source:                "test",
			SourceLastVerified:    "2026-06-04",
		},
		Status:  ModelStatusActive,
		Aliases: []string{id + "-alias"},
	}
}

// TestExtendedCapabilitiesAreValidVocabulary confirms the finer-grained
// capability constants are part of the registry's vocabulary and can be carried
// by a model entry.
func TestExtendedCapabilitiesAreValidVocabulary(t *testing.T) {
	for _, cap := range []ModelCapability{
		ModelCapabilityParallelToolCalls,
		ModelCapabilityThinkingTokens,
		ModelCapabilityImageGeneration,
		ModelCapabilityEmbeddings,
		ModelCapabilityAudio,
	} {
		if !ValidModelCapability(cap) {
			t.Fatalf("expected %q to be a valid capability", cap)
		}
	}

	m := makeModel("m", "M", "m", ProviderOpenAI,
		ModelCapabilityParallelToolCalls,
		ModelCapabilityThinkingTokens,
		ModelCapabilityImageGeneration,
		ModelCapabilityAudio,
	)
	if err := m.Validate(); err != nil {
		t.Fatalf("extended capabilities should validate: %v", err)
	}
	for _, cap := range []ModelCapability{
		ModelCapabilityParallelToolCalls,
		ModelCapabilityThinkingTokens,
		ModelCapabilityImageGeneration,
		ModelCapabilityAudio,
	} {
		if !m.Supports(cap) {
			t.Fatalf("model should report capability %q", cap)
		}
	}

	// Embeddings is a valid standalone capability.
	emb := makeModel("emb", "Emb", "emb", ProviderOpenAI, ModelCapabilityEmbeddings)
	if err := emb.Validate(); err != nil {
		t.Fatalf("standalone embeddings should validate: %v", err)
	}
	if !emb.Supports(ModelCapabilityEmbeddings) {
		t.Fatal("model should report embeddings capability")
	}
}

// TestRejectsEmbeddingsWithChatCapabilities ensures an embeddings model is not
// also tagged as a general-purpose chat/vision/tool model.
func TestRejectsEmbeddingsWithChatCapabilities(t *testing.T) {
	emb := makeModel("emb", "Emb", "emb", ProviderOpenAI,
		ModelCapabilityEmbeddings, ModelCapabilityVision)
	if err := emb.Validate(); err == nil {
		t.Fatal("embeddings+vision should be rejected")
	}
	emb.Capabilities = ModelCapabilities{ModelCapabilityEmbeddings}
	if err := emb.Validate(); err != nil {
		t.Fatalf("standalone embeddings model should validate: %v", err)
	}
}

// TestRejectsReasoningWithoutEngagement ensures a reasoning model exposes at
// least one way to engage reasoning (efforts or thinking tokens).
func TestRejectsReasoningWithoutEngagement(t *testing.T) {
	bad := makeModel("r", "R", "r", ProviderOpenAI, ModelCapabilityReasoning)
	if err := bad.Validate(); err == nil {
		t.Fatal("reasoning without efforts/thinking tokens should be rejected")
	}

	withEfforts := makeModel("re", "RE", "re", ProviderOpenAI, ModelCapabilityReasoning)
	withEfforts.ReasoningEfforts = []ReasoningEffort{ReasoningEffortMedium}
	if err := withEfforts.Validate(); err != nil {
		t.Fatalf("reasoning with efforts should validate: %v", err)
	}

	withThinking := makeModel("rt", "RT", "rt", ProviderOpenAI,
		ModelCapabilityReasoning, ModelCapabilityThinkingTokens)
	if err := withThinking.Validate(); err != nil {
		t.Fatalf("reasoning with thinking tokens should validate: %v", err)
	}
}

// TestNewRegistryRejectsDuplicateIDs confirms the registry rejects duplicate
// canonical model ids (canonical identity is the model id).
func TestNewRegistryRejectsDuplicateIDs(t *testing.T) {
	a := makeModel("dup", "Dup", "dup", ProviderOpenAI, ModelCapabilityChat)
	b := makeModel("dup", "Dup2", "dup2", ProviderOpenAI, ModelCapabilityChat)
	if _, err := NewRegistry([]ModelEntry{a, b}); err == nil {
		t.Fatal("duplicate model id should be rejected")
	}
	if _, err := NewRegistry([]ModelEntry{a, makeModel("other", "Other", "other", ProviderOpenAI, ModelCapabilityChat)}); err != nil {
		t.Fatalf("distinct ids should build: %v", err)
	}
}

// TestDefaultRegistryStillBuilds guards against the curated catalog regressing
// under the (combination) validation rules.
func TestDefaultRegistryStillBuilds(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("default registry should build: %v", err)
	}
	if len(registry.List(ListOptions{})) == 0 {
		t.Fatal("default registry should contain models")
	}
}

// TestProviderQualifiedResolution confirms provider-qualified patterns resolve
// (registry keys are normalized, provider-qualified aliases map to one model).
func TestProviderQualifiedResolution(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("default registry: %v", err)
	}
	// Every curated entry carries a "provider:model" alias; resolving it must
	// return the same canonical model.
	for _, entry := range registry.List(ListOptions{}) {
		for _, alias := range entry.Aliases {
			if !strings.Contains(alias, ":") {
				continue
			}
			got, ok := registry.Get(alias)
			if !ok {
				t.Fatalf("alias %q for %q did not resolve", alias, entry.ID)
			}
			if got.ID != entry.ID {
				t.Fatalf("alias %q resolved to %q, want %q", alias, got.ID, entry.ID)
			}
		}
	}
}
