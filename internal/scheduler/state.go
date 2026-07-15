package scheduler

import (
	"github.com/Gitlawb/zero/internal/planner"
)

// effectiveState computes the current state of a task without mutating the
// stored plan. Explicit transitions (Completed/Failed/Skipped) win; otherwise
// the state is derived from dependency completion and risk level.
//
//	explicit terminal set  -> that terminal state (Completed/Failed/Skipped)
//	dangerous task         -> Blocked
//	all deps Completed     -> Ready (a needs_approval task is still Ready; the
//	                          approval requirement is a runtime gate shown by
//	                          callers, not a scheduler block)
//	otherwise              -> Waiting
func (s *Scheduler) effectiveState(id string) TaskState {
	if t, ok := s.states[id]; ok && isTerminal(t) {
		return t
	}
	task, ok := s.byID[id]
	if !ok {
		return StatePlanned
	}
	if requiresApproval(task) {
		return StateBlocked
	}
	if s.dependenciesComplete(task) {
		return StateReady
	}
	return StateWaiting
}

// dependenciesComplete reports whether every dependency of t is Completed.
func (s *Scheduler) dependenciesComplete(t planner.Task) bool {
	for _, dep := range t.Dependencies {
		if s.effectiveState(dep) != StateCompleted {
			return false
		}
	}
	return true
}

// State returns a deterministic snapshot of the schedule, partitioning tasks
// into Ready/Blocked/Completed/Waiting/Failed/Skipped buckets.
func (s *Scheduler) State() ExecutionState {
	out := ExecutionState{}
	for _, t := range s.tasks {
		switch s.effectiveState(t.ID) {
		case StateReady:
			out.ReadyQueue = append(out.ReadyQueue, t)
		case StateBlocked:
			out.BlockedTasks = append(out.BlockedTasks, t)
		case StateCompleted:
			out.CompletedTasks = append(out.CompletedTasks, t)
		case StateWaiting:
			out.WaitingTasks = append(out.WaitingTasks, t)
		case StateFailed:
			out.FailedTasks = append(out.FailedTasks, t)
		case StateSkipped:
			out.SkippedTasks = append(out.SkippedTasks, t)
		default:
			out.ReadyQueue = append(out.ReadyQueue, t)
		}
	}
	sortTasks(out.ReadyQueue)
	sortTasks(out.BlockedTasks)
	sortTasks(out.CompletedTasks)
	sortTasks(out.WaitingTasks)
	sortTasks(out.FailedTasks)
	sortTasks(out.SkippedTasks)
	return out
}
