package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/planner"
)

// --- parse tests ---

func TestParseOrchestratedMetricsRequiresOrchestration(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--orchestrated-metrics", "do it"})
	if err == nil {
		t.Fatal("expected error: --orchestrated-metrics requires --orchestrated/--orchestrated-once")
	}
}

func TestParseMetricsJSONRequiresOrchestration(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--metrics-json", "out.json", "do it"})
	if err == nil {
		t.Fatal("expected error: --metrics-json requires --orchestrated/--orchestrated-once")
	}
}

func TestParseOrchestratedMetricsAcceptedWithOnce(t *testing.T) {
	opts, _, err := parseExecArgs([]string{"--orchestrated-once", "--orchestrated-metrics", "do it"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.orchestratedMetrics {
		t.Error("orchestratedMetrics = false, want true")
	}
}

func TestParseMetricsJSONAcceptedWithOrchestrated(t *testing.T) {
	opts, _, err := parseExecArgs([]string{"--orchestrated", "--metrics-json", "out.json", "do it"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.metricsJSON != "out.json" {
		t.Errorf("metricsJSON = %q, want out.json", opts.metricsJSON)
	}
}

// --- barrier runner: forces two workers to overlap so concurrency is deterministic ---

// successRunner mirrors the canonical once-mode fixture: a mutating final answer
// with a write_file action and a passing verification, so the completion gate
// reports success (exit 0) under the real plan.
func successRunner() fakeRunner {
	return fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "done"},
		FinalAnswer: "done",
		ToolEvents:  []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}},
	}}
}

type barrierRunner struct {
	barrier *sync.WaitGroup
}

func (r barrierRunner) RunTask(ctx context.Context, req executor.TaskExecutionRequest) (executor.TaskExecutionResult, error) {
	// Start barrier: every worker must reach RunTask before any proceeds, so
	// the concurrency window (both enterWorker'd) is observed deterministically.
	r.barrier.Done()
	r.barrier.Wait()
	return executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "read result for " + req.Task.ID},
		FinalAnswer: "read result for " + req.Task.ID,
		ToolEvents:  []executor.ToolEvent{{Name: "web_search", Kind: "read"}},
	}, nil
}

func parseMetricsFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metrics file: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal metrics: %v\n---\n%s", err, string(data))
	}
	return m
}

func asFloat(t *testing.T, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("expected float64, got %T", v)
	}
	return f
}

// --- run tests ---

// Metrics are OFF by default: the report is byte-identical to before (no
// "Metrics:" line), so existing callers are unaffected.
func TestRunOrchestratedOnceNoMetricsByDefault(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, successRunner(), fakeVerifyPassed, execOutputText)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if strings.Contains(out, "Metrics:") {
		t.Errorf("metrics disabled must not emit a Metrics line\n---\n%s", out)
	}
}

func TestRunOrchestratedOnceMetricsText(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, successRunner(), fakeVerifyPassed, execOutputText)
	od.metrics = &orchestratedRunMetrics{}
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{
		"Metrics:",
		"run wall:",
		"planning:",
		"routing (total):",
		"provider calls: 0",
		"serialized",
		"tasks:",
		"tokens: unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics text missing %q\n---\n%s", want, out)
		}
	}
	// A single once task has no concurrent batch, so the speedup line must
	// report n/a rather than a numeric value.
	if !strings.Contains(out, "effective speedup: n/a") {
		t.Errorf("single once task should report no speedup (n/a)\n---\n%s", out)
	}
	if strings.Contains(out, "NaN") {
		t.Errorf("metrics must never render NaN\n---\n%s", out)
	}
}

func TestRunOrchestratedOnceMetricsJSON(t *testing.T) {
	tmp := t.TempDir()
	od := newOrchestratedTestDeps(t, tmp, successRunner(), fakeVerifyPassed, execOutputJSON)
	od.metrics = &orchestratedRunMetrics{}
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, "\"metrics\"") {
		t.Errorf("JSON output missing \"metrics\" key\n---\n%s", out)
	}
	if !strings.Contains(out, "\"toolUsage\"") {
		t.Errorf("JSON output missing per-task toolUsage\n---\n%s", out)
	}
}

// Two read-only tasks with workers=2 and a barrier overlap, so the measured
// peak concurrency is exactly 2 and the run is reported as parallel.
func TestRunOrchestratedParallelMetricsConcurrency(t *testing.T) {
	tmp := t.TempDir()
	metricPath := filepath.Join(tmp, "metrics.json")
	var barrier sync.WaitGroup
	barrier.Add(2)
	od := newOrchestratedTestDeps(t, tmp, barrierRunner{barrier: &barrier}, fakeVerifyPassed, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: true, MaxWorkers: 2}
	od.metrics = &orchestratedRunMetrics{}
	od.metricsJSONPath = metricPath
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		readonlyTask("t2", "search the docs"),
	})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}

	m := parseMetricsFile(t, metricPath)
	if asFloat(t, m["peak_workers"]) != 2 {
		t.Errorf("peak_workers = %v, want 2", m["peak_workers"])
	}
	if m["concurrency"] != "parallel" {
		t.Errorf("concurrency = %v, want \"parallel\"", m["concurrency"])
	}
	if tasks, ok := m["tasks"].([]any); !ok || len(tasks) != 2 {
		t.Errorf("tasks = %v, want 2 entries", m["tasks"])
	}
	if batches, ok := m["batches"].([]any); !ok || len(batches) != 1 {
		t.Fatalf("batches = %v, want 1 entry", m["batches"])
	} else {
		b0 := batches[0].(map[string]any)
		if asFloat(t, b0["peak_workers"]) != 2 {
			t.Errorf("batch peak_workers = %v, want 2", b0["peak_workers"])
		}
	}
	// The fake runner ignores the provider, so no completion calls are counted.
	if asFloat(t, m["provider_calls"]) != 0 {
		t.Errorf("provider_calls = %v, want 0 (fake runner)", m["provider_calls"])
	}
	// Two tasks overlapped under 2 workers, so the run is genuinely
	// concurrent: speedup is present and strictly greater than 1.
	sp, ok := m["effective_speedup"].(float64)
	if !ok || sp <= 1.0 {
		t.Errorf("effective_speedup = %v (ok=%v), want > 1 for concurrent batch", sp, ok)
	}
	eff, ok := m["worker_efficiency"].(float64)
	if !ok || eff <= 0 {
		t.Errorf("worker_efficiency = %v (ok=%v), want > 0", eff, ok)
	}
}

// With workers=1 the same two tasks run serially, so the measured peak is 1
// and the run is reported as serialized (no speedup claim).
func TestRunOrchestratedParallelMetricsSerialized(t *testing.T) {
	tmp := t.TempDir()
	metricPath := filepath.Join(tmp, "metrics.json")
	od := newOrchestratedTestDeps(t, tmp, readOnlyRunner{}, fakeVerifyPassed, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: true, MaxWorkers: 1}
	od.metrics = &orchestratedRunMetrics{}
	od.metricsJSONPath = metricPath
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		readonlyTask("t2", "search the docs"),
	})
	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	m := parseMetricsFile(t, metricPath)
	if asFloat(t, m["peak_workers"]) != 1 {
		t.Errorf("peak_workers = %v, want 1 (serialized)", m["peak_workers"])
	}
	if m["concurrency"] != "serialized" {
		t.Errorf("concurrency = %v, want \"serialized\"", m["concurrency"])
	}
}

// --metrics-json writes a parseable file even when only the file (not the
// inline flag) is supplied.
func TestRunOrchestratedMetricsJSONFileWritten(t *testing.T) {
	tmp := t.TempDir()
	metricPath := filepath.Join(tmp, "out.json")
	od := newOrchestratedTestDeps(t, tmp, successRunner(), fakeVerifyPassed, execOutputText)
	od.metrics = &orchestratedRunMetrics{}
	od.metricsJSONPath = metricPath
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d", code, exitSuccess)
	}
	if _, err := os.Stat(metricPath); err != nil {
		t.Fatalf("metrics json file not written: %v", err)
	}
	m := parseMetricsFile(t, metricPath)
	if _, ok := m["run_wall_ms"]; !ok {
		t.Errorf("metrics file missing run_wall_ms\n---\n%v", m)
	}
	if _, ok := m["tasks"]; !ok {
		t.Errorf("metrics file missing tasks array\n---\n%v", m)
	}
}
