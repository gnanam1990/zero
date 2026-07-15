package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/hooks"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/orchestration"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/providers"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/scheduler"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/verify"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// Orchestration-specific session event types. These are intentionally minimal
// additions recorded on the single orchestrated session; they reuse the existing
// sessions.AppendEvent machinery and do not alter the core event set.
const (
	EventOrchestratedPlan                   sessions.EventType = "orchestrated_plan"
	EventOrchestratedTaskSelected           sessions.EventType = "orchestrated_task_selected"
	EventOrchestratedModelRouted            sessions.EventType = "orchestrated_model_routed"
	EventOrchestratedTaskStarted            sessions.EventType = "orchestrated_task_started"
	EventOrchestratedTaskCompleted          sessions.EventType = "orchestrated_task_completed"
	EventOrchestratedTaskFailed             sessions.EventType = "orchestrated_task_failed"
	EventOrchestratedVerification           sessions.EventType = "orchestrated_verification"
	EventOrchestratedParallelBatchStarted   sessions.EventType = "orchestrated_parallel_batch_started"
	EventOrchestratedParallelBatchCompleted sessions.EventType = "orchestrated_parallel_batch_completed"
	EventOrchestratedMetricsRunCompleted    sessions.EventType = "metrics.run_completed"
)

// orchestratedBuildPlan is the plan-preview entry point used by the sequential
// engine. It is a package-level variable so tests can substitute a
// deterministic plan without invoking the real classifier/planner.
var orchestratedBuildPlan = buildPlanPreview

// orchestratedOnceDeps carries the already-resolved exec context into the
// orchestrated-once runner. It reuses the same provider/options construction the
// normal exec path performed up to the agent.Run call, so a run without
// --orchestrated-once is byte-identical to before.
type orchestratedOnceDeps struct {
	options                 execOptions
	stdout                  io.Writer
	stderr                  io.Writer
	deps                    appDeps
	workspaceRoot           string
	trustRoot               string
	registry                *tools.Registry
	modelRegistry           modelregistry.Registry
	resolved                config.ResolvedConfig
	modelSwitcher           func(context.Context, string) (agent.Provider, error)
	forwardEffort           string
	images                  []zeroruntime.ImageBlock
	sandboxEngine           *sandbox.Engine
	effectiveDeferThreshold int
	specialistRuntime       *agentToolRuntime
	pluginActivation        pluginActivation
	permissionMode          agent.PermissionMode
	sessionTitle            string
	prompt                  string
	// runner, when set, overrides the default agent-backed executor. Tests inject
	// a fake; production leaves it nil to build an AgentRunner from taskProvider.
	runner executor.Runner
	// verifier, when set, overrides the default verify-backed verifier. Tests
	// inject a fake; production leaves it nil to use the workspace verify plan.
	verifier executor.Verifier
	// resolveRuntimeMetadata, when set, resolves a provider profile to its
	// effective provider kind and api model without constructing a network
	// client. Tests inject a fake; production leaves it nil to use
	// providers.ResolveRuntimeMetadata.
	resolveRuntimeMetadata func(config.ProviderProfile, providers.Options) (providers.RuntimeMetadata, error)
	// parallel configures bounded read-only concurrent execution for the
	// orchestrated DAG. Enabled is only true when the user passed
	// --parallel-readonly with --orchestrated.
	parallel parallelReadonlyOptions
	// metrics, when non-nil, enables native observability/benchmarking
	// collection for the run. It is allocated only when --orchestrated-metrics
	// or --metrics-json is passed, so a run without metrics is byte-identical
	// to before (no timing, no provider wrapping).
	metrics *orchestratedRunMetrics
	// metricsJSONPath, when set, writes the full metrics object to this path
	// as JSON in addition to any inline rendering.
	metricsJSONPath string
	// survey is the shared repository survey built once per run and injected
	// into read-only task prompts to reduce redundant tool calls.
	survey *orchestration.Survey
}

// parallelReadonlyOptions carries the bounded read-only concurrency configuration.
// MaxWorkers bounds how many independent read-only tasks run at once (semaphore).
type parallelReadonlyOptions struct {
	Enabled    bool
	MaxWorkers int
}

// runOrchestratedOnce executes exactly ONE planned task through the existing
// agent runtime, then stops. It classifies/plans/routes via the shared preview
// pipeline, picks the first ready task, routes it, runs it with a one-task
// evidence-collecting runner, computes a baseline-aware repo delta and
// verification, evaluates a deterministic completion gate, advances the
// scheduler state for that single task, records orchestration events on one
// session, and renders a structured report.
// orchestratedExecutionOptions parameterizes the shared sequential orchestrated
// engine. --orchestrated-once runs it with MaxTasks=1; --orchestrated runs the
// full DAG (MaxTasks=0, i.e. unbounded) and stops on the first failure/block.
type orchestratedExecutionOptions struct {
	MaxTasks      int
	StopOnFailure bool
	StopOnBlocked bool
}

type orchestratedTaskExec struct {
	task         planner.Task
	decision     modelrouter.Decision
	result       executor.TaskExecutionResult
	changes      executor.RepoChanges
	verification executor.VerificationOutcome
	status       executor.CompletionStatus
	// ParallelBatch is >0 when the task ran in a concurrent read-only batch
	// (the batch index, 1-based), and 0 for a sequential (or once) task.
	ParallelBatch int
}

type orchestratedSkippedTask struct {
	task   planner.Task
	reason string
}

// orchestratedTaskPerf carries the raw timing/usage samples captured inside a
// single executeOrchestratedTask call. The caller (coordinator) turns it into an
// orchestratedTaskMetric, filling in the task identity and summing run totals.
type orchestratedTaskPerf struct {
	SelectedAt     time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
	VerificationMs int64
	Verified       bool
	ProviderCalls  int32
}

// runOrchestrated is the shared sequential orchestrated engine. It classifies,
// plans, routes, and executes planned tasks one at a time through the existing
// agent runtime, advancing scheduler state and recording orchestration events on a
// single session. --orchestrated-once calls it with MaxTasks=1.
func runOrchestrated(od orchestratedOnceDeps, opts orchestratedExecutionOptions) int {
	ctx, stop := signalContext()
	defer stop()

	mode := "orchestrated"
	if opts.MaxTasks == 1 {
		mode = "orchestrated-once"
	}

	if od.metrics != nil {
		od.metrics.RunStartedAt = orchestratedNow()
	}

	if od.resolveRuntimeMetadata == nil {
		od.resolveRuntimeMetadata = providers.ResolveRuntimeMetadata
	}

	routerOpts := routerFlagOptions{
		provider:          od.options.routerProvider,
		model:             od.options.model,
		allowProviders:    od.options.allowProviders,
		denyModels:        od.options.denyModels,
		requireKnownPrice: od.options.requireKnownPrice,
	}
	if od.options.maxInputCost != 0 {
		v := od.options.maxInputCost
		routerOpts.maxInputCost = &v
	}
	if od.options.maxOutputCost != 0 {
		v := od.options.maxOutputCost
		routerOpts.maxOutputCost = &v
	}

	candidates, profileByCandidate := buildExecutableCandidates(od.resolved, &od.modelRegistry, od.resolveRuntimeMetadata)

	// Build a repository survey once and share it across all read-only tasks.
	// This gives each read-only task a comprehensive starting index so the agent
	// doesn't waste tool calls discovering the repository structure.
	if od.workspaceRoot != "" {
		s, _ := orchestration.GetSurvey(od.workspaceRoot, orchestration.SurveyOptions{})
		od.survey = s
		if od.metrics != nil && s != nil {
			od.metrics.SurveyBuildMs = s.BuildMs
			od.metrics.SurveyCacheHits = s.CacheHits
		}
	}

	var planStart time.Time
	if od.metrics != nil {
		planStart = orchestratedNow()
	}
	preview, err := orchestratedBuildPlan(od.prompt, routerOpts, detectRepositoryPresence(od.deps), candidates)
	if od.metrics != nil {
		od.metrics.PlanningMs = nonNegMs(orchestratedNow().Sub(planStart).Milliseconds())
	}
	if err != nil {
		return writeAppError(od.stderr, "orchestration failed: "+err.Error(), exitCrash)
	}
	plan := preview.Plan

	sched, err := scheduler.NewScheduler(plan)
	if err != nil {
		return writeAppError(od.stderr, err.Error(), exitCrash)
	}
	state := sched.State()

	if _, ready := sched.NextReady(); !ready {
		return renderOrchestratedNoTask(od, mode, preview, state)
	}

	// A single orchestrated session carries the whole run's events.
	prepared, serr := sessions.PrepareExec(sessions.PrepareExecOptions{
		Title:     od.sessionTitle,
		Cwd:       od.workspaceRoot,
		ModelID:   "",
		Provider:  "",
		Tag:       od.options.tag,
		Depth:     od.options.depth,
		AgentName: specialistAgentName(od.options.sessionTitle),
		Store:     od.deps.newSessionStore(),
	})
	if serr != nil {
		return writeExecFormatUsageError(od.stdout, od.stderr, od.options.outputFormat, serr.Error())
	}
	sessionID := prepared.Session.SessionID
	store := prepared.Store
	store.AppendEvent(sessionID, sessions.AppendEventInput{
		Type: EventOrchestratedPlan,
		Payload: map[string]any{
			"planId":    plan.PlanID,
			"summary":   plan.Summary,
			"taskCount": len(plan.Tasks),
			"mode":      mode,
		},
	})

	selfCorrector, fileDiagnostics, lspShutdown := newExecSelfCorrector(od.options.selfCorrect, od.workspaceRoot, od.options.autonomy)
	defer lspShutdown()
	fileTracker := tools.NewFileTracker()
	hookDispatcher, _ := newHookDispatcherWithExtra(od.workspaceRoot, od.pluginActivation.hooks, od.trustRoot)

	executed := make([]orchestratedTaskExec, 0, len(plan.Tasks))
	skipped := make([]orchestratedSkippedTask, 0)
	tasksRun := 0

	parallelBatchCounter := 0
	for opts.MaxTasks == 0 || tasksRun < opts.MaxTasks {
		st := sched.State()
		if len(st.ReadyQueue) == 0 {
			break
		}

		// Independent, read-only, ready tasks can run concurrently in a bounded
		// batch. When none qualify, fall back to the deterministic single-task
		// sequential path so mutating tasks, tasks with dependencies, and approval
		// gates keep their strict ordering guarantees.
		batch := orchestratedCollectParallelBatch(od, sched, preview)
		if len(batch) == 0 {
			task, ok := sched.NextReady()
			if !ok {
				break
			}

			toolAllowlist := orchestratedToolAllowlist(task, od.registry, od.options.enabledTools, od.options.disabledTools)
			if orchestratedTaskRequiresApproval(task, od.permissionMode) {
				store.AppendEvent(sessionID, sessions.AppendEventInput{
					Type:    EventOrchestratedTaskSelected,
					Payload: map[string]any{"taskId": task.ID, "title": task.Title, "taskKind": string(task.TaskKind)},
				})
				store.AppendEvent(sessionID, sessions.AppendEventInput{
					Type:    EventOrchestratedTaskFailed,
					Payload: map[string]any{"taskId": task.ID, "status": "blocked", "error": "requires explicit approval"},
				})
				return renderOrchestratedBlocked(od, mode, preview, task,
					"task requires explicit approval (dangerous); re-run with --skip-permissions-unsafe or approve interactively")
			}
			if msg, missing := orchestratedMissingCapabilityError(task, toolAllowlist, od.registry); missing {
				return writeAppError(od.stderr, "orchestration failed: "+msg, exitIncomplete)
			}
			if od.options.debugOrchestratedTools {
				return renderOrchestratedDebugTools(od, task, toolAllowlist)
			}

			var routeStart time.Time
			if od.metrics != nil {
				routeStart = orchestratedNow()
			}
			providerKind, modelID, taskProvider, decision, terminate, code := orchestratedResolveRouting(od, preview, profileByCandidate, task, routerOpts, mode)
			if od.metrics != nil {
				od.metrics.RoutingMs += nonNegMs(orchestratedNow().Sub(routeStart).Milliseconds())
			}
			if terminate {
				return code
			}

			store.AppendEvent(sessionID, sessions.AppendEventInput{
				Type:    EventOrchestratedTaskSelected,
				Payload: map[string]any{"taskId": task.ID, "title": task.Title, "taskKind": string(task.TaskKind)},
			})
			store.AppendEvent(sessionID, sessions.AppendEventInput{
				Type:    EventOrchestratedModelRouted,
				Payload: map[string]any{"provider": providerKind, "model": modelID},
			})

			selectedAt := time.Time{}
			if od.metrics != nil {
				selectedAt = orchestratedNow()
			}
			baseline, _ := gitStatusPaths(od.workspaceRoot)
			result, changes, verification, status, perf := executeOrchestratedTask(ctx, od, sessionID, store, taskProvider, providerKind, modelID, task, decision, preview, baseline, selfCorrector, fileDiagnostics, fileTracker, hookDispatcher, nil, 0, selectedAt)

			orchestratedApplyStatus(sched, task.ID, status)
			executed = append(executed, orchestratedTaskExec{task: task, decision: decision, result: result, changes: changes, verification: verification, status: status})
			if od.metrics != nil {
				orchestratedAppendTaskMetric(od.metrics, task, providerKind, modelID, status, result, perf, 0)
			}
			tasksRun++

			if status == executor.StatusBlocked {
				markOrchestratedDependentsSkipped(sched, plan, task.ID, "skipped_due_to_dependency", &skipped)
				if opts.StopOnBlocked {
					break
				}
			} else if status == executor.StatusFailed || status == executor.StatusIncomplete {
				markOrchestratedDependentsSkipped(sched, plan, task.ID, "skipped_due_to_dependency", &skipped)
				if opts.StopOnFailure {
					break
				}
			}
		} else {
			ran, stop, errCode := runOrchestratedParallelBatch(ctx, od, sessionID, store, sched, batch, preview, profileByCandidate, routerOpts, selfCorrector, fileDiagnostics, &executed, &skipped, plan, opts, parallelBatchCounter+1)
			parallelBatchCounter++
			tasksRun += ran
			if errCode >= 0 {
				return errCode
			}
			if stop {
				break
			}
		}
	}

	finalState := sched.State()
	finalizeOrchestratedMetrics(od, sessionID, store)
	if opts.MaxTasks == 1 && len(executed) == 1 && len(skipped) == 0 {
		e := executed[0]
		if od.options.outputFormat == execOutputJSON {
			return renderOrchestratedJSON(od, preview, e.task, e.decision, e.result, e.changes, e.verification, e.status, finalState)
		}
		return renderOrchestratedText(od, preview, e.task, e.decision, e.result, e.changes, e.verification, e.status, finalState)
	}
	if od.options.outputFormat == execOutputJSON {
		return renderOrchestratedSummaryJSON(od, mode, preview, executed, skipped, finalState)
	}
	return renderOrchestratedSummary(od, mode, preview, executed, skipped, finalState)
}

// runOrchestratedOnce runs exactly ONE planned task end-to-end through the shared
// sequential orchestrated engine, then stops. It is the first real orchestrated
// execution path and is deliberately distinct from --orchestration-preview
// (offline) and the full --orchestrated DAG run.
func runOrchestratedOnce(od orchestratedOnceDeps) int {
	return runOrchestrated(od, orchestratedExecutionOptions{MaxTasks: 1, StopOnFailure: true, StopOnBlocked: true})
}

// orchestratedTaskDecision returns the routing decision recorded for a task.
func orchestratedTaskDecision(preview planPreviewResult, taskID string) modelrouter.Decision {
	for _, r := range preview.Results {
		if r.Task.ID == taskID {
			return r.Decision
		}
	}
	return modelrouter.Decision{}
}

// orchestratedResolveRouting resolves the provider/model/routing decision for a
// single task and constructs its provider. It mirrors the routing logic that the
// sequential path performed inline, but returns a (terminate, code) signal so the
// caller can abort the whole run (e.g. an incompatible explicit model) instead of
// writing to stdout and returning directly. When terminate is true, code is the
// exit code the run must return.
func orchestratedResolveRouting(od orchestratedOnceDeps, preview planPreviewResult, profileByCandidate map[string]config.ProviderProfile, task planner.Task, routerOpts routerFlagOptions, mode string) (providerKind, modelID string, taskProvider agent.Provider, decision modelrouter.Decision, terminate bool, code int) {
	decision = orchestratedTaskDecision(preview, task.ID)

	explicitModel := strings.TrimSpace(routerOpts.model)
	if explicitModel != "" && !modelEntryMatchesIDOrAliasPtr(decision.Selected, explicitModel) {
		providerName := strings.TrimSpace(routerOpts.provider)
		if providerName == "" {
			providerName = od.resolved.ActiveProvider
		}
		if providerName == "" {
			providerName = "any"
		}
		if profile, ok := orchestratedConfiguredProfile(profileByCandidate, explicitModel); ok {
			if rej := findRejection(decision.Rejected, explicitModel); rej != nil {
				detail := strings.Join(rejectionDetails(rej.Reasons), "; ")
				provider := profileDisplayName(profile)
				return "", "", nil, decision, true, renderModelSelectionError(od, mode, explicitModel, provider, "incompatible",
					fmt.Sprintf("requested model %q is configured through provider %q but is incompatible with this task: %s", explicitModel, provider, detail))
			}
		}
		return "", "", nil, decision, true, renderModelSelectionError(od, mode, explicitModel, providerName, "unavailable",
			fmt.Sprintf("requested model %q is not available through provider %q", explicitModel, providerName))
	}

	if decision.Selected != nil {
		providerKind = string(decision.Selected.Model.Provider)
		modelID = decision.Selected.Model.ID
	}
	if modelID == "" {
		return "", "", nil, decision, true, renderOrchestratedNoModel(od, mode, profileByCandidate, preview, task, decision)
	}

	routedProfile := od.resolved.Provider
	if p, ok := profileByCandidate[strings.ToLower(modelID)]; ok {
		routedProfile = p
	} else {
		routedProfile.Provider = providerKind
		routedProfile.Model = modelID
	}
	taskProvider, perr := od.deps.newProvider(routedProfile)
	if perr != nil || taskProvider == nil {
		return "", "", nil, decision, true, renderOrchestratedProviderError(od, mode, preview, task, providerKind, modelID, errToText(perr))
	}
	return providerKind, modelID, taskProvider, decision, false, 0
}

// orchestratedCollectParallelBatch returns the subset of currently-ready tasks that
// may run concurrently in a bounded read-only batch. Eligibility requires: the
// parallel flag is enabled, the task is marked CanRunParallel and Ready, the task
// is read-only (no repository verification needed), its safety is strictly safe, it
// has no dependencies, and its effective tool allowlist contains no mutating
// (write/edit or shell) tools. Any task failing these stays on the sequential
// path. Returns nil when parallel execution is disabled or nothing qualifies, which
// signals the caller to fall back to single-task sequential execution.
func orchestratedCollectParallelBatch(od orchestratedOnceDeps, sched *scheduler.Scheduler, preview planPreviewResult) []planner.Task {
	if !od.parallel.Enabled {
		return nil
	}
	if od.options.debugOrchestratedTools {
		return nil
	}
	var out []planner.Task
	for _, t := range sched.ReadyParallel() {
		// Read-only tasks never mutate the repository, so verification is
		// not_applicable and they are safe to run concurrently.
		if executor.TaskRequiresRepositoryVerification(t) {
			continue
		}
		// Only strictly safe, dependency-free tasks run in parallel. A task
		// that needs approval, has dependencies, or whose effective tool
		// allowlist contains a mutating tool must stay sequential.
		if t.SafetyLevel != planner.SafetySafe {
			continue
		}
		if len(t.Dependencies) > 0 {
			continue
		}
		allowlist := orchestratedToolAllowlist(t, od.registry, od.options.enabledTools, od.options.disabledTools)
		if orchestratedAllowlistHasMutating(od.registry, allowlist) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// orchestratedAllowlistHasMutating reports whether an effective tool allowlist
// contains any mutating (write/edit or shell) tool, using the registry's
// authoritative side-effect classification.
func orchestratedAllowlistHasMutating(reg *tools.Registry, allowlist []string) bool {
	for _, name := range allowlist {
		switch capabilityForTool(reg, name) {
		case capEdit, capShell:
			return true
		}
	}
	return false
}

type parallelTaskResult struct {
	task         planner.Task
	decision     modelrouter.Decision
	result       executor.TaskExecutionResult
	changes      executor.RepoChanges
	verification executor.VerificationOutcome
	status       executor.CompletionStatus
	perf         orchestratedTaskPerf
}

// runOrchestratedParallelBatch executes a set of independent, read-only, ready
// tasks concurrently under a bounded worker semaphore, then serializes the
// scheduler transitions, event appends, and dependent-skip bookkeeping. The
// scheduler is never touched from a worker goroutine; only prepare (coordinator,
// serial) and the per-task event appends (guarded by eventMu) touch shared
// state. errCode is -1 to continue the run; a non-negative value means the
// whole run must terminate with that exit code (a routing/provider failure for a
// task in the batch). stop reports whether the caller's loop should break after
// this batch (a task failed/blocked/incomplete and the policy stops on it).
func runOrchestratedParallelBatch(
	ctx context.Context,
	od orchestratedOnceDeps,
	sessionID string,
	store *sessions.Store,
	sched *scheduler.Scheduler,
	batch []planner.Task,
	preview planPreviewResult,
	profileByCandidate map[string]config.ProviderProfile,
	routerOpts routerFlagOptions,
	selfCorrector *agent.SelfCorrector,
	fileDiagnostics func(ctx context.Context, absPath string) string,
	executed *[]orchestratedTaskExec,
	skipped *[]orchestratedSkippedTask,
	plan planner.ExecutionPlan,
	opts orchestratedExecutionOptions,
	batchNum int,
) (ran int, stop bool, errCode int) {
	maxWorkers := od.parallel.MaxWorkers
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	// Prepare every job in the coordinator (serial, deterministic) so a routing or
	// provider failure for any task terminates the whole run before any task
	// starts, and so the scheduler is never touched from a worker goroutine.
	type job struct {
		task           planner.Task
		decision       modelrouter.Decision
		providerKind   string
		modelID        string
		taskProvider   agent.Provider
		baseline       map[string]bool
		fileTracker    *tools.FileTracker
		hookDispatcher *hooks.Dispatcher
	}
	jobs := make([]job, 0, len(batch))
	ids := make([]string, 0, len(batch))
	for _, task := range batch {
		var routeStart time.Time
		if od.metrics != nil {
			routeStart = orchestratedNow()
		}
		providerKind, modelID, taskProvider, decision, terminate, code := orchestratedResolveRouting(od, preview, profileByCandidate, task, routerOpts, "orchestrated")
		if od.metrics != nil {
			od.metrics.RoutingMs += nonNegMs(orchestratedNow().Sub(routeStart).Milliseconds())
		}
		if terminate {
			return 0, false, code
		}
		baseline, _ := gitStatusPaths(od.workspaceRoot)
		ft := tools.NewFileTracker()
		hd, _ := newHookDispatcherWithExtra(od.workspaceRoot, od.pluginActivation.hooks, od.trustRoot)
		jobs = append(jobs, job{
			task:           task,
			decision:       decision,
			providerKind:   providerKind,
			modelID:        modelID,
			taskProvider:   taskProvider,
			baseline:       baseline,
			fileTracker:    ft,
			hookDispatcher: hd,
		})
		ids = append(ids, task.ID)
	}

	store.AppendEvent(sessionID, sessions.AppendEventInput{
		Type: EventOrchestratedParallelBatchStarted,
		Payload: map[string]any{
			"batch":   batchNum,
			"workers": maxWorkers,
			"taskIds": ids,
		},
	})

	// batchStart is also the selectedAt for every worker in this batch, so the
	// per-task queue wait measures coordinator-prep to worker-start latency.
	var batchStart time.Time
	if od.metrics != nil {
		batchStart = orchestratedNow()
	}

	var eventMu sync.Mutex
	sem := make(chan struct{}, maxWorkers)
	results := make([]parallelTaskResult, len(jobs))
	var wg sync.WaitGroup
	var batchActive, batchPeak int32
	for i := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			// Batch-local peak tracking (independent of the run-level counter).
			if od.metrics != nil {
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
				defer atomic.AddInt32(&batchActive, -1)
			}
			j := jobs[idx]
			res, ch, ver, st, perf := executeOrchestratedTask(ctx, od, sessionID, store, j.taskProvider, j.providerKind, j.modelID, j.task, j.decision, preview, j.baseline, selfCorrector, fileDiagnostics, j.fileTracker, j.hookDispatcher, &eventMu, batchNum, batchStart)
			results[idx] = parallelTaskResult{task: j.task, decision: j.decision, result: res, changes: ch, verification: ver, status: st, perf: perf}
		}(i)
	}
	wg.Wait()

	var batchWall int64
	if od.metrics != nil {
		batchWall = nonNegMs(orchestratedNow().Sub(batchStart).Milliseconds())
		od.metrics.Batches = append(od.metrics.Batches, orchestratedBatchMetric{
			Batch:       batchNum,
			Workers:     maxWorkers,
			TaskCount:   len(jobs),
			WallMs:      batchWall,
			PeakWorkers: int(atomic.LoadInt32(&batchPeak)),
		})
	}

	store.AppendEvent(sessionID, sessions.AppendEventInput{
		Type: EventOrchestratedParallelBatchCompleted,
		Payload: map[string]any{
			"batch":   batchNum,
			"workers": maxWorkers,
			"taskIds": ids,
		},
	})

	stop = false
	for i := range results {
		r := results[i]
		orchestratedApplyStatus(sched, r.task.ID, r.status)
		*executed = append(*executed, orchestratedTaskExec{
			task:          r.task,
			decision:      r.decision,
			result:        r.result,
			changes:       r.changes,
			verification:  r.verification,
			status:        r.status,
			ParallelBatch: batchNum,
		})
		if od.metrics != nil {
			providerKind := ""
			modelID := ""
			if r.decision.Selected != nil {
				providerKind = string(r.decision.Selected.Model.Provider)
				modelID = r.decision.Selected.Model.ID
			}
			orchestratedAppendTaskMetric(od.metrics, r.task, providerKind, modelID, r.status, r.result, r.perf, batchNum)
		}
		switch r.status {
		case executor.StatusBlocked:
			markOrchestratedDependentsSkipped(sched, plan, r.task.ID, "skipped_due_to_dependency", skipped)
			stop = stop || opts.StopOnBlocked
		case executor.StatusFailed, executor.StatusIncomplete:
			markOrchestratedDependentsSkipped(sched, plan, r.task.ID, "skipped_due_to_dependency", skipped)
			stop = stop || opts.StopOnFailure
		}
	}
	return len(results), stop, -1
}

// markOrchestratedDependentsSkipped marks every task that directly depends on
// doneID as skipped (reason) so the run report is explicit about why
// downstream work did not run.
func markOrchestratedDependentsSkipped(sched *scheduler.Scheduler, plan planner.ExecutionPlan, doneID, reason string, skipped *[]orchestratedSkippedTask) {
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

// executeOrchestratedTask runs a single planned task through the agent runtime,
// computes the baseline-aware repo delta and verification, evaluates the
// deterministic completion gate, and records the task/verification events. It is
// the shared per-task engine reused by both --orchestrated-once and
// --orchestrated. batchNum is the 1-based parallel-batch index (0 for a
// sequential or once task); selectedAt is when the task was chosen (for queue
// wait). It additionally returns the raw timing/usage sample (perf) when metrics
// are enabled; perf is the zero value otherwise and never affects the result.
func executeOrchestratedTask(
	ctx context.Context,
	od orchestratedOnceDeps,
	sessionID string,
	store *sessions.Store,
	taskProvider agent.Provider,
	providerKind, modelID string,
	task planner.Task,
	decision modelrouter.Decision,
	preview planPreviewResult,
	baseline map[string]bool,
	selfCorrector *agent.SelfCorrector,
	fileDiagnostics func(ctx context.Context, absPath string) string,
	fileTracker *tools.FileTracker,
	hookDispatcher *hooks.Dispatcher,
	eventMu *sync.Mutex,
	batchNum int,
	selectedAt time.Time,
) (executor.TaskExecutionResult, executor.RepoChanges, executor.VerificationOutcome, executor.CompletionStatus, orchestratedTaskPerf) {
	perf := orchestratedTaskPerf{SelectedAt: selectedAt}
	if od.metrics != nil {
		perf.StartedAt = orchestratedNow()
		od.metrics.enterWorker()
	}

	taskPrompt := buildOrchestratedTaskPrompt(od.prompt, preview.Plan, task, scheduler.ExecutionState{}, orchestratedCapabilityNote(orchestratedToolAllowlist(task, od.registry, od.options.enabledTools, od.options.disabledTools), od.registry))
	// Inject the repository survey for read-only tasks.
	if od.survey != nil && !executor.TaskRequiresRepositoryVerification(task) {
		view := "all"
		switch task.TaskKind {
		case planner.KindRepositorySearch:
			if strings.Contains(strings.ToLower(task.Title), "doc") {
				view = "docs"
			} else if strings.Contains(strings.ToLower(task.Title), "source") || strings.Contains(strings.ToLower(task.Title), "code") {
				view = "source"
			}
		case planner.KindCodeReview, planner.KindSecurityReview:
			view = "source"
		case planner.KindDocumentation:
			view = "docs"
		case planner.KindArchitecture:
			view = "all"
		default:
			view = ""
		}
		if view != "" {
			surveyCtx := orchestration.RenderSurveyForTask(od.survey, view)
			if surveyCtx != "" {
				taskPrompt = taskPrompt + "\n\n" + surveyCtx
			}
		}
	}

	baseOpts := buildOrchestratedAgentOptions(od, sessionID, modelID, task, selfCorrector, fileDiagnostics, fileTracker, hookDispatcher)
	runner := od.runner
	if runner == nil {
		provider := taskProvider
		if od.metrics != nil {
			// Point the wrapper's counter straight at the perf sample's
			// ProviderCalls field so every StreamCompletion increments it in
			// place during the run. (A deferred copy would be captured by the
			// return-value copy before the deferred write, losing the count.)
			provider = &metricsProvider{inner: taskProvider, calls: &perf.ProviderCalls}
			// Mid-run model escalation (agent.Run swapping to a provider from
			// ModelSwitcher) would otherwise bypass the wrapper. Re-wrap the
			// escalation provider so every completion — including escalated ones
			// — is counted into the same task total.
			if od.modelSwitcher != nil {
				ms := od.modelSwitcher
				baseOpts.ModelSwitcher = func(ctx context.Context, model string) (agent.Provider, error) {
					p, err := ms(ctx, model)
					if err != nil {
						return nil, err
					}
					return &metricsProvider{inner: p, calls: &perf.ProviderCalls}, nil
				}
			}
		}
		runner = executor.NewAgentRunner(provider, baseOpts)
		runner.(*executor.AgentRunner).Live = od.stderr
	}

	appendOrchestratedEvent(store, sessionID, eventMu, EventOrchestratedTaskStarted, map[string]any{"taskId": task.ID})

	result, runErr := runner.RunTask(ctx, executor.TaskExecutionRequest{
		Task:          task,
		Prompt:        taskPrompt,
		ModelID:       modelID,
		ProviderID:    providerKind,
		WorkspaceRoot: od.workspaceRoot,
		SessionID:     sessionID,
	})

	changes := orchestratedRepoDelta(od.workspaceRoot, baseline)
	var verification executor.VerificationOutcome
	if executor.TaskRequiresRepositoryVerification(task) {
		var verifStart time.Time
		if od.metrics != nil {
			verifStart = orchestratedNow()
		}
		verifier := od.verifier
		if verifier == nil {
			verifier = orchestratedVerifier(od.deps)
		}
		verification = verifier(ctx, od.workspaceRoot, changes.All())
		perf.Verified = true
		if od.metrics != nil {
			perf.VerificationMs = nonNegMs(orchestratedNow().Sub(verifStart).Milliseconds())
		}
	} else {
		// Read-only task: no mutation-oriented repository verification is run.
		// The outcome is advisory-only and must not fail the task.
		verification = executor.VerificationOutcome{Status: "not_applicable", Reason: "read-only task"}
	}

	var status executor.CompletionStatus
	if runErr != nil {
		status = executor.MapAgentError(result)
	} else {
		status = executor.EvaluateCompletion(task, result, changes, verification, orchestratedCompletionPolicy(od))
	}

	if status == executor.StatusFailed || status == executor.StatusIncomplete || status == executor.StatusBlocked {
		appendOrchestratedEvent(store, sessionID, eventMu, EventOrchestratedTaskFailed, map[string]any{
			"taskId": task.ID,
			"status": string(status),
			"error":  errToText(runErr),
		})
	} else {
		appendOrchestratedEvent(store, sessionID, eventMu, EventOrchestratedTaskCompleted, map[string]any{
			"taskId":       task.ID,
			"status":       string(status),
			"filesChanged": changes.All(),
		})
	}
	appendOrchestratedEvent(store, sessionID, eventMu, EventOrchestratedVerification, map[string]any{
		"status": verification.Status,
		"passed": verification.Passed,
		"failed": verification.Failed,
		"errors": verification.Errors,
	})

	if od.metrics != nil {
		perf.FinishedAt = orchestratedNow()
		od.metrics.leaveWorker()
	}
	return result, changes, verification, status, perf
}

// appendOrchestratedEvent guards a session event append with an optional mutex so
// concurrent read-only task workers can record events without racing on the shared
// session store. When eventMu is nil (the sequential path) the append is
// uncontended and runs bare.
func appendOrchestratedEvent(store *sessions.Store, sessionID string, eventMu *sync.Mutex, typ sessions.EventType, payload map[string]any) {
	if eventMu != nil {
		eventMu.Lock()
		defer eventMu.Unlock()
	}
	store.AppendEvent(sessionID, sessions.AppendEventInput{Type: typ, Payload: payload})
}
func orchestratedApplyStatus(sched *scheduler.Scheduler, taskID string, status executor.CompletionStatus) {
	switch status {
	case executor.StatusCompleted, executor.StatusCompletedNoChange, executor.StatusCompletedUnverified:
		_ = sched.MarkCompleted(taskID)
	case executor.StatusFailed, executor.StatusIncomplete:
		_ = sched.MarkFailed(taskID)
	case executor.StatusBlocked:
		_ = sched.MarkSkipped(taskID)
	}
}

func errToText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// orchestratedAppendTaskMetric records one finished task's metrics into the run
// accumulator (coordinator-only, so the slice append needs no lock) and rolls
// the token totals into the run counters.
func orchestratedAppendTaskMetric(m *orchestratedRunMetrics, task planner.Task, providerKind, modelID string, status executor.CompletionStatus, result executor.TaskExecutionResult, perf orchestratedTaskPerf, batch int) {
	wall := int64(0)
	if !perf.StartedAt.IsZero() && !perf.FinishedAt.IsZero() {
		wall = nonNegMs(perf.FinishedAt.Sub(perf.StartedAt).Milliseconds())
	}
	queue := int64(0)
	if !perf.SelectedAt.IsZero() && !perf.StartedAt.IsZero() {
		queue = nonNegMs(perf.StartedAt.Sub(perf.SelectedAt).Milliseconds())
	}
	mt := orchestratedTaskMetric{
		TaskID:         task.ID,
		Title:          task.Title,
		Batch:          batch,
		ProviderKind:   providerKind,
		Model:          modelID,
		ProviderCalls:  int(perf.ProviderCalls),
		Status:         string(status),
		WallMs:         wall,
		QueueWaitMs:    queue,
		VerificationMs: nonNegMs(perf.VerificationMs),
		Verified:       perf.Verified,
		Tokens:         newOrchestratedTokenMetrics(result.Usage, result.UsageReported),
		Tools: orchestratedToolMetrics{
			Attempted: result.ToolUsage.Attempted,
			Executed:  result.ToolUsage.Executed,
			Succeeded: result.ToolUsage.Succeeded,
			Failed:    result.ToolUsage.Failed,
			Denied:    result.ToolUsage.Denied,
		},
	}
	m.Tasks = append(m.Tasks, mt)
	m.TotalProviderCalls += int(perf.ProviderCalls)
	m.TotalInputTokens += mt.Tokens.InputTokens
	m.TotalOutputTokens += mt.Tokens.OutputTokens
}

// buildOrchestratedTaskPrompt builds a bounded, task-specific prompt that scopes
// the agent to exactly one planned task. capabilityNote describes the exact tool
// boundary the task may operate within, so the model is never told to use a tool
// it cannot call.
func buildOrchestratedTaskPrompt(request string, plan planner.ExecutionPlan, task planner.Task, state scheduler.ExecutionState, capabilityNote string) string {
	var b strings.Builder
	b.WriteString("Original request: ")
	b.WriteString(request)
	b.WriteString("\n\nYou are executing EXACTLY ONE task from a larger plan. Complete only this task and then stop. Do not start, continue, or anticipate any other planned task.\n\n")
	b.WriteString("Task ID: ")
	b.WriteString(task.ID)
	b.WriteString("\n")
	b.WriteString("Title: ")
	b.WriteString(task.Title)
	b.WriteString("\n")
	b.WriteString("Kind: ")
	b.WriteString(string(task.TaskKind))
	b.WriteString("\n")
	b.WriteString("Description: ")
	b.WriteString(task.Description)
	b.WriteString("\n")
	b.WriteString("Safety level: ")
	b.WriteString(string(task.SafetyLevel))
	b.WriteString("\n")
	if capabilityNote != "" {
		b.WriteString("Tool capability boundary: ")
		b.WriteString(capabilityNote)
		b.WriteString("\n")
	}
	if len(task.Dependencies) > 0 {
		b.WriteString("Dependencies (must already be satisfied): ")
		b.WriteString(strings.Join(task.Dependencies, ", "))
		b.WriteString("\n")
	}
	b.WriteString("\nOther planned tasks (DO NOT work on these now):\n")
	for _, t := range plan.Tasks {
		if t.ID == task.ID {
			continue
		}
		b.WriteString("- ")
		b.WriteString(t.ID)
		b.WriteString(": ")
		b.WriteString(t.Title)
		b.WriteString("\n")
	}
	b.WriteString("\nAcceptance conditions: finish only this task. When done, report what you changed and any verification you ran. ")
	b.WriteString("If the requested feature already exists, say so with concrete evidence (file paths, commands) instead of inventing work.\n")
	return b.String()
}

// buildOrchestratedAgentOptions builds the agent.Options for the single task,
// reusing the resolved config, sandbox, permission mode, and (optionally)
// self-correct/file-diagnostics/hooks wiring from the normal exec path. The task
// drives a task-specific tool allowlist layered on top of the operator's
// --enabled-tools / --disabled-tools flags, reusing the same registry the normal
// exec path built.
func buildOrchestratedAgentOptions(
	od orchestratedOnceDeps,
	sessionID string,
	modelID string,
	task planner.Task,
	selfCorrector *agent.SelfCorrector,
	fileDiagnostics func(ctx context.Context, absPath string) string,
	fileTracker *tools.FileTracker,
	hookDispatcher *hooks.Dispatcher,
) agent.Options {
	// Reuse the single normal-exec tool registry; only the effective allowlist
	// differs per task. Never build a second registry.
	toolAllowlist := orchestratedToolAllowlist(task, od.registry, od.options.enabledTools, od.options.disabledTools)
	opts := agent.Options{
		MaxTurns:                od.resolved.MaxTurns,
		ContextWindow:           resolveAgentContextWindow(context.Background(), od.modelRegistry, od.resolved.Provider),
		DeferThreshold:          od.effectiveDeferThreshold,
		SessionID:               sessionID,
		CallingSessionID:        od.options.callingSessionID,
		CallingToolUseID:        od.options.callingToolUseID,
		Tag:                     od.options.tag,
		Depth:                   od.options.depth,
		SessionTitle:            od.sessionTitle,
		ProviderName:            od.resolved.Provider.Name,
		Model:                   modelID,
		ModelSwitcher:           od.modelSwitcher,
		ReasoningEffort:         od.forwardEffort,
		Cwd:                     od.workspaceRoot,
		Images:                  od.images,
		Registry:                od.registry,
		PermissionMode:          od.permissionMode,
		ToolExposure:            orchestratedExposurePolicy(task),
		Autonomy:                od.options.autonomy,
		SelfCorrect:             selfCorrector,
		FileDiagnostics:         fileDiagnostics,
		RequireCompletionSignal: !od.options.noCompletionGate,
		// Headless orchestrated runs have no interactive approver, so a
		// prompt-required tool must NOT run via sandbox auto-allow — it returns a
		// structured permission-required denial and the task is blocked.
		RequireApproverForPromptTools: true,
		Sandbox:                       od.sandboxEngine,
		FileTracker:                   fileTracker,
		Hooks:                         hookDispatcher,
		EnabledTools:                  toolAllowlist,
		DisabledTools:                 od.options.disabledTools,
	}
	if od.specialistRuntime != nil {
		opts.Specialists = od.specialistRuntime.specialistInfos()
	}
	opts.Skills = od.pluginActivation.skillInfos(od.deps.skillsDir())
	return opts
}

// gitStatusPaths returns the set of paths git considers modified/untracked for
// root, plus whether root is a git repository. It never resets, cleans, or
// commits.
func gitStatusPaths(root string) (map[string]bool, bool) {
	cmd := exec.Command("git", "-C", root, "status", "--porcelain", "-uall")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	paths := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "XY path" (status prefix is 2 chars; a space follows).
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if idx := strings.Index(p, " -> "); idx >= 0 {
			p = p[:idx]
		}
		if p != "" {
			paths[p] = true
		}
	}
	return paths, true
}

// orchestratedRepoDelta computes the repository change set introduced by the
// task, relative to the baseline captured before it ran. Paths present in the
// baseline (pre-existing local changes) are excluded so they are never counted
// as task-generated.
func orchestratedRepoDelta(root string, baseline map[string]bool) executor.RepoChanges {
	current, isGit := gitStatusPaths(root)
	changes := executor.RepoChanges{HasGit: isGit, BaselineDirty: len(baseline) > 0}
	for p := range current {
		if !baseline[p] {
			changes.ChangedFiles = append(changes.ChangedFiles, p)
		}
	}
	for p := range baseline {
		if !current[p] {
			changes.DeletedFiles = append(changes.DeletedFiles, p)
		}
	}
	sort.Strings(changes.ChangedFiles)
	sort.Strings(changes.DeletedFiles)
	return changes
}

// orchestratedVerifier returns an executor.Verifier backed by the workspace
// verification plan (internal/verify). It never runs destructive commands and
// reports "not_available" rather than a false pass when no plan exists.
func orchestratedVerifier(deps appDeps) executor.Verifier {
	return func(ctx context.Context, root string, changed []string) executor.VerificationOutcome {
		plan, err := deps.detectVerifyPlan(root)
		if err != nil {
			return executor.VerificationOutcome{Status: "not_available", Reason: err.Error()}
		}
		if len(plan.Checks) == 0 {
			return executor.VerificationOutcome{Status: "not_available"}
		}
		report := deps.runVerify(ctx, plan, verify.RunOptions{})
		out := executor.VerificationOutcome{
			Total:  report.Summary.Total,
			Passed: report.Summary.Passed,
			Failed: report.Summary.Failed,
			Errors: report.Summary.Errors,
		}
		switch {
		case report.Summary.Total == 0:
			out.Status = "not_run"
		case report.OK:
			out.Status = "passed"
		default:
			out.Status = "failed"
		}
		for _, r := range report.Results {
			out.Checks = append(out.Checks, executor.VerificationCheck{
				ID:       r.ID,
				Name:     r.Name,
				Command:  r.Command,
				Status:   string(r.Status),
				ExitCode: r.ExitCode,
			})
		}
		return out
	}
}

// --- Rendering helpers ---

func orchestratedRoutingLines(decision modelrouter.Decision) (string, []string) {
	provider := ""
	model := ""
	if decision.Selected != nil {
		provider = string(decision.Selected.Model.Provider)
		model = decision.Selected.Model.ID
	}
	rejected := make([]string, 0, len(decision.Rejected))
	for _, r := range decision.Rejected {
		rejected = append(rejected, r.ModelID)
	}
	return fmt.Sprintf("provider: %s\n  model: %s", provider, model), rejected
}

// orchestratedBanner returns the mode-specific report banner. Once mode keeps
// its historic single-task banner verbatim; full sequential mode uses the
// sequential DAG banner, or the combined banner when bounded read-only parallel
// batches are enabled.
func orchestratedBanner(mode string, parallel bool) string {
	if mode == "orchestrated-once" {
		return "ORCHESTRATED EXECUTION — one task only"
	}
	if parallel {
		return "ORCHESTRATED EXECUTION — sequential DAG + read-only parallel batches"
	}
	return "ORCHESTRATED EXECUTION — sequential DAG"
}

// orchestratedFooter returns the mode-specific run-termination line.
func orchestratedFooter(mode string) string {
	if mode == "orchestrated-once" {
		return "Stopped after one task by --orchestrated-once.\n"
	}
	return "Stopped by --orchestrated.\n"
}

// orchestratedConfiguredProfile reports whether the requested model matches a
// configured (executable) candidate and returns the profile that produced it.
// This is the "model exists / is executable" check, distinct from capability
// compatibility.
func orchestratedConfiguredProfile(byID map[string]config.ProviderProfile, model string) (config.ProviderProfile, bool) {
	p, ok := byID[strings.ToLower(strings.TrimSpace(model))]
	return p, ok
}

// profileDisplayName returns the user-facing name for a configured provider
// profile. The profile name (e.g. "xai") is authoritative and is what we print
// in errors. Only when it is absent do we fall back to the provider kind (e.g.
// "openai-compatible") so error output is never empty. This keeps the three
// concepts distinct: profile name (xai), provider kind (openai-compatible), and
// model id (grok-4.5).
func profileDisplayName(p config.ProviderProfile) string {
	if name := strings.TrimSpace(p.Name); name != "" {
		return name
	}
	if kind := strings.TrimSpace(string(p.ProviderKind)); kind != "" {
		return kind
	}
	return strings.TrimSpace(p.Provider)
}

// findRejection returns the rejection record for a model, if present.
func findRejection(rejected []modelrouter.Rejection, model string) *modelrouter.Rejection {
	m := strings.ToLower(strings.TrimSpace(model))
	for i := range rejected {
		if strings.ToLower(strings.TrimSpace(rejected[i].ModelID)) == m {
			return &rejected[i]
		}
	}
	return nil
}

func rejectionDetails(reasons []modelrouter.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.Detail)
	}
	return out
}

// orchestratedRejectedBlock renders the stable, human-readable list of rejected
// executable candidates with the provider that produced each and the reason it
// was rejected.
func orchestratedRejectedBlock(decision modelrouter.Decision, byID map[string]config.ProviderProfile) string {
	var b strings.Builder
	b.WriteString("Routing unavailable:\n")
	for _, r := range decision.Rejected {
		provider := "?"
		if p, ok := byID[strings.ToLower(strings.TrimSpace(r.ModelID))]; ok {
			provider = profileDisplayName(p)
		}
		fmt.Fprintf(&b, "  %s [%s]\n", r.ModelID, provider)
		for _, reason := range r.Reasons {
			fmt.Fprintf(&b, "    - %s\n", reason.Detail)
		}
	}
	return b.String()
}

// orchestratedRejectedJSON renders rejected candidates with provider and reason
// details for machine consumers, distinguishing incompatible (declared but
// capability-missing) from simply unavailable.
func orchestratedRejectedJSON(decision modelrouter.Decision, byID map[string]config.ProviderProfile) []map[string]any {
	out := make([]map[string]any, 0, len(decision.Rejected))
	for _, r := range decision.Rejected {
		provider := ""
		if p, ok := byID[strings.ToLower(strings.TrimSpace(r.ModelID))]; ok {
			provider = profileDisplayName(p)
		}
		reasons := make([]string, 0, len(r.Reasons))
		for _, reason := range r.Reasons {
			reasons = append(reasons, reason.Detail)
		}
		out = append(out, map[string]any{
			"model":    r.ModelID,
			"provider": provider,
			"reasons":  reasons,
		})
	}
	return out
}

// renderModelSelectionError reports an explicit-model selection failure. The
// outcome distinguishes a configured-but-incompatible model from one that is
// simply unavailable, and is emitted as structured JSON when requested.
func renderModelSelectionError(od orchestratedOnceDeps, mode, explicitModel, providerName, outcome, message string) int {
	if od.options.outputFormat == execOutputJSON {
		payload := map[string]any{
			"mode": mode,
			"error": map[string]any{
				"code":     "model_selection",
				"outcome":  outcome,
				"model":    explicitModel,
				"provider": providerName,
				"message":  message,
			},
		}
		if err := writePrettyJSON(od.stdout, payload); err != nil {
			return exitCrash
		}
		return exitProvider
	}
	return writeAppError(od.stderr, message, exitProvider)
}

func renderOrchestratedNoTask(od orchestratedOnceDeps, mode string, preview planPreviewResult, state scheduler.ExecutionState) int {
	if od.options.outputFormat == execOutputJSON {
		payload := map[string]any{
			"mode":          mode,
			"plan":          map[string]any{"planId": preview.Plan.PlanID, "summary": preview.Plan.Summary, "taskCount": len(preview.Plan.Tasks)},
			"selected_task": nil,
			"routing":       nil,
			"execution":     map[string]any{"status": "no_ready_task"},
			"verification":  executor.VerificationOutcome{Status: "not_run"},
			"scheduler":     orchestratedSchedulerJSON(state),
			"stopped_note":  "No ready task. The plan has no task that can run right now.",
		}
		if err := writePrettyJSON(od.stdout, payload); err != nil {
			return exitCrash
		}
		return exitIncomplete
	}
	var b strings.Builder
	b.WriteString(orchestratedBanner(mode, od.parallel.Enabled))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("=", len(orchestratedBanner(mode, od.parallel.Enabled))))
	b.WriteString("\n\n")
	b.WriteString("Plan: ")
	b.WriteString(preview.Plan.Summary)
	b.WriteString(" (id ")
	b.WriteString(preview.Plan.PlanID)
	b.WriteString(")\n\n")
	b.WriteString("No ready task. The plan has no task that can run right now (nothing is Ready; tasks may be Blocked or still Waiting on dependencies).\n\n")
	b.WriteString("Scheduler state:\n")
	b.WriteString(orchestratedSchedulerText(state))
	b.WriteString("\n")
	b.WriteString(orchestratedFooter(mode))
	_, _ = io.WriteString(od.stdout, b.String())
	return exitIncomplete
}

func renderOrchestratedNoModel(od orchestratedOnceDeps, mode string, byID map[string]config.ProviderProfile, preview planPreviewResult, task planner.Task, decision modelrouter.Decision) int {
	if od.options.outputFormat == execOutputJSON {
		payload := map[string]any{
			"mode":          mode,
			"plan":          map[string]any{"planId": preview.Plan.PlanID, "summary": preview.Plan.Summary},
			"selected_task": orchestratedTaskJSON(task),
			"routing": map[string]any{
				"selected":     nil,
				"rejected":     orchestratedRejectedJSON(decision, byID),
				"noCompatible": decision.NoCompatible,
				"outcome":      "no-compatible",
			},
			"execution":    map[string]any{"status": "no_compatible_model"},
			"verification": executor.VerificationOutcome{Status: "not_run"},
			"scheduler":    orchestratedSchedulerJSON(preview.State),
			"stopped_note": "No compatible model for the selected task; scheduler left unchanged.",
		}
		if err := writePrettyJSON(od.stdout, payload); err != nil {
			return exitCrash
		}
		return exitProvider
	}
	var b strings.Builder
	b.WriteString(orchestratedBanner(mode, od.parallel.Enabled))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("=", len(orchestratedBanner(mode, od.parallel.Enabled))))
	b.WriteString("\n\n")
	b.WriteString("Selected task: ")
	b.WriteString(task.ID)
	b.WriteString(": ")
	b.WriteString(task.Title)
	b.WriteString("\n\n")
	b.WriteString("No compatible model for this task.\n")
	if len(decision.Rejected) > 0 {
		b.WriteString("\n")
		b.WriteString(orchestratedRejectedBlock(decision, byID))
	}
	b.WriteString("\nThe scheduler was left unchanged.\n\n")
	b.WriteString(orchestratedFooter(mode))
	_, _ = io.WriteString(od.stdout, b.String())
	return exitProvider
}

func renderOrchestratedBlocked(od orchestratedOnceDeps, mode string, preview planPreviewResult, task planner.Task, reason string) int {
	if od.options.outputFormat == execOutputJSON {
		payload := map[string]any{
			"mode":          mode,
			"plan":          map[string]any{"planId": preview.Plan.PlanID, "summary": preview.Plan.Summary},
			"selected_task": orchestratedTaskJSON(task),
			"routing":       nil,
			"execution":     map[string]any{"status": "blocked", "error": reason},
			"verification":  executor.VerificationOutcome{Status: "not_run"},
			"scheduler":     orchestratedSchedulerJSON(preview.State),
			"stopped_note":  "Task blocked: explicit approval required before execution.",
		}
		if err := writePrettyJSON(od.stdout, payload); err != nil {
			return exitCrash
		}
		return exitIncomplete
	}
	var b strings.Builder
	b.WriteString(orchestratedBanner(mode, od.parallel.Enabled))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("=", len(orchestratedBanner(mode, od.parallel.Enabled))))
	b.WriteString("\n\n")
	b.WriteString("Selected task: ")
	b.WriteString(task.ID)
	b.WriteString(": ")
	b.WriteString(task.Title)
	b.WriteString("\n\n")
	b.WriteString("Blocked: ")
	b.WriteString(reason)
	b.WriteString("\n\n")
	b.WriteString(orchestratedFooter(mode))
	_, _ = io.WriteString(od.stdout, b.String())
	return exitIncomplete
}

// renderOrchestratedDebugTools prints the resolved tool-resolution state and the
// final provider-visible tool schemas without calling any provider. Used by
// --debug-orchestrated-tools to verify tool propagation end-to-end.
func renderOrchestratedDebugTools(od orchestratedOnceDeps, task planner.Task, toolAllowlist []string) int {
	exposure := orchestratedExposurePolicy(task)
	baseOpts := buildOrchestratedAgentOptions(od, "debug-session", "debug-model", task, nil, nil, nil, nil)

	regNames := make([]string, 0)
	deferred := make([]string, 0)
	permByTool := map[string]string{}
	if od.registry != nil {
		for _, t := range od.registry.All() {
			regNames = append(regNames, t.Name())
			if tools.IsDeferralEligible(t) {
				deferred = append(deferred, t.Name())
			}
			permByTool[t.Name()] = string(t.Safety().Permission)
		}
	}
	sort.Strings(regNames)
	sort.Strings(deferred)

	// The provider sees schemas advertised under the SAME permission mode as the
	// run (no Auto->Unsafe promotion); task-compatible exposure widens visibility
	// without widening authority.
	visible := agent.ExposedToolNames(od.registry, od.permissionMode, baseOpts)

	var b strings.Builder
	b.WriteString("ORCHESTRATED TOOL RESOLUTION (debug)\n")
	b.WriteString("===============================\n\n")
	fmt.Fprintf(&b, "task kind:            %s\n", task.TaskKind)
	fmt.Fprintf(&b, "safety level:         %s\n", task.SafetyLevel)
	fmt.Fprintf(&b, "permission mode:      %s\n", od.permissionMode)
	fmt.Fprintf(&b, "exposure policy:      %s\n", exposure)
	fmt.Fprintf(&b, "resolved enabled:     %v\n", toolAllowlist)
	fmt.Fprintf(&b, "resolved disabled:    %v\n", od.options.disabledTools)
	fmt.Fprintf(&b, "registry tool count:  %d\n", len(regNames))
	fmt.Fprintf(&b, "registry tools:       %v\n", regNames)
	fmt.Fprintf(&b, "deferred tools:       %v\n", deferred)
	b.WriteString("\nprovider-visible schema (name: permission-requirement):\n")
	for _, name := range visible {
		fmt.Fprintf(&b, "  %s: %s\n", name, permByTool[name])
	}
	_, _ = io.WriteString(od.stdout, b.String())
	return exitSuccess
}

func renderOrchestratedProviderError(od orchestratedOnceDeps, mode string, preview planPreviewResult, task planner.Task, providerKind, modelID, reason string) int {
	if od.options.outputFormat == execOutputJSON {
		payload := map[string]any{
			"mode":          mode,
			"plan":          map[string]any{"planId": preview.Plan.PlanID, "summary": preview.Plan.Summary},
			"selected_task": orchestratedTaskJSON(task),
			"routing":       map[string]any{"provider": providerKind, "model": modelID},
			"execution":     map[string]any{"status": "provider_error", "error": reason},
			"verification":  executor.VerificationOutcome{Status: "not_run"},
			"scheduler":     orchestratedSchedulerJSON(preview.State),
			"stopped_note":  "Could not construct the routed provider.",
		}
		if err := writePrettyJSON(od.stdout, payload); err != nil {
			return exitCrash
		}
		return exitProvider
	}
	var b strings.Builder
	b.WriteString(orchestratedBanner(mode, od.parallel.Enabled))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("=", len(orchestratedBanner(mode, od.parallel.Enabled))))
	b.WriteString("\n\n")
	b.WriteString("Selected task: ")
	b.WriteString(task.ID)
	b.WriteString(": ")
	b.WriteString(task.Title)
	b.WriteString("\n")
	b.WriteString("Routed model: ")
	b.WriteString(providerKind)
	b.WriteString("/")
	b.WriteString(modelID)
	b.WriteString("\n\n")
	b.WriteString("Could not construct the routed provider")
	if reason != "" {
		b.WriteString(": ")
		b.WriteString(reason)
	}
	b.WriteString("\n\n")
	b.WriteString(orchestratedFooter(mode))
	_, _ = io.WriteString(od.stdout, b.String())
	return exitProvider
}

func renderOrchestratedText(
	od orchestratedOnceDeps,
	preview planPreviewResult,
	task planner.Task,
	decision modelrouter.Decision,
	result executor.TaskExecutionResult,
	changes executor.RepoChanges,
	verification executor.VerificationOutcome,
	status executor.CompletionStatus,
	finalState scheduler.ExecutionState,
) int {
	var b strings.Builder
	b.WriteString("ORCHESTRATED EXECUTION — one task only\n")
	b.WriteString("=====================================\n\n")
	b.WriteString("Plan: ")
	b.WriteString(preview.Plan.Summary)
	b.WriteString(" (id ")
	b.WriteString(preview.Plan.PlanID)
	b.WriteString(")\n")
	b.WriteString("Classification: ")
	fmt.Fprintf(&b, "%v", preview.Classification.Primary)
	b.WriteString("\n\n")

	b.WriteString("Selected task:\n")
	b.WriteString("  ")
	b.WriteString(task.ID)
	b.WriteString(": ")
	b.WriteString(task.Title)
	b.WriteString(" (")
	b.WriteString(string(task.TaskKind))
	b.WriteString(")\n")
	b.WriteString("  ")
	b.WriteString(task.Description)
	b.WriteString("\n\n")

	routing, rejected := orchestratedRoutingLines(decision)
	b.WriteString("Routing:\n  ")
	b.WriteString(routing)
	b.WriteString("\n")
	if len(rejected) > 0 {
		b.WriteString("  rejected: ")
		b.WriteString(strings.Join(rejected, ", "))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("Execution (")
	b.WriteString(string(status))
	b.WriteString("):\n")
	if result.Error != nil {
		b.WriteString("  error: ")
		b.WriteString(result.Error.Error())
		b.WriteString("\n")
	}
	b.WriteString("  final answer:\n")
	b.WriteString(indentBlock(result.FinalAnswer, "    "))
	b.WriteString("\n")
	if len(result.FilesChanged) > 0 {
		b.WriteString("  changed files: ")
		b.WriteString(strings.Join(result.FilesChanged, ", "))
		b.WriteString("\n")
	}
	if len(result.CommandsRun) > 0 {
		b.WriteString("  commands run: ")
		b.WriteString(strings.Join(result.CommandsRun, ", "))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("Verification: ")
	b.WriteString(verification.Status)
	if verification.Total > 0 {
		fmt.Fprintf(&b, " (%d passed, %d failed, %d errors)", verification.Passed, verification.Failed, verification.Errors)
	}
	b.WriteString("\n")
	b.WriteString("Repository changes: ")
	if n := len(changes.All()); n > 0 {
		fmt.Fprintf(&b, "%d file(s)\n", n)
	} else {
		b.WriteString("none\n")
	}
	b.WriteString("Completion signal: ")
	if result.AgentResult.Incomplete {
		b.WriteString("missing (not required; deterministic evidence satisfied)\n")
	} else {
		b.WriteString("present (supporting evidence only)\n")
	}
	b.WriteString("\n")

	b.WriteString("Scheduler state after one task:\n")
	b.WriteString(orchestratedSchedulerText(finalState))
	b.WriteString("\nStopped after one task by --orchestrated-once. Run again or use normal exec for the next task.\n")

	if od.metrics != nil {
		b.WriteString("\n")
		b.WriteString(orchestratedMetricsDetailed(od.metrics))
	}

	_, _ = io.WriteString(od.stdout, b.String())

	switch status {
	case executor.StatusCompleted, executor.StatusCompletedNoChange, executor.StatusCompletedUnverified:
		return exitSuccess
	default:
		return exitIncomplete
	}
}

func renderOrchestratedJSON(
	od orchestratedOnceDeps,
	preview planPreviewResult,
	task planner.Task,
	decision modelrouter.Decision,
	result executor.TaskExecutionResult,
	changes executor.RepoChanges,
	verification executor.VerificationOutcome,
	status executor.CompletionStatus,
	finalState scheduler.ExecutionState,
) int {
	provider := ""
	model := ""
	rejected := make([]string, 0, len(decision.Rejected))
	if decision.Selected != nil {
		provider = string(decision.Selected.Model.Provider)
		model = decision.Selected.Model.ID
	}
	for _, r := range decision.Rejected {
		rejected = append(rejected, r.ModelID)
	}

	payload := map[string]any{
		"mode": "orchestrated-once",
		"plan": map[string]any{
			"planId":    preview.Plan.PlanID,
			"summary":   preview.Plan.Summary,
			"taskCount": len(preview.Plan.Tasks),
		},
		"selected_task": orchestratedTaskJSON(task),
		"routing": map[string]any{
			"provider":     provider,
			"model":        model,
			"rejected":     rejected,
			"noCompatible": decision.NoCompatible,
		},
		"execution": map[string]any{
			"status":             string(status),
			"finalAnswer":        result.FinalAnswer,
			"filesChanged":       changes.All(),
			"commandsRun":        result.CommandsRun,
			"toolEvents":         result.ToolEvents,
			"toolUsage":          result.ToolUsage,
			"usage":              newOrchestratedTokenMetrics(result.Usage, result.UsageReported),
			"incomplete":         result.AgentResult.Incomplete,
			"completionSignal":   !result.AgentResult.Incomplete,
			"permissionRequired": result.PermissionRequired,
			"permissionDenied":   result.PermissionDenied,
			"error":              errToText(result.Error),
		},
		"verification": verification,
		"scheduler":    orchestratedSchedulerJSON(finalState),
		"stopped_note": "Stopped after one task by --orchestrated-once.",
	}
	if od.metrics != nil {
		payload["metrics"] = orchestratedMetricsJSON(od.metrics)
	}
	if err := writePrettyJSON(od.stdout, payload); err != nil {
		return exitCrash
	}
	switch status {
	case executor.StatusCompleted, executor.StatusCompletedNoChange, executor.StatusCompletedUnverified:
		return exitSuccess
	default:
		return exitIncomplete
	}
}

func orchestratedTaskJSON(task planner.Task) map[string]any {
	return map[string]any{
		"id":                   task.ID,
		"title":                task.Title,
		"taskKind":             string(task.TaskKind),
		"description":          task.Description,
		"safetyLevel":          string(task.SafetyLevel),
		"dependencies":         task.Dependencies,
		"estimatedComplexity":  string(task.EstimatedComplexity),
		"requiredCapabilities": task.RequiredCapabilities,
	}
}

func orchestratedSchedulerJSON(state scheduler.ExecutionState) map[string]any {
	return map[string]any{
		"ready":     len(state.ReadyQueue),
		"completed": len(state.CompletedTasks),
		"failed":    len(state.FailedTasks),
		"blocked":   len(state.BlockedTasks),
		"skipped":   len(state.SkippedTasks),
		"waiting":   len(state.WaitingTasks),
		"total":     len(state.ReadyQueue) + len(state.CompletedTasks) + len(state.FailedTasks) + len(state.BlockedTasks) + len(state.SkippedTasks) + len(state.WaitingTasks),
	}
}

func orchestratedSchedulerText(state scheduler.ExecutionState) string {
	return fmt.Sprintf("  ready: %d  completed: %d  failed: %d  blocked: %d  skipped: %d  waiting: %d\n",
		len(state.ReadyQueue), len(state.CompletedTasks), len(state.FailedTasks), len(state.BlockedTasks), len(state.SkippedTasks), len(state.WaitingTasks))
}

func indentBlock(s, prefix string) string {
	if strings.TrimSpace(s) == "" {
		return prefix + "(none)"
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// orchestratedTopStatus derives the headline completion status and process exit
// code for a full orchestrated run from the executed and skipped tasks.
func orchestratedTopStatus(executed []orchestratedTaskExec, skipped []orchestratedSkippedTask) (executor.CompletionStatus, int) {
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
	code := exitSuccess
	if hasFail {
		code = exitIncomplete
	}
	var top executor.CompletionStatus
	switch {
	case hasFail:
		top = executor.StatusFailed
	case sawCompleted:
		top = executor.StatusCompleted
	default:
		top = executor.StatusCompletedUnverified
	}
	return top, code
}

// renderOrchestratedSummary renders the full sequential orchestrated run report.
func renderOrchestratedSummary(
	od orchestratedOnceDeps,
	mode string,
	preview planPreviewResult,
	executed []orchestratedTaskExec,
	skipped []orchestratedSkippedTask,
	finalState scheduler.ExecutionState,
) int {
	top, code := orchestratedTopStatus(executed, skipped)
	var b strings.Builder
	header := orchestratedBanner(mode, od.parallel.Enabled)
	fmt.Fprintf(&b, "%s\n", header)
	b.WriteString(strings.Repeat("=", len(header)))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Plan: %s (id %s)\n", preview.Plan.Summary, preview.Plan.PlanID)
	fmt.Fprintf(&b, "Classification: %v\n\n", preview.Classification.Primary)

	fmt.Fprintf(&b, "Executed tasks (%d):\n", len(executed))
	curBatch := 0
	for i, e := range executed {
		if e.ParallelBatch != curBatch {
			if e.ParallelBatch > 0 {
				fmt.Fprintf(&b, "  Parallel batch %d (ran concurrently):\n", e.ParallelBatch)
			}
			curBatch = e.ParallelBatch
		}
		fmt.Fprintf(&b, "  [%d] %s: %s (%s)\n", i+1, e.task.ID, e.task.Title, e.task.TaskKind)
		fmt.Fprintf(&b, "      status: %s\n", e.status)
		routing, rejected := orchestratedRoutingLines(e.decision)
		fmt.Fprintf(&b, "      routing: %s\n", routing)
		if len(rejected) > 0 {
			fmt.Fprintf(&b, "      rejected: %s\n", strings.Join(rejected, ", "))
		}
		if e.result.Error != nil {
			fmt.Fprintf(&b, "      error: %s\n", e.result.Error.Error())
		}
		fmt.Fprintf(&b, "      final answer:\n%s\n", indentBlock(e.result.FinalAnswer, "        "))
		if len(e.result.FilesChanged) > 0 {
			fmt.Fprintf(&b, "      changed files: %s\n", strings.Join(e.result.FilesChanged, ", "))
		}
		if len(e.result.CommandsRun) > 0 {
			fmt.Fprintf(&b, "      commands run: %s\n", strings.Join(e.result.CommandsRun, ", "))
		}
		fmt.Fprintf(&b, "      verification: %s", e.verification.Status)
		if e.verification.Total > 0 {
			fmt.Fprintf(&b, " (%d passed, %d failed, %d errors)", e.verification.Passed, e.verification.Failed, e.verification.Errors)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if len(skipped) > 0 {
		fmt.Fprintf(&b, "Skipped tasks (%d):\n", len(skipped))
		for i, s := range skipped {
			fmt.Fprintf(&b, "  [%d] %s: %s \u2014 %s\n", i+1, s.task.ID, s.task.Title, s.reason)
		}
		b.WriteString("\n")
	}

	b.WriteString("Scheduler state after run:\n")
	b.WriteString(orchestratedSchedulerText(finalState))
	fmt.Fprintf(&b, "\nTop status: %s\n", top)
	if od.metrics != nil {
		b.WriteString("\n")
		b.WriteString(orchestratedMetricsDetailed(od.metrics))
	}
	b.WriteString(orchestratedFooter(mode))
	_, _ = io.WriteString(od.stdout, b.String())
	return code
}

// renderOrchestratedSummaryJSON renders the full sequential orchestrated run
// report as JSON.
func renderOrchestratedSummaryJSON(
	od orchestratedOnceDeps,
	mode string,
	preview planPreviewResult,
	executed []orchestratedTaskExec,
	skipped []orchestratedSkippedTask,
	finalState scheduler.ExecutionState,
) int {
	top, code := orchestratedTopStatus(executed, skipped)

	execOut := make([]map[string]any, 0, len(executed))
	for _, e := range executed {
		provider := ""
		model := ""
		rejected := make([]string, 0)
		if e.decision.Selected != nil {
			provider = string(e.decision.Selected.Model.Provider)
			model = e.decision.Selected.Model.ID
		}
		for _, r := range e.decision.Rejected {
			rejected = append(rejected, r.ModelID)
		}
		execOut = append(execOut, map[string]any{
			"task":          orchestratedTaskJSON(e.task),
			"parallelBatch": e.ParallelBatch,
			"routing":       map[string]any{"provider": provider, "model": model, "rejected": rejected, "noCompatible": e.decision.NoCompatible},
			"execution":     map[string]any{"status": string(e.status), "finalAnswer": e.result.FinalAnswer, "filesChanged": e.result.FilesChanged, "commandsRun": e.result.CommandsRun, "toolUsage": e.result.ToolUsage, "usage": newOrchestratedTokenMetrics(e.result.Usage, e.result.UsageReported), "error": errToText(e.result.Error)},
			"verification":  e.verification,
		})
	}
	skipOut := make([]map[string]any, 0, len(skipped))
	for _, s := range skipped {
		skipOut = append(skipOut, map[string]any{
			"task":   orchestratedTaskJSON(s.task),
			"reason": s.reason,
		})
	}
	payload := map[string]any{
		"mode":      mode,
		"plan":      map[string]any{"planId": preview.Plan.PlanID, "summary": preview.Plan.Summary, "taskCount": len(preview.Plan.Tasks)},
		"executed":  execOut,
		"skipped":   skipOut,
		"scheduler": orchestratedSchedulerJSON(finalState),
		"topStatus": string(top),
	}
	if od.metrics != nil {
		payload["metrics"] = orchestratedMetricsJSON(od.metrics)
	}
	if err := writePrettyJSON(od.stdout, payload); err != nil {
		return exitCrash
	}
	return code
}

// orchestratedTopStatus derives the headline completion status and process exit
// code for a full orchestrated run from the executed and skipped tasks.
