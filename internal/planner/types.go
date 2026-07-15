package planner

import (
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/taskclass"
)

// TaskKind is the planner's own deterministic task taxonomy. It is intentionally
// a small, fixed set (a subset/renaming of classifier kinds) so execution graphs
// are stable and never depend on an LLM's wording.
type TaskKind string

const (
	KindImplementation   TaskKind = "implementation"
	KindRepositorySearch TaskKind = "repository_search"
	KindCodeReview       TaskKind = "code_review"
	KindSecurityReview   TaskKind = "security_review"
	KindArchitecture     TaskKind = "architecture"
	KindDocumentation    TaskKind = "documentation"
	KindTesting          TaskKind = "testing"
	KindTestExecution    TaskKind = "test_execution"
	KindDebugging        TaskKind = "debugging"
	KindRefactoring      TaskKind = "refactoring"
	KindShellOperation   TaskKind = "shell_operation"
	KindImageAnalysis    TaskKind = "image_analysis"
	KindUnknown          TaskKind = "unknown"
)

// Complexity is a simple, non-AI ordinal estimate.
type Complexity string

const (
	ComplexitySmall   Complexity = "small"
	ComplexityMedium  Complexity = "medium"
	ComplexityLarge   Complexity = "large"
	ComplexityUnknown Complexity = "unknown"
)

// SafetyLevel is a deterministic risk classification.
type SafetyLevel string

const (
	SafetySafe          SafetyLevel = "safe"
	SafetyNeedsApproval SafetyLevel = "needs_approval"
	SafetyDangerous     SafetyLevel = "dangerous"
)

// TaskStatus is the only lifecycle state a planned task has at this stage.
type TaskStatus string

const (
	StatusPlanned TaskStatus = "planned"
)

// Task is a single node in the deterministic execution graph.
type Task struct {
	ID                   string
	Title                string
	TaskKind             TaskKind
	Description          string
	RequiredCapabilities []modelregistry.ModelCapability
	Dependencies         []string
	Priority             int
	CanRunParallel       bool
	EstimatedComplexity  Complexity
	SafetyLevel          SafetyLevel
	Status               TaskStatus
}

// TaskDependency is a directed edge: From depends on (must follow) To.
type TaskDependency struct {
	From string
	To   string
}

// PlannerInput is the deterministic planner input.
type PlannerInput struct {
	Prompt             string
	TaskClassification taskclass.Result
	RepositoryPresent  bool
	AvailableTools     []string
}

// ExecutionPlan is the deterministic output: a DAG of tasks plus metadata.
type ExecutionPlan struct {
	PlanID       string
	Summary      string
	Tasks        []Task
	Dependencies []TaskDependency
	Metadata     map[string]string
}
