package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/scheduler"
	"github.com/Gitlawb/zero/internal/taskclass"
)

// Requirement 14: every deterministic completion status maps to the correct
// scheduler transition (completed/failed/skipped).
func TestOrchestratedApplyStatusMapping(t *testing.T) {
	plan := planner.ExecutionPlan{
		PlanID: "p1",
		Tasks:  []planner.Task{{ID: "t1", Title: "task", TaskKind: planner.KindImplementation, SafetyLevel: planner.SafetySafe}},
	}

	cases := []struct {
		status      executor.CompletionStatus
		completedOK bool
		failedOK    bool
		skippedOK   bool
	}{
		{executor.StatusCompleted, true, false, false},
		{executor.StatusCompletedNoChange, true, false, false},
		{executor.StatusCompletedUnverified, true, false, false},
		{executor.StatusFailed, false, true, false},
		{executor.StatusIncomplete, false, true, false},
		{executor.StatusBlocked, false, false, true},
	}
	for _, c := range cases {
		s, err := scheduler.NewScheduler(plan)
		if err != nil {
			t.Fatalf("scheduler: %v", err)
		}
		orchestratedApplyStatus(s, "t1", c.status)
		st := s.State()
		gotCompleted := len(st.CompletedTasks) == 1
		gotFailed := len(st.FailedTasks) == 1
		gotSkipped := len(st.SkippedTasks) == 1
		if gotCompleted != c.completedOK || gotFailed != c.failedOK || gotSkipped != c.skippedOK {
			t.Errorf("status %s -> completed=%v failed=%v skipped=%v, want completed=%v failed=%v skipped=%v",
				c.status, gotCompleted, gotFailed, gotSkipped, c.completedOK, c.failedOK, c.skippedOK)
		}
	}
}

// Requirement 13: pre-existing (baseline) local changes are NOT counted as task
// delta — only changes introduced by the task are reported.
func TestOrchestratedRepoDeltaExcludesBaseline(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "user.email", "t@example.com")
	runGit("config", "user.name", "t")

	// A pre-existing dirty file present BEFORE the task runs.
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline, isGit := gitStatusPaths(dir)
	if !isGit {
		t.Fatal("expected a git repository")
	}
	if !baseline["dirty.txt"] {
		t.Fatal("baseline must capture the pre-existing dirty file")
	}

	// The task introduces a NEW change.
	if err := os.WriteFile(filepath.Join(dir, "added.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes := orchestratedRepoDelta(dir, baseline)
	all := changes.All()
	if schemaHas(all, "dirty.txt") {
		t.Fatalf("baseline dirty file must NOT count as task delta: %v", all)
	}
	if !schemaHas(all, "added.txt") {
		t.Fatalf("task-introduced file must count as delta: %v", all)
	}
}

// Requirement 15: the text and JSON renderers expose the SAME status string for
// every completion outcome.
func TestOrchestratedRenderSameStatus(t *testing.T) {
	plan := planner.ExecutionPlan{
		PlanID: "p1",
		Tasks:  []planner.Task{{ID: "t1", Title: "task", TaskKind: planner.KindImplementation, SafetyLevel: planner.SafetySafe}},
	}
	task := plan.Tasks[0]
	preview := planPreviewResult{Plan: plan, Classification: taskclass.Result{}}
	decision := modelrouter.Decision{}
	finalState := scheduler.ExecutionState{}

	statuses := []executor.CompletionStatus{
		executor.StatusCompleted, executor.StatusCompletedNoChange, executor.StatusCompletedUnverified,
		executor.StatusFailed, executor.StatusIncomplete, executor.StatusBlocked,
	}
	for _, status := range statuses {
		od := buildOrchestratedTestDeps(newCoreRegistry(t.TempDir()), agent.PermissionModeAuto)

		var textBuf, jsonBuf bytes.Buffer
		od.stdout = &textBuf
		od.options.outputFormat = execOutputText
		renderOrchestratedText(od, preview, task, decision, executor.TaskExecutionResult{}, executor.RepoChanges{}, executor.VerificationOutcome{Status: "not_available"}, status, finalState)

		odJSON := buildOrchestratedTestDeps(newCoreRegistry(t.TempDir()), agent.PermissionModeAuto)
		odJSON.stdout = &jsonBuf
		odJSON.options.outputFormat = execOutputJSON
		renderOrchestratedJSON(odJSON, preview, task, decision, executor.TaskExecutionResult{}, executor.RepoChanges{}, executor.VerificationOutcome{Status: "not_available"}, status, finalState)

		if !strings.Contains(textBuf.String(), "Execution ("+string(status)+")") {
			t.Errorf("text renderer missing %q:\n%s", status, textBuf.String())
		}
		if !strings.Contains(jsonBuf.String(), `"status": "`+string(status)+`"`) {
			t.Errorf("json renderer missing %q:\n%s", status, jsonBuf.String())
		}
	}
}
