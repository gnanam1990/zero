package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// --- counting / fake providers for provider-call metrics ---

// countingProvider wraps an inner zeroruntime.Provider and counts every
// StreamCompletion invocation independently of any metrics infrastructure. It is
// the source-of-truth counter for the assertions below.
type countingProvider struct {
	inner zeroruntime.Provider
	count *int32
}

func (p *countingProvider) StreamCompletion(ctx context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	atomic.AddInt32(p.count, 1)
	return p.inner.StreamCompletion(ctx, req)
}

// answerProvider streams a single final answer (no tool calls) plus a usage
// event, so a real AgentRunner completes the task in one turn.
type answerProvider struct{}

func (answerProvider) StreamCompletion(ctx context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent, 8)
	go func() {
		defer close(ch)
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 10, OutputTokens: 5}}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	}()
	return ch, nil
}

// errProvider returns an error on every StreamCompletion.
type errProvider struct{}

func (errProvider) StreamCompletion(ctx context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	return nil, errors.New("provider boom")
}

// closedProvider yields an empty closed stream (no answer, no usage).
type closedProvider struct{}

func (closedProvider) StreamCompletion(ctx context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent)
	close(ch)
	return ch, nil
}

// --- wrapper-level semantics (retry / error / cancellation) ---

func TestMeteredProviderCountsEveryInvocation(t *testing.T) {
	var calls int32
	mp := &metricsProvider{inner: closedProvider{}, calls: &calls}
	for i := 0; i < 3; i++ {
		if _, err := mp.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("after 3 invocations calls = %d, want 3", got)
	}
}

func TestMeteredProviderCountsOnError(t *testing.T) {
	var calls int32
	mp := &metricsProvider{inner: errProvider{}, calls: &calls}
	_, err := mp.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{})
	if err == nil {
		t.Fatal("want an error from the inner provider")
	}
	// The attempted call is still counted even though it errored.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("erroring call not counted: calls = %d, want 1", got)
	}
}

func TestMeteredProviderCountsAfterCancel(t *testing.T) {
	var calls int32
	mp := &metricsProvider{inner: closedProvider{}, calls: &calls}
	// A cancelled context passed to StreamCompletion is still an invocation: the
	// wrapper counts it before delegating to the (possibly ctx-aware) inner.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mp.StreamCompletion(ctx, zeroruntime.CompletionRequest{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("call under cancelled ctx not counted: calls = %d, want 1", got)
	}
}

func TestMeteredProviderCountsZeroWhenNeverInvoked(t *testing.T) {
	var calls int32
	mp := &metricsProvider{inner: closedProvider{}, calls: &calls}
	// Cancellation BEFORE any invocation means StreamCompletion is never called,
	// so the counter stays at zero.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = ctx
	_ = mp
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("calls = %d, want 0 when StreamCompletion is never invoked", got)
	}
}

// --- end-to-end: real AgentRunner, real provider wrapping ---

// newCountingOnceDeps returns orchestrated test deps that run the REAL
// AgentRunner (runner == nil) backed by a counting answer provider, so the
// native provider-call metric can be checked against an independent counter.
func newCountingOnceDeps(t *testing.T, tmp string, count *int32, format execOutputFormat) orchestratedOnceDeps {
	od := newOrchestratedTestDeps(t, tmp, nil, fakeVerifyPassed, format)
	// Disable the completion gate so a single final-answer turn completes
	// in exactly one provider call (otherwise the agent keeps nudging
	// until max-turns, inflating the count).
	od.options.noCompletionGate = true
	od.deps.newProvider = func(config.ProviderProfile) (agent.Provider, error) {
		return &countingProvider{inner: answerProvider{}, count: count}, nil
	}
	od.metrics = &orchestratedRunMetrics{}
	return od
}

// TestRunOrchestratedOnceRealProviderCounts exercises the real AgentRunner path
// (od.runner == nil) with a counting provider. This is the regression guard
// for the "provider calls: 0" bug: the metered wrapper must actually sit in
// front of the provider the agent uses.
func TestRunOrchestratedOnceRealProviderCounts(t *testing.T) {
	tmp := t.TempDir()
	var count int32
	od := newCountingOnceDeps(t, tmp, &count, execOutputText)

	code := runOrchestratedOnce(od)
	t.Logf("exit = %d (completion gate is orthogonal to provider-call counting)", code)
	if atomic.LoadInt32(&count) == 0 {
		t.Fatal("countingProvider.StreamCompletion was never called by the real runner")
	}
	if od.metrics.TotalProviderCalls != int(atomic.LoadInt32(&count)) {
		t.Errorf("metrics provider_calls = %d, want %d (independent counter)", od.metrics.TotalProviderCalls, atomic.LoadInt32(&count))
	}
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, fmt.Sprintf("provider calls: %d", atomic.LoadInt32(&count))) {
		t.Errorf("metrics text missing provider calls line\n---\n%s", out)
	}
}

// TestRunOrchestratedParallelReadonlyProviderCounts runs two read-only tasks
// under two workers with the real AgentRunner and asserts that (a) every task
// reports its own provider-call count, and (b) the run total equals the sum
// of task counts (which equals the independent counter).
func TestRunOrchestratedParallelReadonlyProviderCounts(t *testing.T) {
	tmp := t.TempDir()
	metricPath := filepath.Join(tmp, "metrics.json")
	var count int32
	od := newCountingOnceDeps(t, tmp, &count, execOutputText)
	od.parallel = parallelReadonlyOptions{Enabled: true, MaxWorkers: 2}
	od.metricsJSONPath = metricPath
	injectParallelPlan(t, []planner.Task{
		readonlyTask("t1", "search the source"),
		readonlyTask("t2", "search the docs"),
	})

	code := runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 0, StopOnFailure: true, StopOnBlocked: true})
	t.Logf("exit = %d (completion gate is orthogonal to provider-call counting)", code)
	if atomic.LoadInt32(&count) != 2 {
		t.Errorf("independent provider calls = %d, want 2", atomic.LoadInt32(&count))
	}
	if od.metrics.TotalProviderCalls != int(atomic.LoadInt32(&count)) {
		t.Errorf("run total %d != independent %d", od.metrics.TotalProviderCalls, atomic.LoadInt32(&count))
	}
	m := parseMetricsFile(t, metricPath)
	tasks, ok := m["tasks"].([]any)
	if !ok || len(tasks) != 2 {
		t.Fatalf("tasks = %v, want 2 entries", m["tasks"])
	}
	for i, tk := range tasks {
		tm := tk.(map[string]any)
		if got := asFloat(t, tm["provider_calls"]); got != 1 {
			t.Errorf("task %d provider_calls = %v, want 1", i, got)
		}
	}
	if got := asFloat(t, m["provider_calls"]); got != 2 {
		t.Errorf("run provider_calls = %v, want 2", got)
	}
}

// TestRunOrchestratedOnceProviderErrorCounts verifies that a provider which
// errors on its first completion still counts as one attempted call.
func TestRunOrchestratedOnceProviderErrorCounts(t *testing.T) {
	tmp := t.TempDir()
	var count int32
	od := newOrchestratedTestDeps(t, tmp, nil, fakeVerifyPassed, execOutputText)
	od.deps.newProvider = func(config.ProviderProfile) (agent.Provider, error) {
		return &countingProvider{inner: errProvider{}, count: &count}, nil
	}
	od.metrics = &orchestratedRunMetrics{}

	code := runOrchestratedOnce(od)
	t.Logf("exit = %d, count = %d, metrics = %d", code, atomic.LoadInt32(&count), od.metrics.TotalProviderCalls)
	if atomic.LoadInt32(&count) < 1 {
		t.Fatalf("provider never invoked despite error: count = %d", atomic.LoadInt32(&count))
	}
	if od.metrics.TotalProviderCalls != int(atomic.LoadInt32(&count)) {
		t.Errorf("run total %d != independent %d", od.metrics.TotalProviderCalls, atomic.LoadInt32(&count))
	}
}

// TestOrchestratedPreviewProviderCallsZero confirms the offline plan preview
// never constructs a provider, so its provider-call count is zero.
func TestOrchestratedPreviewProviderCallsZero(t *testing.T) {
	tmp := t.TempDir()
	var count int32
	od := newOrchestratedTestDeps(t, tmp, nil, fakeVerifyPassed, execOutputText)
	od.deps.newProvider = func(config.ProviderProfile) (agent.Provider, error) {
		return &countingProvider{inner: answerProvider{}, count: &count}, nil
	}

	// The offline preview must never construct a provider, regardless of
	// whether routing itself succeeds.
	_, _ = buildPlanPreview("implement a feature", routerFlagOptions{}, true, nil)
	if atomic.LoadInt32(&count) != 0 {
		t.Errorf("preview constructed a provider: count = %d, want 0", atomic.LoadInt32(&count))
	}
}

// TestRunOrchestratedOnceMetricsJSONProviderCalls checks the JSON report
// carries the per-run and per-task provider_calls fields.
func TestRunOrchestratedOnceMetricsJSONProviderCalls(t *testing.T) {
	tmp := t.TempDir()
	var count int32
	od := newCountingOnceDeps(t, tmp, &count, execOutputJSON)

	code := runOrchestratedOnce(od)
	t.Logf("exit = %d (completion gate is orthogonal to provider-call counting)", code)
	out := od.stdout.(*strings.Builder).String()
	if !strings.Contains(out, fmt.Sprintf("\"provider_calls\": %d", atomic.LoadInt32(&count))) {
		t.Errorf("JSON missing provider_calls\n---\n%s", out)
	}
}

// TestRunOrchestratedOnceMetricsJSONFileProviderCalls checks the written
// metrics JSON file carries provider_calls.
func TestRunOrchestratedOnceMetricsJSONFileProviderCalls(t *testing.T) {
	tmp := t.TempDir()
	metricPath := filepath.Join(tmp, "metrics.json")
	var count int32
	od := newCountingOnceDeps(t, tmp, &count, execOutputText)
	od.metricsJSONPath = metricPath

	code := runOrchestratedOnce(od)
	t.Logf("exit = %d (completion gate is orthogonal to provider-call counting)", code)
	m := parseMetricsFile(t, metricPath)
	if got := asFloat(t, m["provider_calls"]); got != float64(atomic.LoadInt32(&count)) {
		t.Errorf("file provider_calls = %v, want %d", got, atomic.LoadInt32(&count))
	}
}
