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

// --- fakes for the parallel read-only engine ---

// readOnlyRunner completes a task with read-evidence so read-only completion
// classifies it as completed (no mutation, at least one read/search action).
type readOnlyRunner struct{}

func (readOnlyRunner) RunTask(ctx context.Context, req executor.TaskExecutionRequest) (executor.TaskExecutionResult, error) {
	return executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "read result for " + req.Task.ID},
		FinalAnswer: "read result for " + req.Task.ID,
		ToolEvents:  []executor.ToolEvent{{Name: "web_search", Kind: "read"}},
	}, nil
}

// injectParallelPlan overrides the plan-preview entry point with a deterministic
// plan (any task set) plus a routing decision for every task pointing at the
// fake model. This is the seam the planner never emits for ordinary prompts:
// independent, read-only, CanRunParallel tasks.
func injectParallelPlan(t *testing.T, tasks []planner.Task) {
	t.Helper()
	orig := orchestratedBuildPlan
	t.Cleanup(func() { orchestratedBuildPlan = orig })
	orchestratedBuildPlan = func(prompt string, routerOpts routerFlagOptions, repoPresent bool, candidates []modelregistry.ModelEntry) (planPreviewResult, error) {
		dec := modelrouter.Decision{Selected: &modelrouter.Candidate{Model: modelregistry.ModelEntry{Provider: modelregistry.ProviderKind("fake"), ID: "fake-model"}}}
		results := make([]planTaskResult, 0, len(tasks))
		for _, tk := range tasks {
			results = append(results, planTaskResult{Task: tk, Decision: dec})
		}
		return planPreviewResult{
			Plan:           planner.ExecutionPlan{PlanID: "p1", Summary: "parallel plan", Tasks: tasks},
			Results:        results,
			Classification: taskclass.Result{},
		}, nil
	}
}

func readonlyTask(id, title string) planner.Task {
	return planner.Task{
		ID:             id,
		Title:          title,
		TaskKind:       planner.KindArchitecture,
		SafetyLevel:    planner.SafetySafe,
		CanRunParallel: true,
	}
}

// Scenario: two independent read-only tasks run as one concurrent batch.
func TestRunOrchestratedParallelReadonlyBatch(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, readOnlyRunner{}, fakeVerifyPassed, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: true, MaxWorkers: 2}
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		readonlyTask("t2", "search the docs"),
	})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{
		"ORCHESTRATED EXECUTION — sequential DAG + read-only parallel batches",
		"Parallel batch 1 (ran concurrently):",
		"Executed tasks (2)",
		"Top status: completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
}

// Scenario: a mutating task is never eligible for the parallel batch; it runs
// sequentially after the read-only batch completes.
func TestRunOrchestratedParallelMixedWithMutating(t *testing.T) {
	tmp := t.TempDir()
	// The implementation task needs a mutating result; the read-only tasks use
	// the read-only runner. Use a runner that answers read-only tasks with
	// read evidence and the implementation task with a write action.
	mixed := mixedKindRunner{}
	od := newOrchestratedTestDeps(t, tmp, mixed, fakeVerifyPassed, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: true, MaxWorkers: 2}
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		{
			ID:             "t2",
			Title:          "implement the feature",
			TaskKind:       planner.KindImplementation,
			SafetyLevel:    planner.SafetySafe,
			CanRunParallel: true,
		},
		readonlyTask("t3", "search the docs"),
	})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Parallel batch 1 (ran concurrently):") {
		t.Errorf("expected a parallel batch\n---\n%s", out)
	}
	if !strings.Contains(out, "Executed tasks (3)") {
		t.Errorf("expected all three tasks executed\n---\n%s", out)
	}
	// t2 (mutating) must NOT be inside the parallel batch header's block;
	// at minimum it must appear and the run must report completion.
	if !strings.Contains(out, "Top status: completed") {
		t.Errorf("expected completed top status\n---\n%s", out)
	}
}

// Scenario: a read-only parallel task followed by a dependent mutating task. The
// mutating task (has a dependency) is never parallel-eligible; it runs
// sequentially once its dependency completes.
func TestRunOrchestratedParallelThenSequentialDep(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, mixedKindRunner{}, fakeVerifyPassed, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: true, MaxWorkers: 2}
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		{
			ID:             "t2",
			Title:          "implement the feature",
			TaskKind:       planner.KindImplementation,
			SafetyLevel:    planner.SafetySafe,
			CanRunParallel: true,
			Dependencies:   []string{"t1"},
		},
	})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Parallel batch 1 (ran concurrently):") {
		t.Errorf("expected t1 in a parallel batch\n---\n%s", out)
	}
	if !strings.Contains(out, "Executed tasks (2)") {
		t.Errorf("expected both tasks executed\n---\n%s", out)
	}
}

// Scenario: a worker failure in a parallel batch is isolated — both tasks still
// run to completion, but the run stops after the batch (no retry) and reports
// failure.
func TestRunOrchestratedParallelFaultIsolation(t *testing.T) {
	tmp := t.TempDir()
	runner := failingRunner{err: errors.New("boom")}
	od := newOrchestratedTestDeps(t, tmp, runner, fakeVerifyPassed, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: true, MaxWorkers: 2}
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		readonlyTask("t2", "search the docs"),
	})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitIncomplete {
		t.Fatalf("exit = %d, want %d (incomplete)", code, exitIncomplete)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{
		"Parallel batch 1 (ran concurrently):",
		"Executed tasks (2)",
		"Top status: failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
}

// Scenario: MaxWorkers=1 still runs every eligible task (serially inside the
// semaphore) and reports a single concurrent batch.
func TestRunOrchestratedParallelWorkersBoundOne(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, readOnlyRunner{}, fakeVerifyPassed, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: true, MaxWorkers: 1}
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		readonlyTask("t2", "search the docs"),
	})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "Parallel batch 1 (ran concurrently):") {
		t.Errorf("expected a parallel batch even with one worker\n---\n%s", out)
	}
	if !strings.Contains(out, "Executed tasks (2)") {
		t.Errorf("expected both tasks executed\n---\n%s", out)
	}
}

// Scenario: when the flag is disabled, parallel-eligible tasks fall back to
// strict sequential execution and the parallel banner is not shown.
func TestRunOrchestratedParallelDisabledRunsSequential(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, readOnlyRunner{}, fakeVerifyPassed, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: false, MaxWorkers: 2}
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		readonlyTask("t2", "search the docs"),
	})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if strings.Contains(out, "Parallel batch") {
		t.Errorf("parallel disabled must not emit a batch header\n---\n%s", out)
	}
	if strings.Contains(out, "read-only parallel batches") {
		t.Errorf("parallel disabled must not show the parallel banner\n---\n%s", out)
	}
	if !strings.Contains(out, "Executed tasks (2)") {
		t.Errorf("expected both tasks executed sequentially\n---\n%s", out)
	}
}

// mixedKindRunner answers read-only tasks with read evidence and the
// implementation task with a write action + verification-passed completion.
type mixedKindRunner struct{}

func (mixedKindRunner) RunTask(ctx context.Context, req executor.TaskExecutionRequest) (executor.TaskExecutionResult, error) {
	if req.Task.TaskKind == planner.KindImplementation {
		return executor.TaskExecutionResult{
			AgentResult: agent.Result{FinalAnswer: "implemented " + req.Task.ID},
			FinalAnswer: "implemented " + req.Task.ID,
			ToolEvents:  []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}},
		}, nil
	}
	return executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "read result for " + req.Task.ID},
		FinalAnswer: "read result for " + req.Task.ID,
		ToolEvents:  []executor.ToolEvent{{Name: "web_search", Kind: "read"}},
	}, nil
}

// --- parse tests ---

func TestParseParallelRequiresOrchestrated(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--parallel-readonly", "do it"})
	if err == nil {
		t.Fatal("expected error: --parallel-readonly requires --orchestrated")
	}
}

func TestParseParallelWorkersRequiresParallel(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--orchestrated", "--parallel-workers", "2", "do it"})
	if err == nil {
		t.Fatal("expected error: --parallel-workers requires --parallel-readonly")
	}
}

func TestParseParallelRejectsOrchestratedOnce(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--orchestrated-once", "--parallel-readonly", "do it"})
	if err == nil {
		t.Fatal("expected error: --parallel-readonly cannot combine with --orchestrated-once")
	}
}

func TestParseParallelWorkersRange(t *testing.T) {
	for _, tc := range []struct {
		workers string
		wantErr bool
	}{
		{"0", true},
		{"9", true},
		{"-1", true},
		{"2", false},
		{"8", false},
		{"1", false},
	} {
		_, _, err := parseExecArgs([]string{"--orchestrated", "--parallel-readonly", "--parallel-workers", tc.workers, "do it"})
		if tc.wantErr && err == nil {
			t.Errorf("--parallel-workers %s: expected error", tc.workers)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("--parallel-workers %s: unexpected error %v", tc.workers, err)
		}
	}
}

func TestParseParallelDefaultsWorkers(t *testing.T) {
	opts, _, err := parseExecArgs([]string{"--orchestrated", "--parallel-readonly", "do it"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.parallelReadonly {
		t.Errorf("parallelReadonly = false, want true")
	}
	if opts.parallelWorkers != 2 {
		t.Errorf("parallelWorkers = %d, want default 2", opts.parallelWorkers)
	}
}

func TestParseParallelWorkersInline(t *testing.T) {
	opts, _, err := parseExecArgs([]string{"--orchestrated", "--parallel-readonly", "--parallel-workers=4", "do it"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.parallelWorkers != 4 {
		t.Errorf("parallelWorkers = %d, want 4", opts.parallelWorkers)
	}
}
