package executor

import (
	"strings"

	"github.com/Gitlawb/zero/internal/planner"
)

// readOnlyKinds are task kinds whose completion does not require repository
// changes; they are validated by performed read/search/review actions instead.
// This set is intentionally aligned with the read-only tool allowlist granted by
// the orchestrated runner (see cli/orchestrated_tools.go taskAllowedCapabilities)
// so a task restricted to read tools is also completed by read evidence.
var readOnlyKinds = map[planner.TaskKind]bool{
	planner.KindRepositorySearch: true,
	planner.KindCodeReview:       true,
	planner.KindSecurityReview:   true,
	planner.KindArchitecture:     true,
	planner.KindDocumentation:    true,
	planner.KindImageAnalysis:    true,
}

func isReadOnlyKind(k planner.TaskKind) bool { return readOnlyKinds[k] }

// TaskRequiresRepositoryVerification reports whether a task must run
// mutation-oriented repository verification. Read-only task kinds (search,
// review, architecture, documentation, image analysis) do not mutate the
// repository and therefore skip verification entirely; the CLI marks their
// verification status as not_applicable. Mutating kinds (implementation,
// refactoring, debugging, testing, test_execution, shell_operation) always
// require verification when a plan is available.
func TaskRequiresRepositoryVerification(task planner.Task) bool {
	return !isReadOnlyKind(task.TaskKind)
}

func countCategory(events []ToolEvent, cat ToolCategory) int {
	n := 0
	for _, e := range events {
		if ToolCategory(e.Kind) == cat {
			n++
		}
	}
	return n
}

// existingFeatureMarkers are phrases that, in a no-change final answer, indicate
// the requested feature already exists (with evidence) rather than the run
// simply failing to act.
var existingFeatureMarkers = []string{
	"already exists",
	"already implemented",
	"already present",
	"no changes needed",
	"no change required",
	"feature is present",
	"already in place",
	"nothing to do",
}

func existingFeatureEvidence(answer string) bool {
	low := strings.ToLower(answer)
	for _, m := range existingFeatureMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// EvaluateCompletion decides a single task's deterministic outcome from its
// execution evidence. Deterministic evidence (repository delta, verification,
// permission requirements) dominates the model-generated completion signal: the
// signal is supporting evidence only and can never prove a file change, override
// failed verification, or by itself mark a task complete.
func EvaluateCompletion(task planner.Task, ev TaskExecutionResult, repo RepoChanges, verification VerificationOutcome, policy CompletionPolicy) CompletionStatus {
	// A permission requirement/denial blocks the task regardless of any signal or
	// delta — approval is the gating prerequisite and is not auto-granted.
	if ev.PermissionDenied || ev.PermissionRequired {
		return StatusBlocked
	}
	// Verification that ran and failed blocks completion.
	if verification.Status == "failed" {
		return StatusFailed
	}

	changes := repo.All()
	hasChange := len(changes) > 0
	mutatingActions := countCategory(ev.ToolEvents, CategoryMutating)
	commandActions := countCategory(ev.ToolEvents, CategoryCommand)
	readActions := countCategory(ev.ToolEvents, CategoryRead)
	actions := len(ev.ToolEvents)
	// The model's completion signal is the ABSENCE of a headless "still working"
	// flag. It is supporting evidence only, never a proof of completion.
	signalPresent := !ev.AgentResult.Incomplete

	if isReadOnlyKind(task.TaskKind) {
		// A read-only task must not introduce repository changes. An actual delta
		// is an unexpected mutation and fails the task; a pure read/search/review
		// result (even with an attempted but non-persisting mutating action) is
		// decided by read evidence below.
		if hasChange {
			return StatusFailed
		}
		// Relevant evidence: at least one read/search/review action (or any
		// other non-mutating, non-command action such as lsp navigation or
		// session search). A detailed answer with no tool evidence is incomplete
		// unless the task is validly answerable from provided context alone.
		readEvidence := readActions + countCategory(ev.ToolEvents, CategoryOther)
		if actions == 0 || readEvidence == 0 {
			return StatusIncomplete
		}
		if strings.TrimSpace(ev.FinalAnswer) == "" {
			return StatusIncomplete
		}
		return StatusCompleted
	}

	// Code-change task.

	// No mutating/command action occurred: the repo delta (or strong no-change
	// evidence) decides; the signal alone is never sufficient.
	if mutatingActions == 0 && commandActions == 0 {
		if hasChange {
			return StatusCompleted
		}
		if existingFeatureEvidence(ev.FinalAnswer) {
			return StatusCompletedNoChange
		}
		return StatusIncomplete
	}

	// Mutating/command actions occurred. Deterministic evidence decides.
	switch verification.Status {
	case "passed":
		return StatusCompleted
	case "failed":
		return StatusFailed
	default:
		// not_available / not_run: no verification ran.
		if policy.RequireVerificationForMutations {
			return StatusIncomplete
		}
		if hasChange {
			if policy.RequireModelSignal && !signalPresent {
				return StatusIncomplete
			}
			return StatusCompletedUnverified
		}
		// Actions occurred but no repository delta was detected. Without a delta
		// there is nothing deterministic to confirm, so the task cannot be marked
		// complete on a signal alone.
		if verification.Status == "passed" {
			return StatusCompleted
		}
		return StatusIncomplete
	}
}

// MapAgentError converts a non-nil agent runtime error into a scheduler-facing
// status. The CLI still controls the process exit code (e.g. cancellation →
// interrupted); this only decides how the scheduler state is updated.
func MapAgentError(ev TaskExecutionResult) CompletionStatus {
	if ev.Cancelled {
		return StatusIncomplete
	}
	if ev.PermissionDenied || ev.PermissionRequired {
		return StatusBlocked
	}
	return StatusFailed
}
