package cli

import (
	"context"
	"sort"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/tools"
)

func testTask(kind planner.TaskKind, safety planner.SafetyLevel) planner.Task {
	return planner.Task{
		ID:          "t1",
		Title:       "task",
		Description: "desc",
		TaskKind:    kind,
		SafetyLevel: safety,
	}
}

func allowed(allowlist []string, name string) bool {
	if allowlist == nil {
		return true
	}
	for _, n := range allowlist {
		if n == name {
			return true
		}
	}
	return false
}

func allowlistContains(allowlist []string, name string) bool {
	for _, n := range allowlist {
		if n == name {
			return true
		}
	}
	return false
}

// Requirement 1: implementation task propagates file-edit capability.
func TestOrchestratedToolAllowlist_ImplementationGetsEdit(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	al := orchestratedToolAllowlist(testTask(planner.KindImplementation, planner.SafetySafe), reg, nil, nil)
	if !allowed(al, "write_file") {
		t.Fatalf("implementation task must expose write_file, allowlist=%v", al)
	}
	if !allowed(al, "edit_file") {
		t.Fatalf("implementation task must expose edit_file, allowlist=%v", al)
	}
	if !allowed(al, "exec_command") {
		t.Fatalf("implementation task must expose exec_command, allowlist=%v", al)
	}
	if !allowed(al, "read_file") {
		t.Fatalf("implementation task must expose read_file, allowlist=%v", al)
	}
}

// Requirement 2: planning task is read-only.
func TestOrchestratedToolAllowlist_PlanningReadOnly(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	al := orchestratedToolAllowlist(testTask(planner.KindCodeReview, planner.SafetySafe), reg, nil, nil)
	if allowed(al, "write_file") {
		t.Fatalf("planning task must NOT expose write_file, allowlist=%v", al)
	}
	if allowed(al, "edit_file") {
		t.Fatalf("planning task must NOT expose edit_file, allowlist=%v", al)
	}
	if allowed(al, "apply_patch") {
		t.Fatalf("planning task must NOT expose apply_patch, allowlist=%v", al)
	}
	if !allowed(al, "read_file") {
		t.Fatalf("planning task must expose read_file, allowlist=%v", al)
	}
	if !allowed(al, "grep") {
		t.Fatalf("planning task must expose grep, allowlist=%v", al)
	}
}

// Requirement 3: --disabled-tools removes a default capability.
func TestOrchestratedToolAllowlist_DisabledTools(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	al := orchestratedToolAllowlist(testTask(planner.KindImplementation, planner.SafetySafe), reg, nil, []string{"write_file"})
	if allowed(al, "write_file") {
		t.Fatalf("--disabled-tools must remove write_file, allowlist=%v", al)
	}
	if !allowed(al, "read_file") {
		t.Fatalf("read_file should remain, allowlist=%v", al)
	}
}

// Requirement 4: --enabled-tools restricts to the given set.
func TestOrchestratedToolAllowlist_EnabledToolsRestricts(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	al := orchestratedToolAllowlist(testTask(planner.KindImplementation, planner.SafetySafe), reg, []string{"read_file", "grep"}, nil)
	if !allowed(al, "read_file") {
		t.Fatalf("read_file should be allowed, allowlist=%v", al)
	}
	if allowed(al, "write_file") {
		t.Fatalf("--enabled-tools must exclude write_file, allowlist=%v", al)
	}
	if allowed(al, "exec_command") {
		t.Fatalf("--enabled-tools must exclude exec_command, allowlist=%v", al)
	}
}

// Requirement 5 & 8: only tools present in the registry are ever advertised; a
// bogus override name is dropped.
func TestOrchestratedToolAllowlist_NoUnknownTools(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	al := orchestratedToolAllowlist(testTask(planner.KindImplementation, planner.SafetySafe), reg, []string{"read_file", "totally_fake_tool_xyz"}, nil)
	if allowlistContains(al, "totally_fake_tool_xyz") {
		t.Fatalf("must not advertise a tool absent from the registry, allowlist=%v", al)
	}
	if !allowlistContains(al, "read_file") {
		t.Fatalf("read_file should be advertised, allowlist=%v", al)
	}
}

// Requirement 6: dangerous task does not receive shell/write and must be approved.
func TestOrchestratedToolAllowlist_DangerousReadOnly(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	danger := testTask(planner.KindImplementation, planner.SafetyDangerous)
	al := orchestratedToolAllowlist(danger, reg, nil, nil)
	if allowed(al, "write_file") {
		t.Fatalf("dangerous task must not expose write_file, allowlist=%v", al)
	}
	if allowed(al, "exec_command") {
		t.Fatalf("dangerous task must not expose exec_command, allowlist=%v", al)
	}
	if !allowed(al, "read_file") {
		t.Fatalf("dangerous task may read, allowlist=%v", al)
	}
	if !orchestratedTaskRequiresApproval(danger, agent.PermissionModeAuto) {
		t.Fatalf("dangerous task must require approval in non-unsafe mode")
	}
	if orchestratedTaskRequiresApproval(danger, agent.PermissionModeUnsafe) {
		t.Fatalf("dangerous task must NOT require approval when unsafe mode is set")
	}
}

// Requirement 9: implementation task with no overrides yields an EXPLICIT full
// allowlist (every registered tool name), never nil — "nil means unrestricted"
// is ambiguous across layers, so we spell out the exact set, which is identical
// in membership to the full normal-exec tool set.
func TestOrchestratedToolAllowlist_ImplementationFullSet(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	al := orchestratedToolAllowlist(testTask(planner.KindImplementation, planner.SafetySafe), reg, nil, nil)
	if al == nil {
		t.Fatalf("implementation allowlist must be explicit (non-nil), got nil")
	}
	if len(al) != len(reg.All()) {
		t.Fatalf("explicit allowlist must contain every registered tool; got %d want %d: %v", len(al), len(reg.All()), al)
	}
	if !allowed(al, "write_file") || !allowed(al, "exec_command") {
		t.Fatalf("explicit full allowlist must include editing/shell tools: %v", al)
	}
}

// Requirement 11: shell_operation task gets shell + read but not edit.
func TestOrchestratedToolAllowlist_ShellOperation(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	al := orchestratedToolAllowlist(testTask(planner.KindShellOperation, planner.SafetySafe), reg, nil, nil)
	if !allowed(al, "exec_command") {
		t.Fatalf("shell_operation must expose exec_command, allowlist=%v", al)
	}
	if !allowed(al, "read_file") {
		t.Fatalf("shell_operation must expose read_file, allowlist=%v", al)
	}
	if allowed(al, "write_file") {
		t.Fatalf("shell_operation must NOT expose write_file, allowlist=%v", al)
	}
}

// Requirement 13: missing-capability guard fires for implementation without edit.
func TestOrchestratedMissingCapability_Implementation(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	readOnly := orchestratedToolAllowlist(testTask(planner.KindCodeReview, planner.SafetySafe), reg, nil, nil)
	if msg, missing := orchestratedMissingCapabilityError(testTask(planner.KindImplementation, planner.SafetySafe), readOnly, reg); !missing {
		t.Fatalf("implementation task on read-only allowlist must report missing capability, msg=%q", msg)
	}
}

// Requirement 13: shell_operation without shell is reported as missing capability.
func TestOrchestratedMissingCapability_ShellOperation(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	readOnly := orchestratedToolAllowlist(testTask(planner.KindCodeReview, planner.SafetySafe), reg, nil, nil)
	if msg, missing := orchestratedMissingCapabilityError(testTask(planner.KindShellOperation, planner.SafetySafe), readOnly, reg); !missing {
		t.Fatalf("shell_operation on read-only allowlist must report missing capability, msg=%q", msg)
	}
}

// Requirement 13: no missing-capability error when the allowlist matches the task.
func TestOrchestratedMissingCapability_None(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	implAl := orchestratedToolAllowlist(testTask(planner.KindImplementation, planner.SafetySafe), reg, nil, nil)
	if _, missing := orchestratedMissingCapabilityError(testTask(planner.KindImplementation, planner.SafetySafe), implAl, reg); missing {
		t.Fatalf("implementation allowlist should satisfy implementation task")
	}
}

// Requirement 11: allowlist is sorted for determinism.
func TestOrchestratedToolAllowlist_Sorted(t *testing.T) {
	reg := newCoreRegistry(t.TempDir())
	al := orchestratedToolAllowlist(testTask(planner.KindCodeReview, planner.SafetySafe), reg, nil, nil)
	if sort.StringsAreSorted(al) {
		return
	}
	t.Fatalf("allowlist must be sorted, got %v", al)
}

// Requirement 9/5 sanity: a tool that does not exist in the registry is never in
// the derived allowlist, and the explicit full allowlist for an implementation
// task enumerates exactly the registered tool names (here, just "alpha").
func TestOrchestratedToolAllowlist_FullSetRespectsRegistry(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&stubTool{name: "alpha"})
	al := orchestratedToolAllowlist(testTask(planner.KindImplementation, planner.SafetySafe), reg, nil, nil)
	if al == nil {
		t.Fatalf("full allowlist must be explicit (non-nil)")
	}
	if len(al) != 1 || al[0] != "alpha" {
		t.Fatalf("single-tool registry must yield [alpha], got %v", al)
	}
}

type stubTool struct{ name string }

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub" }
func (s *stubTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", Properties: map[string]tools.PropertySchema{}}
}
func (s *stubTool) Safety() tools.Safety { return tools.Safety{SideEffect: tools.SideEffectNone} }
func (s *stubTool) Run(ctx context.Context, input map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}
