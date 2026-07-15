package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/orchestration"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/scheduler"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// orchestrationMode is the current TUI orchestration mode.
type orchestrationMode int

const (
	orchModeNormal orchestrationMode = iota
	orchModeOrchestrated
)

func (m orchestrationMode) String() string {
	switch m {
	case orchModeOrchestrated:
		return "Orchestrated"
	default:
		return "Normal"
	}
}

// orchestrationState holds session-local TUI orchestration configuration and
// runtime state. It is NOT persisted to config — it resets on /new.
type orchestrationState struct {
	mode             orchestrationMode
	parallelReadonly bool
	parallelWorkers  int
	metricsEnabled   bool
	previewBeforeRun bool

	// Runtime state during an orchestration run.
	active           bool
	preview          *orchestration.PlanPreview
	coordinator      *orchestration.Coordinator
	cancelFunc       context.CancelFunc
	taskStates       map[string]orchTaskState
	metrics          *orchestration.RunMetrics
	awaitingApproval bool

	// runID is the orchestration run identifier (mirrors the normal runID).
	runID int
}

type orchTaskState struct {
	status       executor.CompletionStatus
	providerKind string
	modelID      string
	startedAt    time.Time
	finishedAt   time.Time
	toolCalls    int
	tokensIn     int
	tokensOut    int
	tokensAvail  bool
	toolName     string
	finalAnswer  string
	verified     bool
}

func newOrchestrationState() orchestrationState {
	return orchestrationState{
		mode:             orchModeNormal,
		parallelWorkers:  2,
		previewBeforeRun: true,
		taskStates:       map[string]orchTaskState{},
	}
}

// toggleMode switches between Normal and Orchestrated.
func (s *orchestrationState) toggleMode() {
	if s.mode == orchModeNormal {
		s.mode = orchModeOrchestrated
	} else {
		s.mode = orchModeNormal
	}
}

// modeLabel returns the status label for the title bar.
func (s orchestrationState) modeLabel() string {
	if s.mode == orchModeNormal {
		return "Normal"
	}
	label := "Orchestrated"
	if s.parallelReadonly {
		label += fmt.Sprintf(" · Workers %d", s.parallelWorkers)
	}
	return label
}

// canToggle reports whether mode can be safely toggled right now.
// A toggle during an active run is rejected to prevent corruption.
func (s orchestrationState) canToggle() bool {
	return !s.active && !s.awaitingApproval
}

// --- Bubble Tea message types ---

// orchEventMsg wraps an orchestration coordinator event for Bubble Tea.
type orchEventMsg struct {
	event orchestration.Event
}

// orchPlanReadyMsg is sent when the plan preview is built and awaiting approval.
type orchPlanReadyMsg struct {
	preview *orchestration.PlanPreview
}

// orchPermissionMsg bridges a permission request from an orchestration worker
// to the TUI's interactive approval dialog. The worker blocks on the decision
// channel; the TUI resolves it through the existing permissionRequestMsg flow.
type orchPermissionMsg struct {
	runID   int
	taskID  string
	request agent.PermissionRequest
	decide  func(agent.PermissionDecision)
}

// orchestrationTickMsg drives periodic UI updates during an orchestration run.
type orchestrationTickMsg struct{}

// --- Orchestration submission flow ---

// startOrchestration begins the orchestration pipeline: classify → plan →
// preview. It does NOT execute yet — the user must approve.
func (m model) startOrchestration(prompt string) (model, tea.Cmd) {
	m.orch.active = true
	m.orch.preview = nil
	m.orch.taskStates = map[string]orchTaskState{}
	m.orch.metrics = nil
	m.orch.awaitingApproval = false
	m.orch.runID++
	m.pending = true
	m.turnStartedAt = m.now()

	// Persist the user prompt to the session.
	m = m.orchAppendSessionEvent(sessions.EventMessage, map[string]any{
		"role":    "user",
		"content": prompt,
	})

	return m, func() tea.Msg {
		ctx := context.Background()
		cands, _, err := m.orchCandidates(ctx)
		if err != nil {
			return orchEventMsg{event: orchestration.Event{Type: orchestration.EventRunCancelled, Error: err.Error()}}
		}
		cfg := m.orchRunConfig(prompt)
		preview, err := orchestration.BuildPlan(ctx, prompt, cfg, m.orchRepoPresent(), cands)
		if err != nil {
			return orchEventMsg{event: orchestration.Event{Type: orchestration.EventRunCancelled, Error: err.Error()}}
		}
		return orchPlanReadyMsg{preview: preview}
	}
}

// approveAndRunOrchestration starts execution after the user approves the plan.
func (m model) approveAndRunOrchestration() (model, tea.Cmd) {
	if m.orch.preview == nil {
		return m, nil
	}
	m.orch.awaitingApproval = false

	ctx, cancel := context.WithCancel(m.ctx)
	m.orch.cancelFunc = cancel

	cfg := m.orchRunConfig(m.orch.preview.Prompt)
	cands, profileMap, _ := m.orchCandidates(ctx)
	builder := m.orchProviderBuilder()
	candidates := func(ctx context.Context) ([]modelregistry.ModelEntry, map[string]orchestration.ProviderCandidate, error) {
		return cands, profileMap, nil
	}

	// Build agent options with the TUI's permission system wired in.
	// The OnPermissionRequest callback bridges orchestration worker
	// permission requests to the existing TUI approval dialog.
	baseOpts := m.agentOptions
	baseOpts.Cwd = m.cwd
	baseOpts.Registry = m.registry
	baseOpts.PermissionMode = m.permissionMode
	baseOpts.ToolExposure = agent.ToolExposureDefault
	// Orchestration runs are interactive (the TUI has an approver), so
	// RequireApproverForPromptTools stays false — the TUI's own
	// OnPermissionRequest handles approvals. The headless CLI path
	// (which has no interactive approver) sets it true.
	baseOpts.RequireApproverForPromptTools = false

	// Wire the TUI permission callback into the orchestration runner.
	// This reuses the EXACT same flow as normal TUI prompts: the worker
	// calls OnPermissionRequest, which sends a permissionRequestMsg
	// through runtimeMessageSink, the TUI shows the approval dialog,
	// and the user's decision flows back through the callback channel.
	orchRunID := m.orch.runID
	sink := m.runtimeMessageSink
	baseOpts.OnPermissionRequest = func(ctx context.Context, request agent.PermissionRequest) (agent.PermissionDecision, error) {
		if sink == nil {
			return agent.PermissionDecision{Action: agent.PermissionDecisionDeny, Reason: "no interactive approver available"}, nil
		}
		decisionCh := make(chan agent.PermissionDecision, 1)
		sink(orchPermissionMsg{
			runID:   orchRunID,
			request: request,
			decide: func(decision agent.PermissionDecision) {
				select {
				case decisionCh <- decision:
				default:
				}
			},
		})
		select {
		case decision := <-decisionCh:
			if strings.TrimSpace(decision.Reason) == "" {
				decision.Reason = permissionDecisionReason(permissionDecision(decision.Action))
			}
			return decision, nil
		case <-ctx.Done():
			return agent.PermissionDecision{Action: agent.PermissionDecisionDeny, Reason: ctx.Err().Error()}, ctx.Err()
		}
	}

	// Wire OnText for live streaming through the transcript.
	baseOpts.OnText = func(delta string) {
		if sink != nil {
			sink(orchEventMsg{event: orchestration.Event{
				Type:      orchestration.EventTaskUsageUpdated,
				Timestamp: time.Now(),
				// Reuse TaskUsageUpdated to carry text deltas via
				// the Tokens field as a side-channel is not ideal;
				// instead we use a separate mechanism below.
			}})
		}
	}
	// Reset OnText to nil — the AgentRunner handles its own streaming
	// via its Live writer; the TUI renders task cards from events.
	baseOpts.OnText = nil

	opts := []orchestration.Option{
		orchestration.WithRunnerFactory(func(provider agent.Provider, o agent.Options) executor.Runner {
			// Merge the permission-wired options with the per-task model.
			merged := baseOpts
			merged.Model = o.Model
			merged.Cwd = o.Cwd
			if o.Registry != nil {
				merged.Registry = o.Registry
			}
			r := executor.NewAgentRunner(provider, merged)
			return r
		}),
	}

	coord := orchestration.New(cfg, m.cwd, builder, candidates, m.registry, baseOpts, opts...)
	m.orch.coordinator = coord

	// Persist the plan to the session.
	m = m.orchAppendSessionEvent(sessions.EventMessage, map[string]any{
		"role":    "assistant",
		"content": orchRenderPlanPreview(m.orch.preview, m.orch),
	})

	return m, func() tea.Msg {
		events := coord.Run(ctx, m.orch.preview)
		var cmds []tea.Cmd
		for ev := range events {
			evCopy := ev
			cmds = append(cmds, func() tea.Msg {
				return orchEventMsg{event: evCopy}
			})
		}
		if len(cmds) > 0 {
			firstMsg := cmds[0]()
			rest := cmds[1:]
			return tea.Batch(func() tea.Msg { return firstMsg }, func() tea.Msg {
				_ = tea.Batch(rest...)
				return nil
			})()
		}
		return nil
	}
}

// cancelOrchestration cancels an in-flight orchestration run.
func (m model) cancelOrchestration() model {
	if m.orch.cancelFunc != nil {
		m.orch.cancelFunc()
	}
	m.orch.active = false
	m.orch.preview = nil
	m.orch.coordinator = nil
	return m
}

// --- Helpers ---

func (m model) orchRunConfig(prompt string) orchestration.RunConfig {
	return orchestration.RunConfig{
		Prompt:           prompt,
		ParallelReadonly: m.orch.parallelReadonly,
		MaxWorkers:       m.orch.parallelWorkers,
		EnableMetrics:    m.orch.metricsEnabled,
		RouterProvider:   m.providerName,
		PreferredModel:   m.modelName,
	}
}

func (m model) orchRepoPresent() bool {
	return m.cwd != ""
}

func (m model) orchCandidates(ctx context.Context) ([]modelregistry.ModelEntry, map[string]orchestration.ProviderCandidate, error) {
	reg, err := modelregistry.DefaultRegistry()
	if err != nil {
		return nil, nil, err
	}
	entries := reg.List(modelregistry.ListOptions{IncludeDeprecated: true})
	profileMap := map[string]orchestration.ProviderCandidate{}
	for _, e := range entries {
		profileMap[strings.ToLower(e.ID)] = orchestration.ProviderCandidate{
			ProfileName:  m.providerName,
			ProviderKind: string(e.Provider),
		}
	}
	return entries, profileMap, nil
}

func (m model) orchProviderBuilder() orchestration.ProviderBuilder {
	return func(ctx context.Context, providerKind, modelID string) (agent.Provider, error) {
		if m.newProvider != nil {
			prof := m.providerProfile
			prof.Provider = providerKind
			prof.Model = modelID
			return m.newProvider(prof)
		}
		return m.provider, nil
	}
}

// orchAppendSessionEvent persists an orchestration event to the active session.
func (m model) orchAppendSessionEvent(eventType sessions.EventType, payload map[string]any) model {
	if m.activeSession.SessionID == "" || m.sessionStore == nil {
		return m
	}
	m.sessionStore.AppendEvent(m.activeSession.SessionID, sessions.AppendEventInput{
		Type:    eventType,
		Payload: payload,
	})
	return m
}

// --- Event handling ---

func (m model) handleOrchEvent(msg orchEventMsg) (model, tea.Cmd) {
	ev := msg.event
	switch ev.Type {
	case orchestration.EventPlanCreated:
		m.orch.preview = ev.Plan
		m.orch.awaitingApproval = true
		return m, nil

	case orchestration.EventTaskStarted:
		if ev.Task != nil {
			s := m.orch.taskStates[ev.Task.ID]
			s.status = executor.StatusIncomplete
			s.providerKind = ev.ProviderKind
			s.modelID = ev.ModelID
			s.startedAt = time.Now()
			m.orch.taskStates[ev.Task.ID] = s
			// Live transcript: task started.
			label := orchTaskLabel(ev.Task.ID, ev.Task.Title)
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: fmt.Sprintf("● %s started — %s/%s", label, ev.ProviderKind, ev.ModelID),
			})
			// Persist to session.
			m = m.orchAppendSessionEvent(sessions.EventMessage, map[string]any{
				"role":    "system",
				"content": fmt.Sprintf("Task %s started: %s (%s/%s)", ev.Task.ID, ev.Task.Title, ev.ProviderKind, ev.ModelID),
			})
		}
		return m, m.scheduleOrchTick()

	case orchestration.EventTaskCompleted:
		if ev.Task != nil {
			s := m.orch.taskStates[ev.Task.ID]
			s.status = ev.Status
			s.finishedAt = time.Now()
			if ev.Result != nil {
				s.finalAnswer = ev.Result.FinalAnswer
				s.toolCalls = ev.Result.ToolUsage.Attempted
				s.tokensIn = ev.Result.Usage.EffectiveInputTokens()
				s.tokensOut = ev.Result.Usage.EffectiveOutputTokens()
				s.tokensAvail = ev.Result.UsageReported
			}
			if ev.Verification != nil {
				s.verified = ev.Verification.Status == "passed"
			}
			m.orch.taskStates[ev.Task.ID] = s
			// Live transcript: task completed.
			label := orchTaskLabel(ev.Task.ID, ev.Task.Title)
			verifyNote := ""
			if ev.Verification != nil && ev.Verification.Status != "" {
				verifyNote = " · verification: " + ev.Verification.Status
			}
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: fmt.Sprintf("✓ %s completed%s", label, verifyNote),
			})
			// Persist final answer to session.
			if s.finalAnswer != "" {
				m = m.orchAppendSessionEvent(sessions.EventMessage, map[string]any{
					"role":    "assistant",
					"content": fmt.Sprintf("[Task %s: %s]\n%s", ev.Task.ID, ev.Task.Title, s.finalAnswer),
				})
			}
		}
		return m, nil

	case orchestration.EventTaskFailed:
		if ev.Task != nil {
			s := m.orch.taskStates[ev.Task.ID]
			s.status = ev.Status
			s.finishedAt = time.Now()
			if ev.Result != nil {
				s.finalAnswer = ev.Result.FinalAnswer
			}
			m.orch.taskStates[ev.Task.ID] = s
			label := orchTaskLabel(ev.Task.ID, ev.Task.Title)
			errNote := ""
			if ev.Error != "" {
				errNote = " — " + ev.Error
			}
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: fmt.Sprintf("✗ %s failed%s", label, errNote),
			})
			m = m.orchAppendSessionEvent(sessions.EventMessage, map[string]any{
				"role":    "system",
				"content": fmt.Sprintf("Task %s failed: %s%s", ev.Task.ID, ev.Task.Title, errNote),
			})
		}
		return m, nil

	case orchestration.EventTaskBlocked:
		if ev.Task != nil {
			s := m.orch.taskStates[ev.Task.ID]
			s.status = executor.StatusBlocked
			s.finishedAt = time.Now()
			m.orch.taskStates[ev.Task.ID] = s
			label := orchTaskLabel(ev.Task.ID, ev.Task.Title)
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: fmt.Sprintf("⊘ %s blocked — requires approval", label),
			})
			m = m.orchAppendSessionEvent(sessions.EventMessage, map[string]any{
				"role":    "system",
				"content": fmt.Sprintf("Task %s blocked: requires approval", ev.Task.ID),
			})
		}
		return m, nil

	case orchestration.EventTaskSkipped:
		if ev.Task != nil {
			s := m.orch.taskStates[ev.Task.ID]
			s.status = executor.StatusBlocked
			s.finishedAt = time.Now()
			m.orch.taskStates[ev.Task.ID] = s
			label := orchTaskLabel(ev.Task.ID, ev.Task.Title)
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: fmt.Sprintf("⊘ %s skipped — %s", label, ev.SkippedReason),
			})
		}
		return m, nil

	case orchestration.EventBatchStarted:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: fmt.Sprintf("Parallel batch %d started — %d worker(s)", ev.BatchNum, ev.Workers),
		})
		return m, nil

	case orchestration.EventBatchCompleted:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: fmt.Sprintf("Parallel batch %d completed", ev.BatchNum),
		})
		return m, nil

	case orchestration.EventRunCompleted:
		m.orch.active = false
		m.orch.metrics = ev.Metrics
		m.orch.coordinator = nil
		m.orch.cancelFunc = nil
		// Append the orchestration summary.
		summary := orchRenderSummary(ev, m.orch)
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: summary})
		// Persist the summary to the session.
		m = m.orchAppendSessionEvent(sessions.EventMessage, map[string]any{
			"role":    "system",
			"content": summary,
		})
		// Reset pending state.
		m.pending = false
		if m.runCancel != nil {
			m.runCancel()
		}
		m.runCancel = nil
		m.activeRunID = 0
		m.turnStartedAt = time.Time{}
		return m, nil

	case orchestration.EventRunCancelled:
		m.orch.active = false
		m.orch.coordinator = nil
		m.orch.cancelFunc = nil
		errMsg := "Orchestration cancelled."
		if ev.Error != "" {
			errMsg = "Orchestration failed: " + ev.Error
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: errMsg})
		m = m.orchAppendSessionEvent(sessions.EventError, map[string]any{
			"error": errMsg,
		})
		m.pending = false
		if m.runCancel != nil {
			m.runCancel()
		}
		m.runCancel = nil
		m.activeRunID = 0
		m.turnStartedAt = time.Time{}
		return m, nil

	default:
		return m, nil
	}
}

func (m model) handleOrchPlanReady(msg orchPlanReadyMsg) (model, tea.Cmd) {
	m.orch.preview = msg.preview
	m.orch.awaitingApproval = true
	// Render the plan preview in the transcript.
	previewText := orchRenderPlanPreview(msg.preview, m.orch)
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: previewText})
	return m, nil
}

// handleOrchPermission bridges an orchestration worker's permission request
// into the existing TUI permission dialog. It reuses the same
// pendingPermissionPrompt mechanism as normal prompts.
func (m model) handleOrchPermission(msg orchPermissionMsg) (model, tea.Cmd) {
	if msg.runID != m.orch.runID {
		// Stale request from a cancelled run — unblock the worker.
		if msg.decide != nil {
			msg.decide(agent.PermissionDecision{Action: agent.PermissionDecisionCancel, Reason: "run cancelled"})
		}
		return m, nil
	}
	if msg.request.Action != agent.PermissionActionPrompt {
		// Non-interactive permission action — resolve immediately, fail closed.
		if msg.decide != nil {
			msg.decide(autoResolvedPermissionDecision(msg.request.Action))
		}
		return m, nil
	}
	// Store the permission request in the same pendingPermissionPrompt used
	// by normal prompts. The TUI's existing Enter/a/y/d handlers resolve it.
	promptRow := permissionTranscriptRow(permissionEventFromRequest(msg.request))
	promptRow.runID = msg.runID
	m.transcript = appendTranscriptRow(m.transcript, promptRow)
	m.pendingPermission = &pendingPermissionPrompt{
		request: msg.request,
		decide:  msg.decide,
	}
	return m, nil
}

func (m model) scheduleOrchTick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg {
		return orchestrationTickMsg{}
	})
}

func (m model) handleOrchTick() (model, tea.Cmd) {
	if !m.orch.active {
		return m, nil
	}
	return m, m.scheduleOrchTick()
}

// --- Command handling ---

// handleOrchestrateCommand processes /orchestrate [on|off|workers <n>|parallel|metrics|preview].
func (m model) handleOrchestrateCommand(args string) (model, tea.Cmd) {
	parts := strings.Fields(strings.TrimSpace(args))
	if len(parts) == 0 {
		// Toggle mode — reject during active run.
		if !m.orch.canToggle() {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: "Cannot toggle orchestration mode while a run is active. Cancel with Esc first.",
			})
			return m, nil
		}
		m.orch.toggleMode()
		modeText := m.orch.modeLabel()
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: "Mode: " + modeText,
		})
		return m, nil
	}
	switch parts[0] {
	case "on":
		if m.orch.active {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: "Orchestration already running. Cancel with Esc first.",
			})
			return m, nil
		}
		m.orch.mode = orchModeOrchestrated
	case "off":
		if m.orch.active {
			// Cancelling via /orchestrate off during an active run cancels safely.
			m = m.cancelOrchestration()
			m.pending = false
			m.turnStartedAt = time.Time{}
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: "Orchestration cancelled. Mode: Normal",
			})
			m.orch.mode = orchModeNormal
			return m, nil
		}
		m.orch.mode = orchModeNormal
	case "workers":
		if len(parts) >= 2 {
			n, err := strconv.Atoi(parts[1])
			if err != nil || n < 1 || n > 8 {
				m.transcript = reduceTranscript(m.transcript, transcriptAction{
					kind: actionAppendSystem,
					text: "Workers must be 1–8.",
				})
				return m, nil
			}
			m.orch.parallelWorkers = n
		}
	case "parallel":
		m.orch.parallelReadonly = !m.orch.parallelReadonly
	case "metrics":
		m.orch.metricsEnabled = !m.orch.metricsEnabled
	case "preview":
		m.orch.previewBeforeRun = !m.orch.previewBeforeRun
	default:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: "Usage: /orchestrate [on|off|workers <n>|parallel|metrics|preview]",
		})
		return m, nil
	}
	m.transcript = reduceTranscript(m.transcript, transcriptAction{
		kind: actionAppendSystem,
		text: "Orchestration: " + orchStatusLine(m.orch),
	})
	return m, nil
}

func orchStatusLine(s orchestrationState) string {
	var b strings.Builder
	b.WriteString(s.modeLabel())
	if s.parallelReadonly {
		b.WriteString(" · parallel-readonly")
	}
	b.WriteString(fmt.Sprintf(" · workers %d", s.parallelWorkers))
	if s.metricsEnabled {
		b.WriteString(" · metrics on")
	}
	if s.previewBeforeRun {
		b.WriteString(" · preview on")
	}
	return b.String()
}

// --- Submission flow integration ---

// launchOrchestratedPrompt starts the orchestration pipeline for a prompt.
func (m model) launchOrchestratedPrompt(prompt string) (model, tea.Cmd) {
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendUser, text: prompt})
	m.lastPrompt = prompt
	return m.startOrchestration(prompt)
}

// --- Rendering helpers ---

func orchTaskLabel(taskID, title string) string {
	return fmt.Sprintf("Task %s — %s", taskID, title)
}

func orchRenderPlanPreview(preview *orchestration.PlanPreview, orch orchestrationState) string {
	if preview == nil {
		return "Orchestration plan unavailable."
	}
	var b strings.Builder
	b.WriteString("Orchestration Plan\n")
	b.WriteString(strings.Repeat("─", 18))
	b.WriteString("\n")
	for i, r := range preview.TaskResults {
		t := r.Task
		fmt.Fprintf(&b, "%d. %s\n", i+1, t.Title)
		b.WriteString("   ")
		b.WriteString(string(r.State))
		b.WriteString(" · ")
		if executor.TaskRequiresRepositoryVerification(t) {
			b.WriteString("mutating")
		} else {
			b.WriteString("read-only")
		}
		if t.CanRunParallel {
			b.WriteString(" · parallel")
		}
		b.WriteString("\n")
		if r.Decision.Selected != nil {
			fmt.Fprintf(&b, "   %s / %s\n", string(r.Decision.Selected.Model.Provider), r.Decision.Selected.Model.ID)
		} else {
			b.WriteString("   (no compatible model)\n")
		}
		b.WriteString("\n")
	}
	if orch.parallelReadonly {
		fmt.Fprintf(&b, "Workers: %d\n", orch.parallelWorkers)
	}
	b.WriteString("Press Enter to run · Esc to cancel")
	return b.String()
}

func orchRenderSummary(ev orchestration.Event, orch orchestrationState) string {
	var b strings.Builder
	b.WriteString("Orchestration Summary\n")
	b.WriteString(strings.Repeat("─", 21))
	b.WriteString("\n")
	b.WriteString("Status: ")
	b.WriteString(string(ev.Status))
	b.WriteString("\n")

	if ev.Metrics != nil {
		b.WriteString(orchestration.FormatMetricsCompact(ev.Metrics))
		b.WriteString("\n")
	}

	// Combined final answers in deterministic task order.
	if orch.preview != nil {
		for _, r := range orch.preview.TaskResults {
			if s, ok := orch.taskStates[r.Task.ID]; ok && s.finalAnswer != "" {
				b.WriteString("\n")
				b.WriteString(fmt.Sprintf("[Task %s: %s]\n", r.Task.ID, r.Task.Title))
				b.WriteString(s.finalAnswer)
				b.WriteString("\n")
			}
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

func (m model) orchTaskCards() string {
	if len(m.orch.taskStates) == 0 || m.orch.preview == nil {
		return ""
	}
	var b strings.Builder
	// Render in plan order for determinism.
	for _, r := range m.orch.preview.TaskResults {
		s, ok := m.orch.taskStates[r.Task.ID]
		if !ok {
			continue
		}
		statusIcon := "○"
		switch s.status {
		case executor.StatusCompleted, executor.StatusCompletedNoChange, executor.StatusCompletedUnverified:
			statusIcon = "✓"
		case executor.StatusFailed, executor.StatusIncomplete:
			statusIcon = "✗"
		case executor.StatusBlocked:
			statusIcon = "⊘"
		}
		fmt.Fprintf(&b, "%s %s", statusIcon, orchTaskLabel(r.Task.ID, r.Task.Title))
		if s.modelID != "" {
			fmt.Fprintf(&b, " · %s/%s", s.providerKind, s.modelID)
		}
		if !s.startedAt.IsZero() {
			elapsed := time.Since(s.startedAt)
			if s.finishedAt.IsZero() {
				fmt.Fprintf(&b, " · %s", formatDuration(elapsed))
			} else {
				fmt.Fprintf(&b, " · %s", formatDuration(s.finishedAt.Sub(s.startedAt)))
			}
		}
		b.WriteString("\n")
		if s.toolCalls > 0 {
			fmt.Fprintf(&b, "  %d tool calls", s.toolCalls)
			if s.tokensAvail {
				fmt.Fprintf(&b, " · %d in / %d out tokens", s.tokensIn, s.tokensOut)
			} else {
				b.WriteString(" · tokens unavailable")
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// Compile-time guards to ensure imports are used.
var _ = sync.Mutex{}
var _ = zeroruntime.Provider(nil)
var _ = planner.Task{}
var _ = scheduler.TaskState("")
var _ = sandbox.Engine{}
var _ = tools.Registry{}
