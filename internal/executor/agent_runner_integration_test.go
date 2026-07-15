package executor

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// scriptedProvider emits a fixed sequence of turns; if there are more turns
// requested than defined, it repeats a single looping turn so unbounded runs
// can be detected/cancelled.
type scriptedProvider struct {
	mu    sync.Mutex
	turns [][]zeroruntime.StreamEvent
	count int
}

func (p *scriptedProvider) StreamCompletion(ctx context.Context, _ zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
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

func toolCallTurn(id, name, args string) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "working"},
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: id, ToolName: name},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: id, ArgumentsFragment: args},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: id},
		{Type: zeroruntime.StreamEventDone},
	}
}

// Requirement 10 (README creation): the orchestrated agent runner can create a
// file when the task is granted edit capability and unsafe permission mode.
func TestAgentRunner_CreatesReadme(t *testing.T) {
	workspace := t.TempDir()
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(workspace, nil) {
		registry.Register(tool)
	}
	runner := &AgentRunner{
		provider: &scriptedProvider{turns: [][]zeroruntime.StreamEvent{
			toolCallTurn("c1", "write_file", `{"path":"README.md","content":"# Sample\n"}`),
			{{Type: zeroruntime.StreamEventText, Content: "done"}, {Type: zeroruntime.StreamEventDone}},
		}},
		Live: io.Discard,
		options: agent.Options{
			Model:          "test-model",
			Cwd:            workspace,
			Registry:       registry,
			PermissionMode: agent.PermissionModeUnsafe,
		},
	}
	res, err := runner.RunTask(context.Background(), TaskExecutionRequest{
		Prompt:        "Add a README explaining this repository",
		WorkspaceRoot: workspace,
		ModelID:       "test-model",
		SessionID:     "sess-readme",
	})
	if err != nil {
		t.Fatalf("RunTask returned error: %v", err)
	}
	got := filepath.Join(workspace, "README.md")
	data, rerr := os.ReadFile(got)
	if rerr != nil {
		t.Fatalf("README.md not created: %v (events=%d)", rerr, len(res.ToolEvents))
	}
	if string(data) != "# Sample\n" {
		t.Fatalf("README.md content mismatch: %q", string(data))
	}
}

// Requirement 10 (bounded unavailable-tool loops): when the model repeatedly
// calls a tool the run filtered out, the runner cancels the loop after a small
// number of consecutive failures instead of looping forever.
func TestAgentRunner_BoundedUnavailableToolLoop(t *testing.T) {
	workspace := t.TempDir()
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(workspace, nil) {
		registry.Register(tool)
	}
	loopTurn := toolCallTurn("c1", "write_file", `{"path":"x","content":"y"}`)
	sp := &scriptedProvider{turns: [][]zeroruntime.StreamEvent{loopTurn}}
	runner := &AgentRunner{
		provider: sp,
		Live:     io.Discard,
		// Only read_file is allowed, so write_file is filtered out.
		options: agent.Options{
			Model:          "test-model",
			Cwd:            workspace,
			Registry:       registry,
			PermissionMode: agent.PermissionModeAuto,
			EnabledTools:   []string{"read_file"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*1e9)
	defer cancel()
	res, err := runner.RunTask(ctx, TaskExecutionRequest{
		Prompt:        "do something",
		WorkspaceRoot: workspace,
		ModelID:       "test-model",
		SessionID:     "sess-bounded",
	})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("RunTask returned unexpected error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("run did not terminate within timeout (unbounded loop)")
	}
	var writeCalls int
	for _, ev := range res.ToolEvents {
		if ev.Name == "write_file" {
			writeCalls++
		}
	}
	if writeCalls > maxConsecutiveUnavailableToolErrors+1 {
		t.Fatalf("expected at most %d unavailable write_file calls, got %d", maxConsecutiveUnavailableToolErrors+1, writeCalls)
	}
}
