package orchestration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/scheduler"
	"github.com/Gitlawb/zero/internal/taskclass"
	"github.com/Gitlawb/zero/internal/tools"
)

// maxConsecutiveUnavailableToolErrors bounds repetitive unavailable-tool loops.
const maxConsecutiveUnavailableToolErrors = 3

// New creates a Coordinator for one run. The caller reads Events and
// Cancel/Result as needed. The coordinator never imports cli or tui.
func New(cfg RunConfig, cwd string, builder ProviderBuilder, candidates CandidateBuilder, registry *tools.Registry, agentOpts agent.Options, opts ...Option) *Coordinator {
	c := &Coordinator{
		cfg:        cfg,
		cwd:        cwd,
		builder:    builder,
		candidates: candidates,
		registry:   registry,
		clock:      time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	_ = agentOpts // reserved for future per-task option customization
	return c
}

// BuildPlan runs the deterministic classify → plan → validate → schedule →
// per-task route pipeline and returns the plan preview without executing. It
// is the shared entry point for both the plan-preview display and the run.
func BuildPlan(ctx context.Context, prompt string, cfg RunConfig, repoPresent bool, candidates []modelregistry.ModelEntry) (*PlanPreview, error) {
	classification := taskclass.Classify(taskclass.Request{
		Prompt:            prompt,
		HasImages:         false,
		RepositoryPresent: repoPresent,
	})

	plan, err := planner.Plan(planner.PlannerInput{
		Prompt:             prompt,
		TaskClassification: classification,
		RepositoryPresent:  repoPresent,
		AvailableTools:     nil,
	})
	if err != nil {
		return nil, err
	}
	if err := planner.Validate(plan); err != nil {
		return nil, err
	}

	sched, err := scheduler.NewScheduler(plan)
	if err != nil {
		return nil, err
	}
	state := sched.State()

	routerOpts := routerFlagOptions{
		provider:          cfg.RouterProvider,
		model:             cfg.PreferredModel,
		allowProviders:    cfg.AllowProviders,
		denyModels:        cfg.DenyModels,
		requireKnownPrice: cfg.RequireKnownPrice,
		maxInputCost:      cfg.MaxInputCost,
		maxOutputCost:     cfg.MaxOutputCost,
	}

	results := make([]TaskRoute, 0, len(plan.Tasks))
	for _, task := range plan.Tasks {
		decision, rerr := routeTask(task, candidates, routerOpts)
		if rerr != nil {
			return nil, rerr
		}
		taskState := taskStateFromScheduler(state, task.ID)
		results = append(results, TaskRoute{Task: task, State: taskState, Decision: decision})
	}

	return &PlanPreview{
		Prompt:         prompt,
		Classification: classification,
		Plan:           plan,
		TaskResults:    results,
		State:          state,
	}, nil
}

func taskStateFromScheduler(state scheduler.ExecutionState, id string) scheduler.TaskState {
	for _, t := range state.ReadyQueue {
		if t.ID == id {
			return scheduler.StateReady
		}
	}
	for _, t := range state.BlockedTasks {
		if t.ID == id {
			return scheduler.StateBlocked
		}
	}
	for _, t := range state.WaitingTasks {
		if t.ID == id {
			return scheduler.StateWaiting
		}
	}
	for _, t := range state.CompletedTasks {
		if t.ID == id {
			return scheduler.StateCompleted
		}
	}
	for _, t := range state.FailedTasks {
		if t.ID == id {
			return scheduler.StateFailed
		}
	}
	for _, t := range state.SkippedTasks {
		if t.ID == id {
			return scheduler.StateSkipped
		}
	}
	return scheduler.StatePlanned
}

// routerFlagOptions mirrors cli.routerFlagOptions to avoid importing cli.
type routerFlagOptions struct {
	provider          string
	model             string
	allowProviders    []string
	denyModels        []string
	requireKnownPrice bool
	maxInputCost      *float64
	maxOutputCost     *float64
}

func routeTask(task planner.Task, entries []modelregistry.ModelEntry, opts routerFlagOptions) (modelrouter.Decision, error) {
	classification := taskclass.Result{
		Primary:              plannerKindToClassKind(task.TaskKind),
		RequiredCapabilities: task.RequiredCapabilities,
	}
	return modelrouter.Decide(modelrouter.Request{
		Task:              classification,
		Candidates:        entries,
		PreferredProvider: opts.provider,
		PreferredModel:    opts.model,
		AllowedProviders:  opts.allowProviders,
		DisallowedModels:  opts.denyModels,
		MaxInputCost:      opts.maxInputCost,
		MaxOutputCost:     opts.maxOutputCost,
		RequireKnownPrice: opts.requireKnownPrice,
	})
}

func plannerKindToClassKind(k planner.TaskKind) taskclass.Kind {
	switch k {
	case planner.KindImplementation:
		return taskclass.KindImplementation
	case planner.KindRepositorySearch:
		return taskclass.KindRepoExploration
	case planner.KindCodeReview:
		return taskclass.KindCodeReview
	case planner.KindSecurityReview:
		return taskclass.KindSecurityReview
	case planner.KindArchitecture:
		return taskclass.KindArchitecturePlanning
	case planner.KindDocumentation:
		return taskclass.KindDocumentation
	case planner.KindTesting, planner.KindTestExecution:
		return taskclass.KindTesting
	case planner.KindDebugging:
		return taskclass.KindDebugging
	case planner.KindRefactoring:
		return taskclass.KindRefactoring
	case planner.KindShellOperation:
		return taskclass.KindShellSystem
	case planner.KindImageAnalysis:
		return taskclass.KindImageVisualAnalysis
	default:
		return taskclass.KindUnknown
	}
}

// Run executes the orchestration pipeline and emits events on the returned
// channel. The channel is closed when the run finishes (completed, failed, or
// cancelled). The caller should read events in a select loop with ctx.Done().
// The coordinator owns all goroutines; the consumer must not spawn workers.
func (c *Coordinator) Run(ctx context.Context, preview *PlanPreview) <-chan Event {
	events := make(chan Event, 64)

	go func() {
		defer close(events)
		c.runLoop(ctx, events, preview)
	}()
	return events
}

func (c *Coordinator) runLoop(ctx context.Context, events chan<- Event, preview *PlanPreview) {
	now := c.Now()

	if preview == nil {
		// Build the plan from scratch.
		emit(events, Event{Type: EventRunStarted, Timestamp: now})

		cands, _, err := c.candidates(ctx)
		if err != nil {
			emit(events, Event{Type: EventRunCancelled, Error: err.Error(), Timestamp: c.Now()})
			return
		}
		preview, err = BuildPlan(ctx, c.cfg.Prompt, c.cfg, true, cands)
		if err != nil {
			emit(events, Event{Type: EventRunCancelled, Error: err.Error(), Timestamp: c.Now()})
			return
		}
	}

	emit(events, Event{Type: EventPlanCreated, Plan: preview, Timestamp: c.Now()})

	plan := preview.Plan
	sched, err := scheduler.NewScheduler(plan)
	if err != nil {
		emit(events, Event{Type: EventRunCancelled, Error: err.Error(), Timestamp: c.Now()})
		return
	}

	cands, profileByCandidate, err := c.candidates(ctx)
	if err != nil {
		emit(events, Event{Type: EventRunCancelled, Error: err.Error(), Timestamp: c.Now()})
		return
	}

	var metrics *runMetricsAccum
	if c.cfg.EnableMetrics {
		metrics = &runMetricsAccum{clock: c.clock, runStartedAt: c.Now()}
	}

	executed := make([]orchestratedTaskExec, 0, len(plan.Tasks))
	skipped := make([]orchestratedSkippedTask, 0)
	tasksRun := 0
	batchCounter := 0

	for c.cfg.MaxTasks == 0 || tasksRun < c.cfg.MaxTasks {
		_ = tasksRun
		st := sched.State()
		if len(st.ReadyQueue) == 0 {
			break
		}

		// Try parallel batch.
		batch := c.collectParallelBatch(sched, preview)
		if len(batch) == 0 {
			// Sequential single task.
			task, ok := sched.NextReady()
			if !ok {
				break
			}

			if requiresApproval(task) {
				emit(events, Event{Type: EventTaskBlocked, Task: &task, Status: executor.StatusBlocked, Timestamp: c.Now()})
				skipped = append(skipped, orchestratedSkippedTask{task: task, reason: "requires approval"})
				_ = sched.MarkSkipped(task.ID)
				break
			}

			result, changes, verification, status, taskMetrics := c.executeTask(ctx, events, task, preview, cands, profileByCandidate, sched, 0)
			applyStatus(sched, task.ID, status)
			executed = append(executed, orchestratedTaskExec{task: task, result: result, changes: changes, verification: verification, status: status})
			if metrics != nil {
				metrics.appendTask(task, "", "", status, result, taskMetrics, 0)
			}
			tasksRun++

			if status == executor.StatusBlocked || status == executor.StatusFailed || status == executor.StatusIncomplete {
				markDependentsSkipped(sched, plan, task.ID, "skipped_due_to_dependency", &skipped)
				break
			}
		} else {
			// Parallel batch.
			batchCounter++
			ran, stop := c.runParallelBatch(ctx, events, sched, batch, preview, cands, profileByCandidate, plan, batchCounter, metrics, &executed, &skipped)
			tasksRun += ran
			if stop {
				break
			}
		}
	}

	// Emit run completed.
	finalState := sched.State()
	topStatus := topStatus(executed, skipped)
	metricsSnapshot := c.buildMetricsSnapshot(metrics, executed)

	emit(events, Event{
		Type:      EventRunCompleted,
		Status:    topStatus,
		Metrics:   metricsSnapshot,
		Timestamp: c.Now(),
	})
	_ = finalState
}

func (c *Coordinator) collectParallelBatch(sched *scheduler.Scheduler, preview *PlanPreview) []planner.Task {
	if !c.cfg.ParallelReadonly {
		return nil
	}
	var out []planner.Task
	for _, t := range sched.ReadyParallel() {
		if executor.TaskRequiresRepositoryVerification(t) {
			continue
		}
		if t.SafetyLevel != planner.SafetySafe {
			continue
		}
		if len(t.Dependencies) > 0 {
			continue
		}
		out = append(out, t)
	}
	return out
}

func (c *Coordinator) runParallelBatch(
	ctx context.Context,
	events chan<- Event,
	sched *scheduler.Scheduler,
	batch []planner.Task,
	preview *PlanPreview,
	cands []modelregistry.ModelEntry,
	profileByCandidate map[string]ProviderCandidate,
	plan planner.ExecutionPlan,
	batchNum int,
	metrics *runMetricsAccum,
	executed *[]orchestratedTaskExec,
	skipped *[]orchestratedSkippedTask,
) (ran int, stop bool) {
	maxWorkers := c.cfg.MaxWorkers
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	emit(events, Event{Type: EventBatchStarted, BatchNum: batchNum, Workers: maxWorkers, Timestamp: c.Now()})

	type job struct {
		task         planner.Task
		provider     agent.Provider
		providerKind string
		modelID      string
		decision     modelrouter.Decision
	}
	jobs := make([]job, 0, len(batch))
	for _, task := range batch {
		decision := taskDecision(preview, task.ID)
		providerKind, modelID := "", ""
		if decision.Selected != nil {
			providerKind = string(decision.Selected.Model.Provider)
			modelID = decision.Selected.Model.ID
		}
		prov, perr := c.builder(ctx, providerKind, modelID)
		if perr != nil || prov == nil {
			emit(events, Event{Type: EventTaskFailed, Task: &task, Status: executor.StatusFailed, Error: fmt.Sprintf("provider build failed: %v", perr), Timestamp: c.Now()})
			return 0, true
		}
		jobs = append(jobs, job{task: task, provider: prov, providerKind: providerKind, modelID: modelID, decision: decision})
	}

	var batchStart time.Time
	if metrics != nil {
		batchStart = c.Now()
	}

	var eventMu sync.Mutex
	var batchActive, batchPeak int32
	results := make([]parallelResult, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxWorkers)

	for i := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			if metrics != nil {
				n := atomic.AddInt32(&batchActive, 1)
				for {
					peak := atomic.LoadInt32(&batchPeak)
					if n <= peak {
						break
					}
					if atomic.CompareAndSwapInt32(&batchPeak, peak, n) {
						break
					}
				}
				metrics.enterWorker()
				defer func() {
					atomic.AddInt32(&batchActive, -1)
					metrics.leaveWorker()
				}()
			}
			j := jobs[idx]
			result, changes, verification, status, taskMetrics := c.executeTaskWithMu(ctx, events, &eventMu, j.task, preview, j.provider, j.providerKind, j.modelID, j.decision, batchNum)
			results[idx] = parallelResult{task: j.task, decision: j.decision, result: result, changes: changes, verification: verification, status: status, taskMetrics: taskMetrics}
		}(i)
	}
	wg.Wait()

	if metrics != nil {
		batchWall := int64(0)
		if !batchStart.IsZero() {
			batchWall = c.Now().Sub(batchStart).Milliseconds()
		}
		metrics.batches = append(metrics.batches, BatchMetric{
			Batch:       batchNum,
			Workers:     maxWorkers,
			TaskCount:   len(jobs),
			WallMs:      batchWall,
			PeakWorkers: int(atomic.LoadInt32(&batchPeak)),
		})
	}

	emit(events, Event{Type: EventBatchCompleted, BatchNum: batchNum, Workers: maxWorkers, Timestamp: c.Now()})

	for i := range results {
		r := results[i]
		applyStatus(sched, r.task.ID, r.status)
		*executed = append(*executed, orchestratedTaskExec{task: r.task, result: r.result, changes: r.changes, verification: r.verification, status: r.status})
		if metrics != nil {
			providerKind := ""
			modelID := ""
			if r.decision.Selected != nil {
				providerKind = string(r.decision.Selected.Model.Provider)
				modelID = r.decision.Selected.Model.ID
			}
			metrics.appendTask(r.task, providerKind, modelID, r.status, r.result, r.taskMetrics, batchNum)
		}
		switch r.status {
		case executor.StatusBlocked:
			markDependentsSkipped(sched, plan, r.task.ID, "skipped_due_to_dependency", skipped)
			stop = true
		case executor.StatusFailed, executor.StatusIncomplete:
			markDependentsSkipped(sched, plan, r.task.ID, "skipped_due_to_dependency", skipped)
			stop = true
		}
	}
	return len(results), stop
}

type parallelResult struct {
	task         planner.Task
	decision     modelrouter.Decision
	result       executor.TaskExecutionResult
	changes      executor.RepoChanges
	verification executor.VerificationOutcome
	status       executor.CompletionStatus
	taskMetrics  taskPerf
}

func (c *Coordinator) executeTask(
	ctx context.Context,
	events chan<- Event,
	task planner.Task,
	preview *PlanPreview,
	cands []modelregistry.ModelEntry,
	profileByCandidate map[string]ProviderCandidate,
	sched *scheduler.Scheduler,
	batchNum int,
) (executor.TaskExecutionResult, executor.RepoChanges, executor.VerificationOutcome, executor.CompletionStatus, taskPerf) {
	return c.executeTaskWithMu(ctx, events, nil, task, preview, nil, "", "", modelrouter.Decision{}, batchNum)
}

func (c *Coordinator) executeTaskWithMu(
	ctx context.Context,
	events chan<- Event,
	eventMu *sync.Mutex,
	task planner.Task,
	preview *PlanPreview,
	taskProvider agent.Provider,
	providerKind string,
	modelID string,
	decision modelrouter.Decision,
	batchNum int,
) (executor.TaskExecutionResult, executor.RepoChanges, executor.VerificationOutcome, executor.CompletionStatus, taskPerf) {
	perf := taskPerf{startedAt: c.Now()}

	// Route if not already done.
	if taskProvider == nil {
		decision = taskDecision(preview, task.ID)
		if decision.Selected != nil {
			providerKind = string(decision.Selected.Model.Provider)
			modelID = decision.Selected.Model.ID
		}
		var perr error
		taskProvider, perr = c.builder(ctx, providerKind, modelID)
		if perr != nil || taskProvider == nil {
			emitMu(events, eventMu, Event{Type: EventTaskFailed, Task: &task, Status: executor.StatusFailed, Error: fmt.Sprintf("provider build failed: %v", perr), Timestamp: c.Now()})
			return executor.TaskExecutionResult{}, executor.RepoChanges{}, executor.VerificationOutcome{}, executor.StatusFailed, perf
		}
	}

	emitMu(events, eventMu, Event{Type: EventTaskStarted, Task: &task, ProviderKind: providerKind, ModelID: modelID, BatchNum: batchNum, Timestamp: c.Now()})

	taskPrompt := c.buildTaskPromptWithSurvey(preview.Plan, task)

	// Build runner.
	var runner executor.Runner
	if c.runnerFactory != nil {
		runner = c.runnerFactory(taskProvider, agent.Options{Cwd: c.cwd, Model: modelID, Registry: c.registry})
	} else {
		opts := agent.Options{Cwd: c.cwd, Model: modelID}
		if c.registry != nil {
			opts.Registry = c.registry
		}
		runner = executor.NewAgentRunner(taskProvider, opts)
		if ar, ok := runner.(*executor.AgentRunner); ok {
			ar.Live = nil
		}
	}

	result, runErr := runner.RunTask(ctx, executor.TaskExecutionRequest{
		Task:          task,
		Prompt:        taskPrompt,
		ModelID:       modelID,
		ProviderID:    providerKind,
		WorkspaceRoot: c.cwd,
	})

	changes := executor.RepoChanges{}
	if c.repoDelta != nil {
		ch, _ := c.repoDelta(ctx, c.cwd)
		changes = ch
	}

	var verification executor.VerificationOutcome
	if executor.TaskRequiresRepositoryVerification(task) {
		if c.verifier != nil {
			verification = c.verifier(ctx, c.cwd, changes.All())
			perf.verified = true
		}
	} else {
		verification = executor.VerificationOutcome{Status: "not_applicable", Reason: "read-only task"}
	}

	var status executor.CompletionStatus
	if runErr != nil {
		status = executor.MapAgentError(result)
	} else {
		status = executor.EvaluateCompletion(task, result, changes, verification, executor.CompletionPolicy{})
	}

	perf.finishedAt = c.Now()

	switch status {
	case executor.StatusCompleted, executor.StatusCompletedNoChange, executor.StatusCompletedUnverified:
		emitMu(events, eventMu, Event{Type: EventTaskCompleted, Task: &task, Status: status, Result: &result, Changes: &changes, Verification: &verification, ProviderKind: providerKind, ModelID: modelID, Timestamp: c.Now()})
	case executor.StatusFailed:
		emitMu(events, eventMu, Event{Type: EventTaskFailed, Task: &task, Status: status, Result: &result, Error: errToText(runErr), Timestamp: c.Now()})
	case executor.StatusBlocked:
		emitMu(events, eventMu, Event{Type: EventTaskBlocked, Task: &task, Status: status, Result: &result, Timestamp: c.Now()})
	case executor.StatusIncomplete:
		emitMu(events, eventMu, Event{Type: EventTaskFailed, Task: &task, Status: status, Result: &result, Error: "incomplete", Timestamp: c.Now()})
	}

	return result, changes, verification, status, perf
}

// buildTaskPromptWithSurvey builds the task prompt and injects a repository
// survey for read-only tasks when available. The survey gives the agent a
// comprehensive starting index so it doesn't waste tool calls discovering the
// repository structure.
func (c *Coordinator) buildTaskPromptWithSurvey(plan planner.ExecutionPlan, task planner.Task) string {
	prompt := buildTaskPrompt(c.cfg.Prompt, plan, task)
	if c.survey == nil || c.cwd == "" {
		return prompt
	}
	// Only inject the survey for read-only task kinds that benefit from it.
	if !executor.TaskRequiresRepositoryVerification(task) {
		view := "all"
		// Choose the filtered view based on task kind.
		switch task.TaskKind {
		case planner.KindRepositorySearch:
			// If the task title mentions docs, use the docs view.
			if strings.Contains(strings.ToLower(task.Title), "doc") {
				view = "docs"
			} else if strings.Contains(strings.ToLower(task.Title), "source") ||
				strings.Contains(strings.ToLower(task.Title), "code") {
				view = "source"
			}
		case planner.KindCodeReview, planner.KindSecurityReview:
			view = "source"
		case planner.KindDocumentation:
			view = "docs"
		case planner.KindArchitecture:
			view = "all"
		default:
			return prompt
		}
		surveyCtx := RenderSurveyForTask(c.survey, view)
		if surveyCtx != "" {
			prompt = prompt + "\n\n" + surveyCtx
		}
	}
	return prompt
}

func buildTaskPrompt(request string, plan planner.ExecutionPlan, task planner.Task) string {
	var b strings.Builder
	b.WriteString("Original request: ")
	b.WriteString(request)
	b.WriteString("\n\nYou are executing EXACTLY ONE task from a larger plan. Complete only this task and then stop.\n\n")
	b.WriteString("Task ID: ")
	b.WriteString(task.ID)
	b.WriteString("\nTitle: ")
	b.WriteString(task.Title)
	b.WriteString("\nKind: ")
	b.WriteString(string(task.TaskKind))
	b.WriteString("\nDescription: ")
	b.WriteString(task.Description)
	b.WriteString("\n\nAcceptance conditions: finish only this task. Report what you changed and any verification you ran. If the requested feature already exists, say so with concrete evidence.\n")
	return b.String()
}

func taskDecision(preview *PlanPreview, taskID string) modelrouter.Decision {
	if preview == nil {
		return modelrouter.Decision{}
	}
	for _, r := range preview.TaskResults {
		if r.Task.ID == taskID {
			return r.Decision
		}
	}
	return modelrouter.Decision{}
}

func requiresApproval(task planner.Task) bool {
	return task.SafetyLevel == planner.SafetyDangerous
}

func applyStatus(sched *scheduler.Scheduler, taskID string, status executor.CompletionStatus) {
	switch status {
	case executor.StatusCompleted, executor.StatusCompletedNoChange, executor.StatusCompletedUnverified:
		_ = sched.MarkCompleted(taskID)
	case executor.StatusFailed, executor.StatusIncomplete:
		_ = sched.MarkFailed(taskID)
	case executor.StatusBlocked:
		_ = sched.MarkSkipped(taskID)
	}
}

func markDependentsSkipped(sched *scheduler.Scheduler, plan planner.ExecutionPlan, doneID, reason string, skipped *[]orchestratedSkippedTask) {
	for _, t := range plan.Tasks {
		for _, dep := range t.Dependencies {
			if dep == doneID {
				if err := sched.MarkSkipped(t.ID); err == nil {
					*skipped = append(*skipped, orchestratedSkippedTask{task: t, reason: reason})
				}
				break
			}
		}
	}
}

func topStatus(executed []orchestratedTaskExec, skipped []orchestratedSkippedTask) executor.CompletionStatus {
	hasFail := false
	sawCompleted := false
	for _, e := range executed {
		switch e.status {
		case executor.StatusBlocked, executor.StatusFailed, executor.StatusIncomplete:
			hasFail = true
		case executor.StatusCompleted:
			sawCompleted = true
		}
	}
	switch {
	case hasFail:
		return executor.StatusFailed
	case sawCompleted:
		return executor.StatusCompleted
	default:
		return executor.StatusCompletedUnverified
	}
}

func errToText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func emit(ch chan<- Event, e Event) {
	select {
	case ch <- e:
	case <-time.After(5 * time.Second):
	}
}

func emitMu(ch chan<- Event, mu *sync.Mutex, e Event) {
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	emit(ch, e)
}

// --- internal types ---

type orchestratedTaskExec struct {
	task         planner.Task
	result       executor.TaskExecutionResult
	changes      executor.RepoChanges
	verification executor.VerificationOutcome
	status       executor.CompletionStatus
}

type orchestratedSkippedTask struct {
	task   planner.Task
	reason string
}

type taskPerf struct {
	startedAt      time.Time
	finishedAt     time.Time
	verificationMs int64
	verified       bool
	providerCalls  int32
}

// runMetricsAccum is the internal metrics accumulator.
type runMetricsAccum struct {
	clock              func() time.Time
	runStartedAt       time.Time
	runFinishedAt      time.Time
	planningMs         int64
	routingMs          int64
	activeWorkers      int32
	peakWorkers        int32
	tasks              []TaskMetric
	batches            []BatchMetric
	totalProviderCalls int
	totalInputTokens   int
	totalOutputTokens  int
}

func (m *runMetricsAccum) enterWorker() {
	n := atomic.AddInt32(&m.activeWorkers, 1)
	for {
		peak := atomic.LoadInt32(&m.peakWorkers)
		if n <= peak {
			return
		}
		if atomic.CompareAndSwapInt32(&m.peakWorkers, peak, n) {
			return
		}
	}
}

func (m *runMetricsAccum) leaveWorker() {
	atomic.AddInt32(&m.activeWorkers, -1)
}

func (m *runMetricsAccum) appendTask(task planner.Task, providerKind, modelID string, status executor.CompletionStatus, result executor.TaskExecutionResult, perf taskPerf, batch int) {
	wall := int64(0)
	if !perf.startedAt.IsZero() && !perf.finishedAt.IsZero() {
		wall = perf.finishedAt.Sub(perf.startedAt).Milliseconds()
	}
	mt := TaskMetric{
		TaskID:         task.ID,
		Title:          task.Title,
		Batch:          batch,
		ProviderKind:   providerKind,
		Model:          modelID,
		ProviderCalls:  int(perf.providerCalls),
		Status:         string(status),
		WallMs:         wall,
		VerificationMs: perf.verificationMs,
		Verified:       perf.verified,
		Tokens: TokenUsage{
			Available:       result.UsageReported,
			InputTokens:     result.Usage.EffectiveInputTokens(),
			OutputTokens:    result.Usage.EffectiveOutputTokens(),
			ReasoningTokens: result.Usage.ReasoningTokens,
			TotalTokens:     result.Usage.TotalTokens(),
		},
		Tools: ToolMetrics{
			Attempted: result.ToolUsage.Attempted,
			Executed:  result.ToolUsage.Executed,
			Succeeded: result.ToolUsage.Succeeded,
			Failed:    result.ToolUsage.Failed,
			Denied:    result.ToolUsage.Denied,
		},
	}
	m.tasks = append(m.tasks, mt)
	m.totalProviderCalls += mt.ProviderCalls
	m.totalInputTokens += mt.Tokens.InputTokens
	m.totalOutputTokens += mt.Tokens.OutputTokens
}

func (c *Coordinator) buildMetricsSnapshot(m *runMetricsAccum, executed []orchestratedTaskExec) *RunMetrics {
	if m == nil {
		return nil
	}
	m.runFinishedAt = c.Now()
	runMs := int64(0)
	if !m.runStartedAt.IsZero() && !m.runFinishedAt.IsZero() {
		runMs = m.runFinishedAt.Sub(m.runStartedAt).Milliseconds()
	}
	concurrency := "serialized"
	if atomic.LoadInt32(&m.peakWorkers) > 1 {
		concurrency = "parallel"
	}
	snapshot := &RunMetrics{
		RunWallMs:          runMs,
		PlanningMs:         m.planningMs,
		RoutingMs:          m.routingMs,
		PeakWorkers:        int(atomic.LoadInt32(&m.peakWorkers)),
		Concurrency:        concurrency,
		TotalProviderCalls: m.totalProviderCalls,
		TotalInputTokens:   m.totalInputTokens,
		TotalOutputTokens:  m.totalOutputTokens,
		Tasks:              m.tasks,
		Batches:            m.batches,
	}
	if c.survey != nil {
		snapshot.SurveyBuildMs = c.survey.BuildMs
		snapshot.SurveyCacheHits = c.survey.CacheHits
	}
	// effective speedup
	if len(m.batches) > 0 {
		var summed, wall int64
		for _, b := range m.batches {
			wall += b.WallMs
		}
		for _, t := range m.tasks {
			if t.Batch > 0 {
				summed += t.WallMs
			}
		}
		if wall > 0 {
			sp := float64(summed) / float64(wall)
			snapshot.EffectiveSpeedup = &sp
			eff := sp / float64(atomic.LoadInt32(&m.peakWorkers))
			snapshot.WorkerEfficiency = &eff
		}
	}
	return snapshot
}

// FormatMetricsCompact renders a concise metrics block for TUI display.
func FormatMetricsCompact(m *RunMetrics) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	completed := 0
	for _, t := range m.Tasks {
		if t.Status == "completed" || t.Status == "completed_no_change" || t.Status == "completed_unverified" {
			completed++
		}
	}
	fmt.Fprintf(&b, "Completed %d/%d · %s\n", completed, len(m.Tasks), formatMs(m.RunWallMs))
	fmt.Fprintf(&b, "Peak workers: %d\n", m.PeakWorkers)
	fmt.Fprintf(&b, "Provider calls: %d\n", m.TotalProviderCalls)
	if m.TotalInputTokens > 0 || m.TotalOutputTokens > 0 {
		fmt.Fprintf(&b, "Tokens: %s in / %s out\n", formatInt(m.TotalInputTokens), formatInt(m.TotalOutputTokens))
	} else {
		b.WriteString("Tokens: unavailable\n")
	}
	if m.EffectiveSpeedup != nil {
		fmt.Fprintf(&b, "Effective speedup: %.2fx\n", *m.EffectiveSpeedup)
	}
	if m.WorkerEfficiency != nil {
		fmt.Fprintf(&b, "Efficiency: %.2f\n", *m.WorkerEfficiency)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatMs(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000.0)
}

func formatInt(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%d", n)
}

// SortTaskMetrics ensures deterministic task ordering in metrics.
func SortTaskMetrics(tasks []TaskMetric) {
	sort.SliceStable(tasks, func(i, j int) bool {
		return tasks[i].TaskID < tasks[j].TaskID
	})
}
