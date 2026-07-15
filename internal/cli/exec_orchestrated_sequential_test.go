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

// --- fakes for the sequential engine ---

// failingRunner always returns an error so the deterministic completion gate
// classifies the task as failed.
type failingRunner struct{ err error }

func (f failingRunner) RunTask(ctx context.Context, req executor.TaskExecutionRequest) (executor.TaskExecutionResult, error) {
	return executor.TaskExecutionResult{}, f.err
}

// injectTwoTaskPlan overrides the plan-preview entry point with a deterministic
// two-task plan where t2 depends on t1, both routable to the fake model.
func injectTwoTaskPlan(t *testing.T) {
	t.Helper()
	orig := orchestratedBuildPlan
	t.Cleanup(func() { orchestratedBuildPlan = orig })
	orchestratedBuildPlan = func(prompt string, routerOpts routerFlagOptions, repoPresent bool, candidates []modelregistry.ModelEntry) (planPreviewResult, error) {
		dec := modelrouter.Decision{Selected: &modelrouter.Candidate{Model: modelregistry.ModelEntry{Provider: modelregistry.ProviderKind("fake"), ID: "fake-model"}}}
		plan := planner.ExecutionPlan{
			PlanID:  "p1",
			Summary: "two tasks",
			Tasks: []planner.Task{
				{ID: "t1", Title: "first", TaskKind: planner.KindImplementation},
				{ID: "t2", Title: "second", TaskKind: planner.KindImplementation, Dependencies: []string{"t1"}},
			},
		}
		return planPreviewResult{
			Plan:           plan,
			Results:        []planTaskResult{{Task: plan.Tasks[0], Decision: dec}, {Task: plan.Tasks[1], Decision: dec}},
			Classification: taskclass.Result{},
		}, nil
	}
}

// Scenario: full sequential mode over a default single-task plan behaves like the
// once path but renders the new aggregate summary.
func TestRunOrchestratedSequentialSingleTask(t *testing.T) {
	tmp := t.TempDir()
	runner := fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "done"},
		FinalAnswer: "done",
		ToolEvents:  []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}},
	}}
	od := newOrchestratedTestDeps(t, tmp, runner, fakeVerifyPassed, execOutputText)
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{
		"ORCHESTRATED EXECUTION — sequential DAG",
		"Executed tasks (1)",
		"Top status: completed",
		"Stopped by --orchestrated.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
	for _, bad := range []string{"ORCHESTRATED EXECUTION — one task only", "Stopped after one task by --orchestrated-once."} {
		if strings.Contains(out, bad) {
			t.Errorf("sequential output must not use once-mode banner/footer; found %q\n---\n%s", bad, out)
		}
	}
}

// Scenario: full sequential mode renders JSON with the orchestrated mode and the
// executed task list.
func TestRunOrchestratedSequentialSingleTaskJSON(t *testing.T) {
	tmp := t.TempDir()
	runner := fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "done"},
		FinalAnswer: "done",
		ToolEvents:  []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}},
	}}
	od := newOrchestratedTestDeps(t, tmp, runner, fakeVerifyPassed, execOutputJSON)
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{`"mode": "orchestrated"`, `"executed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("json summary missing %q\n---\n%s", want, out)
		}
	}
}

// Scenario: a clean two-task dependency chain runs both tasks in order and reports
// two executed tasks with no skips.
func TestRunOrchestratedSequentialChain(t *testing.T) {
	tmp := t.TempDir()
	runner := fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "done"},
		FinalAnswer: "done",
		ToolEvents:  []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}},
	}}
	od := newOrchestratedTestDeps(t, tmp, runner, fakeVerifyPassed, execOutputText)
	injectTwoTaskPlan(t)
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "ORCHESTRATED EXECUTION — sequential DAG") {
		t.Errorf("expected sequential DAG banner\n---\n%s", out)
	}
	if !strings.Contains(out, "Executed tasks (2)") {
		t.Errorf("expected both tasks executed\n---\n%s", out)
	}
	if strings.Contains(out, "Skipped tasks") {
		t.Errorf("did not expect skipped tasks\n---\n%s", out)
	}
}

// Scenario: when the first task fails, the run stops and the dependent task is
// marked skipped_due_to_dependency.
func TestRunOrchestratedSequentialFailureSkipsDependents(t *testing.T) {
	tmp := t.TempDir()
	runner := failingRunner{err: errors.New("boom")}
	od := newOrchestratedTestDeps(t, tmp, runner, fakeVerifyPassed, execOutputText)
	injectTwoTaskPlan(t)
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitIncomplete {
		t.Fatalf("exit = %d, want %d (incomplete)", code, exitIncomplete)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{
		"ORCHESTRATED EXECUTION — sequential DAG",
		"Executed tasks (1)",
		"Top status: failed",
		"Skipped tasks (1)",
		"skipped_due_to_dependency",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
}
