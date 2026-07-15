package orchestration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// fakeRunner returns a predetermined result without calling any provider.
type fakeRunner struct {
	result executor.TaskExecutionResult
	err    error
}

func (r *fakeRunner) RunTask(ctx context.Context, req executor.TaskExecutionRequest) (executor.TaskExecutionResult, error) {
	return r.result, r.err
}

// fakeProvider emits a fixed sequence of turns.
type fakeProvider struct {
	mu    sync.Mutex
	turns [][]zeroruntime.StreamEvent
	count int
}

func (p *fakeProvider) StreamCompletion(ctx context.Context, _ zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	p.mu.Lock()
	idx := p.count
	p.count++
	p.mu.Unlock()
	events := []zeroruntime.StreamEvent{{Type: zeroruntime.StreamEventDone}}
	if idx < len(p.turns) {
		events = p.turns[idx]
	}
	ch := make(chan zeroruntime.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func fakeAnswerProvider(answer string) *fakeProvider {
	return &fakeProvider{turns: [][]zeroruntime.StreamEvent{
		{{Type: zeroruntime.StreamEventText, Content: answer}, {Type: zeroruntime.StreamEventDone}},
	}}
}

func fakeCandidates() ([]modelregistry.ModelEntry, map[string]ProviderCandidate, error) {
	reg, err := modelregistry.DefaultRegistry()
	if err != nil {
		return nil, nil, err
	}
	entries := reg.List(modelregistry.ListOptions{IncludeDeprecated: true})
	profileMap := map[string]ProviderCandidate{}
	for _, e := range entries {
		profileMap[strings.ToLower(e.ID)] = ProviderCandidate{ProfileName: string(e.Provider), ProviderKind: string(e.Provider)}
	}
	return entries, profileMap, nil
}

func TestBuildPlanDeterministic(t *testing.T) {
	ctx := context.Background()
	cfg := RunConfig{Prompt: "Implement OAuth login and write tests"}
	cands, _, _ := fakeCandidates()
	first, err := BuildPlan(ctx, cfg.Prompt, cfg, true, cands)
	if err != nil {
		t.Fatalf("BuildPlan error: %v", err)
	}
	second, err := BuildPlan(ctx, cfg.Prompt, cfg, true, cands)
	if err != nil {
		t.Fatalf("BuildPlan error: %v", err)
	}
	if first.Plan.PlanID != second.Plan.PlanID {
		t.Fatalf("PlanID not deterministic: %s vs %s", first.Plan.PlanID, second.Plan.PlanID)
	}
	if len(first.Plan.Tasks) != len(second.Plan.Tasks) {
		t.Fatalf("task count changed: %d vs %d", len(first.Plan.Tasks), len(second.Plan.Tasks))
	}
}

func TestBuildPlanHasTasks(t *testing.T) {
	ctx := context.Background()
	cfg := RunConfig{Prompt: "Implement OAuth login and write tests"}
	cands, _, _ := fakeCandidates()
	preview, err := BuildPlan(ctx, cfg.Prompt, cfg, true, cands)
	if err != nil {
		t.Fatalf("BuildPlan error: %v", err)
	}
	if len(preview.Plan.Tasks) < 2 {
		t.Fatalf("expected at least 2 tasks, got %d", len(preview.Plan.Tasks))
	}
	if len(preview.TaskResults) != len(preview.Plan.Tasks) {
		t.Fatalf("TaskResults count %d != Tasks count %d", len(preview.TaskResults), len(preview.Plan.Tasks))
	}
}

func TestCoordinatorRunSequentialCompleted(t *testing.T) {
	ctx := context.Background()
	cands, profileMap, _ := fakeCandidates()
	cfg := RunConfig{
		Prompt:         "Find all references to the session store",
		PreferredModel: "gpt-4o-mini",
	}

	builder := func(ctx context.Context, providerKind, modelID string) (agent.Provider, error) {
		return fakeAnswerProvider("Found 3 references in pkg/x"), nil
	}
	candidates := func(ctx context.Context) ([]modelregistry.ModelEntry, map[string]ProviderCandidate, error) {
		return cands, profileMap, nil
	}

	// Use a RunnerFactory that produces a completed read-only result.
	runnerFactory := func(provider agent.Provider, opts agent.Options) executor.Runner {
		return &fakeRunner{
			result: executor.TaskExecutionResult{
				FinalAnswer: "Found 3 references in pkg/x",
				ToolEvents:  []executor.ToolEvent{{Name: "grep", Kind: "read"}},
				AgentResult: agent.Result{FinalAnswer: "Found 3 references in pkg/x"},
			},
		}
	}

	coord := New(cfg, "/tmp", builder, candidates, nil, agent.Options{}, WithRunnerFactory(runnerFactory))
	preview, err := BuildPlan(ctx, cfg.Prompt, cfg, true, cands)
	if err != nil {
		t.Fatalf("BuildPlan error: %v", err)
	}

	events := coord.Run(ctx, preview)
	var sawCompleted, sawRunCompleted bool
	for ev := range events {
		switch ev.Type {
		case EventTaskCompleted:
			sawCompleted = true
		case EventRunCompleted:
			sawRunCompleted = true
			if ev.Status != executor.StatusCompleted {
				t.Fatalf("expected completed status, got %s", ev.Status)
			}
		}
	}
	if !sawCompleted {
		t.Fatal("expected task completed event")
	}
	if !sawRunCompleted {
		t.Fatal("expected run completed event")
	}
}

func TestCoordinatorRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cands, profileMap, _ := fakeCandidates()
	cfg := RunConfig{Prompt: "Implement OAuth login"}

	builder := func(ctx context.Context, providerKind, modelID string) (agent.Provider, error) {
		return fakeAnswerProvider("done"), nil
	}
	candidates := func(ctx context.Context) ([]modelregistry.ModelEntry, map[string]ProviderCandidate, error) {
		return cands, profileMap, nil
	}

	coord := New(cfg, "/tmp", builder, candidates, nil, agent.Options{})
	preview, _ := BuildPlan(ctx, cfg.Prompt, cfg, true, cands)

	// Cancel before reading events.
	cancel()
	events := coord.Run(ctx, preview)
	// Drain — the coordinator should terminate.
	for range events {
	}
}

func TestFormatMetricsCompact(t *testing.T) {
	m := &RunMetrics{
		RunWallMs:          32960,
		PeakWorkers:        2,
		Concurrency:        "parallel",
		TotalProviderCalls: 13,
		TotalInputTokens:   247849,
		TotalOutputTokens:  3018,
		Tasks: []TaskMetric{
			{TaskID: "task-1", Status: "completed"},
			{TaskID: "task-2", Status: "completed"},
		},
	}
	sp := 1.5
	eff := 0.75
	m.EffectiveSpeedup = &sp
	m.WorkerEfficiency = &eff

	out := FormatMetricsCompact(m)
	if !strings.Contains(out, "Completed 2/2") {
		t.Fatalf("missing completed count: %s", out)
	}
	if !strings.Contains(out, "Peak workers: 2") {
		t.Fatalf("missing peak workers: %s", out)
	}
	if !strings.Contains(out, "Provider calls: 13") {
		t.Fatalf("missing provider calls: %s", out)
	}
	if !strings.Contains(out, "Effective speedup: 1.50x") {
		t.Fatalf("missing speedup: %s", out)
	}
}

func TestFormatMetricsCompactUnavailable(t *testing.T) {
	m := &RunMetrics{
		RunWallMs:          5000,
		PeakWorkers:        1,
		Concurrency:        "serialized",
		TotalProviderCalls: 0,
		Tasks:              []TaskMetric{},
	}
	out := FormatMetricsCompact(m)
	if !strings.Contains(out, "Tokens: unavailable") {
		t.Fatalf("expected unavailable tokens: %s", out)
	}
	if strings.Contains(out, "ran concurrently") {
		t.Fatalf("should not claim concurrency for peak 1: %s", out)
	}
}

func TestFormatMetricsCompactSerialized(t *testing.T) {
	m := &RunMetrics{
		RunWallMs:          67830,
		PeakWorkers:        1,
		Concurrency:        "serialized",
		TotalProviderCalls: 16,
		TotalInputTokens:   317980,
		TotalOutputTokens:  3138,
		Tasks: []TaskMetric{
			{TaskID: "task-1", Status: "completed"},
			{TaskID: "task-2", Status: "completed"},
		},
	}
	out := FormatMetricsCompact(m)
	if !strings.Contains(out, "Peak workers: 1") {
		t.Fatalf("missing peak 1: %s", out)
	}
	if strings.Contains(out, "Effective speedup") {
		t.Fatalf("should not show speedup for serialized: %s", out)
	}
}

func TestEventTypesComplete(t *testing.T) {
	expected := []EventType{
		EventPlanCreated,
		EventPlanAwaitingApproval,
		EventRunStarted,
		EventBatchStarted,
		EventTaskQueued,
		EventTaskStarted,
		EventTaskToolStarted,
		EventTaskToolFinished,
		EventTaskUsageUpdated,
		EventTaskCompleted,
		EventTaskFailed,
		EventTaskBlocked,
		EventTaskSkipped,
		EventBatchCompleted,
		EventMetricsUpdated,
		EventRunCompleted,
		EventRunCancelled,
	}
	seen := map[EventType]bool{}
	for _, e := range expected {
		seen[e] = true
	}
	if len(seen) != len(expected) {
		t.Fatalf("duplicate event types detected")
	}
}

func TestBuildPlanReadonlyTask(t *testing.T) {
	ctx := context.Background()
	cfg := RunConfig{Prompt: "Find all references to the session store"}
	cands, _, _ := fakeCandidates()
	preview, err := BuildPlan(ctx, cfg.Prompt, cfg, true, cands)
	if err != nil {
		t.Fatalf("BuildPlan error: %v", err)
	}
	if len(preview.Plan.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(preview.Plan.Tasks))
	}
	if preview.Plan.Tasks[0].TaskKind != "repository_search" {
		t.Fatalf("expected repository_search, got %s", preview.Plan.Tasks[0].TaskKind)
	}
}

func TestBuildPlanParallelSearch(t *testing.T) {
	ctx := context.Background()
	cfg := RunConfig{Prompt: "Search the docs and search the code"}
	cands, _, _ := fakeCandidates()
	preview, err := BuildPlan(ctx, cfg.Prompt, cfg, true, cands)
	if err != nil {
		t.Fatalf("BuildPlan error: %v", err)
	}
	if len(preview.Plan.Tasks) != 2 {
		t.Fatalf("expected 2 parallel tasks, got %d", len(preview.Plan.Tasks))
	}
	for _, task := range preview.Plan.Tasks {
		if !task.CanRunParallel {
			t.Fatalf("task %s should be parallel", task.ID)
		}
	}
}

func TestRunMetricsSumProviderCalls(t *testing.T) {
	ctx := context.Background()
	cands, profileMap, _ := fakeCandidates()
	cfg := RunConfig{
		Prompt:           "Search the docs and search the code",
		ParallelReadonly: true,
		MaxWorkers:       2,
		EnableMetrics:    true,
	}

	builder := func(ctx context.Context, providerKind, modelID string) (agent.Provider, error) {
		return fakeAnswerProvider(fmt.Sprintf("Search result from %s", modelID)), nil
	}
	candidates := func(ctx context.Context) ([]modelregistry.ModelEntry, map[string]ProviderCandidate, error) {
		return cands, profileMap, nil
	}

	runnerFactory := func(provider agent.Provider, opts agent.Options) executor.Runner {
		return &fakeRunner{
			result: executor.TaskExecutionResult{
				FinalAnswer: "Search complete",
				ToolEvents:  []executor.ToolEvent{{Name: "grep", Kind: "read"}},
				AgentResult: agent.Result{FinalAnswer: "Search complete"},
			},
		}
	}

	coord := New(cfg, "/tmp", builder, candidates, nil, agent.Options{}, WithRunnerFactory(runnerFactory))
	preview, _ := BuildPlan(ctx, cfg.Prompt, cfg, true, cands)
	events := coord.Run(ctx, preview)
	var metrics *RunMetrics
	var taskCount int
	for ev := range events {
		if ev.Type == EventTaskCompleted {
			taskCount++
		}
		if ev.Type == EventRunCompleted && ev.Metrics != nil {
			metrics = ev.Metrics
		}
	}
	if taskCount < 2 {
		t.Fatalf("expected at least 2 task completed events, got %d", taskCount)
	}
	if metrics == nil {
		t.Fatal("expected metrics in run completed event")
	}
	if metrics.PeakWorkers < 1 {
		t.Fatalf("expected peak >= 1, got %d", metrics.PeakWorkers)
	}
}
