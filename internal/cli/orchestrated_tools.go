package cli

import (
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/tools"
)

// toolCapability groups tools by the kind of effect they have, so the
// orchestrated task policy can grant or deny whole categories at once.
type toolCapability int

const (
	capRead toolCapability = iota
	capEdit
	capShell
	capMeta
)

// taskAllowedCapabilities returns the tool-capability categories a task may use.
// It mirrors the documented policy: read/planning tasks get read-only tools;
// implementation/refactoring/debugging/testing get read + edit + bounded shell;
// shell_operation gets read + shell; dangerous tasks are downgraded to read-only
// (and additionally blocked from executing unless explicitly approved).
func taskAllowedCapabilities(task planner.Task) map[toolCapability]bool {
	switch task.TaskKind {
	case planner.KindRepositorySearch, planner.KindCodeReview, planner.KindSecurityReview,
		planner.KindDocumentation, planner.KindArchitecture, planner.KindImageAnalysis:
		return map[toolCapability]bool{capRead: true, capMeta: true}
	case planner.KindShellOperation:
		return map[toolCapability]bool{capRead: true, capShell: true, capMeta: true}
	case planner.KindImplementation, planner.KindRefactoring, planner.KindDebugging,
		planner.KindTesting, planner.KindTestExecution:
		return map[toolCapability]bool{capRead: true, capEdit: true, capShell: true, capMeta: true}
	default:
		return map[toolCapability]bool{capRead: true, capEdit: true, capShell: true, capMeta: true}
	}
}

func capForCategory(c executor.ToolCategory) toolCapability {
	switch c {
	case executor.CategoryMutating:
		return capEdit
	case executor.CategoryCommand:
		return capShell
	case executor.CategoryRead:
		return capRead
	default:
		return capMeta
	}
}

// capabilityForTool classifies a concrete registered tool by its actual safety
// side effect, which is the authoritative source of truth for what a tool can
// do. It falls back to the name-based executor classification when the registry
// is unavailable or does not contain the tool.
func capabilityForTool(reg *tools.Registry, name string) toolCapability {
	if reg != nil {
		if tool, ok := reg.Get(name); ok {
			switch tool.Safety().SideEffect {
			case tools.SideEffectShell:
				return capShell
			case tools.SideEffectWrite:
				return capEdit
			case tools.SideEffectRead, tools.SideEffectNone, tools.SideEffectNetwork:
				return capRead
			default:
				return capMeta
			}
		}
	}
	return capForCategory(executor.ClassifyTool(name))
}

// orchestratedToolAllowlist computes the effective, task-specific tool allowlist
// for one orchestrated task. It reuses the SAME registry the normal exec path
// built (od.registry) — it never creates a second tool registry — and intersects
// it with the task policy, the operator's --enabled-tools override, and the
// --disabled-tools denylist. The result contains only tool names that actually
// exist in the registry, so the model is never advertised a tool that cannot run.
//
// The allowlist is ALWAYS an explicit, non-nil list of real registered tool names
// (never nil). "nil means unrestricted" is ambiguous across the agent's tool
// layers (advertising gate, deferred-tool loader, missing-capability check, task
// prompt), so instead of returning nil we spell out the exact set — which for an
// unrestricted implementation task is the same complete tool set a normal zero
// exec run advertises. A nil registry (which never happens on the real path) is
// the only case that returns nil, signalling "no restriction possible".
func orchestratedToolAllowlist(task planner.Task, reg *tools.Registry, userEnabled, userDisabled []string) []string {
	allowed := taskAllowedCapabilities(task)
	// Dangerous tasks never receive edit/shell capability, even before the
	// higher-level "blocked until explicit approval" guard fires.
	if task.SafetyLevel == planner.SafetyDangerous {
		allowed = map[toolCapability]bool{capRead: true, capMeta: true}
	}

	if reg == nil {
		// No registry to enumerate from: honor the operator overrides literally.
		// With none set there is nothing to restrict, so return nil (unrestricted).
		if len(userEnabled) == 0 && len(userDisabled) == 0 {
			return nil
		}
		return append([]string(nil), userEnabled...)
	}

	var out []string
	for _, tool := range reg.All() {
		if allowed[capabilityForTool(reg, tool.Name())] {
			out = append(out, tool.Name())
		}
	}
	if len(userEnabled) > 0 {
		out = intersectNames(out, userEnabled)
	}
	out = subtractNames(out, userDisabled)
	sort.Strings(out)
	return out
}

func intersectNames(base, override []string) []string {
	set := make(map[string]bool, len(override))
	for _, n := range override {
		set[strings.TrimSpace(n)] = true
	}
	var out []string
	for _, n := range base {
		if set[n] {
			out = append(out, n)
		}
	}
	return out
}

func subtractNames(base, deny []string) []string {
	set := make(map[string]bool, len(deny))
	for _, n := range deny {
		set[strings.TrimSpace(n)] = true
	}
	var out []string
	for _, n := range base {
		if !set[n] {
			out = append(out, n)
		}
	}
	return out
}

// orchestratedTaskRequiresApproval reports whether a task is too risky to run
// headlessly and must be blocked until the operator explicitly approves it
// (e.g. via --skip-permissions-unsafe, which resolves to PermissionModeUnsafe).
func orchestratedTaskRequiresApproval(task planner.Task, permissionMode agent.PermissionMode) bool {
	return task.SafetyLevel == planner.SafetyDangerous && permissionMode != agent.PermissionModeUnsafe
}

// taskRequiresMutation reports whether a task kind needs a file-editing or shell
// capability to be useful (implementation/refactor/debug/test/shell work).
func taskRequiresMutation(task planner.Task) bool {
	switch task.TaskKind {
	case planner.KindImplementation, planner.KindRefactoring, planner.KindDebugging,
		planner.KindTesting, planner.KindTestExecution, planner.KindShellOperation:
		return true
	}
	return false
}

// orchestratedExposurePolicy selects how aggressively tools are advertised for a
// task, independent of the permission mode. Mutating tasks (implementation/
// refactor/debug/test/shell) get TaskCompatible exposure so the model can SEE and
// REQUEST the editing tools it needs even under PermissionModeAuto — but the
// permission evaluator still decides at call time whether to allow, prompt, or
// deny, so advertising never grants execution authority. Read-only and dangerous
// tasks keep the default exposure (the task allowlist already restricts them to
// read-only/not-applicable). Explicit user permission modes are never consulted
// here; the CLI passes od.permissionMode straight through to the agent untouched.
func orchestratedExposurePolicy(task planner.Task) agent.ToolExposurePolicy {
	if taskRequiresMutation(task) {
		return agent.ToolExposureTaskCompatible
	}
	return agent.ToolExposureDefault
}

// orchestratedCompletionPolicy selects how strictly deterministic evidence is
// required for completion. Defaults favor deterministic evidence: a repository
// delta is sufficient even without verification, and the model's completion
// signal is only supporting evidence. No public flag exists for this milestone;
// the policy is intentionally lenient so a real file change is never downgraded
// to incomplete purely for lacking a model signal or verification plan.
func orchestratedCompletionPolicy(od orchestratedOnceDeps) executor.CompletionPolicy {
	return executor.CompletionPolicy{
		RequireVerificationForMutations: false,
		RequireModelSignal:              false,
	}
}

// orchestratedMissingCapabilityError returns a clear pre-execution error when the
// selected task fundamentally needs a capability the effective tool set removed
// (e.g. an implementation task whose write/edit tools were all filtered out). It
// returns ("", false) when no such precondition fails.
func orchestratedMissingCapabilityError(task planner.Task, allowlist []string, reg *tools.Registry) (string, bool) {
	has := func(cap toolCapability) bool {
		for _, name := range allowlist {
			if capabilityForTool(reg, name) == cap {
				return true
			}
		}
		return false
	}
	// allowlist == nil means "all tools"; capabilities are present.
	if allowlist == nil {
		return "", false
	}
	switch task.TaskKind {
	case planner.KindImplementation, planner.KindRefactoring, planner.KindDebugging,
		planner.KindTesting, planner.KindTestExecution:
		if !has(capEdit) {
			return "task requires file editing but no write/edit tool is available (all editing tools were filtered out)", true
		}
	case planner.KindShellOperation:
		if !has(capShell) {
			return "task requires shell access but no shell tool is available (all shell tools were filtered out)", true
		}
	}
	return "", false
}

// orchestratedCapabilityNote renders a human/model-readable summary of the tool
// capability boundary for the task, used in the task-specific prompt so the
// model knows exactly which tools it may call.
func orchestratedCapabilityNote(allowlist []string, reg *tools.Registry) string {
	read, edit, shell := false, false, false
	if allowlist == nil {
		read, edit, shell = true, true, true
	} else {
		for _, name := range allowlist {
			switch capabilityForTool(reg, name) {
			case capRead:
				read = true
			case capEdit:
				edit = true
			case capShell:
				shell = true
			}
		}
	}
	var parts []string
	if read {
		parts = append(parts, "read/search tools")
	}
	if edit {
		parts = append(parts, "file editing tools (write_file, edit_file, apply_patch)")
	}
	if shell {
		parts = append(parts, "bounded shell command tools")
	}
	if len(parts) == 0 {
		return "No tools are available for this task."
	}
	return "Allowed capabilities for this task: " + strings.Join(parts, ", ") + "."
}
