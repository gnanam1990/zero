package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/providers"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/verify"
)

// fakeResolveMeta is a network-free resolver: it maps a configured profile to
// its effective provider kind and api model deterministically. It never touches
// the curated registry or the network.
func fakeResolveMeta(p config.ProviderProfile, _ providers.Options) (providers.RuntimeMetadata, error) {
	return providers.RuntimeMetadata{
		ProviderKind: config.ProviderKind(strings.ToLower(strings.TrimSpace(p.Provider))),
		APIModel:     strings.TrimSpace(p.Model),
	}, nil
}

// orchestratedDepsForProviders builds a runnable orchestrated-once dependency set
// from an explicit list of configured provider profiles, a fake runtime-metadata
// resolver, and optional router flags. It isolates the orchestrated routing path
// from any network or credential dependency.
func orchestratedDepsForProviders(t *testing.T, tmp string, profiles []config.ProviderProfile, runner executor.Runner, verifier executor.Verifier, format execOutputFormat, routerProvider, routerModel string, denyModels []string) orchestratedOnceDeps {
	reg, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("modelregistry: %v", err)
	}
	active := ""
	var primary config.ProviderProfile
	if len(profiles) > 0 {
		active = profiles[0].Name
		primary = profiles[0]
	}
	return orchestratedOnceDeps{
		options: execOptions{outputFormat: format, autonomy: "low", routerProvider: routerProvider, model: routerModel, denyModels: denyModels},
		stdout:  &strings.Builder{},
		stderr:  &strings.Builder{},
		deps: appDeps{
			getwd: func() (string, error) { return tmp, nil },
			newProvider: func(config.ProviderProfile) (agent.Provider, error) {
				return fakeProvider{}, nil
			},
			newSessionStore: func() *sessions.Store {
				return sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
			},
			detectVerifyPlan: func(string) (verify.Plan, error) { return verify.Plan{}, nil },
			runVerify: func(context.Context, verify.Plan, verify.RunOptions) verify.Report {
				return verify.Report{}
			},
			skillsDir: func() string { return "" },
		},
		workspaceRoot:          tmp,
		trustRoot:              tmp,
		registry:               newCoreRegistry(tmp),
		modelRegistry:          reg,
		resolved:               config.ResolvedConfig{ActiveProvider: active, Providers: profiles, Provider: primary, MaxTurns: 10},
		permissionMode:         agent.PermissionModeAuto,
		sessionTitle:           "test",
		prompt:                 "implement a feature that parses durations from strings",
		runner:                 runner,
		verifier:               verifier,
		resolveRuntimeMetadata: fakeResolveMeta,
	}
}

func orchCompletedRunner() executor.Runner {
	return fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "done"},
		FinalAnswer: "done",
		ToolEvents:  []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}},
	}}
}

// Scenario 1 + 6: with only xai/grok-4.5 configured, the executable candidate
// set contains grok-4.5 and must NOT include any curated model (gpt-4o-mini).
func TestBuildExecutableCandidatesOnlyXAIExcludesCurated(t *testing.T) {
	reg, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("modelregistry: %v", err)
	}
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	cands, byID := buildExecutableCandidates(config.ResolvedConfig{Providers: profiles}, &reg, fakeResolveMeta)

	if len(cands) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(cands))
	}
	if cands[0].ID != "grok-4.5" {
		t.Errorf("candidate ID = %q, want grok-4.5", cands[0].ID)
	}
	if !strings.EqualFold(string(cands[0].Provider), "xai") {
		t.Errorf("candidate provider = %q, want xai", cands[0].Provider)
	}
	for _, c := range cands {
		if c.ID == "gpt-4o-mini" {
			t.Fatalf("curated model gpt-4o-mini leaked into executable candidate set")
		}
	}
	// The profile map must let the runner reconstruct the exact xai profile.
	if _, ok := byID["grok-4.5"]; !ok {
		t.Fatalf("profile map missing grok-4.5")
	}
}

// Scenario 7: a multi-provider configuration yields one candidate per configured
// profile, adopting curated metadata where available and synthesizing otherwise.
func TestBuildExecutableCandidatesMultiProvider(t *testing.T) {
	reg, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("modelregistry: %v", err)
	}
	profiles := []config.ProviderProfile{
		{Name: "xai", Provider: "xai", Model: "grok-4.5"},
		{Name: "openai", Provider: "openai", Model: "gpt-4o-mini"},
	}
	cands, _ := buildExecutableCandidates(config.ResolvedConfig{Providers: profiles}, &reg, fakeResolveMeta)
	if len(cands) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(cands))
	}

	var grok, mini modelregistry.ModelEntry
	for _, c := range cands {
		switch c.ID {
		case "grok-4.5":
			grok = c
		case "gpt-4o-mini":
			mini = c
		}
	}
	if grok.ID == "" {
		t.Fatal("grok-4.5 candidate missing")
	}
	if mini.ID == "" {
		t.Fatal("gpt-4o-mini candidate missing")
	}
	// gpt-4o-mini is curated, so it carries factual pricing.
	if mini.Cost.InputPerMillion <= 0 {
		t.Errorf("gpt-4o-mini should adopt curated pricing, got %v", mini.Cost.InputPerMillion)
	}
	// grok-4.5 is synthesized: no pricing, neutral context window.
	if grok.Cost.InputPerMillion != 0 || grok.Cost.OutputPerMillion != 0 {
		t.Errorf("synthetic grok-4.5 must not invent pricing, got %+v", grok.Cost)
	}
}

// Scenario 4: alias resolution — a configured profile whose model is a curated
// alias resolves to the canonical curated entry (with its factual metadata).
func TestBuildExecutableCandidatesAliasResolution(t *testing.T) {
	reg, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("modelregistry: %v", err)
	}
	// "openai:gpt-4o-mini" is a known alias of gpt-4o-mini in the curated catalog.
	profiles := []config.ProviderProfile{{Name: "openai", Provider: "openai", Model: "openai:gpt-4o-mini"}}
	cands, _ := buildExecutableCandidates(config.ResolvedConfig{Providers: profiles}, &reg, fakeResolveMeta)
	if len(cands) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(cands))
	}
	if cands[0].ID != "gpt-4o-mini" {
		t.Errorf("alias did not resolve to canonical id, got %q", cands[0].ID)
	}
	if cands[0].Cost.InputPerMillion <= 0 {
		t.Errorf("alias candidate should carry curated pricing, got %v", cands[0].Cost.InputPerMillion)
	}
}

// Scenario 2: no router flags -> the single configured model is routed and runs.
func TestOrchestratedOnlyXAINoFlagsRoutesGrok(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "", "", nil)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d (success)", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "provider: xai") {
		t.Errorf("routing missing provider: xai\n---\n%s", out)
	}
	if !strings.Contains(out, "model: grok-4.5") {
		t.Errorf("routing missing model: grok-4.5\n---\n%s", out)
	}
}

// Scenario 3: explicit --provider/--model are honored and select the exact model.
func TestOrchestratedExplicitModelProviderSelectsGrok(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "xai", "grok-4.5", nil)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d (success)", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "provider: xai") || !strings.Contains(out, "model: grok-4.5") {
		t.Errorf("explicit --provider/--model not honored\n---\n%s", out)
	}
}

// Scenario 5: an explicit model that no configured provider serves fails before
// any provider is constructed, with the required message.
func TestOrchestratedUnavailableModelFailsBeforeExecution(t *testing.T) {
	tmp := t.TempDir()
	// Only xai/grok-4.5 is configured; request gpt-4o-mini through xai.
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "xai", "gpt-4o-mini", nil)
	code := runOrchestratedOnce(od)
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d (provider error)", code, exitProvider)
	}
	errOut := od.stderr.(*strings.Builder).String()
	want := `requested model "gpt-4o-mini" is not available through provider "xai"`
	if !strings.Contains(errOut, want) {
		t.Errorf("missing expected error message %q\nstderr: %s", want, errOut)
	}
}

// Scenario: multi-provider with --model selects the requested configured model.
func TestOrchestratedMultiProviderModelSelection(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{
		{Name: "xai", Provider: "xai", Model: "grok-4.5"},
		{Name: "openai", Provider: "openai", Model: "gpt-4o-mini"},
	}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "", "gpt-4o-mini", nil)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d (success)", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "provider: openai") || !strings.Contains(out, "model: gpt-4o-mini") {
		t.Errorf("explicit --model did not select gpt-4o-mini\n---\n%s", out)
	}
}

// Scenario: a hard --allow-provider filter restricts the candidate set.
func TestOrchestratedAllowProviderFilter(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{
		{Name: "xai", Provider: "xai", Model: "grok-4.5"},
		{Name: "openai", Provider: "openai", Model: "gpt-4o-mini"},
	}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "openai", "", nil)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d (success)", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "provider: openai") {
		t.Errorf("--allow-provider did not restrict to openai\n---\n%s", out)
	}
}

// Scenario 8: when every candidate is rejected (here via --deny-model), no
// compatible model is found and execution is prevented.
func TestOrchestratedNoCompatiblePreventsExecution(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "", "", []string{"grok-4.5"})
	code := runOrchestratedOnce(od)
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d (provider/no-model)", code, exitProvider)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "No compatible model") {
		t.Errorf("expected no-compatible-model report\n---\n%s", out)
	}
}

// Scenario: a curated openai model configured alone routes to that model.
func TestOrchestratedCuratedOpenAIModelRoutes(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{Name: "openai", Provider: "openai", Model: "gpt-4o-mini"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "", "", nil)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d (success)", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "provider: openai") || !strings.Contains(out, "model: gpt-4o-mini") {
		t.Errorf("did not route to configured gpt-4o-mini\n---\n%s", out)
	}
}

// Scenario 10: buildPlanPreview with nil candidates uses the curated registry
// (preview commands), while an executable candidate set isolates routing.
func TestBuildPlanPreviewCandidateIsolation(t *testing.T) {
	reg, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("modelregistry: %v", err)
	}
	// Curated (nil) preview must include gpt-4o-mini.
	curated, err := buildPlanPreview("implement auth", routerFlagOptions{}, true, nil)
	if err != nil {
		t.Fatalf("curated buildPlanPreview: %v", err)
	}
	curatedHasMini := false
	for _, r := range curated.Results {
		if r.Decision.Selected != nil && r.Decision.Selected.Model.ID == "gpt-4o-mini" {
			curatedHasMini = true
		}
	}
	if !curatedHasMini {
		t.Errorf("curated preview should be able to route gpt-4o-mini")
	}

	// Executable-only preview (xai/grok-4.5) must NOT route gpt-4o-mini.
	xaiOnly, _ := buildExecutableCandidates(config.ResolvedConfig{Providers: []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}}, &reg, fakeResolveMeta)
	orch, err := buildPlanPreview("implement auth", routerFlagOptions{}, true, xaiOnly)
	if err != nil {
		t.Fatalf("orchestrated buildPlanPreview: %v", err)
	}
	for _, r := range orch.Results {
		if r.Decision.Selected != nil && r.Decision.Selected.Model.ID == "gpt-4o-mini" {
			t.Errorf("executable candidate set leaked curated gpt-4o-mini")
		}
	}
}

// Scenario 11: alias-based --model selection through a configured alias.
func TestOrchestratedAliasModelSelection(t *testing.T) {
	tmp := t.TempDir()
	// Profile configured with the curated alias "openai:gpt-4o-mini".
	profiles := []config.ProviderProfile{{Name: "openai", Provider: "openai", Model: "openai:gpt-4o-mini"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "", "openai:gpt-4o-mini", nil)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d (success)", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "model: gpt-4o-mini") {
		t.Errorf("alias --model did not resolve to gpt-4o-mini\n---\n%s", out)
	}
}
