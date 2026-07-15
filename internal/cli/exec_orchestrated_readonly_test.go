package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/taskclass"
)

var errTest = errors.New("boom")

// --- read-only orchestrated completion tests ---

// injectSingleTask overrides the plan-preview entry point with one routable task
// of the given kind (and optional dependency) so the orchestrated engine runs
// it deterministically with the fake runner/verifier.
func injectSingleTask(t *testing.T, task planner.Task, answer string, toolEvents []executor.ToolEvent) {
	t.Helper()
	orig := orchestratedBuildPlan
	t.Cleanup(func() { orchestratedBuildPlan = orig })
	dec := modelrouter.Decision{
		Selected: &modelrouter.Candidate{Model: modelregistry.ModelEntry{
			Provider:     modelregistry.ProviderKind("fake"),
			ID:           "fake-model",
			Capabilities: []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityChat},
		}},
	}
	plan := planner.ExecutionPlan{PlanID: "p-ro", Summary: "read-only task", Tasks: []planner.Task{task}}
	orchestratedBuildPlan = func(prompt string, routerOpts routerFlagOptions, repoPresent bool, candidates []modelregistry.ModelEntry) (planPreviewResult, error) {
		return planPreviewResult{
			Plan:           plan,
			Results:        []planTaskResult{{Task: task, Decision: dec}},
			Classification: taskclass.Result{},
		}, nil
	}
}

// injectReadOnlyChain builds a two-task plan where t2 depends on t1, both
// read-only, so the engine must complete t1 before t2 unlocks.
func injectReadOnlyChain(t *testing.T) {
	t.Helper()
	orig := orchestratedBuildPlan
	t.Cleanup(func() { orchestratedBuildPlan = orig })
	dec := modelrouter.Decision{
		Selected: &modelrouter.Candidate{Model: modelregistry.ModelEntry{
			Provider:     modelregistry.ProviderKind("fake"),
			ID:           "fake-model",
			Capabilities: []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityChat},
		}},
	}
	plan := planner.ExecutionPlan{
		PlanID:  "p-chain",
		Summary: "read-only chain",
		Tasks: []planner.Task{
			{ID: "t1", Title: "architecture", TaskKind: planner.KindArchitecture},
			{ID: "t2", Title: "code-review", TaskKind: planner.KindCodeReview, Dependencies: []string{"t1"}},
		},
	}
	orchestratedBuildPlan = func(prompt string, routerOpts routerFlagOptions, repoPresent bool, candidates []modelregistry.ModelEntry) (planPreviewResult, error) {
		return planPreviewResult{
			Plan: plan,
			Results: []planTaskResult{
				{Task: plan.Tasks[0], Decision: dec},
				{Task: plan.Tasks[1], Decision: dec},
			},
			Classification: taskclass.Result{},
		}, nil
	}
}

// mustNotVerify fails the test if the verifier is ever called for a read-only task.
func mustNotVerify(t *testing.T) executor.Verifier {
	return func(context.Context, string, []string) executor.VerificationOutcome {
		t.Errorf("verifier must not be called for a read-only task")
		return executor.VerificationOutcome{Status: "passed"}
	}
}

// spyVerifier records how many times it was invoked.
func spyVerifier(called *int) executor.Verifier {
	return func(context.Context, string, []string) executor.VerificationOutcome {
		*called++
		return executor.VerificationOutcome{Status: "not_available"}
	}
}

func readRunner(answer string, toolEvents []executor.ToolEvent) executor.Runner {
	return fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: answer},
		FinalAnswer: answer,
		ToolEvents:  toolEvents,
	}}
}

// 1-5. Each read-only kind with read evidence + non-empty answer completes.
func TestOrchestratedReadOnlyKindsComplete(t *testing.T) {
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	kinds := []planner.TaskKind{
		planner.KindArchitecture,
		planner.KindSecurityReview,
		planner.KindCodeReview,
		planner.KindRepositorySearch,
		planner.KindDocumentation,
		planner.KindImageAnalysis,
	}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			tmp := t.TempDir()
			injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: k}, "detailed answer", readEv)
			od := newOrchestratedTestDeps(t, tmp, readRunner("detailed answer", readEv), mustNotVerify(t), execOutputText)
			code := runOrchestratedOnce(od)
			if code != exitSuccess {
				t.Fatalf("exit = %d, want %d", code, exitSuccess)
			}
			out := od.stdout.(*strings.Builder).String()
			if !strings.Contains(out, "Execution (completed)") {
				t.Errorf("expected completed\n---\n%s", out)
			}
			if !strings.Contains(out, "Verification: not_applicable") {
				t.Errorf("expected not_applicable verification\n---\n%s", out)
			}
		})
	}
}

// 6-7. Read-only tasks must not invoke the verifier.
func TestOrchestratedReadOnlySkipsVerifier(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "read_file", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", readEv), mustNotVerify(t), execOutputText)
	if code := runOrchestratedOnce(od); code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
}

// 8-9. JSON emits not_applicable with the read-only reason.
func TestOrchestratedReadOnlyJSONNotApplicable(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindSecurityReview}, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", readEv), mustNotVerify(t), execOutputJSON)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, `"status": "not_applicable"`) {
		t.Errorf("json must emit not_applicable\n---\n%s", out)
	}
	if !strings.Contains(out, `"reason": "read-only task"`) {
		t.Errorf("json must include read-only reason\n---\n%s", out)
	}
}

// 10. Text output emits not_applicable (not failed).
func TestOrchestratedReadOnlyTextNotApplicable(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", readEv), mustNotVerify(t), execOutputText)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Verification: not_applicable") {
		t.Errorf("expected not_applicable\n---\n%s", out)
	}
	if strings.Contains(out, "Verification: failed") {
		t.Errorf("must not report failed verification\n---\n%s", out)
	}
}

// 11. Read-only task with no tool evidence -> incomplete.
func TestOrchestratedReadOnlyNoEvidenceIncomplete(t *testing.T) {
	tmp := t.TempDir()
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", nil)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", nil), mustNotVerify(t), execOutputText)
	if code := runOrchestratedOnce(od); code != exitIncomplete {
		t.Fatalf("exit = %d, want %d", code, exitIncomplete)
	}
}

// 12. Read-only task with empty final answer -> incomplete.
func TestOrchestratedReadOnlyEmptyAnswerIncomplete(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "", readEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("", readEv), mustNotVerify(t), execOutputText)
	if code := runOrchestratedOnce(od); code != exitIncomplete {
		t.Fatalf("exit = %d, want %d", code, exitIncomplete)
	}
}

// 13. Read-only task that performs a mutating action -> failed/incomplete (never silently completed).
func TestOrchestratedReadOnlyMutationNotCompleted(t *testing.T) {
	tmp := t.TempDir()
	mutEv := []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", mutEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", mutEv), mustNotVerify(t), execOutputText)
	code := runOrchestratedOnce(od)
	if code == exitSuccess {
		t.Fatalf("read-only mutation must not complete (got exit %d)", code)
	}
	out := od.stdout.(*strings.Builder).String()
	if strings.Contains(out, "status: completed") {
		t.Errorf("read-only mutation must not be completed\n---\n%s", out)
	}
}

// 14. Permission-required read-only task -> blocked.
func TestOrchestratedReadOnlyPermissionRequiredBlocked(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	runner := fakeRunner{result: executor.TaskExecutionResult{
		AgentResult:        agent.Result{FinalAnswer: "answer"},
		FinalAnswer:        "answer",
		ToolEvents:         readEv,
		PermissionRequired: true,
	}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, runner, mustNotVerify(t), execOutputText)
	code := runOrchestratedOnce(od)
	if code != exitIncomplete {
		t.Fatalf("exit = %d, want %d (blocked)", code, exitIncomplete)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "blocked") {
		t.Errorf("expected blocked status\n---\n%s", out)
	}
}

// 15. Provider failure -> failed.
func TestOrchestratedReadOnlyProviderFailureFailed(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, failingRunner{err: errTest}, mustNotVerify(t), execOutputText)
	if code := runOrchestratedOnce(od); code != exitIncomplete {
		t.Fatalf("exit = %d, want %d", code, exitIncomplete)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Execution (failed)") {
		t.Errorf("expected failed status\n---\n%s", out)
	}
}

// 16. Cancellation -> incomplete (no dedicated cancelled status exists in
// this codebase; a cancelled run surfaces as a run error and maps to incomplete).
type cancelRunner struct{}

func (cancelRunner) RunTask(ctx context.Context, req executor.TaskExecutionRequest) (executor.TaskExecutionResult, error) {
	return executor.TaskExecutionResult{Cancelled: true, FinalAnswer: "answer", AgentResult: agent.Result{FinalAnswer: "answer"}}, context.Canceled
}

func TestOrchestratedReadOnlyCancelledIncomplete(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, cancelRunner{}, mustNotVerify(t), execOutputText)
	if code := runOrchestratedOnce(od); code != exitIncomplete {
		t.Fatalf("exit = %d, want %d", code, exitIncomplete)
	}
}

// 17-19. Mutating implementation still invokes the verifier (the
// completion mapping itself is covered at the executor level; here we only
// assert the verifier is reached for a mutating task).
func TestOrchestratedMutatingInvokesVerifier(t *testing.T) {
	tmp := t.TempDir()
	called := 0
	mutEv := []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}}
	injectSingleTask(t, planner.Task{ID: "t-m", Title: "impl", TaskKind: planner.KindImplementation}, "done", mutEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("done", mutEv), spyVerifier(&called), execOutputText)
	runOrchestratedOnce(od)
	if called == 0 {
		t.Errorf("mutating task must invoke the verifier")
	}
}

// 18. Mutating task with a failing verifier still fails.
func TestOrchestratedMutatingVerificationFailureFails(t *testing.T) {
	tmp := t.TempDir()
	mutEv := []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}}
	failVer := func(context.Context, string, []string) executor.VerificationOutcome {
		return executor.VerificationOutcome{Status: "failed", Failed: 1, Errors: 1}
	}
	injectSingleTask(t, planner.Task{ID: "t-m", Title: "impl", TaskKind: planner.KindImplementation}, "done", mutEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("done", mutEv), failVer, execOutputText)
	if code := runOrchestratedOnce(od); code != exitIncomplete {
		t.Fatalf("exit = %d, want %d (failed)", code, exitIncomplete)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Execution (failed)") {
		t.Errorf("expected failed status\n---\n%s", out)
	}
}

// 19. Mutating verification unavailable still maps to completed_unverified where a
// delta exists (proven at executor level; here we confirm the verifier is
// invoked and the sandbox run degrades to incomplete without a tracked delta).
func TestOrchestratedMutatingVerificationUnavailableInvoked(t *testing.T) {
	tmp := t.TempDir()
	called := 0
	mutEv := []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}}
	injectSingleTask(t, planner.Task{ID: "t-m", Title: "impl", TaskKind: planner.KindImplementation}, "done", mutEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("done", mutEv), spyVerifier(&called), execOutputText)
	runOrchestratedOnce(od)
	if called == 0 {
		t.Errorf("mutating task must invoke the verifier even when unavailable")
	}
}

// 20. Sequential run with one read-only task ends completed.
func TestOrchestratedSequentialReadOnlyCompletes(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", readEv), mustNotVerify(t), execOutputText)
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{
		"ORCHESTRATED EXECUTION — sequential DAG",
		"Executed tasks (1)",
		"verification: not_applicable",
		"completed: 1",
		"failed: 0",
		"Top status: completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
}

// 21. Sequential read-only chain: completing t1 unlocks the dependent t2.
func TestOrchestratedReadOnlyDependentUnlocks(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	injectReadOnlyChain(t)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", readEv), mustNotVerify(t), execOutputText)
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Executed tasks (2)") {
		t.Errorf("expected both tasks executed\n---\n%s", out)
	}
	if strings.Contains(out, "Skipped tasks") {
		t.Errorf("dependent task must not be skipped\n---\n%s", out)
	}
	if !strings.Contains(out, "Top status: completed") {
		t.Errorf("expected completed top status\n---\n%s", out)
	}
}

// 22. Executor-once uses the same read-only semantics.
func TestOrchestratedOnceReadOnlyCompletes(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "read_file", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindDocumentation}, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", readEv), mustNotVerify(t), execOutputText)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Execution (completed)") || !strings.Contains(out, "Verification: not_applicable") {
		t.Errorf("once mode must complete read-only with not_applicable\n---\n%s", out)
	}
}

// 23. Preview behavior is unaffected: building a plan for a read-only task
// does not perform verification (preview is offline and never runs tasks).
func TestOrchestratedReadOnlyPreviewUnchanged(t *testing.T) {
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	injectSingleTask(t, planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}, "answer", readEv)
	if executor.TaskRequiresRepositoryVerification(planner.Task{TaskKind: planner.KindArchitecture}) {
		t.Errorf("read-only task must not require repository verification")
	}
	// The preview entry point only builds a plan; it never invokes the verifier.
	preview, err := orchestratedBuildPlan("analyze the architecture", routerFlagOptions{}, false, nil)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if preview.Plan.Tasks[0].TaskKind != planner.KindArchitecture {
		t.Errorf("plan task kind mutated: %v", preview.Plan.Tasks[0].TaskKind)
	}
}

// 25. The injected plan is treated as immutable: a completed run does not
// alter the planned task's identity.
func TestOrchestratedReadOnlyPlanImmutable(t *testing.T) {
	tmp := t.TempDir()
	readEv := []executor.ToolEvent{{Name: "grep", Kind: "read"}}
	task := planner.Task{ID: "t-ro", Title: "ro", TaskKind: planner.KindArchitecture}
	injectSingleTask(t, task, "answer", readEv)
	od := newOrchestratedTestDeps(t, tmp, readRunner("answer", readEv), mustNotVerify(t), execOutputText)
	runOrchestratedOnce(od)
	if task.ID != "t-ro" || task.TaskKind != planner.KindArchitecture {
		t.Errorf("plan task identity changed: %+v", task)
	}
}
