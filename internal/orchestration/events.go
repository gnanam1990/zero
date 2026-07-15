// Package orchestration provides a reusable, UI-agnostic orchestration
// coordinator that drives the deterministic classify → plan → schedule → route →
// execute → verify → completion-gate pipeline. Both the CLI (internal/cli) and
// the TUI (internal/tui) consume it without depending on each other.
//
// The coordinator emits typed Event values through a channel; the consumer
// (CLI renderer, Bubble Tea msg translator) reads them and renders accordingly.
// Workers never touch UI state directly — all mutation happens through events.
package orchestration

import (
	"context"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/scheduler"
	"github.com/Gitlawb/zero/internal/taskclass"
	"github.com/Gitlawb/zero/internal/tools"
)

// EventType identifies one orchestration lifecycle event.
type EventType string

const (
	EventPlanCreated          EventType = "plan_created"
	EventPlanAwaitingApproval EventType = "plan_awaiting_approval"
	EventRunStarted           EventType = "run_started"
	EventBatchStarted         EventType = "batch_started"
	EventTaskQueued           EventType = "task_queued"
	EventTaskStarted          EventType = "task_started"
	EventTaskToolStarted      EventType = "task_tool_started"
	EventTaskToolFinished     EventType = "task_tool_finished"
	EventTaskUsageUpdated     EventType = "task_usage_updated"
	EventTaskCompleted        EventType = "task_completed"
	EventTaskFailed           EventType = "task_failed"
	EventTaskBlocked          EventType = "task_blocked"
	EventTaskSkipped          EventType = "task_skipped"
	EventBatchCompleted       EventType = "batch_completed"
	EventMetricsUpdated       EventType = "metrics_updated"
	EventRunCompleted         EventType = "run_completed"
	EventRunCancelled         EventType = "run_cancelled"
)

// Event is a typed orchestration lifecycle event. Only the fields relevant to
// Type are populated; consumers should switch on Type and read accordingly.
// Events for a single task are emitted in deterministic order; events from
// concurrent tasks may arrive interleaved at the channel boundary.
type Event struct {
	Type EventType

	// Plan is set for plan_created and plan_awaiting_approval.
	Plan *PlanPreview

	// Task is set for task_* events.
	Task *planner.Task

	// BatchNum is the 1-based parallel-batch index (0 for sequential).
	BatchNum int

	// Workers is the configured worker limit (batch_started/completed).
	Workers int

	// ProviderKind and ModelID identify the routed provider for task events.
	ProviderKind string
	ModelID      string

	// Status is the completion status (task_completed/failed/blocked/skipped).
	Status executor.CompletionStatus

	// Result carries the execution evidence (task_completed/failed).
	Result *executor.TaskExecutionResult

	// Changes carries the repository delta.
	Changes *executor.RepoChanges

	// Verification carries the verification outcome.
	Verification *executor.VerificationOutcome

	// ToolName is set for task_tool_started/finished.
	ToolName string

	// ToolStatus is set for task_tool_finished.
	ToolStatus string

	// Tokens carries usage when available (task_usage_updated).
	Tokens *TokenUsage

	// Metrics is set for metrics_updated and run_completed.
	Metrics *RunMetrics

	// Error is set for task_failed and run_cancelled.
	Error string

	// SkippedReason is set for task_skipped.
	SkippedReason string

	// Timestamp is when the event was emitted.
	Timestamp time.Time
}

// PlanPreview is the computed plan preview shared by CLI and TUI.
type PlanPreview struct {
	Prompt         string
	Classification taskclass.Result
	Plan           planner.ExecutionPlan
	TaskResults    []TaskRoute
	State          scheduler.ExecutionState
}

// TaskRoute binds one planner task to its routing decision.
type TaskRoute struct {
	Task     planner.Task
	State    scheduler.TaskState
	Decision modelrouter.Decision
}

// TokenUsage is the per-task token accounting.
type TokenUsage struct {
	Available         bool
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	ReasoningTokens   int
	TotalTokens       int
}

// RunMetrics is the accumulated run-level metrics snapshot.
type RunMetrics struct {
	RunWallMs          int64
	PlanningMs         int64
	RoutingMs          int64
	PeakWorkers        int
	Concurrency        string // "parallel" or "serialized"
	TotalProviderCalls int
	TotalInputTokens   int
	TotalOutputTokens  int
	Tasks              []TaskMetric
	Batches            []BatchMetric
	EffectiveSpeedup   *float64
	WorkerEfficiency   *float64
}

// TaskMetric is the per-task timing/usage record.
type TaskMetric struct {
	TaskID         string
	Title          string
	Batch          int
	ProviderKind   string
	Model          string
	ProviderCalls  int
	Status         string
	WallMs         int64
	QueueWaitMs    int64
	VerificationMs int64
	Verified       bool
	Tokens         TokenUsage
	Tools          ToolMetrics
}

// BatchMetric is the per-parallel-batch record.
type BatchMetric struct {
	Batch       int
	Workers     int
	TaskCount   int
	WallMs      int64
	PeakWorkers int
}

// ToolMetrics is the tool-usage counters.
type ToolMetrics struct {
	Attempted int
	Executed  int
	Succeeded int
	Failed    int
	Denied    int
}

// RunConfig configures one orchestration run.
type RunConfig struct {
	Prompt            string
	MaxTasks          int // 0 = full DAG (unlimited); 1 = once mode
	ParallelReadonly  bool
	MaxWorkers        int
	EnableMetrics     bool
	RouterProvider    string
	PreferredModel    string
	AllowProviders    []string
	DenyModels        []string
	RequireKnownPrice bool
	MaxInputCost      *float64
	MaxOutputCost     *float64
}

// ProviderBuilder constructs a provider for a routed model. The TUI supplies
// its own implementation that reuses the session's provider factory; tests
// inject a fake. Returning nil with a nil error means "no provider available".
type ProviderBuilder func(ctx context.Context, providerKind, modelID string) (agent.Provider, error)

// ModelSwitcherBuilder constructs an escalation provider when the agent swaps
// models mid-run. May be nil (no escalation).
type ModelSwitcherBuilder func(ctx context.Context, model string) (agent.Provider, error)

// ApproverFunc is the interactive approval callback. When non-nil, a prompt-
// required tool routes through it instead of the headless deny. The TUI supplies
// its real approver; tests inject a fake; the CLI leaves it nil for headless runs.
type ApproverFunc func(ctx context.Context, request agent.PermissionRequest) (agent.PermissionDecision, error)

// RunnerFactory builds an executor.Runner for a task. When nil, the coordinator
// builds an executor.AgentRunner from the provider and options.
type RunnerFactory func(provider agent.Provider, opts agent.Options) executor.Runner

// VerifierFunc wraps the verification callback.
type VerifierFunc func(ctx context.Context, workspaceRoot string, changedFiles []string) executor.VerificationOutcome

// RepoDeltaFunc captures the repository change set.
type RepoDeltaFunc func(ctx context.Context, workspaceRoot string) (executor.RepoChanges, error)

// CandidateBuilder builds routing candidates from configured providers.
type CandidateBuilder func(ctx context.Context) ([]modelregistry.ModelEntry, map[string]ProviderCandidate, error)

// ProviderCandidate maps a model ID to its source provider profile name.
type ProviderCandidate struct {
	ProfileName  string
	ProviderKind string
}

// Coordinator orchestrates one full run. It is constructed per-run and not
// reused. Events are emitted on Events channel; the consumer reads them in a
// select loop. The coordinator owns the goroutine(s); the consumer never does.
type Coordinator struct {
	cfg           RunConfig
	cwd           string
	builder       ProviderBuilder
	switcher      ModelSwitcherBuilder
	approver      ApproverFunc
	runnerFactory RunnerFactory
	verifier      VerifierFunc
	repoDelta     RepoDeltaFunc
	candidates    CandidateBuilder
	registry      *tools.Registry
	clock         func() time.Time
}

// Option configures a Coordinator.
type Option func(*Coordinator)

// WithApprover sets the interactive approver.
func WithApprover(fn ApproverFunc) Option {
	return func(c *Coordinator) { c.approver = fn }
}

// WithModelSwitcher sets the escalation provider builder.
func WithModelSwitcher(fn ModelSwitcherBuilder) Option {
	return func(c *Coordinator) { c.switcher = fn }
}

// WithRunnerFactory overrides the default AgentRunner.
func WithRunnerFactory(fn RunnerFactory) Option {
	return func(c *Coordinator) { c.runnerFactory = fn }
}

// WithVerifier sets the verification callback.
func WithVerifier(fn VerifierFunc) Option {
	return func(c *Coordinator) { c.verifier = fn }
}

// WithRepoDelta sets the repo-delta callback.
func WithRepoDelta(fn RepoDeltaFunc) Option {
	return func(c *Coordinator) { c.repoDelta = fn }
}

// WithClock overrides the clock (for tests).
func WithClock(fn func() time.Time) Option {
	return func(c *Coordinator) { c.clock = fn }
}

// Now returns the current time from the coordinator's clock.
func (c *Coordinator) Now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}
