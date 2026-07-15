package modelrouter

import (
	"reflect"
	"testing"

	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/taskclass"
)

func mkModel(id string, provider modelregistry.ProviderKind, caps []modelregistry.ModelCapability, in, out float64, status modelregistry.ModelStatus) modelregistry.ModelEntry {
	return modelregistry.ModelEntry{
		ID:            id,
		DisplayName:   id,
		APIModel:      id,
		Provider:      provider,
		ContextLimits: modelregistry.ContextLimits{ContextWindow: 200_000, MaxOutputTokens: 8_000},
		Capabilities:  caps,
		Cost:          modelregistry.ModelCost{Currency: "USD", Unit: "per_1m_tokens", InputPerMillion: in, OutputPerMillion: out, CachedInputPerMillion: 0, Source: "test", SourceLastVerified: "2026-01-01"},
		Status:        status,
		Aliases:       []string{id},
	}
}

// mkNoPrice builds an entry with no factual pricing metadata.
func mkNoPrice(id string, provider modelregistry.ProviderKind, caps []modelregistry.ModelCapability) modelregistry.ModelEntry {
	e := mkModel(id, provider, caps, 0, 0, modelregistry.ModelStatusActive)
	e.Cost = modelregistry.ModelCost{}
	return e
}

func task(caps ...modelregistry.ModelCapability) taskclass.Result {
	return taskclass.Result{RequiredCapabilities: caps}
}

func TestPreferredCompatibleModelSelected(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("cheap", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 0.1, 0.2, modelregistry.ModelStatusActive),
		mkModel("pref", modelregistry.ProviderAnthropic, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 5, 15, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, PreferredModel: "pref"}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Selected == nil || dec.Selected.Model.ID != "pref" {
		t.Fatalf("selected = %v, want pref", dec.Selected)
	}
}

func TestPreferredModelRejectedWhenCapabilityMissing(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("pref", modelregistry.ProviderAnthropic, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 5, 15, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityVision), Candidates: cands, PreferredModel: "pref"}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Selected != nil {
		t.Fatalf("preferred model should be rejected, got %v", dec.Selected)
	}
	if len(dec.Rejected) != 1 || dec.Rejected[0].ModelID != "pref" {
		t.Fatalf("expected one rejection for pref, got %+v", dec.Rejected)
	}
	found := false
	for _, r := range dec.Rejected[0].Reasons {
		if r.Signal == "capability-missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected capability-missing reason, got %+v", dec.Rejected[0].Reasons)
	}
}

func TestVisionTaskRejectsNonVision(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("no-vision", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
		mkModel("vision", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityVision}, 1, 2, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityVision), Candidates: cands}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Selected == nil || dec.Selected.Model.ID != "vision" {
		t.Fatalf("selected = %v, want vision", dec.Selected)
	}
	for _, r := range dec.Rejected {
		if r.ModelID == "no-vision" {
			for _, reason := range r.Reasons {
				if reason.Signal == "capability-missing" {
					return
				}
			}
			t.Fatalf("no-vision should be rejected for missing vision: %+v", r.Reasons)
		}
	}
}

func TestToolTaskRejectsWithoutTools(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("no-tools", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityChat}, 1, 2, modelregistry.ModelStatusActive),
		mkModel("tools", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 1 || dec.Ranked[0].Model.ID != "tools" {
		t.Fatalf("expected only tools ranked, got %+v", dec.Ranked)
	}
}

func TestReasoningRequiredWhenClassifierSaysSo(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("no-reason", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
		mkModel("reason", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning}, 1, 2, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityReasoning), Candidates: cands}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Selected == nil || !dec.Selected.Model.Supports(modelregistry.ModelCapabilityReasoning) {
		t.Fatalf("reasoning model should win, got %+v", dec.Selected)
	}
}

func TestAllowedProviderFiltering(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("anthropic", modelregistry.ProviderAnthropic, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
		mkModel("openai", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, AllowedProviders: []string{"anthropic"}}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 1 || dec.Ranked[0].Model.ID != "anthropic" {
		t.Fatalf("only anthropic should survive, got %+v", dec.Ranked)
	}
	if len(dec.Rejected) != 1 || dec.Rejected[0].ModelID != "openai" {
		t.Fatalf("openai should be rejected, got %+v", dec.Rejected)
	}
}

func TestDisallowedModelFiltering(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("good", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
		mkModel("bad", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, DisallowedModels: []string{"bad"}}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 1 || dec.Ranked[0].Model.ID != "good" {
		t.Fatalf("only good should survive, got %+v", dec.Ranked)
	}
}

func TestRequireKnownPriceRejectsMissing(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkNoPrice("unknown-price", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}),
		mkModel("priced", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, RequireKnownPrice: true}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 1 || dec.Ranked[0].Model.ID != "priced" {
		t.Fatalf("only priced should survive, got %+v", dec.Ranked)
	}
	for _, r := range dec.Rejected {
		if r.ModelID == "unknown-price" {
			for _, reason := range r.Reasons {
				if reason.Signal == "price-missing" {
					return
				}
			}
			t.Fatalf("expected price-missing reason, got %+v", r.Reasons)
		}
	}
}

func TestInputCostLimit(t *testing.T) {
	max := 1.0
	cands := []modelregistry.ModelEntry{
		mkModel("expensive", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 5, 2, modelregistry.ModelStatusActive),
		mkModel("cheap", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 0.5, 1, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, MaxInputCost: &max}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 1 || dec.Ranked[0].Model.ID != "cheap" {
		t.Fatalf("only cheap should survive, got %+v", dec.Ranked)
	}
}

func TestOutputCostLimit(t *testing.T) {
	max := 1.0
	cands := []modelregistry.ModelEntry{
		mkModel("expensive", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 5, modelregistry.ModelStatusActive),
		mkModel("cheap", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 0.5, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, MaxOutputCost: &max}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 1 || dec.Ranked[0].Model.ID != "cheap" {
		t.Fatalf("only cheap should survive, got %+v", dec.Ranked)
	}
}

func TestMissingPriceNotTreatedAsFree(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkNoPrice("unknown-price", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}),
	}
	// A tight input-cost limit must NOT reject the missing-price model, because
	// missing price is never assumed to be free.
	max := 0.0
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, MaxInputCost: &max}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 1 {
		t.Fatalf("missing-price model should survive cost limits, got %+v", dec)
	}
}

func TestDeprecatedModelBehavior(t *testing.T) {
	// Deprecated + active alternative present + not preferred => rejected.
	active := mkModel("active", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive)
	dep := mkModel("dep", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusDeprecated)
	dep.Deprecation = &modelregistry.DeprecationRule{FallbackID: "active", WarningMsg: "use active"}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: []modelregistry.ModelEntry{dep, active}}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Selected == nil || dec.Selected.Model.ID != "active" {
		t.Fatalf("active should be selected, got %+v", dec.Selected)
	}
	if len(dec.Rejected) != 1 || dec.Rejected[0].ModelID != "dep" {
		t.Fatalf("dep should be rejected, got %+v", dec.Rejected)
	}

	// Deprecated but explicitly preferred => kept.
	req2 := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: []modelregistry.ModelEntry{dep, active}, PreferredModel: "dep"}
	dec2, err := Decide(req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec2.Selected == nil || dec2.Selected.Model.ID != "dep" {
		t.Fatalf("dep should be kept when preferred, got %+v", dec2.Selected)
	}

	// Only deprecated candidates (no active alternative) => kept.
	depOnly := mkModel("dep2", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusDeprecated)
	req3 := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: []modelregistry.ModelEntry{depOnly}}
	dec3, err := Decide(req3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec3.Selected == nil || dec3.Selected.Model.ID != "dep2" {
		t.Fatalf("sole deprecated model should be kept, got %+v", dec3.Selected)
	}
}

func TestUnavailableModelRejected(t *testing.T) {
	bad := mkModel("retired", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive)
	bad.Status = modelregistry.ModelStatus("retired") // not a valid lifecycle status
	good := mkModel("good", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive)
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: []modelregistry.ModelEntry{bad, good}}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 1 || dec.Ranked[0].Model.ID != "good" {
		t.Fatalf("only good should survive, got %+v", dec.Ranked)
	}
	found := false
	for _, r := range dec.Rejected {
		if r.ModelID == "retired" {
			for _, reason := range r.Reasons {
				if reason.Signal == "invalid" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("retired should be rejected as invalid, got %+v", dec.Rejected)
	}
}

func TestNoCompatibleModels(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("no-vision", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityVision), Candidates: cands}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Selected != nil {
		t.Fatalf("expected no selection, got %+v", dec.Selected)
	}
	if !dec.NoCompatible {
		t.Fatalf("expected NoCompatible=true")
	}
}

func TestEmptyCandidatesIsError(t *testing.T) {
	_, err := Decide(Request{Task: task(), Candidates: nil})
	if err == nil {
		t.Fatal("expected error for empty candidates")
	}
}

func TestStableRanking(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("b", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
		mkModel("a", modelregistry.ProviderAnthropic, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 5, 15, modelregistry.ModelStatusActive),
		mkModel("c", modelregistry.ProviderGoogle, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 0.5, 1, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, PreferredProvider: "anthropic"}
	first, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, err := Decide(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("ranking not stable:\n first=%+v\n again=%+v", first, again)
		}
	}
}

func TestStableReasonOrdering(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("m", modelregistry.ProviderAnthropic, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 5, 15, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, PreferredProvider: "anthropic", PreferredModel: "m"}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	signals := []string{}
	for _, r := range dec.Ranked[0].Reasons {
		signals = append(signals, r.Signal)
	}
	want := []string{"capability", "exact-fit", "provider-preferred", "preferred-model", "cost-known"}
	if !reflect.DeepEqual(signals, want) {
		t.Fatalf("reason order = %v, want %v", signals, want)
	}
}

func TestNoInputMutation(t *testing.T) {
	orig := []modelregistry.ModelEntry{
		mkModel("m", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
	}
	caps := orig[0].Capabilities
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: orig}
	if _, err := Decide(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(orig[0].Capabilities, caps) {
		t.Fatalf("candidate capabilities mutated: %v", orig[0].Capabilities)
	}
	if orig[0].Cost.InputPerMillion != 1 {
		t.Fatalf("candidate cost mutated: %v", orig[0].Cost)
	}
}

func TestDuplicateCandidateIDs(t *testing.T) {
	dup := mkModel("dup", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive)
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: []modelregistry.ModelEntry{dup, dup}}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	count := 0
	for _, c := range dec.Ranked {
		if c.Model.ID == "dup" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate ID should appear once, got %d", count)
	}
	if len(dec.Rejected) != 0 {
		t.Fatalf("no rejections expected, got %+v", dec.Rejected)
	}
}

func TestPreferredProviderIsRankingSignalOnly(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("other", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
		mkModel("pref-provider", modelregistry.ProviderAnthropic, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 5, 15, modelregistry.ModelStatusActive),
	}
	req := Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, PreferredProvider: "anthropic"}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 2 {
		t.Fatalf("both models should rank (provider is not a filter), got %d", len(dec.Ranked))
	}
	if dec.Ranked[0].Model.ID != "pref-provider" {
		t.Fatalf("preferred provider should rank first, got %+v", dec.Ranked[0].Model.ID)
	}
}

func TestRegistryOrderDeterministic(t *testing.T) {
	// Two equally-scored candidates must resolve by registry order, not by map
	// iteration or any nondeterministic source.
	a := mkModel("a", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive)
	b := mkModel("b", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive)
	for run := 0; run < 20; run++ {
		dec, err := Decide(Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: []modelregistry.ModelEntry{a, b}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dec.Ranked[0].Model.ID != "a" {
			t.Fatalf("run %d: expected 'a' first by registry order, got %q", run, dec.Ranked[0].Model.ID)
		}
	}
}

func TestIntegrationWithTaskclass(t *testing.T) {
	cls := taskclass.Classify(taskclass.Request{Prompt: "analyze this screenshot and describe the layout", HasImages: true})
	if len(cls.RequiredCapabilities) == 0 {
		t.Fatalf("expected required capabilities from taskclass, got none")
	}
	// Required capabilities must include vision for an image task.
	hasVision := false
	for _, c := range cls.RequiredCapabilities {
		if c == modelregistry.ModelCapabilityVision {
			hasVision = true
		}
	}
	if !hasVision {
		t.Fatalf("image task should require vision, got %v", cls.RequiredCapabilities)
	}
	cands := []modelregistry.ModelEntry{
		mkModel("no-vision", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
		mkModel("vision", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityVision, modelregistry.ModelCapabilityStreaming}, 1, 2, modelregistry.ModelStatusActive),
	}
	dec, err := Decide(Request{Task: cls, Candidates: cands})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Selected == nil || dec.Selected.Model.ID != "vision" {
		t.Fatalf("integration: vision model should be selected, got %+v", dec.Selected)
	}
}

func TestContradictoryConstraintsError(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("m", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
	}
	req := Request{
		Task:              task(modelregistry.ModelCapabilityToolCalling),
		Candidates:        cands,
		PreferredProvider: "anthropic",
		AllowedProviders:  []string{"openai"},
	}
	if _, err := Decide(req); err == nil {
		t.Fatal("expected error for contradictory provider constraints")
	}
}

func TestLocalOnlyRequiresPredicate(t *testing.T) {
	cands := []modelregistry.ModelEntry{
		mkModel("m", modelregistry.ProviderOpenAI, []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling}, 1, 2, modelregistry.ModelStatusActive),
	}
	if _, err := Decide(Request{Task: task(modelregistry.ModelCapabilityToolCalling), Candidates: cands, LocalOnly: true}); err == nil {
		t.Fatal("expected error when LocalOnly set without IsLocal predicate")
	}

	// With a predicate, local models pass and non-local are rejected.
	req := Request{
		Task:       task(modelregistry.ModelCapabilityToolCalling),
		Candidates: cands,
		LocalOnly:  true,
		IsLocal: func(e modelregistry.ModelEntry) bool {
			return e.Provider == modelregistry.ProviderOpenAICompatible
		},
	}
	dec, err := Decide(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dec.Ranked) != 0 {
		t.Fatalf("openai model should be rejected as non-local, got %+v", dec.Ranked)
	}
}
