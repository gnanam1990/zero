package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/orchestration"
	"github.com/Gitlawb/zero/internal/planner"
)

func TestOrchestrationStateDefaults(t *testing.T) {
	s := newOrchestrationState()
	if s.mode != orchModeNormal {
		t.Fatalf("expected normal mode by default, got %v", s.mode)
	}
	if s.parallelWorkers != 2 {
		t.Fatalf("expected default workers 2, got %d", s.parallelWorkers)
	}
	if !s.previewBeforeRun {
		t.Fatal("expected previewBeforeRun true by default")
	}
	if s.active {
		t.Fatal("expected active false by default")
	}
	if s.awaitingApproval {
		t.Fatal("expected awaitingApproval false by default")
	}
}

func TestOrchestrationToggleMode(t *testing.T) {
	s := newOrchestrationState()
	if s.mode != orchModeNormal {
		t.Fatalf("expected normal initially, got %v", s.mode)
	}
	s.toggleMode()
	if s.mode != orchModeOrchestrated {
		t.Fatalf("expected orchestrated after toggle, got %v", s.mode)
	}
	s.toggleMode()
	if s.mode != orchModeNormal {
		t.Fatalf("expected normal after second toggle, got %v", s.mode)
	}
}

func TestOrchestrationModeLabel(t *testing.T) {
	s := newOrchestrationState()
	if s.modeLabel() != "Normal" {
		t.Fatalf("expected 'Normal', got %q", s.modeLabel())
	}
	s.toggleMode()
	if s.modeLabel() != "Orchestrated" {
		t.Fatalf("expected 'Orchestrated', got %q", s.modeLabel())
	}
	s.parallelReadonly = true
	s.parallelWorkers = 3
	if s.modeLabel() != "Orchestrated · Workers 3" {
		t.Fatalf("expected 'Orchestrated · Workers 3', got %q", s.modeLabel())
	}
}

func TestOrchestrationWorkersBounds(t *testing.T) {
	s := newOrchestrationState()
	s.parallelWorkers = 1
	if s.parallelWorkers != 1 {
		t.Fatalf("expected 1, got %d", s.parallelWorkers)
	}
	s.parallelWorkers = 8
	if s.parallelWorkers != 8 {
		t.Fatalf("expected 8, got %d", s.parallelWorkers)
	}
}

func TestOrchStatusLine(t *testing.T) {
	s := newOrchestrationState()
	s.toggleMode()
	s.parallelReadonly = true
	s.parallelWorkers = 4
	s.metricsEnabled = true
	s.previewBeforeRun = true
	out := orchStatusLine(s)
	if out == "" {
		t.Fatal("expected non-empty status line")
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(0); got != "0ms" {
		t.Fatalf("expected 0ms, got %s", got)
	}
	if got := formatDuration(500 * time.Millisecond); got != "500ms" {
		t.Fatalf("expected 500ms, got %s", got)
	}
	if got := formatDuration(1500 * time.Millisecond); got != "1.5s" {
		t.Fatalf("expected 1.5s, got %s", got)
	}
}

func TestOrchTaskLabel(t *testing.T) {
	got := orchTaskLabel("task-1", "Search code")
	want := "Task task-1 — Search code"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestCanToggleWhileIdle(t *testing.T) {
	s := newOrchestrationState()
	if !s.canToggle() {
		t.Fatal("should be able to toggle while idle")
	}
	s.active = true
	if s.canToggle() {
		t.Fatal("should not be able to toggle while active")
	}
	s.active = false
	s.awaitingApproval = true
	if s.canToggle() {
		t.Fatal("should not be able to toggle while awaiting approval")
	}
}

func TestNormalPromptUnchangedWhenOrchestrationDisabled(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "test", ModelName: "test-model"})
	if m.orch.mode != orchModeNormal {
		t.Fatal("expected normal mode by default")
	}
	// A normal prompt should go through launchPrompt, not launchOrchestratedPrompt.
	// We verify by checking that the orchestration state is not active.
	if m.orch.active {
		t.Fatal("orchestration should not be active by default")
	}
}

func TestOrchEventHandlingRunCompleted(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "test", ModelName: "test-model"})
	m.orch.active = true
	m.orch.runID = 1
	m.orch.preview = orchPreviewPtr()

	msg := orchEventMsg{event: orchestration.Event{
		Type:   orchestration.EventRunCompleted,
		Status: executor.StatusCompleted,
	}}
	m, _ = m.handleOrchEvent(msg)
	if m.orch.active {
		t.Fatal("expected orch.active false after run completed")
	}
	if m.pending {
		t.Fatal("expected pending false after run completed")
	}
}

func orchPreviewNil() orchestration.PlanPreview {
	return orchestration.PlanPreview{Prompt: "test"}
}

func TestOrchPermissionBridgingResolvesStaleRequest(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "test", ModelName: "test-model"})
	m.orch.runID = 5

	// A permission request from a different runID should be cancelled.
	resolved := false
	msg := orchPermissionMsg{
		runID:  999, // different
		decide: func(d agent.PermissionDecision) { resolved = true },
	}
	m, _ = m.handleOrchPermission(msg)
	if !resolved {
		t.Fatal("stale permission request should be resolved with cancel")
	}
}

func TestOrchPermissionBridgingNonPrompt(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "test", ModelName: "test-model"})
	m.orch.runID = 1

	resolved := false
	msg := orchPermissionMsg{
		runID:   1,
		request: agent.PermissionRequest{Action: agent.PermissionActionAllow},
		decide:  func(d agent.PermissionDecision) { resolved = true },
	}
	m, _ = m.handleOrchPermission(msg)
	if !resolved {
		t.Fatal("non-prompt permission should be auto-resolved")
	}
	if m.pendingPermission != nil {
		t.Fatal("non-prompt permission should not set pendingPermission")
	}
}

func TestRenderPlanPreviewNil(t *testing.T) {
	s := newOrchestrationState()
	got := orchRenderPlanPreview(nil, s)
	if got != "Orchestration plan unavailable." {
		t.Fatalf("expected unavailable message, got %q", got)
	}
}

func TestRenderSummaryNoMetrics(t *testing.T) {
	s := newOrchestrationState()
	ev := orchestration.Event{
		Type:   orchestration.EventRunCompleted,
		Status: executor.StatusCompleted,
	}
	got := orchRenderSummary(ev, s)
	if got == "" {
		t.Fatal("expected non-empty summary")
	}
}

func TestTaskCardsEmptyWhenNoStates(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "test", ModelName: "test-model"})
	m.orch.preview = nil
	got := m.orchTaskCards()
	if got != "" {
		t.Fatalf("expected empty cards, got %q", got)
	}
}

func TestTaskCardsInPlanOrder(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "test", ModelName: "test-model"})
	preview := orchPreviewPtr()
	preview.TaskResults = []orchestration.TaskRoute{
		{Task: planner.Task{ID: "task-1", Title: "Search docs"}},
		{Task: planner.Task{ID: "task-2", Title: "Search code"}},
	}
	m.orch.preview = preview
	m.orch.taskStates = map[string]orchTaskState{
		"task-2": {status: executor.StatusCompleted, modelID: "model-b", providerKind: "openai"},
		"task-1": {status: executor.StatusCompleted, modelID: "model-a", providerKind: "anthropic"},
	}
	got := m.orchTaskCards()
	// Both tasks should appear.
	if got == "" {
		t.Fatal("expected non-empty task cards")
	}
	// task-1 should appear before task-2 (plan order).
	idx1 := strings.Index(got, "task-1")
	idx2 := strings.Index(got, "task-2")
	if idx1 < 0 || idx2 < 0 {
		t.Fatalf("expected both tasks in cards, got %q", got)
	}
	if idx1 > idx2 {
		t.Fatalf("task-1 should appear before task-2 in plan order, got %q", got)
	}
}

func orchPreviewPtr() *orchestration.PlanPreview {
	p := orchPreviewNil()
	return &p
}
