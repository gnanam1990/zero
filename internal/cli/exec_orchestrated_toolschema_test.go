package cli

import (
	"context"
	"io"
	"sort"
	"sync"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// captureProvider records the tool schemas from the FIRST CompletionRequest it
// receives — i.e. the exact schemas the agent would send to a real provider. It
// then ends the turn immediately so the run terminates without calling tools.
type captureProvider struct {
	mu    sync.Mutex
	tools []zeroruntime.ToolDefinition
	calls int
}

func (p *captureProvider) StreamCompletion(ctx context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	if p.tools == nil {
		p.tools = append([]zeroruntime.ToolDefinition(nil), req.Tools...)
	}
	p.mu.Unlock()
	ch := make(chan zeroruntime.StreamEvent, 1)
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func captureToolNames(p *captureProvider) []string {
	names := make([]string, 0, len(p.tools))
	for _, t := range p.tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

// buildOrchestratedTestDeps builds a minimal orchestratedOnceDeps that exercises
// buildOrchestratedAgentOptions exactly as production does, reusing the SAME
// registry (never a second one).
func buildOrchestratedTestDeps(reg *tools.Registry, permissionMode agent.PermissionMode) orchestratedOnceDeps {
	mr, _ := modelregistry.DefaultRegistry()
	return orchestratedOnceDeps{
		options:       execOptions{enabledTools: nil, disabledTools: nil, autonomy: "medium"},
		stdout:        io.Discard,
		stderr:        io.Discard,
		registry:      reg,
		modelRegistry: mr,
		resolved: config.ResolvedConfig{
			MaxTurns: 10,
			Provider: config.ProviderProfile{Name: "xai", Model: "grok-4.5"},
		},
		effectiveDeferThreshold: 0,
		permissionMode:          permissionMode,
		sessionTitle:            "diag",
		prompt:                  "do the task",
		deps:                    appDeps{skillsDir: func() string { return "" }},
		pluginActivation:        pluginActivation{},
	}
}

func implTask() planner.Task {
	return testTask(planner.KindImplementation, planner.SafetySafe)
}

// Requirement 3/5/10: the orchestrated implementation request captures a schema
// that contains the real registered editing tools, and the same capture through
// the REAL agent.Run -> provider-request path.
func TestOrchestratedToolSchema_ImplementationHasEditingTool(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())

	// Normal exec options (writable mode) built the same way production does.
	normalOpts := agent.Options{
		Registry:       reg,
		PermissionMode: agent.PermissionModeUnsafe,
		Cwd:            t.TempDir(),
		DeferThreshold: 0,
	}
	normalCap := &captureProvider{}
	_, _ = agent.Run(context.Background(), "add a readme", normalCap, normalOpts)
	normalNames := captureToolNames(normalCap)

	// Orchestrated options via the real buildOrchestratedAgentOptions wiring.
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	orchCap := &captureProvider{}
	_, _ = agent.Run(context.Background(), "add a readme", orchCap, orchOpts)
	orchNames := captureToolNames(orchCap)

	for _, want := range []string{"write_file", "edit_file", "apply_patch", "exec_command"} {
		if !schemaHas(orchNames, want) {
			t.Fatalf("orchestrated provider schema missing real editing tool %q; got %v", want, orchNames)
		}
		if !schemaHas(normalNames, want) {
			t.Fatalf("NORMAL provider schema missing real editing tool %q; got %v", want, normalNames)
		}
	}
}

// Requirement 1/3/9: the orchestrated provider schema for an unrestricted
// implementation task is the SAME set as normal exec (same registry, no second
// registry), and the editing capability is present in both.
func TestOrchestratedToolSchema_EqualsNormalExec(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())

	normalOpts := agent.Options{
		Registry:       reg,
		PermissionMode: agent.PermissionModeUnsafe,
		Cwd:            t.TempDir(),
		DeferThreshold: 0,
	}
	normalNames := agent.ExposedToolNames(reg, agent.PermissionModeUnsafe, normalOpts)

	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	orchNames := agent.ExposedToolNames(reg, orchOpts.PermissionMode, orchOpts)

	if orchOpts.Registry != reg {
		t.Fatalf("orchestrated must reuse the SAME registry object (no second registry)")
	}
	if !sameStringSet(normalNames, orchNames) {
		t.Fatalf("orchestrated schema must equal normal exec schema\n normal: %v\n orch:   %v", normalNames, orchNames)
	}
	for _, want := range []string{"write_file", "edit_file", "exec_command"} {
		if !schemaHas(orchNames, want) {
			t.Fatalf("orchestrated schema missing %q: %v", want, orchNames)
		}
	}
}

// Requirement 8: a read-only task excludes write tools from the provider schema.
func TestOrchestratedToolSchema_ReadOnlyExcludesWrites(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	readTask := testTask(planner.KindCodeReview, planner.SafetySafe)
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", readTask, nil, nil, nil, nil)
	names := agent.ExposedToolNames(reg, orchOpts.PermissionMode, orchOpts)
	for _, no := range []string{"write_file", "edit_file", "apply_patch"} {
		if schemaHas(names, no) {
			t.Fatalf("read-only task must NOT advertise %q: %v", no, names)
		}
	}
	if !schemaHas(names, "read_file") {
		t.Fatalf("read-only task must advertise read_file: %v", names)
	}
}

// Requirement 6: explicit --enabled-tools restricts the outgoing provider schemas.
func TestOrchestratedToolSchema_EnabledToolsRestricts(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	od.options.enabledTools = []string{"read_file", "grep"}
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	names := agent.ExposedToolNames(reg, orchOpts.PermissionMode, orchOpts)
	if !schemaHas(names, "read_file") {
		t.Fatalf("enabled-tools must keep read_file: %v", names)
	}
	if schemaHas(names, "write_file") || schemaHas(names, "exec_command") {
		t.Fatalf("--enabled-tools must exclude write/shell: %v", names)
	}
}

// Requirement 7: --disabled-tools removes the tool from outgoing schemas.
func TestOrchestratedToolSchema_DisabledTools(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	od.options.disabledTools = []string{"write_file"}
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	names := agent.ExposedToolNames(reg, orchOpts.PermissionMode, orchOpts)
	if schemaHas(names, "write_file") {
		t.Fatalf("--disabled-tools must remove write_file: %v", names)
	}
	if !schemaHas(names, "read_file") {
		t.Fatalf("read_file should remain: %v", names)
	}
}

// Requirement 11: nil EnabledTools and empty EnabledTools are handled identically
// by the agent layer (both mean "no restriction" => every tool), whereas a
// NON-empty allowlist restricts the provider schema. The orchestrated path never
// passes an empty slice where it means "none" — it passes the explicit full list
// (or a real subset), so there is no ambiguity.
func TestOrchestratedToolSchema_NilVsEmptyEnabled(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	nilOpts := agent.Options{Registry: reg, PermissionMode: agent.PermissionModeUnsafe, Cwd: t.TempDir(), EnabledTools: nil}
	emptyOpts := agent.Options{Registry: reg, PermissionMode: agent.PermissionModeUnsafe, Cwd: t.TempDir(), EnabledTools: []string{}}
	nilNames := agent.ExposedToolNames(reg, agent.PermissionModeUnsafe, nilOpts)
	emptyNames := agent.ExposedToolNames(reg, agent.PermissionModeUnsafe, emptyOpts)
	if !schemaHas(nilNames, "write_file") {
		t.Fatalf("nil EnabledTools must expose every tool: %v", nilNames)
	}
	if !sameStringSet(nilNames, emptyNames) {
		t.Fatalf("nil and empty EnabledTools must be equivalent (both unrestricted):\n nil:   %v\n empty: %v", nilNames, emptyNames)
	}
	// A non-empty allowlist DOES restrict.
	restricted := agent.Options{Registry: reg, PermissionMode: agent.PermissionModeUnsafe, Cwd: t.TempDir(), EnabledTools: []string{"read_file"}}
	restrictedNames := agent.ExposedToolNames(reg, agent.PermissionModeUnsafe, restricted)
	if schemaHas(restrictedNames, "write_file") || schemaHas(restrictedNames, "exec_command") {
		t.Fatalf("non-empty EnabledTools must restrict: %v", restrictedNames)
	}
	if !schemaHas(restrictedNames, "read_file") {
		t.Fatalf("restricted allowlist must keep read_file: %v", restrictedNames)
	}
}

// Requirement 5: only tools actually registered are advertised — no fake
// "write_file" name is invented. When the registry has no write_file, none is
// advertised.
func TestOrchestratedToolSchema_NoFakeWriteFile(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&stubTool{name: "alpha"})
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	names := agent.ExposedToolNames(reg, orchOpts.PermissionMode, orchOpts)
	if schemaHas(names, "write_file") {
		t.Fatalf("must not advertise write_file when it is not registered: %v", names)
	}
	if !schemaHas(names, "alpha") {
		t.Fatalf("registered tool alpha must be advertised: %v", names)
	}
}

// Requirement 12: a deferred (MCP-style) editing tool registered in the SAME
// registry is discoverable by an implementation task (present in the allowlist
// and exposed when deferral is inactive), so it can be invoked.
func TestOrchestratedToolSchema_DeferredDiscoverable(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	// A deferred tool (implements tools.Deferred) named like an MCP editing tool.
	reg.Register(&deferredEditTool{name: "mcp_edit"})
	od := buildOrchestratedTestDeps(reg, agent.PermissionModeAuto)
	allow := orchestratedToolAllowlist(implTask(), reg, nil, nil)
	if !schemaHas(allow, "mcp_edit") {
		t.Fatalf("implementation task allowlist must include registered deferred editing tool: %v", allow)
	}
	orchOpts := buildOrchestratedAgentOptions(od, "sess", "grok-4.5", implTask(), nil, nil, nil, nil)
	// DeferThreshold 0 => deferral inactive => deferred tool is eager-exposed.
	names := agent.ExposedToolNames(reg, orchOpts.PermissionMode, orchOpts)
	if !schemaHas(names, "mcp_edit") {
		t.Fatalf("deferred editing tool must be discoverable in orchestrated schema: %v", names)
	}
}

func schemaHas(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// deferredEditTool is a deferred-eligible (MCP-style) editing tool used to verify
// deferred-tool discovery in the orchestrated path.
type deferredEditTool struct{ name string }

func (d *deferredEditTool) Name() string        { return d.name }
func (d *deferredEditTool) Description() string { return "deferred edit" }
func (d *deferredEditTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", Properties: map[string]tools.PropertySchema{}}
}
func (d *deferredEditTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectWrite}
}
func (d *deferredEditTool) Run(ctx context.Context, input map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}
func (d *deferredEditTool) Deferred() bool { return true }

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}
