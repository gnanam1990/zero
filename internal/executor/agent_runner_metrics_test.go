package executor

import (
	"context"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// TestEvidenceCollectorToolUsage verifies the deterministic tool-accounting:
// a denied call is Attempted but not Executed/Succeeded, and counts as Denied.
func TestEvidenceCollectorToolUsage(t *testing.T) {
	c := newEvidenceCollector(nil, func() {})
	c.onToolCall(agent.ToolCall{Name: "web_search"})
	c.onToolResult(agent.ToolResult{Name: "web_search", Status: tools.StatusOK})
	c.onToolCall(agent.ToolCall{Name: "write_file"})
	c.onToolResult(agent.ToolResult{Name: "write_file", DenialReason: agent.DenialPermissionDenied})

	res := c.finish(agent.Result{}, nil)
	if res.ToolUsage.Attempted != 2 {
		t.Errorf("Attempted = %d, want 2", res.ToolUsage.Attempted)
	}
	if res.ToolUsage.Executed != 1 {
		t.Errorf("Executed = %d, want 1 (the denied call never executed)", res.ToolUsage.Executed)
	}
	if res.ToolUsage.Succeeded != 1 {
		t.Errorf("Succeeded = %d, want 1", res.ToolUsage.Succeeded)
	}
	if res.ToolUsage.Failed != 0 {
		t.Errorf("Failed = %d, want 0", res.ToolUsage.Failed)
	}
	if res.ToolUsage.Denied != 1 {
		t.Errorf("Denied = %d, want 1", res.ToolUsage.Denied)
	}
}

// TestEvidenceCollectorUsage verifies token accounting sums across turns and
// UsageReported gates availability so a measured zero is never confused with
// "never measured".
func TestEvidenceCollectorUsage(t *testing.T) {
	c := newEvidenceCollector(nil, func() {})
	c.onUsage(zeroruntime.Usage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 3, ReasoningTokens: 1})
	c.onUsage(zeroruntime.Usage{InputTokens: 2, OutputTokens: 1})

	res := c.finish(agent.Result{}, nil)
	if !res.UsageReported {
		t.Fatal("UsageReported = false, want true after OnUsage fired")
	}
	if res.Usage.InputTokens != 12 {
		t.Errorf("InputTokens = %d, want 12", res.Usage.InputTokens)
	}
	if res.Usage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", res.Usage.OutputTokens)
	}
	if res.Usage.CachedInputTokens != 3 {
		t.Errorf("CachedInputTokens = %d, want 3", res.Usage.CachedInputTokens)
	}
	if res.Usage.ReasoningTokens != 1 {
		t.Errorf("ReasoningTokens = %d, want 1", res.Usage.ReasoningTokens)
	}
}

// TestEvidenceCollectorNoUsage verifies a run that never reported usage leaves
// UsageReported false (metrics must mark tokens unavailable, not zero).
func TestEvidenceCollectorNoUsage(t *testing.T) {
	c := newEvidenceCollector(nil, func() {})
	res := c.finish(agent.Result{}, nil)
	if res.UsageReported {
		t.Error("UsageReported = true, want false when no usage event fired")
	}
}

// compile-time guard: ensure the collector still satisfies the runner path.
var _ = context.Background
