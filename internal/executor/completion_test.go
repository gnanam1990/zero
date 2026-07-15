package executor

import (
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/planner"
)

func toolEvent(name, kind string) ToolEvent {
	return ToolEvent{Name: name, Kind: kind}
}

func repoChanged(paths ...string) RepoChanges {
	c := RepoChanges{HasGit: true}
	c.ChangedFiles = append(c.ChangedFiles, paths...)
	return c
}

type gateCase struct {
	name     string
	taskKind planner.TaskKind
	evidence TaskExecutionResult
	repo     RepoChanges
	verify   VerificationOutcome
	policy   CompletionPolicy
	want     CompletionStatus
}

func ev(actions ...ToolEvent) TaskExecutionResult {
	return TaskExecutionResult{AgentResult: agent.Result{}, ToolEvents: actions}
}

func withAnswer(r TaskExecutionResult, answer string) TaskExecutionResult {
	r.FinalAnswer = answer
	r.AgentResult.FinalAnswer = answer
	return r
}

// withSignal marks the result as having emitted a completion signal (i.e. NOT
// the headless "still working" incomplete flag).
func withSignal(r TaskExecutionResult) TaskExecutionResult {
	r.AgentResult.Incomplete = false
	return r
}

// withoutSignal marks the result as missing the completion signal.
func withoutSignal(r TaskExecutionResult) TaskExecutionResult {
	r.AgentResult.Incomplete = true
	return r
}

func TestEvaluateCompletion(t *testing.T) {
	cases := []gateCase{
		// --- deterministic evidence dominates the model signal ---

		// 1. Delta + verification pass + no signal -> completed.
		{"impl change + verify passed + no signal", planner.KindImplementation, withoutSignal(ev(toolEvent("write_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "passed"}, CompletionPolicy{}, StatusCompleted},
		// 2. Delta + verification pass + signal -> completed.
		{"impl change + verify passed + signal", planner.KindImplementation, withSignal(ev(toolEvent("write_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "passed"}, CompletionPolicy{}, StatusCompleted},
		// 3. Delta + verification unavailable + no signal -> completed_unverified.
		{"impl change + verify none + no signal", planner.KindImplementation, withoutSignal(ev(toolEvent("edit_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompletedUnverified},
		// 3b. Delta + verification not_run -> completed_unverified.
		{"impl change + verify not_run", planner.KindImplementation, withSignal(ev(toolEvent("edit_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "not_run"}, CompletionPolicy{}, StatusCompletedUnverified},
		// 4. Delta + verification failure -> failed (signal cannot override).
		{"impl change + verify failed + signal", planner.KindImplementation, withSignal(ev(toolEvent("write_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "failed"}, CompletionPolicy{}, StatusFailed},
		// 12. Signal cannot override failed verification.
		{"impl change + verify failed + no signal", planner.KindImplementation, withoutSignal(ev(toolEvent("write_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "failed"}, CompletionPolicy{}, StatusFailed},

		// --- no delta: a signal alone is never sufficient ---

		// 5. No delta + signal only -> incomplete.
		{"impl action + no change + signal", planner.KindImplementation, withSignal(ev(toolEvent("write_file", "mutating"))), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusIncomplete},
		// 6. No delta + no signal -> incomplete.
		{"impl action + no change + no signal", planner.KindImplementation, withoutSignal(ev(toolEvent("write_file", "mutating"))), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusIncomplete},

		// 7. No delta + existing-feature evidence -> completed_no_change.
		{"impl no action, feature exists evidence", planner.KindImplementation, withAnswer(TaskExecutionResult{AgentResult: agent.Result{}}, "The feature already exists at src/foo.go"), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompletedNoChange},
		// 7b. No delta + vague answer -> incomplete.
		{"impl no action, vague answer", planner.KindImplementation, withAnswer(TaskExecutionResult{AgentResult: agent.Result{}}, "Done I think"), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusIncomplete},
		// repo delta proves change without a tracked tool action.
		{"impl no action, repo changed externally", planner.KindImplementation, ev(), repoChanged("a.go"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompleted},

		// --- permission gating ---

		// 9. Permission required (headless Auto denial) -> blocked even with a delta.
		{"permission required + delta", planner.KindImplementation, TaskExecutionResult{PermissionRequired: true, ToolEvents: []ToolEvent{toolEvent("write_file", "mutating")}, FilesChanged: []string{"a.go"}}, repoChanged("a.go"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusBlocked},
		// 10. Permission denied -> blocked.
		{"permission denied", planner.KindImplementation, TaskExecutionResult{PermissionDenied: true, ToolEvents: []ToolEvent{toolEvent("write_file", "mutating")}}, RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusBlocked},

		// --- read-only tasks: evidence + answer, no signal required ---

		// 8. Read-only evidence + final answer + no signal -> completed.
		{"search read action + no signal", planner.KindRepositorySearch, withoutSignal(withAnswer(ev(toolEvent("grep", "read")), "Found usages in pkg/x")), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompleted},
		{"code review with review action", planner.KindCodeReview, withSignal(withAnswer(ev(toolEvent("code_review", "read")), "Reviewed 3 files")), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompleted},
		{"security review with read action", planner.KindSecurityReview, withAnswer(ev(toolEvent("security_review", "read")), "No critical issues"), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompleted},
		{"image analysis with read action", planner.KindImageAnalysis, withAnswer(ev(toolEvent("read_file", "read")), "Detected 2 logos"), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompleted},
		{"documentation with read evidence + answer", planner.KindDocumentation, withAnswer(ev(toolEvent("read_file", "read")), "Docs cover the public API"), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_applicable"}, CompletionPolicy{}, StatusCompleted},
		{"search with no action", planner.KindRepositorySearch, ev(), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusIncomplete},
		{"search with only mutating action", planner.KindRepositorySearch, ev(toolEvent("write_file", "mutating")), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusIncomplete},
		{"search with read action but empty answer", planner.KindRepositorySearch, ev(toolEvent("grep", "read")), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusIncomplete},

		// --- strict policy: RequireVerificationForMutations ---

		{"strict: delta + verify none -> incomplete", planner.KindImplementation, withoutSignal(ev(toolEvent("edit_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{RequireVerificationForMutations: true}, StatusIncomplete},

		// --- strict policy: RequireModelSignal ---

		{"strict signal: delta + verify none + no signal -> incomplete", planner.KindImplementation, withoutSignal(ev(toolEvent("edit_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{RequireModelSignal: true}, StatusIncomplete},
		{"strict signal: delta + verify none + signal -> completed_unverified", planner.KindImplementation, withSignal(ev(toolEvent("edit_file", "mutating"))), repoChanged("a.go"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{RequireModelSignal: true}, StatusCompletedUnverified},

		// --- documentation / architecture are read-only; a write is an unexpected
		// mutation and must fail ---

		{"docs with write + change", planner.KindDocumentation, ev(toolEvent("write_file", "mutating")), repoChanged("doc.md"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusFailed},
		{"docs with write, no change", planner.KindDocumentation, ev(toolEvent("write_file", "mutating")), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusIncomplete},

		// --- testing / debugging ---

		{"test with command + change", planner.KindTesting, ev(toolEvent("bash", "command")), repoChanged("x_test.go"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompletedUnverified},
		{"debug with command + change", planner.KindDebugging, ev(toolEvent("bash", "command")), repoChanged("a.go"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusCompletedUnverified},
		{"debug with command, no change, verify passed", planner.KindDebugging, ev(toolEvent("bash", "command")), RepoChanges{HasGit: true}, VerificationOutcome{Status: "passed"}, CompletionPolicy{}, StatusCompleted},
		{"debug with command, no change, verify none", planner.KindDebugging, ev(toolEvent("bash", "command")), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusIncomplete},

		// --- architecture (read-only) ---

		{"architecture with read evidence + answer", planner.KindArchitecture, withAnswer(ev(toolEvent("grep", "read")), "Layered ports-and-adapters design"), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_applicable"}, CompletionPolicy{}, StatusCompleted},
		{"architecture with write + change", planner.KindArchitecture, ev(toolEvent("write_file", "mutating")), repoChanged("design.md"), VerificationOutcome{Status: "not_available"}, CompletionPolicy{}, StatusFailed},
		{"architecture with no action", planner.KindArchitecture, ev(), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_applicable"}, CompletionPolicy{}, StatusIncomplete},
		{"architecture with read action, empty answer", planner.KindArchitecture, ev(toolEvent("read_file", "read")), RepoChanges{HasGit: true}, VerificationOutcome{Status: "not_applicable"}, CompletionPolicy{}, StatusIncomplete},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := planner.Task{TaskKind: c.taskKind}
			got := EvaluateCompletion(task, c.evidence, c.repo, c.verify, c.policy)
			if got != c.want {
				t.Errorf("EvaluateCompletion(%s) = %s, want %s", c.name, got, c.want)
			}
		})
	}
}

func TestMapAgentError(t *testing.T) {
	cancelled := TaskExecutionResult{Cancelled: true}
	if got := MapAgentError(cancelled); got != StatusIncomplete {
		t.Errorf("cancelled -> %s, want %s", got, StatusIncomplete)
	}
	denied := TaskExecutionResult{PermissionDenied: true}
	if got := MapAgentError(denied); got != StatusBlocked {
		t.Errorf("denied -> %s, want %s", got, StatusBlocked)
	}
	// 11. Fatal (non-permission, non-cancel) provider error -> failed.
	other := TaskExecutionResult{Error: errTest}
	if got := MapAgentError(other); got != StatusFailed {
		t.Errorf("other error -> %s, want %s", got, StatusFailed)
	}
	// Permission required (without a run-level error) also blocks.
	required := TaskExecutionResult{PermissionRequired: true}
	if got := MapAgentError(required); got != StatusBlocked {
		t.Errorf("permission required -> %s, want %s", got, StatusBlocked)
	}
}

func TestRepoChangesAll(t *testing.T) {
	c := RepoChanges{
		ChangedFiles:   []string{"a"},
		UntrackedFiles: []string{"b"},
		StagedFiles:    []string{"c"},
		DeletedFiles:   []string{"d"},
	}
	all := c.All()
	if len(all) != 4 {
		t.Fatalf("All() returned %d paths, want 4", len(all))
	}
}

var errTest = errString("boom")

type errString string

func (e errString) Error() string { return string(e) }
