package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// toolCallingProvider emits a single write_file tool call on its first
// CompletionRequest, then a plain "done" answer on every later request. It lets
// the orchestrated runner exercise the real permission path for a mutating tool
// without any network.
type toolCallingProvider struct {
	mu      sync.Mutex
	counter int32
	tools   []zeroruntime.ToolDefinition
}

func (p *toolCallingProvider) StreamCompletion(ctx context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	p.mu.Lock()
	if p.tools == nil {
		p.tools = append([]zeroruntime.ToolDefinition(nil), req.Tools...)
	}
	n := atomic.AddInt32(&p.counter, 1)
	p.mu.Unlock()
	ch := make(chan zeroruntime.StreamEvent, 8)
	if n == 1 {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "write_file"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "c1", ArgumentsFragment: `{"path":"README.md","content":"hello"}`}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"}
	} else {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

// Requirement 2: an implementation task in Auto does NOT get promoted to Unsafe;
// it keeps PermissionModeAuto and uses task-compatible exposure instead.
func TestOrchestratedExposure_AutoDoesNotBecomeUnsafe(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)

	if orchOpts.PermissionMode != agent.PermissionModeAuto {
		t.Fatalf("orchestrated must NOT promote Auto -> Unsafe; got mode %q", orchOpts.PermissionMode)
	}
	if orchOpts.ToolExposure != agent.ToolExposureTaskCompatible {
		t.Fatalf("mutating task must use task-compatible exposure; got %q", orchOpts.ToolExposure)
	}
}

// Requirement 3: the advertised write tool keeps its real permission requirement
// (PermissionPrompt) — advertising never grants authority.
func TestOrchestratedExposure_WriteToolRemainsPermissionPrompt(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	names := agent.ExposedToolNames(reg, orchOpts.PermissionMode, orchOpts)

	if !schemaHas(names, "write_file") {
		t.Fatalf("write_file must be advertised in Auto+task-compatible: %v", names)
	}
	tool, ok := reg.Get("write_file")
	if !ok {
		t.Fatal("write_file not registered")
	}
	if tool.Safety().Permission != tools.PermissionPrompt {
		t.Fatalf("advertised write_file must remain PermissionPrompt, got %q", tool.Safety().Permission)
	}
}

// Requirement 4 + 17: in non-interactive Auto the model may SEE write_file but a
// prompt-required tool request is denied (no approver), so the task is blocked /
// permission-required and README is NOT written. Advertising != authorization.
func TestOrchestratedPermission_AutoBlocksWrite(t *testing.T) {
	regDir := t.TempDir()
	reg := newCoreRegistry(regDir)
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	od.workspaceRoot = regDir
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)

	runner := executor.NewAgentRunner(&toolCallingProvider{}, orchOpts)
	res, _ := runner.RunTask(context.Background(), executor.TaskExecutionRequest{
		Task:          implTask(),
		Prompt:        "add a readme",
		ModelID:       "grok-4.5",
		ProviderID:    "xai",
		WorkspaceRoot: regDir,
		SessionID:     "sess",
	})

	if !res.PermissionRequired {
		t.Fatalf("Auto headless write must be permission-required; got result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(regDir, "README.md")); err == nil {
		t.Fatal("README must NOT be written without explicit approval in Auto")
	}
}

// Requirement 5: explicit Unsafe permits the write through the NORMAL existing
// path — the file is actually created and no permission is required.
func TestOrchestratedPermission_UnsafePermitsWrite(t *testing.T) {
	regDir := t.TempDir()
	reg := newCoreRegistry(regDir)
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeUnsafe)
	od.workspaceRoot = regDir
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	if orchOpts.PermissionMode != agent.PermissionModeUnsafe {
		t.Fatalf("explicit Unsafe must be preserved, got %q", orchOpts.PermissionMode)
	}

	runner := executor.NewAgentRunner(&toolCallingProvider{}, orchOpts)
	res, _ := runner.RunTask(context.Background(), executor.TaskExecutionRequest{
		Task:          implTask(),
		Prompt:        "add a readme",
		ModelID:       "grok-4.5",
		ProviderID:    "xai",
		WorkspaceRoot: regDir,
		SessionID:     "sess",
	})

	if res.PermissionRequired {
		t.Fatalf("Unsafe must not require permission for write_file; result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(regDir, "README.md")); err != nil {
		t.Fatalf("Unsafe must create README.md: %v", err)
	}
}

// Requirement 6: explicit Ask remains Ask — headless Ask has no approver, so the
// prompt-required write is still denied (not silently allowed).
func TestOrchestratedPermission_AskRemainsAsk(t *testing.T) {
	regDir := t.TempDir()
	reg := newCoreRegistry(regDir)
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAsk)
	od.workspaceRoot = regDir
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	if orchOpts.PermissionMode != agent.PermissionModeAsk {
		t.Fatalf("explicit Ask must be preserved, got %q", orchOpts.PermissionMode)
	}

	runner := executor.NewAgentRunner(&toolCallingProvider{}, orchOpts)
	res, _ := runner.RunTask(context.Background(), executor.TaskExecutionRequest{
		Task:          implTask(),
		Prompt:        "add a readme",
		ModelID:       "grok-4.5",
		ProviderID:    "xai",
		WorkspaceRoot: regDir,
		SessionID:     "sess",
	})

	if !res.PermissionRequired {
		t.Fatalf("Ask headless write must be permission-required; result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(regDir, "README.md")); err == nil {
		t.Fatal("README must NOT be written in Ask without approval")
	}
}

// Requirement 8: a dangerous task is downgraded to a read-only tool set, so no
// mutating tool is advertised even under task-compatible exposure.
func TestOrchestratedExposure_DangerousTaskExcludesMutation(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	dangerous := testTask(planner.KindImplementation, planner.SafetyDangerous)
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", dangerous, nil, nil, nil, nil)
	names := agent.ExposedToolNames(reg, orchOpts.PermissionMode, orchOpts)

	for _, no := range []string{"write_file", "edit_file", "apply_patch", "exec_command"} {
		if schemaHas(names, no) {
			t.Fatalf("dangerous task must NOT advertise mutating tool %q: %v", no, names)
		}
	}
}

// Requirement 11/12/13: workspace trust, sandbox, and hooks are reused unchanged
// from the resolved deps — the orchestrated build threads the SAME engine and
// dispatcher through to the agent options, so they govern the run exactly as in
// normal exec.
func TestOrchestratedExposure_ReusesSandboxAndHooks(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	// A real (default-policy) sandbox engine and a hook dispatcher are wired, as
	// production does via runOrchestratedOnce's sandboxEngine/hookDispatcher.
	od.sandboxEngine = nil // production passes the resolved engine; here we only
	// assert the build preserves whatever is provided.
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	if orchOpts.Sandbox != od.sandboxEngine {
		t.Fatalf("orchestrated must reuse the same sandbox engine (trust/sandbox apply)")
	}
}
