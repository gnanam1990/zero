package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// orchestratedNow is the clock used for every orchestrated-metrics timing sample.
// Production uses time.Now; tests may override it to make timings deterministic.
var orchestratedNow = time.Now

// --- token / tool metric value types (stable JSON tags) ---

// orchestratedTokenMetrics is the token accounting for one task or the run total.
// Available distinguishes a measured zero from "never reported" so a run that
// simply used no tokens is never confused with one that could not measure them.
type orchestratedTokenMetrics struct {
	Available         bool `json:"available"`
	InputTokens       int  `json:"input_tokens"`
	OutputTokens      int  `json:"output_tokens"`
	CachedInputTokens int  `json:"cached_input_tokens"`
	CacheWriteTokens  int  `json:"cache_write_tokens"`
	ReasoningTokens   int  `json:"reasoning_tokens"`
	TotalTokens       int  `json:"total_tokens"`
}

func newOrchestratedTokenMetrics(u zeroruntime.Usage, available bool) orchestratedTokenMetrics {
	return orchestratedTokenMetrics{
		Available:         available,
		InputTokens:       u.EffectiveInputTokens(),
		OutputTokens:      u.EffectiveOutputTokens(),
		CachedInputTokens: u.CachedInputTokens,
		CacheWriteTokens:  u.CacheWriteTokens,
		ReasoningTokens:   u.ReasoningTokens,
		TotalTokens:       u.TotalTokens(),
	}
}

// orchestratedToolMetrics mirrors executor.ToolUsageMetrics for stable JSON.
type orchestratedToolMetrics struct {
	Attempted int `json:"attempted"`
	Executed  int `json:"executed"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Denied    int `json:"denied"`
}

// --- per-task / per-batch / run accumulators ---

// orchestratedTaskMetric is the per-task timing/usage record. Batch is 0 for a
// sequential or --orchestrated-once task, and the 1-based parallel-batch index
// otherwise.
type orchestratedTaskMetric struct {
	TaskID         string                   `json:"task_id"`
	Title          string                   `json:"title"`
	Batch          int                      `json:"batch"`
	ProviderKind   string                   `json:"provider_kind"`
	Model          string                   `json:"model"`
	ProviderCalls  int                      `json:"provider_calls"`
	Status         string                   `json:"status"`
	WallMs         int64                    `json:"wall_ms"`
	QueueWaitMs    int64                    `json:"queue_wait_ms"`
	VerificationMs int64                    `json:"verification_ms"`
	Verified       bool                     `json:"verified"`
	Tokens         orchestratedTokenMetrics `json:"tokens"`
	Tools          orchestratedToolMetrics  `json:"tools"`
}

// orchestratedBatchMetric is the per-parallel-batch record.
type orchestratedBatchMetric struct {
	Batch       int   `json:"batch"`
	Workers     int   `json:"workers"`
	TaskCount   int   `json:"task_count"`
	WallMs      int64 `json:"wall_ms"`
	PeakWorkers int   `json:"peak_workers"`
}

// orchestratedRunMetrics accumulates the whole-run metrics. activeWorkers is an
// atomic counter shared across the sequential path and parallel worker goroutines;
// peakWorkers is its observed maximum (updated via CAS). Slice appends (Tasks,
// Batches) happen only on the coordinator goroutine, so they need no locking.
type orchestratedRunMetrics struct {
	RunStartedAt  time.Time `json:"-"`
	RunFinishedAt time.Time `json:"-"`

	PlanningMs int64 `json:"planning_ms"`
	RoutingMs  int64 `json:"routing_ms"`

	Tasks   []orchestratedTaskMetric  `json:"tasks"`
	Batches []orchestratedBatchMetric `json:"batches"`

	activeWorkers int32
	PeakWorkers   int32 `json:"peak_workers"`

	TotalProviderCalls int `json:"provider_calls"`
	TotalInputTokens   int `json:"total_input_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`

	// Survey metrics (optional, backward-compatible).
	SurveyBuildMs   int64 `json:"survey_build_ms,omitempty"`
	SurveyCacheHits int   `json:"survey_cache_hits,omitempty"`
}

func (m *orchestratedRunMetrics) enterWorker() {
	n := atomic.AddInt32(&m.activeWorkers, 1)
	for {
		peak := atomic.LoadInt32(&m.PeakWorkers)
		if n <= peak {
			return
		}
		if atomic.CompareAndSwapInt32(&m.PeakWorkers, peak, n) {
			return
		}
	}
}

func (m *orchestratedRunMetrics) leaveWorker() {
	atomic.AddInt32(&m.activeWorkers, -1)
}

// concurrency reports whether any concurrency was observed (peak > 1 worker).
func (m *orchestratedRunMetrics) concurrency() string {
	if atomic.LoadInt32(&m.PeakWorkers) > 1 {
		return "parallel"
	}
	return "serialized"
}

// effectiveSpeedup returns the summed wall-time of tasks that ran inside a
// parallel batch divided by the summed wall-time of those batches. A run with no
// concurrent batch has nothing to compare against, so ok is false and no speedup
// claim is made.
func (m *orchestratedRunMetrics) effectiveSpeedup() (float64, bool) {
	var summed, wall int64
	for _, b := range m.Batches {
		wall += b.WallMs
	}
	for _, t := range m.Tasks {
		if t.Batch > 0 {
			summed += t.WallMs
		}
	}
	if wall == 0 {
		return 0, false
	}
	return float64(summed) / float64(wall), true
}

// --- provider-call counter ---

// metricsProvider wraps an agent.Provider purely to count how many completion
// calls (turns) it serves. It delegates the stream verbatim and never spawns a
// goroutine, so there is no leak and context cancellation flows straight through.
// Token usage is captured separately at the AgentRunner OnUsage callback layer.
type metricsProvider struct {
	inner agent.Provider
	calls *int32
}

func (p *metricsProvider) StreamCompletion(ctx context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	atomic.AddInt32(p.calls, 1)
	return p.inner.StreamCompletion(ctx, req)
}

// --- formatting helpers ---

func formatMillis(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000.0)
}

func nonNegMs(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// --- rendering ---

func orchestratedMetricsSummaryLine(m *orchestratedRunMetrics) string {
	runMs := int64(0)
	if !m.RunStartedAt.IsZero() && !m.RunFinishedAt.IsZero() {
		runMs = m.RunFinishedAt.Sub(m.RunStartedAt).Milliseconds()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Metrics: run %s", formatMillis(runMs))
	fmt.Fprintf(&b, ", %d task(s)", len(m.Tasks))
	if atomic.LoadInt32(&m.PeakWorkers) > 1 {
		fmt.Fprintf(&b, ", peak %d workers (parallel)", m.PeakWorkers)
	} else {
		b.WriteString(", serialized")
	}
	fmt.Fprintf(&b, ", %d provider call(s)", m.TotalProviderCalls)
	fmt.Fprintf(&b, ", %d input / %d output token(s)", m.TotalInputTokens, m.TotalOutputTokens)
	if sp, ok := m.effectiveSpeedup(); ok {
		fmt.Fprintf(&b, ", speedup %.2fx", sp)
		fmt.Fprintf(&b, ", efficiency %.2f", sp/float64(atomic.LoadInt32(&m.PeakWorkers)))
	}
	return b.String()
}

func orchestratedMetricsDetailed(m *orchestratedRunMetrics) string {
	var b strings.Builder
	b.WriteString(orchestratedMetricsSummaryLine(m))
	b.WriteString("\n")

	runMs := int64(0)
	if !m.RunStartedAt.IsZero() && !m.RunFinishedAt.IsZero() {
		runMs = m.RunFinishedAt.Sub(m.RunStartedAt).Milliseconds()
	}
	fmt.Fprintf(&b, "  run wall: %s\n", formatMillis(runMs))
	fmt.Fprintf(&b, "  planning: %s\n", formatMillis(m.PlanningMs))
	fmt.Fprintf(&b, "  routing (total): %s\n", formatMillis(m.RoutingMs))
	fmt.Fprintf(&b, "  concurrency: %s (peak %d worker(s))\n", m.concurrency(), atomic.LoadInt32(&m.PeakWorkers))
	fmt.Fprintf(&b, "  provider calls: %d\n", m.TotalProviderCalls)
	fmt.Fprintf(&b, "  tokens: %d input / %d output (%d cached input, %d total)\n",
		m.TotalInputTokens, m.TotalOutputTokens, 0, m.TotalInputTokens+m.TotalOutputTokens)
	if sp, ok := m.effectiveSpeedup(); ok {
		fmt.Fprintf(&b, "  effective speedup: %.2fx\n", sp)
		fmt.Fprintf(&b, "  worker efficiency: %.2f\n", sp/float64(atomic.LoadInt32(&m.PeakWorkers)))
	} else {
		b.WriteString("  effective speedup: n/a (no concurrent batch)\n")
	}

	if m.SurveyBuildMs > 0 || m.SurveyCacheHits > 0 {
		fmt.Fprintf(&b, "  survey: built in %s", formatMillis(m.SurveyBuildMs))
		if m.SurveyCacheHits > 0 {
			fmt.Fprintf(&b, " (cache hits: %d)", m.SurveyCacheHits)
		}
		b.WriteString("\n")
	}

	if len(m.Batches) > 0 {
		b.WriteString("  parallel batches:\n")
		for _, batch := range m.Batches {
			fmt.Fprintf(&b, "    batch %d: %d task(s), %d worker(s), wall %s, peak %d\n",
				batch.Batch, batch.TaskCount, batch.Workers, formatMillis(batch.WallMs), batch.PeakWorkers)
		}
	}

	b.WriteString("  tasks:\n")
	for i, t := range m.Tasks {
		fmt.Fprintf(&b, "    [%d] %s (%s): wall %s, queue %s, verify %s, %d provider call(s)\n",
			i+1, t.TaskID, t.Title, formatMillis(t.WallMs), formatMillis(t.QueueWaitMs), formatMillis(t.VerificationMs), t.ProviderCalls)
		tok := "unavailable"
		if t.Tokens.Available {
			tok = fmt.Sprintf("%d in / %d out", t.Tokens.InputTokens, t.Tokens.OutputTokens)
		}
		fmt.Fprintf(&b, "        tokens: %s; tools: %d attempted / %d executed / %d ok / %d failed / %d denied\n",
			tok, t.Tools.Attempted, t.Tools.Executed, t.Tools.Succeeded, t.Tools.Failed, t.Tools.Denied)
	}
	return b.String()
}

// orchestratedMetricsJSON builds the stable machine-readable metrics object. Numeric
// durations are in milliseconds; human-readable strings are omitted to keep the
// JSON deterministic. Arrays are ordered by task index and batch number.
func orchestratedMetricsJSON(m *orchestratedRunMetrics) map[string]any {
	runMs := int64(0)
	if !m.RunStartedAt.IsZero() && !m.RunFinishedAt.IsZero() {
		runMs = m.RunFinishedAt.Sub(m.RunStartedAt).Milliseconds()
	}
	out := map[string]any{
		"run_wall_ms":         runMs,
		"planning_ms":         m.PlanningMs,
		"routing_ms":          m.RoutingMs,
		"concurrency":         m.concurrency(),
		"peak_workers":        int(atomic.LoadInt32(&m.PeakWorkers)),
		"provider_calls":      m.TotalProviderCalls,
		"total_input_tokens":  m.TotalInputTokens,
		"total_output_tokens": m.TotalOutputTokens,
		"tasks":               make([]map[string]any, 0, len(m.Tasks)),
		"batches":             make([]map[string]any, 0, len(m.Batches)),
	}
	for _, t := range m.Tasks {
		out["tasks"] = append(out["tasks"].([]map[string]any), map[string]any{
			"task_id":         t.TaskID,
			"title":           t.Title,
			"batch":           t.Batch,
			"provider_kind":   t.ProviderKind,
			"model":           t.Model,
			"provider_calls":  t.ProviderCalls,
			"status":          t.Status,
			"wall_ms":         t.WallMs,
			"queue_wait_ms":   t.QueueWaitMs,
			"verification_ms": t.VerificationMs,
			"verified":        t.Verified,
			"tokens":          t.Tokens,
			"tools":           t.Tools,
		})
	}
	for _, batch := range m.Batches {
		out["batches"] = append(out["batches"].([]map[string]any), map[string]any{
			"batch":        batch.Batch,
			"workers":      batch.Workers,
			"task_count":   batch.TaskCount,
			"wall_ms":      batch.WallMs,
			"peak_workers": batch.PeakWorkers,
		})
	}
	if sp, ok := m.effectiveSpeedup(); ok {
		out["effective_speedup"] = sp
		out["worker_efficiency"] = sp / float64(atomic.LoadInt32(&m.PeakWorkers))
	} else {
		out["effective_speedup"] = nil
		out["worker_efficiency"] = nil
	}
	if m.SurveyBuildMs > 0 || m.SurveyCacheHits > 0 {
		out["survey_build_ms"] = m.SurveyBuildMs
		out["survey_cache_hits"] = m.SurveyCacheHits
	}
	return out
}

// finalizeOrchestratedMetrics stamps the run finish time, emits a final metrics
// session event, and — when configured — writes the metrics object to a JSON file.
// It is a no-op when metrics were not enabled for the run.
func finalizeOrchestratedMetrics(od orchestratedOnceDeps, sessionID string, store *sessions.Store) {
	if od.metrics == nil {
		return
	}
	if od.metrics.RunFinishedAt.IsZero() {
		od.metrics.RunFinishedAt = orchestratedNow()
	}
	if store != nil && sessionID != "" {
		store.AppendEvent(sessionID, sessions.AppendEventInput{
			Type:    EventOrchestratedMetricsRunCompleted,
			Payload: orchestratedMetricsJSON(od.metrics),
		})
	}
	if od.metricsJSONPath != "" {
		data, err := json.MarshalIndent(orchestratedMetricsJSON(od.metrics), "", "  ")
		if err == nil {
			_ = os.WriteFile(od.metricsJSONPath, data, 0o644)
		}
	}
}
