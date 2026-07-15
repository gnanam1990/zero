package scheduler

import (
	"fmt"
	"sort"

	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/planner"
)

// Scheduler owns the runtime scheduling state for one ExecutionPlan. It never
// mutates the planner output it was given; it keeps its own copy of the tasks.
type Scheduler struct {
	plan   planner.ExecutionPlan
	tasks  []planner.Task
	byID   map[string]planner.Task
	states map[string]TaskState
}

// NewScheduler validates the plan and returns a scheduler. It rejects invalid
// graphs (duplicate ids, self/unknown dependencies, cycles) up front. The
// provided plan is never mutated.
func NewScheduler(plan planner.ExecutionPlan) (*Scheduler, error) {
	if err := planner.Validate(plan); err != nil {
		return nil, err
	}
	tasks := make([]planner.Task, len(plan.Tasks))
	byID := make(map[string]planner.Task, len(plan.Tasks))
	for i, t := range plan.Tasks {
		copied := copyTask(t)
		tasks[i] = copied
		byID[t.ID] = copied
	}
	return &Scheduler{
		plan:   plan,
		tasks:  tasks,
		byID:   byID,
		states: make(map[string]TaskState, len(tasks)),
	}, nil
}

// copyTask deep-copies the planner task so the scheduler never aliases or
// mutates the caller's ExecutionPlan.
func copyTask(t planner.Task) planner.Task {
	c := t
	if t.Dependencies != nil {
		c.Dependencies = append([]string(nil), t.Dependencies...)
	}
	if t.RequiredCapabilities != nil {
		c.RequiredCapabilities = append([]modelregistry.ModelCapability(nil), t.RequiredCapabilities...)
	}
	return c
}

// NextReady returns the next Ready task in a deterministic order (highest
// Priority first, then ID ascending). It does NOT change any state — the
// scheduler never executes, and Running is only represented, not set here.
func (s *Scheduler) NextReady() (planner.Task, bool) {
	var ready []planner.Task
	for _, t := range s.tasks {
		if s.effectiveState(t.ID) == StateReady {
			ready = append(ready, t)
		}
	}
	sortTasks(ready)
	if len(ready) == 0 {
		return planner.Task{}, false
	}
	return ready[0], true
}

// ReadyParallel returns all Ready tasks that are also marked CanRunParallel.
// The scheduler only *reports* them; it never launches them.
func (s *Scheduler) ReadyParallel() []planner.Task {
	var out []planner.Task
	for _, t := range s.tasks {
		if s.effectiveState(t.ID) == StateReady && t.CanRunParallel {
			out = append(out, t)
		}
	}
	sortTasks(out)
	return out
}

// MarkCompleted explicitly transitions a task to Completed.
func (s *Scheduler) MarkCompleted(id string) error {
	return s.mark(id, StateCompleted)
}

// MarkFailed explicitly transitions a task to Failed.
func (s *Scheduler) MarkFailed(id string) error {
	return s.mark(id, StateFailed)
}

// MarkSkipped explicitly transitions a task to Skipped.
func (s *Scheduler) MarkSkipped(id string) error {
	return s.mark(id, StateSkipped)
}

func (s *Scheduler) mark(id string, state TaskState) error {
	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("scheduler: unknown task id %q", id)
	}
	s.states[id] = state
	return nil
}

// Reset clears all runtime transitions, returning every task to its
// dependency/approval-derived state (Planned → Ready/Waiting/Blocked).
func (s *Scheduler) Reset() {
	s.states = make(map[string]TaskState, len(s.tasks))
}

// sortTasks orders by Priority descending, then ID ascending, for determinism.
func sortTasks(tasks []planner.Task) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].Priority != tasks[j].Priority {
			return tasks[i].Priority > tasks[j].Priority
		}
		return tasks[i].ID < tasks[j].ID
	})
}

// requiresApproval reports whether a task is hard-blocked behind human approval.
// Only dangerous tasks are scheduler-blocked; needs_approval tasks remain Ready
// (the approval requirement is a runtime gate surfaced by callers, not a block).
func requiresApproval(t planner.Task) bool {
	return t.SafetyLevel == planner.SafetyDangerous
}
