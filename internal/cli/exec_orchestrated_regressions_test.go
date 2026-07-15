package cli

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/scheduler"
	"github.com/Gitlawb/zero/internal/taskclass"
)

// --- Bug 1: configured model is executable but may be capability-incompatible ---

// TestConfiguredXAIExecutableCandidate confirms a configured xai/grok-4.5 profile
// yields an executable candidate (it is "available"), independent of routing.
func TestConfiguredXAIExecutableCandidate(t *testing.T) {
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
	if _, ok := byID["grok-4.5"]; !ok {
		t.Errorf("profile map missing grok-4.5")
	}
}

// TestSecurityTaskRejectsGrokForMissingReasoning routes a security task (which
// requires reasoning) against the synthetic grok-4.5 candidate and confirms it
// is rejected specifically for missing reasoning.
func TestSecurityTaskRejectsGrokForMissingReasoning(t *testing.T) {
	reg, _ := modelregistry.DefaultRegistry()
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	cands, _ := buildExecutableCandidates(config.ResolvedConfig{Providers: profiles}, &reg, fakeResolveMeta)
	req := modelrouter.Request{
		Task:           taskclass.Result{RequiredCapabilities: []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming}},
		Candidates:     cands,
		PreferredModel: "grok-4.5",
	}
	dec, err := modelrouter.Decide(req)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Selected != nil {
		t.Fatalf("expected grok-4.5 rejected, got selected %q", dec.Selected.Model.ID)
	}
	if !dec.NoCompatible {
		t.Errorf("expected NoCompatible=true")
	}
	found := false
	for _, r := range dec.Rejected {
		if !strings.EqualFold(r.ModelID, "grok-4.5") {
			continue
		}
		found = true
		hasReasoning := false
		for _, reason := range r.Reasons {
			if strings.Contains(reason.Detail, "reasoning") {
				hasReasoning = true
			}
		}
		if !hasReasoning {
			t.Errorf("rejection for grok-4.5 missing reasoning reason: %+v", r.Reasons)
		}
	}
	if !found {
		t.Errorf("grok-4.5 not present in rejected set: %+v", dec.Rejected)
	}
}

// TestImplementationTaskSelectsGrok confirms the same synthetic candidate is
// selected for an implementation task that does not require reasoning.
func TestImplementationTaskSelectsGrok(t *testing.T) {
	reg, _ := modelregistry.DefaultRegistry()
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	cands, _ := buildExecutableCandidates(config.ResolvedConfig{Providers: profiles}, &reg, fakeResolveMeta)
	req := modelrouter.Request{
		Task:           taskclass.Result{RequiredCapabilities: []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming}},
		Candidates:     cands,
		PreferredModel: "grok-4.5",
	}
	dec, err := modelrouter.Decide(req)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Selected == nil || !strings.EqualFold(dec.Selected.Model.ID, "grok-4.5") {
		t.Fatalf("expected grok-4.5 selected, got %+v", dec.Selected)
	}
}

// TestCapabilityOverrideDeclaresReasoning confirms an explicit, valid capability
// override lets the same configured model satisfy a reasoning-requiring task.
func TestCapabilityOverrideDeclaresReasoning(t *testing.T) {
	reg, _ := modelregistry.DefaultRegistry()
	profiles := []config.ProviderProfile{{
		Name:         "xai",
		Provider:     "xai",
		Model:        "grok-4.5",
		Capabilities: []string{"reasoning", "tool-calling", "streaming", "chat", "system-prompt"},
	}}
	cands, _ := buildExecutableCandidates(config.ResolvedConfig{Providers: profiles}, &reg, fakeResolveMeta)
	for _, c := range cands {
		if !c.Supports(modelregistry.ModelCapabilityReasoning) {
			t.Fatalf("override should declare reasoning, got caps %+v", c.Capabilities)
		}
	}
	req := modelrouter.Request{
		Task:           taskclass.Result{RequiredCapabilities: []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming}},
		Candidates:     cands,
		PreferredModel: "grok-4.5",
	}
	dec, err := modelrouter.Decide(req)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Selected == nil || !dec.Selected.Model.Supports(modelregistry.ModelCapabilityReasoning) {
		t.Fatalf("expected reasoning-capable grok-4.5 selected, got %+v", dec.Selected)
	}
}

// TestSyntheticCandidateNoNameInference confirms Zero never infers capabilities
// (e.g. reasoning) from a model name like "grok-4.5".
func TestSyntheticCandidateNoNameInference(t *testing.T) {
	reg, _ := modelregistry.DefaultRegistry()
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	cands, _ := buildExecutableCandidates(config.ResolvedConfig{Providers: profiles}, &reg, fakeResolveMeta)
	for _, c := range cands {
		if c.Supports(modelregistry.ModelCapabilityReasoning) {
			t.Errorf("synthetic grok-4.5 must not infer reasoning from name; caps=%+v", c.Capabilities)
		}
	}
}

// TestConfigValidationRejectsUnknownCapability confirms a non-existent capability
// name in the override is rejected at config validation.
func TestConfigValidationRejectsUnknownCapability(t *testing.T) {
	_, issues := config.ValidateBytes([]byte(`{"providers":[{"name":"xai","provider":"openai-compatible","baseURL":"https://xai.example/v1","model":"grok-4.5","capabilities":["structured-output"]}]}`))
	if len(issues) == 0 {
		t.Fatalf("expected validation issue for unknown capability")
	}
	ok := false
	for _, i := range issues {
		if strings.Contains(strings.ToLower(i.Message), "capability") || strings.Contains(i.Message, "structured-output") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("no capability-related issue; got %+v", issues)
	}
}

// TestConfigValidationAcceptsKnownCapability confirms a valid override is accepted.
func TestConfigValidationAcceptsKnownCapability(t *testing.T) {
	_, issues := config.ValidateBytes([]byte(`{"providers":[{"name":"xai","provider":"openai-compatible","baseURL":"https://xai.example/v1","model":"grok-4.5","capabilities":["reasoning","tool-calling","streaming"]}]}`))
	for _, i := range issues {
		if strings.Contains(strings.ToLower(i.Message), "capability") {
			t.Errorf("unexpected capability issue: %s", i.Message)
		}
	}
}

// injectSecurityTaskRejectingModel installs a deterministic plan whose single
// task requires reasoning and whose routing decision rejects the named model for
// missing capability, without a compatible selection.
func injectSecurityTaskRejectingModel(t *testing.T, modelID, reasonDetail string) {
	t.Helper()
	orig := orchestratedBuildPlan
	t.Cleanup(func() { orchestratedBuildPlan = orig })
	dec := modelrouter.Decision{
		Selected:     nil,
		NoCompatible: true,
		Rejected: []modelrouter.Rejection{
			{ModelID: modelID, Reasons: []modelrouter.Reason{{Signal: "capability-missing", Detail: reasonDetail}}},
		},
	}
	plan := planner.ExecutionPlan{
		PlanID:  "p-sec",
		Summary: "security review",
		Tasks: []planner.Task{
			{ID: "t-sec", Title: "security review", TaskKind: planner.KindSecurityReview, SafetyLevel: planner.SafetySafe,
				RequiredCapabilities: []modelregistry.ModelCapability{modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming}},
		},
	}
	orchestratedBuildPlan = func(prompt string, routerOpts routerFlagOptions, repoPresent bool, candidates []modelregistry.ModelEntry) (planPreviewResult, error) {
		return planPreviewResult{
			Plan:           plan,
			Results:        []planTaskResult{{Task: plan.Tasks[0], Decision: dec}},
			Classification: taskclass.Result{Primary: taskclass.KindSecurityReview},
		}, nil
	}
}

// TestOrchestratedIncompatibleModelFailsBeforeExecution confirms an explicit model
// that is configured but capability-incompatible reports "incompatible" (not
// "unavailable"), names the provider, and never constructs a provider.
func TestOrchestratedIncompatibleModelFailsBeforeExecution(t *testing.T) {
	tmp := t.TempDir()
	providerConstructed := false
	od := orchestratedDepsForProviders(t, tmp, []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "xai", "grok-4.5", nil)
	od.deps.newProvider = func(config.ProviderProfile) (agent.Provider, error) {
		providerConstructed = true
		return fakeProvider{}, nil
	}
	injectSecurityTaskRejectingModel(t, "grok-4.5", "missing required capabilities: reasoning")
	code := runOrchestratedOnce(od)
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d (provider error)", code, exitProvider)
	}
	errOut := od.stderr.(*strings.Builder).String()
	if !strings.Contains(errOut, "incompatible") {
		t.Errorf("expected incompatible message\nstderr: %s", errOut)
	}
	if !strings.Contains(errOut, "reasoning") {
		t.Errorf("expected reasoning reason\nstderr: %s", errOut)
	}
	if !strings.Contains(errOut, "xai") {
		t.Errorf("expected provider xai\nstderr: %s", errOut)
	}
	if strings.Contains(errOut, "not available") {
		t.Errorf("must not say unavailable\nstderr: %s", errOut)
	}
	if providerConstructed {
		t.Errorf("provider must not be constructed when model is incompatible")
	}
}

// TestOrchestratedIncompatibleJSONOutcome confirms the JSON error distinguishes
// an incompatible (configured-but-rejected) model via the outcome field.
func TestOrchestratedIncompatibleJSONOutcome(t *testing.T) {
	tmp := t.TempDir()
	od := orchestratedDepsForProviders(t, tmp, []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}, orchCompletedRunner(), fakeVerifyPassed, execOutputJSON, "xai", "grok-4.5", nil)
	injectSecurityTaskRejectingModel(t, "grok-4.5", "missing required capabilities: reasoning")
	code := runOrchestratedOnce(od)
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d", code, exitProvider)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, `"outcome": "incompatible"`) {
		t.Errorf("json must signal incompatible\n---\n%s", out)
	}
	if !strings.Contains(out, `"model": "grok-4.5"`) {
		t.Errorf("json must include model\n---\n%s", out)
	}
}

// TestOrchestratedIncompatibleProviderNameIsProfileName confirms a configured
// profile that uses provider_kind (and leaves the legacy provider field empty)
// still prints its profile NAME ("xai") in the incompatibility error, never the
// empty legacy field.
func TestOrchestratedIncompatibleProviderNameIsProfileName(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{Name: "xai", ProviderKind: config.ProviderKindOpenAICompatible, Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "xai", "grok-4.5", nil)
	od.deps.newProvider = func(config.ProviderProfile) (agent.Provider, error) {
		t.Errorf("provider must not be constructed when model is incompatible")
		return fakeProvider{}, nil
	}
	injectSecurityTaskRejectingModel(t, "grok-4.5", "missing required capabilities: reasoning")
	code := runOrchestratedOnce(od)
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d", code, exitProvider)
	}
	errOut := od.stderr.(*strings.Builder).String()
	want := `requested model "grok-4.5" is configured through provider "xai" but is incompatible with this task: missing required capabilities: reasoning`
	if !strings.Contains(errOut, want) {
		t.Errorf("expected profile name in error\nstderr: %s", errOut)
	}
	if strings.Contains(errOut, `provider ""`) {
		t.Errorf("provider name must not be empty\nstderr: %s", errOut)
	}
	if strings.Contains(errOut, "openai-compatible") {
		t.Errorf("error must use profile name, not provider kind\nstderr: %s", errOut)
	}
}

// TestOrchestratedIncompatibleProviderNameFallbackToKind confirms that when a
// profile has no name, the error falls back to the provider kind rather than
// rendering an empty string.
func TestOrchestratedIncompatibleProviderNameFallbackToKind(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{ProviderKind: config.ProviderKindOpenAICompatible, Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "xai", "grok-4.5", nil)
	od.deps.newProvider = func(config.ProviderProfile) (agent.Provider, error) {
		t.Errorf("provider must not be constructed when model is incompatible")
		return fakeProvider{}, nil
	}
	injectSecurityTaskRejectingModel(t, "grok-4.5", "missing required capabilities: reasoning")
	code := runOrchestratedOnce(od)
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d", code, exitProvider)
	}
	errOut := od.stderr.(*strings.Builder).String()
	if !strings.Contains(errOut, `configured through provider "openai-compatible"`) {
		t.Errorf("expected fallback to provider kind\nstderr: %s", errOut)
	}
}

// TestOrchestratedIncompatibleJSONProviderIsProfileName confirms the JSON error
// payload carries the configured profile name as "provider".
func TestOrchestratedIncompatibleJSONProviderIsProfileName(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{Name: "xai", ProviderKind: config.ProviderKindOpenAICompatible, Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputJSON, "xai", "grok-4.5", nil)
	injectSecurityTaskRejectingModel(t, "grok-4.5", "missing required capabilities: reasoning")
	code := runOrchestratedOnce(od)
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d", code, exitProvider)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, `"provider": "xai"`) {
		t.Errorf("json provider must be profile name xai\n---\n%s", out)
	}
}

// TestOrchestratedUnavailablePreservesProviderName confirms the unavailable path
// preserves the configured profile name even when the legacy provider field is
// empty (modern configs use provider_kind).
func TestOrchestratedUnavailablePreservesProviderName(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{Name: "xai", ProviderKind: config.ProviderKindOpenAICompatible, Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "xai", "gpt-4o-mini", nil)
	code := runOrchestratedOnce(od)
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d", code, exitProvider)
	}
	errOut := od.stderr.(*strings.Builder).String()
	want := `requested model "gpt-4o-mini" is not available through provider "xai"`
	if !strings.Contains(errOut, want) {
		t.Errorf("expected profile name in unavailable error\nstderr: %s", errOut)
	}
	if strings.Contains(errOut, `provider ""`) {
		t.Errorf("provider name must not be empty\nstderr: %s", errOut)
	}
}

// TestOrchestratedNoCompatibleJSONOutcome confirms the no-compatible path emits a
// distinct outcome with the rejected reasons.
func TestOrchestratedNoCompatibleJSONOutcome(t *testing.T) {
	tmp := t.TempDir()
	od := orchestratedDepsForProviders(t, tmp, []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}, orchCompletedRunner(), fakeVerifyPassed, execOutputJSON, "", "", []string{"grok-4.5"})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitProvider {
		t.Fatalf("exit = %d, want %d", code, exitProvider)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, `"outcome": "no-compatible"`) {
		t.Errorf("json must signal no-compatible\n---\n%s", out)
	}
}

// --- Bug 2: full sequential mode must never render once-mode output ---

// TestOrchestratedNoCompatibleUsesSequentialBanner confirms the no-compatible path
// in full sequential mode uses the sequential DAG banner, never the once banner.
func TestOrchestratedNoCompatibleUsesSequentialBanner(t *testing.T) {
	tmp := t.TempDir()
	profiles := []config.ProviderProfile{{Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	od := orchestratedDepsForProviders(t, tmp, profiles, orchCompletedRunner(), fakeVerifyPassed, execOutputText, "", "", []string{"grok-4.5"})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitProvider {
		t.Fatalf("exit = %d, want exitProvider", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "ORCHESTRATED EXECUTION — sequential DAG") {
		t.Errorf("expected sequential banner\n---\n%s", out)
	}
	if strings.Contains(out, "ORCHESTRATED EXECUTION — one task only") {
		t.Errorf("must not use once-mode banner\n---\n%s", out)
	}
}

// TestOrchestratedBlockedUsesSequentialBanner confirms the blocked (approval)
// renderer in full sequential mode uses the sequential DAG banner and never the
// once banner.
func TestOrchestratedBlockedUsesSequentialBanner(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, orchCompletedRunner(), fakeVerifyPassed, execOutputText)
	preview := planPreviewResult{Plan: planner.ExecutionPlan{PlanID: "p1", Summary: "plan"}}
	task := planner.Task{ID: "t1", Title: "blocked task"}
	code := renderOrchestratedBlocked(od, "orchestrated", preview, task, "requires explicit approval")
	if code != exitIncomplete {
		t.Fatalf("exit = %d, want exitIncomplete", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "ORCHESTRATED EXECUTION — sequential DAG") {
		t.Errorf("expected sequential banner\n---\n%s", out)
	}
	if strings.Contains(out, "ORCHESTRATED EXECUTION — one task only") {
		t.Errorf("must not use once-mode banner\n---\n%s", out)
	}
	if !strings.Contains(out, "Blocked:") {
		t.Errorf("expected blocked message\n---\n%s", out)
	}
}

// TestOrchestratedBlockedOnceBanner confirms the same renderer keeps the once
// banner when mode is orchestrated-once.
func TestOrchestratedBlockedOnceBanner(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, orchCompletedRunner(), fakeVerifyPassed, execOutputText)
	preview := planPreviewResult{Plan: planner.ExecutionPlan{PlanID: "p1", Summary: "plan"}}
	task := planner.Task{ID: "t1", Title: "blocked task"}
	code := renderOrchestratedBlocked(od, "orchestrated-once", preview, task, "requires explicit approval")
	if code != exitIncomplete {
		t.Fatalf("exit = %d, want exitIncomplete", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "ORCHESTRATED EXECUTION — one task only") {
		t.Errorf("expected once banner\n---\n%s", out)
	}
	if !strings.Contains(out, "Stopped after one task by --orchestrated-once.") {
		t.Errorf("expected once footer\n---\n%s", out)
	}
}

// TestOrchestratedNoModelSequentialBanner confirms the no-compatible renderer in
// full sequential mode uses the sequential DAG banner and emits the rejected
// reasons with their provider.
func TestOrchestratedNoModelSequentialBanner(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, orchCompletedRunner(), fakeVerifyPassed, execOutputText)
	preview := planPreviewResult{Plan: planner.ExecutionPlan{PlanID: "p1", Summary: "plan"}}
	task := planner.Task{ID: "t1", Title: "task"}
	dec := modelrouter.Decision{
		NoCompatible: true,
		Rejected:     []modelrouter.Rejection{{ModelID: "grok-4.5", Reasons: []modelrouter.Reason{{Signal: "capability-missing", Detail: "missing required capabilities: reasoning"}}}},
	}
	byID := map[string]config.ProviderProfile{"grok-4.5": {Name: "xai", Provider: "xai", Model: "grok-4.5"}}
	code := renderOrchestratedNoModel(od, "orchestrated", byID, preview, task, dec)
	if code != exitProvider {
		t.Fatalf("exit = %d, want exitProvider", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "ORCHESTRATED EXECUTION — sequential DAG") {
		t.Errorf("expected sequential banner\n---\n%s", out)
	}
	if strings.Contains(out, "ORCHESTRATED EXECUTION — one task only") {
		t.Errorf("must not use once-mode banner\n---\n%s", out)
	}
	if !strings.Contains(out, "Routing unavailable:") {
		t.Errorf("expected rejected list header\n---\n%s", out)
	}
	if !strings.Contains(out, "grok-4.5 [xai]") {
		t.Errorf("expected rejected model with provider\n---\n%s", out)
	}
	if !strings.Contains(out, "missing required capabilities: reasoning") {
		t.Errorf("expected rejection reason\n---\n%s", out)
	}
}

// TestOrchestratedNoTaskSequentialBanner confirms the no-ready-task renderer in
// full sequential mode uses the sequential DAG banner.
func TestOrchestratedNoTaskSequentialBanner(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, orchCompletedRunner(), fakeVerifyPassed, execOutputText)
	preview := planPreviewResult{Plan: planner.ExecutionPlan{PlanID: "p1", Summary: "plan"}}
	state := scheduler.ExecutionState{}
	code := renderOrchestratedNoTask(od, "orchestrated", preview, state)
	if code != exitIncomplete {
		t.Fatalf("exit = %d, want exitIncomplete", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "ORCHESTRATED EXECUTION — sequential DAG") {
		t.Errorf("expected sequential banner\n---\n%s", out)
	}
	if strings.Contains(out, "ORCHESTRATED EXECUTION — one task only") {
		t.Errorf("must not use once-mode banner\n---\n%s", out)
	}
}

// TestOrchestratedNoTaskOnceBanner confirms the no-ready-task renderer keeps the
// once banner when mode is orchestrated-once (regression guard for #12).
func TestOrchestratedNoTaskOnceBanner(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, orchCompletedRunner(), fakeVerifyPassed, execOutputText)
	preview := planPreviewResult{Plan: planner.ExecutionPlan{PlanID: "p1", Summary: "plan"}}
	code := renderOrchestratedNoTask(od, "orchestrated-once", preview, scheduler.ExecutionState{})
	if code != exitIncomplete {
		t.Fatalf("exit = %d, want exitIncomplete", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "ORCHESTRATED EXECUTION — one task only") {
		t.Errorf("expected once banner\n---\n%s", out)
	}
	if !strings.Contains(out, "Stopped after one task by --orchestrated-once.") {
		t.Errorf("expected once footer\n---\n%s", out)
	}
}

// TestOrchestratedModeBannerMapping locks the mode->banner/footer mapping used by
// every sequential and once exit path.
func TestOrchestratedModeBannerMapping(t *testing.T) {
	if got := orchestratedBanner("orchestrated-once", false); got != "ORCHESTRATED EXECUTION — one task only" {
		t.Errorf("once banner = %q", got)
	}
	if got := orchestratedBanner("orchestrated", false); got != "ORCHESTRATED EXECUTION — sequential DAG" {
		t.Errorf("sequential banner = %q", got)
	}
	if got := orchestratedBanner("orchestrated", true); got != "ORCHESTRATED EXECUTION — sequential DAG + read-only parallel batches" {
		t.Errorf("parallel banner = %q", got)
	}
	if got := orchestratedFooter("orchestrated-once"); got != "Stopped after one task by --orchestrated-once.\n" {
		t.Errorf("once footer = %q", got)
	}
	if got := orchestratedFooter("orchestrated"); got != "Stopped by --orchestrated.\n" {
		t.Errorf("sequential footer = %q", got)
	}
}

// TestOrchestratedSequentialRunsAllTasksUnbounded confirms full sequential mode
// (MaxTasks=0) executes the entire DAG, not just one task.
func TestOrchestratedSequentialRunsAllTasksUnbounded(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, orchCompletedRunner(), fakeVerifyPassed, execOutputText)
	injectTwoTaskPlan(t)
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want exitSuccess", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Executed tasks (2)") {
		t.Errorf("unbounded mode should run all tasks\n---\n%s", out)
	}
}

// TestOrchestratedOnceRunsExactlyOneTask confirms once mode (MaxTasks=1) runs
// exactly one task and uses the once banner.
func TestOrchestratedOnceRunsExactlyOneTask(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, orchCompletedRunner(), fakeVerifyPassed, execOutputText)
	injectTwoTaskPlan(t)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want exitSuccess", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "ORCHESTRATED EXECUTION — one task only") {
		t.Errorf("expected once banner\n---\n%s", out)
	}
	if strings.Contains(out, "Executed tasks (2)") {
		t.Errorf("once mode must not run 2 tasks\n---\n%s", out)
	}
}
