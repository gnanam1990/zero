package scheduler

import (
	"github.com/Gitlawb/zero/internal/planner"
)

// TaskState is the scheduler's runtime state for a single task. The scheduler
// only ever *decides* these states; it never executes, never calls providers,
// and never transitions a task into Running itself (Running is reserved for a
// future executor).
type TaskState string

const (
	StatePlanned   TaskState = "planned"
	StateReady     TaskState = "ready"
	StateRunning   TaskState = "running"
	StateWaiting   TaskState = "waiting"
	StateBlocked   TaskState = "blocked"
	StateCompleted TaskState = "completed"
	StateFailed    TaskState = "failed"
	StateSkipped   TaskState = "skipped"
)

// terminalStates are the states from which a task never leaves without an
// explicit transition.
var terminalStates = map[TaskState]bool{
	StateCompleted: true,
	StateFailed:    true,
	StateSkipped:   true,
}

func isTerminal(s TaskState) bool {
	return terminalStates[s]
}

// ExecutionState is the deterministic snapshot of the schedule: every task is
// placed into exactly one bucket according to its effective state.
type ExecutionState struct {
	ReadyQueue     []planner.Task
	BlockedTasks   []planner.Task
	CompletedTasks []planner.Task
	WaitingTasks   []planner.Task
	FailedTasks    []planner.Task
	SkippedTasks   []planner.Task
}
