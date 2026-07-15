package executor

import (
	"context"
	"io"
	"strings"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/tools"
)

// AgentRunner is the production Runner backed by the existing Zero agent
// runtime. It is constructed with an already-built provider and a base set of
// agent options (tools registry, sandbox, permission mode, hooks, …) produced by
// the CLI's normal exec setup, then swaps in the task-specific model, workspace
// root, and session id and runs the agent for the single task.
type AgentRunner struct {
	provider agent.Provider
	options  agent.Options
	// Live, when set, receives the agent's streamed final-answer text so a
	// headless orchestrated run can echo progress to stderr while the structured
	// report is reserved for stdout.
	Live io.Writer
}

// NewAgentRunner builds a Runner from a provider and the base agent options.
func NewAgentRunner(provider agent.Provider, options agent.Options) *AgentRunner {
	return &AgentRunner{provider: provider, options: options}
}

// RunTask executes one task through agent.Run, collecting deterministic
// evidence (tool actions, changed files, commands, final answer) via callbacks
// it installs on a copy of the base options.
func (r *AgentRunner) RunTask(ctx context.Context, req TaskExecutionRequest) (TaskExecutionResult, error) {
	opts := r.options
	opts.Model = req.ModelID
	opts.Cwd = req.WorkspaceRoot
	if req.SessionID != "" {
		opts.SessionID = req.SessionID
	}

	// Bound repetitive unavailable-tool loops: when the model keeps calling
	// tools that are not enabled / not available for this run, stop the agent
	// after a small number of consecutive such failures instead of letting it
	// retry forever. This is an orchestrated-run safety net layered on top of the
	// agent's own dispatch filter, which already rejects unknown tools.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	coll := newEvidenceCollector(r.Live, cancel)
	opts.OnToolCall = coll.onToolCall
	opts.OnToolResult = coll.onToolResult
	opts.OnPermission = coll.onPermission
	opts.OnUsage = coll.onUsage
	opts.OnText = coll.onText

	res, err := agent.Run(runCtx, req.Prompt, r.provider, opts)
	result := coll.finish(res, err)
	return result, err
}

// maxConsecutiveUnavailableToolErrors bounds how many times the model may call a
// tool that the run filtered out / does not expose before the orchestrated run
// halts the loop.
const maxConsecutiveUnavailableToolErrors = 3

// evidenceCollector accumulates task execution evidence from agent callbacks.
type evidenceCollector struct {
	events   []ToolEvent
	files    map[string]bool
	commands map[string]bool
	final    strings.Builder
	live     io.Writer
	// cancel, when set, halts the agent run after too many consecutive
	// unavailable-tool failures.
	cancel func()
	// consecutiveUnavailable counts back-to-back tool results that were filtered
	// out / not exposed for this run; a successful (or merely errored-but-valid)
	// tool resets it.
	consecutiveUnavailable int
	// permissionRequired records that a tool needed approval (prompt-required)
	// but none was granted — e.g. a headless Auto run where no approver exists.
	// The completion gate maps this to blocked.
	permissionRequired bool
	// permissionDenied records an explicit approval denial.
	permissionDenied bool
	// usage accumulates the normalized token accounting across every agent turn
	// of the task, via the OnUsage callback. usageReported is true once at
	// least one usage event has arrived.
	usage         agent.Usage
	usageReported bool
	// tool counts, derived from the OnToolCall / OnToolResult callbacks.
	toolCalls     int
	toolExecuted  int
	toolSucceeded int
	toolFailed    int
	toolDenied    int
}

func newEvidenceCollector(live io.Writer, cancel func()) *evidenceCollector {
	return &evidenceCollector{
		files:    map[string]bool{},
		commands: map[string]bool{},
		live:     live,
		cancel:   cancel,
	}
}

func (c *evidenceCollector) onToolCall(call agent.ToolCall) {
	c.toolCalls++
	c.events = append(c.events, ToolEvent{
		Name:    call.Name,
		Kind:    toolCategory(call.Name).String(),
		ArgsRaw: call.Arguments,
	})
}

// isUnavailableToolResult reports whether a tool result indicates the model
// called a tool the run does not expose (filtered / unknown), which is the
// repetitive failure the bounded-correction guard targets.
func isUnavailableToolResult(result agent.ToolResult) bool {
	if result.DenialReason == agent.DenialFiltered {
		return true
	}
	if result.Status != tools.StatusError {
		return false
	}
	out := strings.ToLower(result.Output)
	return strings.Contains(out, "not enabled for this run") ||
		strings.Contains(out, "not available in spec-draft mode") ||
		strings.Contains(out, "is not a known tool") ||
		strings.Contains(out, "unknown tool")
}

func (c *evidenceCollector) onToolResult(result agent.ToolResult) {
	ev := ToolEvent{
		Name:          result.Name,
		Kind:          toolCategory(result.Name).String(),
		Status:        string(result.Status),
		Denial:        string(result.DenialReason),
		ChangedFiles:  append([]string(nil), result.ChangedFiles...),
		OutputSummary: truncate(result.Output, 200),
	}
	c.events = append(c.events, ev)
	for _, f := range result.ChangedFiles {
		c.files[f] = true
	}
	if toolCategory(result.Name) == CategoryCommand {
		c.commands[result.Name] = true
	}

	if isUnavailableToolResult(result) {
		c.consecutiveUnavailable++
		if c.consecutiveUnavailable >= maxConsecutiveUnavailableToolErrors && c.cancel != nil {
			c.cancel()
		}
	} else {
		c.consecutiveUnavailable = 0
	}

	// Tool-usage accounting for metrics. A denied or filtered call never
	// executed, so it counts as denied and is excluded from the
	// executed/succeeded/failed partition.
	if result.DenialReason != agent.DenialNone || isUnavailableToolResult(result) {
		c.toolDenied++
		return
	}
	c.toolExecuted++
	if result.Status == tools.StatusOK {
		c.toolSucceeded++
	} else {
		c.toolFailed++
	}
}

func (c *evidenceCollector) onPermission(event agent.PermissionEvent) {
	// Record a permission outcome as an evidence event so the completion gate can
	// see it even if the run ultimately surfaces it as an error.
	switch event.Action {
	case agent.PermissionActionDeny:
		c.permissionDenied = true
		c.permissionRequired = true
		c.events = append(c.events, ToolEvent{
			Name:   event.ToolName,
			Kind:   "permission",
			Status: string(event.Action),
		})
	case agent.PermissionActionCancel:
		c.permissionDenied = true
		c.events = append(c.events, ToolEvent{
			Name:   event.ToolName,
			Kind:   "permission",
			Status: string(event.Action),
		})
	case agent.PermissionActionPrompt:
		// The tool required approval and the run had no approver (e.g. headless
		// Auto). Mark the task as permission-required so it maps to blocked.
		c.permissionRequired = true
		c.events = append(c.events, ToolEvent{
			Name:   event.ToolName,
			Kind:   "permission",
			Status: string(event.Action),
		})
	}
}

func (c *evidenceCollector) onUsage(u agent.Usage) {
	c.usageReported = true
	c.usage.InputTokens += u.EffectiveInputTokens()
	c.usage.OutputTokens += u.EffectiveOutputTokens()
	c.usage.CachedInputTokens += u.CachedInputTokens
	c.usage.CacheWriteTokens += u.CacheWriteTokens
	c.usage.ReasoningTokens += u.ReasoningTokens
}

func (c *evidenceCollector) onText(s string) {
	c.final.WriteString(s)
	if c.live != nil {
		io.WriteString(c.live, s)
	}
}

func (c *evidenceCollector) finish(res agent.Result, runErr error) TaskExecutionResult {
	out := TaskExecutionResult{
		AgentResult:   res,
		ToolEvents:    c.events,
		FinalAnswer:   strings.TrimSpace(c.final.String()),
		Error:         runErr,
		Usage:         c.usage,
		UsageReported: c.usageReported,
		ToolUsage: ToolUsageMetrics{
			Attempted: c.toolCalls,
			Executed:  c.toolExecuted,
			Succeeded: c.toolSucceeded,
			Failed:    c.toolFailed,
			Denied:    c.toolDenied,
		},
	}
	if out.FinalAnswer == "" {
		out.FinalAnswer = res.FinalAnswer
	}
	for f := range c.files {
		out.FilesChanged = append(out.FilesChanged, f)
	}
	for cmd := range c.commands {
		out.CommandsRun = append(out.CommandsRun, cmd)
	}
	sortStrings(out.FilesChanged)
	sortStrings(out.CommandsRun)
	if runErr != nil {
		if isCancellation(runErr) {
			out.Cancelled = true
		}
		if isPermissionError(runErr) {
			out.PermissionDenied = true
		}
	}
	// Permission events observed during the run (including headless denials that
	// surfaced as tool results rather than run errors) block the task.
	if c.permissionRequired {
		out.PermissionRequired = true
	}
	if c.permissionDenied {
		out.PermissionDenied = true
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
