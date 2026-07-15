// Package executor provides a narrow adapter around Zero's existing agent
// runtime for executing a single planned task. It deliberately separates the
// execution of one task (the Runner) from orchestration concerns (planning,
// scheduling, completion gating, verification, repository delta) which live in
// the CLI. The package is dependency-light so the Runner can be faked in tests.
package executor

import (
	"context"
	"errors"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/planner"
)

// CompletionStatus is the deterministic outcome of a single orchestrated task.
type CompletionStatus string

const (
	// StatusCompleted means the task ran and satisfied its acceptance evidence.
	StatusCompleted CompletionStatus = "completed"
	// StatusCompletedNoChange means the task needed no code change (e.g. the
	// requested feature already exists) and the agent reported that with evidence.
	StatusCompletedNoChange CompletionStatus = "completed_no_change"
	// StatusCompletedUnverified means the task produced a repository change but no
	// verification plan was available/run to confirm it. Deterministic evidence
	// (the repo delta) satisfied completion; the missing verification is advisory.
	StatusCompletedUnverified CompletionStatus = "completed_unverified"
	// StatusFailed means the task errored, was denied, or failed verification.
	StatusFailed CompletionStatus = "failed"
	// StatusIncomplete means the run stopped without finishing the task.
	StatusIncomplete CompletionStatus = "incomplete"
	// StatusBlocked means the task could not run (e.g. required approval).
	StatusBlocked CompletionStatus = "blocked"
)

// CompletionPolicy tunes how strictly deterministic evidence is required for a
// task to be marked complete. Defaults (both false) favor deterministic evidence:
// a repository delta is sufficient even without verification, and the model's
// completion signal is only supporting evidence.
type CompletionPolicy struct {
	// RequireVerificationForMutations forces a mutating task to fail (incomplete)
	// when it produced a change but no verification ran/passed. When false, such a
	// task is completed_unverified.
	RequireVerificationForMutations bool
	// RequireModelSignal forces a mutating task with a repository delta but no
	// verification to be incomplete unless the model emitted a completion signal.
	// When false, the signal is optional supporting evidence and the delta alone
	// completes the task.
	RequireModelSignal bool
}

// ErrPermissionDenied is returned by a Runner when a task was blocked by a
// permission decision. The CLI maps it to a blocked/failed scheduler state.
var ErrPermissionDenied = errors.New("executor: permission denied")

// ToolUsageMetrics is the deterministic count of tool actions observed during a
// single task execution. It is derived from the agent's tool callbacks, not from
// parsing text. Attempted is every tool call the model requested; Executed is the
// subset that was actually dispatched (a denied/filtered call is Attempted but not
// Executed); Succeeded/Failed partition Executed by terminal status.
type ToolUsageMetrics struct {
	Attempted int `json:"attempted"`
	Executed  int `json:"executed"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Denied    int `json:"denied,omitempty"`
}

// ToolEvent is a stable, serializable view of one agent tool action, safe to
// persist or render. It never carries provider clients or secrets.
type ToolEvent struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind,omitempty"`
	Status        string   `json:"status,omitempty"`
	Denial        string   `json:"denial,omitempty"`
	ChangedFiles  []string `json:"changed_files,omitempty"`
	ArgsRaw       string   `json:"args_raw,omitempty"`
	OutputSummary string   `json:"output_summary,omitempty"`
}

// ToolCategory classifies a tool action for completion-gate logic.
type ToolCategory string

const (
	CategoryMutating ToolCategory = "mutating"
	CategoryRead     ToolCategory = "read"
	CategoryCommand  ToolCategory = "command"
	CategoryOther    ToolCategory = "other"
)

// VerificationCheck is one ran verification check, serializable.
type VerificationCheck struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Command  []string `json:"command"`
	Status   string   `json:"status"`
	ExitCode int      `json:"exit_code"`
	Summary  string   `json:"summary,omitempty"`
}

// VerificationOutcome summarizes verification for a task.
type VerificationOutcome struct {
	// Status is one of: passed, failed, not_run, not_available, not_applicable.
	Status string              `json:"status"`
	Total  int                 `json:"total,omitempty"`
	Passed int                 `json:"passed,omitempty"`
	Failed int                 `json:"failed,omitempty"`
	Errors int                 `json:"errors,omitempty"`
	Checks []VerificationCheck `json:"checks,omitempty"`
	Reason string              `json:"reason,omitempty"`
}

// RepoSnapshot is a baseline-aware capture of repository state.
type RepoSnapshot struct {
	Paths map[string]bool
	IsGit bool
}

// RepoChanges holds the delta introduced by a task, excluding pre-existing
// (baseline) local changes.
type RepoChanges struct {
	ChangedFiles   []string `json:"changed_files,omitempty"`
	UntrackedFiles []string `json:"untracked_files,omitempty"`
	StagedFiles    []string `json:"staged_files,omitempty"`
	DeletedFiles   []string `json:"deleted_files,omitempty"`
	HasGit         bool     `json:"has_git"`
	BaselineDirty  bool     `json:"baseline_dirty"`
}

// All returns every changed path (any category) as a flat slice.
func (c RepoChanges) All() []string {
	out := make([]string, 0, len(c.ChangedFiles)+len(c.UntrackedFiles)+len(c.StagedFiles)+len(c.DeletedFiles))
	out = append(out, c.ChangedFiles...)
	out = append(out, c.UntrackedFiles...)
	out = append(out, c.StagedFiles...)
	out = append(out, c.DeletedFiles...)
	return out
}

// TaskExecutionRequest is the narrow input to a single task execution.
type TaskExecutionRequest struct {
	Task          planner.Task
	Prompt        string
	ModelID       string
	ProviderID    string
	WorkspaceRoot string
	SessionID     string
}

// TaskExecutionResult is the evidence collected from one task execution.
type TaskExecutionResult struct {
	AgentResult  agent.Result
	ToolEvents   []ToolEvent
	FilesChanged []string
	CommandsRun  []string
	FinalAnswer  string
	// Error carries a non-nil agent runtime error (provider/permission/cancel).
	Error error
	// Cancelled is true when the run was stopped by context cancellation.
	Cancelled bool
	// PermissionDenied is true when the run was blocked by a permission decision.
	PermissionDenied bool
	// PermissionRequired is true when a tool was requested that requires approval
	// (prompt-required) but no approver was available (e.g. headless Auto mode),
	// so execution was denied. Distinct from PermissionDenied only for messaging;
	// both map the task to blocked. It is set from permission events even when the
	// denial did not surface as a run-level error.
	PermissionRequired bool
	// Usage is the normalized, summed provider token accounting for the whole
	// task (every agent turn). It is populated from the agent runtime's OnUsage
	// callback; a run that reports no usage leaves it zero with UsageReported
	// false so a measured zero is never confused with "not measured".
	Usage agent.Usage
	// UsageReported is true once the agent runtime emitted at least one usage
	// event for the task. It gates token-metric availability.
	UsageReported bool
	// ToolUsage is the deterministic count of tool actions observed during the
	// task, derived from the agent's tool callbacks.
	ToolUsage ToolUsageMetrics
}

// Runner executes exactly one planned task through the agent runtime.
type Runner interface {
	RunTask(ctx context.Context, req TaskExecutionRequest) (TaskExecutionResult, error)
}

// Verifier runs verification for a task's changed files and returns an outcome.
// The CLI provides the real implementation (wrapping internal/verify); tests
// provide a fake. A nil Verifier is treated as "not available".
type Verifier func(ctx context.Context, workspaceRoot string, changedFiles []string) VerificationOutcome

// RepoDeltaFunc captures the repository change set introduced by a task, relative
// to a baseline captured before the task ran, and reports whether a baseline was
// already dirty. The CLI provides the real implementation (wrapping git); tests
// provide a fake.
type RepoDeltaFunc func(ctx context.Context, workspaceRoot string) (RepoChanges, error)
