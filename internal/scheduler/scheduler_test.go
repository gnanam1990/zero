package scheduler

import (
	"testing"

	"github.com/Gitlawb/zero/internal/planner"
)

func task(id, title string, deps []string, safety planner.SafetyLevel, parallel bool, priority int) planner.Task {
	return planner.Task{
		ID:             id,
		Title:          title,
		TaskKind:       planner.KindUnknown,
		Dependencies:   deps,
		SafetyLevel:    safety,
		CanRunParallel: parallel,
		Priority:       priority,
		Status:         planner.StatusPlanned,
	}
}

func plan(tasks ...planner.Task) planner.ExecutionPlan {
	return planner.ExecutionPlan{PlanID: "p1", Tasks: tasks}
}

func TestNewSchedulerRejectsCycle(t *testing.T) {
	p := plan(
		task("a", "A", []string{"b"}, planner.SafetySafe, false, 0),
		task("b", "B", []string{"a"}, planner.SafetySafe, false, 0),
	)
	if _, err := NewScheduler(p); err == nil {
		t.Fatal("expected cycle to be rejected, got nil")
	}
}

func TestNewSchedulerRejectsUnknownDependency(t *testing.T) {
	p := plan(task("a", "A", []string{"missing"}, planner.SafetySafe, false, 0))
	if _, err := NewScheduler(p); err == nil {
		t.Fatal("expected unknown dependency to be rejected, got nil")
	}
}

func TestNewSchedulerRejectsDuplicateIDs(t *testing.T) {
	p := plan(
		task("a", "A", nil, planner.SafetySafe, false, 0),
		task("a", "A2", nil, planner.SafetySafe, false, 0),
	)
	if _, err := NewScheduler(p); err == nil {
		t.Fatal("expected duplicate ids to be rejected, got nil")
	}
}

func TestSingleTaskReady(t *testing.T) {
	p := plan(task("a", "A", nil, planner.SafetySafe, false, 0))
	s, err := NewScheduler(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	st := s.State()
	if len(st.ReadyQueue) != 1 || st.ReadyQueue[0].ID != "a" {
		t.Fatalf("expected one ready task, got %+v", st)
	}
	if got, ok := s.NextReady(); !ok || got.ID != "a" {
		t.Fatalf("expected next ready a, got %v ok=%v", got, ok)
	}
}

func TestApprovalGateBlocks(t *testing.T) {
	p := plan(task("a", "A", nil, planner.SafetyDangerous, false, 0))
	s, _ := NewScheduler(p)
	st := s.State()
	if len(st.BlockedTasks) != 1 {
		t.Fatalf("expected 1 blocked task, got %+v", st)
	}
	if _, ok := s.NextReady(); ok {
		t.Fatalf("did not expect a ready task behind a dangerous gate")
	}
}

func TestNeedsApprovalIsReady(t *testing.T) {
	// A needs_approval task is schedulable (Ready); the approval requirement is a
	// runtime gate, not a scheduler block.
	p := plan(task("a", "A", nil, planner.SafetyNeedsApproval, false, 0))
	s, _ := NewScheduler(p)
	st := s.State()
	if len(st.ReadyQueue) != 1 {
		t.Fatalf("expected 1 ready task, got %+v", st)
	}
	if len(st.BlockedTasks) != 0 {
		t.Fatalf("needs_approval must not block the scheduler, got %+v", st)
	}
}

func TestDependencyChain(t *testing.T) {
	p := plan(
		task("root", "Root", nil, planner.SafetySafe, false, 0),
		task("child", "Child", []string{"root"}, planner.SafetySafe, false, 0),
	)
	s, err := NewScheduler(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	st := s.State()
	if len(st.ReadyQueue) != 1 || st.ReadyQueue[0].ID != "root" {
		t.Fatalf("expected root ready, got %+v", st)
	}
	if len(st.WaitingTasks) != 1 || st.WaitingTasks[0].ID != "child" {
		t.Fatalf("expected child waiting, got %+v", st)
	}
	if err := s.MarkCompleted("root"); err != nil {
		t.Fatalf("unexpected mark error: %v", err)
	}
	st = s.State()
	if len(st.ReadyQueue) != 1 || st.ReadyQueue[0].ID != "child" {
		t.Fatalf("expected child ready after root completed, got %+v", st)
	}
	if len(st.CompletedTasks) != 1 || st.CompletedTasks[0].ID != "root" {
		t.Fatalf("expected root completed, got %+v", st)
	}
}

func TestParallelReporting(t *testing.T) {
	p := plan(
		task("a", "A", nil, planner.SafetySafe, true, 0),
		task("b", "B", nil, planner.SafetySafe, true, 0),
		task("c", "C", nil, planner.SafetySafe, false, 0),
	)
	s, _ := NewScheduler(p)
	par := s.ReadyParallel()
	if len(par) != 2 {
		t.Fatalf("expected 2 parallel tasks, got %d", len(par))
	}
	for _, tsk := range par {
		if !tsk.CanRunParallel {
			t.Fatalf("non-parallel task reported in ReadyParallel: %v", tsk.ID)
		}
	}
}

func TestFailureAndSkip(t *testing.T) {
	p := plan(
		task("a", "A", nil, planner.SafetySafe, false, 0),
		task("b", "B", []string{"a"}, planner.SafetySafe, false, 0),
	)
	s, _ := NewScheduler(p)
	if err := s.MarkFailed("a"); err != nil {
		t.Fatalf("unexpected mark error: %v", err)
	}
	st := s.State()
	if len(st.FailedTasks) != 1 {
		t.Fatalf("expected 1 failed, got %+v", st)
	}
	if len(st.ReadyQueue) != 0 || len(st.WaitingTasks) != 1 {
		t.Fatalf("expected b still waiting after failure, got %+v", st)
	}
	if err := s.MarkSkipped("b"); err != nil {
		t.Fatalf("unexpected mark error: %v", err)
	}
	st = s.State()
	if len(st.SkippedTasks) != 1 {
		t.Fatalf("expected 1 skipped, got %+v", st)
	}
}

func TestMarkUnknownID(t *testing.T) {
	s, _ := NewScheduler(plan(task("a", "A", nil, planner.SafetySafe, false, 0)))
	if err := s.MarkCompleted("nope"); err == nil {
		t.Fatal("expected error marking unknown id")
	}
}

func TestDeterministicOrderAndRepeatability(t *testing.T) {
	p := plan(
		task("a", "A", nil, planner.SafetySafe, false, 1),
		task("b", "B", nil, planner.SafetySafe, false, 5),
		task("c", "C", nil, planner.SafetySafe, false, 3),
	)
	s1, _ := NewScheduler(p)
	s2, _ := NewScheduler(p)
	for i := 0; i < 20; i++ {
		r1, _ := s1.NextReady()
		r2, _ := s2.NextReady()
		if r1.ID != r2.ID {
			t.Fatalf("non-deterministic ordering: %s vs %s", r1.ID, r2.ID)
		}
	}
	// highest priority first
	r, _ := s1.NextReady()
	if r.ID != "b" {
		t.Fatalf("expected highest priority b first, got %s", r.ID)
	}
}

func TestReset(t *testing.T) {
	p := plan(task("a", "A", nil, planner.SafetySafe, false, 0))
	s, _ := NewScheduler(p)
	_ = s.MarkCompleted("a")
	if st := s.State(); len(st.CompletedTasks) != 1 {
		t.Fatalf("expected completed before reset, got %+v", st)
	}
	s.Reset()
	if st := s.State(); len(st.CompletedTasks) != 0 || len(st.ReadyQueue) != 1 {
		t.Fatalf("expected clean state after reset, got %+v", st)
	}
}

func TestNoMutationOfInputPlan(t *testing.T) {
	task := task("a", "A", nil, planner.SafetySafe, false, 0)
	originalStatus := task.Status
	p := plan(task)
	s, err := NewScheduler(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = s.MarkCompleted("a")
	if p.Tasks[0].Status != originalStatus {
		t.Fatalf("input plan was mutated: %v", p.Tasks[0].Status)
	}
}
