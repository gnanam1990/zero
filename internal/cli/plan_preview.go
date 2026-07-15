package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/scheduler"
	"github.com/Gitlawb/zero/internal/taskclass"
)

// runPlanPreview is a fully local, dry-run inspector. It connects the existing
// local-only modules end to end — task classification → execution planning →
// scheduler state → per-task model routing — and prints a human-readable (or
// JSON) preview. It performs no network calls, executes no tools, creates no
// session, changes no provider/model selection, and writes no config or files.
func runPlanPreview(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, err := parsePlanPreviewArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if options.help {
		if err := writePlanPreviewHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	prompt := strings.TrimSpace(options.prompt)
	if prompt == "" {
		return writeExecUsageError(stderr, "plan-preview requires a non-empty prompt (positional or --prompt)")
	}

	result, err := buildPlanPreview(prompt, options.routerFlagOptions, detectRepositoryPresence(deps), nil)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	if options.json {
		if err := writePrettyJSON(stdout, buildPlanPreviewJSON(result)); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	if err := writePlanPreviewText(stdout, result, options.showRejected); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// planPreviewResult is the fully computed orchestration preview, rendered
// identically by both `zero plan-preview` and `zero exec --orchestration-preview`.
type planPreviewResult struct {
	Prompt         string
	Classification taskclass.Result
	Plan           planner.ExecutionPlan
	Results        []planTaskResult
	State          scheduler.ExecutionState
}

// buildPlanPreview runs the deterministic local pipeline end to end — classify →
// plan → validate → schedule → per-task route — and returns the structured
// result. It performs no network calls, executes no tools, constructs no
// provider, opens no session, and never mutates the planner output.
// buildPlanPreview runs the deterministic local pipeline end to end — classify →
// plan → validate → schedule → per-task route — and returns the structured
// result. It performs no network calls, executes no tools, constructs no
// provider, opens no session, and never mutates the planner output.
//
// When candidates is non-nil it is used as the routing candidate set; when nil
// the full curated registry is used (the default for the standalone
// plan-preview and exec --orchestration-preview commands). The orchestrated-once
// path passes the configured-executable candidate set so routing honors only
// models the user can actually run.
func buildPlanPreview(prompt string, routerOpts routerFlagOptions, repoPresent bool, candidates []modelregistry.ModelEntry) (planPreviewResult, error) {
	classification := taskclass.Classify(taskclass.Request{
		Prompt:            prompt,
		HasImages:         false,
		RepositoryPresent: repoPresent,
	})

	var entries []modelregistry.ModelEntry
	if candidates != nil {
		entries = candidates
	} else {
		registry, err := modelregistry.DefaultRegistry()
		if err != nil {
			return planPreviewResult{}, err
		}
		entries = registry.List(modelregistry.ListOptions{IncludeDeprecated: true})
	}

	plan, err := planner.Plan(planner.PlannerInput{
		Prompt:             prompt,
		TaskClassification: classification,
		RepositoryPresent:  repoPresent,
		AvailableTools:     nil,
	})
	if err != nil {
		return planPreviewResult{}, err
	}

	// Pipeline validation step: the plan is already validated inside Plan, but we
	// make the validation explicit per the documented pipeline.
	if err := planner.Validate(plan); err != nil {
		return planPreviewResult{}, err
	}

	// Build the scheduler and read its initial state. The scheduler is never
	// transitioned (no MarkCompleted/Failed/Skipped); it stays in its planned
	// derived state so the preview reflects what WOULD run, not what ran.
	sched, err := scheduler.NewScheduler(plan)
	if err != nil {
		return planPreviewResult{}, err
	}
	state := sched.State()
	taskStates := schedulerStateMap(state)

	// Per-task routing: every planned task gets its own independent model
	// decision based on its RequiredCapabilities. No task is executed and the
	// planner output is never mutated.
	results := make([]planTaskResult, 0, len(plan.Tasks))
	for _, task := range plan.Tasks {
		decision, rerr := routeTask(task, entries, routerOpts)
		if rerr != nil {
			return planPreviewResult{}, rerr
		}
		results = append(results, planTaskResult{
			Task:     task,
			State:    taskStates[task.ID],
			Decision: decision,
		})
	}

	return planPreviewResult{
		Prompt:         prompt,
		Classification: classification,
		Plan:           plan,
		Results:        results,
		State:          state,
	}, nil
}

// planTaskResult binds one planner task to its scheduler-derived state and its
// independent routing decision.
type planTaskResult struct {
	Task     planner.Task
	State    scheduler.TaskState
	Decision modelrouter.Decision
}

// routeTask converts a planner task into a routing request and decides the best
// model from the registry candidates. It uses the task's own RequiredCapabilities
// (not the original prompt), giving each task an independent decision.
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

// plannerKindToClassKind maps the planner's task taxonomy back to the
// classifier's kind so per-task routing can carry the task's context.
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

// schedulerStateMap derives a stable id → state map from a scheduler snapshot.
func schedulerStateMap(state scheduler.ExecutionState) map[string]scheduler.TaskState {
	m := make(map[string]scheduler.TaskState)
	for _, t := range state.ReadyQueue {
		m[t.ID] = scheduler.StateReady
	}
	for _, t := range state.BlockedTasks {
		m[t.ID] = scheduler.StateBlocked
	}
	for _, t := range state.WaitingTasks {
		m[t.ID] = scheduler.StateWaiting
	}
	for _, t := range state.CompletedTasks {
		m[t.ID] = scheduler.StateCompleted
	}
	for _, t := range state.FailedTasks {
		m[t.ID] = scheduler.StateFailed
	}
	for _, t := range state.SkippedTasks {
		m[t.ID] = scheduler.StateSkipped
	}
	return m
}

// ---- argument parsing ----

type planPreviewOptions struct {
	routerFlagOptions
	prompt       string
	json         bool
	showRejected bool
	help         bool
}

func parsePlanPreviewArgs(args []string) (planPreviewOptions, error) {
	var opts planPreviewOptions
	var ropts routerFlagOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			opts.help = true
		case arg == "--json":
			opts.json = true
		case arg == "--show-rejected":
			opts.showRejected = true
		case arg == "--prompt":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return opts, err
			}
			if opts.prompt != "" {
				return opts, execUsageError{"multiple prompts provided; pass a single prompt positionally or via --prompt"}
			}
			opts.prompt = value
			i = next
		case strings.HasPrefix(arg, "--prompt="):
			value, err := requiredInlineFlagValue(arg, "--prompt")
			if err != nil {
				return opts, err
			}
			if opts.prompt != "" {
				return opts, execUsageError{"multiple prompts provided; pass a single prompt positionally or via --prompt"}
			}
			opts.prompt = value
		default:
			if matched, next, err := tryParseRouterFlag(arg, args, i, &ropts); matched {
				if err != nil {
					return opts, err
				}
				i = next
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return opts, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
			}
			if opts.prompt != "" {
				return opts, execUsageError{"multiple prompts provided; pass a single prompt positionally or via --prompt"}
			}
			opts.prompt = arg
		}
	}
	opts.routerFlagOptions = ropts
	return opts, nil
}

// ---- text output ----

func writePlanPreviewText(stdout io.Writer, res planPreviewResult, showRejected bool) error {
	plan := res.Plan
	results := res.Results
	state := res.State

	var b strings.Builder

	fmt.Fprintf(&b, "Plan:\n")
	fmt.Fprintf(&b, "  ID: %s\n", plan.PlanID)
	fmt.Fprintf(&b, "  Summary: %s\n", plan.Summary)

	b.WriteString("\nTasks:\n\n")

	parallelIDs := []string{}
	for idx, r := range results {
		t := r.Task
		fmt.Fprintf(&b, "  %d. %s\n", idx+1, t.Title)
		fmt.Fprintf(&b, "     Kind: %s\n", t.TaskKind)
		fmt.Fprintf(&b, "     Complexity: %s\n", t.EstimatedComplexity)
		fmt.Fprintf(&b, "     Safety: %s\n", t.SafetyLevel)
		fmt.Fprintf(&b, "     Status: %s\n", r.State)
		if len(t.Dependencies) == 0 {
			b.WriteString("     Dependencies: none\n")
		} else {
			b.WriteString("     Dependencies:\n")
			for _, d := range t.Dependencies {
				fmt.Fprintf(&b, "       - %s\n", d)
			}
		}
		parallel := "no"
		if t.CanRunParallel {
			parallel = "yes"
			for _, ready := range state.ReadyQueue {
				if ready.ID == t.ID {
					parallelIDs = append(parallelIDs, t.ID)
					break
				}
			}
		}
		fmt.Fprintf(&b, "     Parallel: %s\n", parallel)

		if t.SafetyLevel == planner.SafetyNeedsApproval || t.SafetyLevel == planner.SafetyDangerous {
			b.WriteString("     Approval required before execution.\n")
		}

		b.WriteString("\n     Selected model:\n")
		if r.Decision.Selected == nil {
			b.WriteString("       (none)\n")
		} else {
			c := r.Decision.Selected
			fmt.Fprintf(&b, "       %s\n", c.Model.ID)
			fmt.Fprintf(&b, "       Provider: %s\n", c.Model.Provider)
			fmt.Fprintf(&b, "       Score: %d\n", c.Score)
		}

		b.WriteString("\n     Reasons:\n")
		if r.Decision.Selected == nil {
			b.WriteString("       (none)\n")
		} else {
			for _, reason := range r.Decision.Selected.Reasons {
				fmt.Fprintf(&b, "       - %s\n", reason.Detail)
			}
		}

		if r.Decision.NoCompatible {
			b.WriteString("\n     No compatible model was found for this task. Adjust filters or capabilities and retry.\n")
		}

		if showRejected {
			b.WriteString("\n     Rejected models:\n")
			if len(r.Decision.Rejected) == 0 {
				b.WriteString("       (none)\n")
			} else {
				for _, rej := range r.Decision.Rejected {
					fmt.Fprintf(&b, "       %s\n", rej.ModelID)
					for _, reason := range rej.Reasons {
						fmt.Fprintf(&b, "         - %s\n", reason.Detail)
					}
				}
			}
		}

		b.WriteString("\n")
	}

	b.WriteString("Scheduler:\n")
	fmt.Fprintf(&b, "  Ready: %d\n", len(state.ReadyQueue))
	fmt.Fprintf(&b, "  Waiting: %d\n", len(state.WaitingTasks))
	fmt.Fprintf(&b, "  Blocked: %d\n", len(state.BlockedTasks))
	fmt.Fprintf(&b, "  Completed: %d\n", len(state.CompletedTasks))
	fmt.Fprintf(&b, "  Failed: %d\n", len(state.FailedTasks))
	fmt.Fprintf(&b, "  Skipped: %d\n", len(state.SkippedTasks))

	if len(parallelIDs) > 0 {
		sort.Strings(parallelIDs)
		b.WriteString("\nParallel tasks (ready now):\n")
		for _, id := range parallelIDs {
			fmt.Fprintf(&b, "  - %s\n", id)
		}
	}

	if len(state.BlockedTasks) > 0 {
		b.WriteString("\nApproval required before execution.\n")
	}

	_, err := io.WriteString(stdout, b.String())
	return err
}

// ---- JSON output ----

type planPreviewJSON struct {
	Prompt         string                    `json:"prompt"`
	Classification planPreviewClassification `json:"classification"`
	Plan           planPreviewPlan           `json:"plan"`
	Scheduler      planPreviewSchedulerState `json:"scheduler"`
}

type planPreviewClassification struct {
	Primary              string                   `json:"primary"`
	Secondary            []string                 `json:"secondary"`
	Confidence           string                   `json:"confidence"`
	RequiredCapabilities []string                 `json:"required_capabilities"`
	Evidence             []routePreviewReasonPair `json:"evidence"`
}

type planPreviewPlan struct {
	ID      string            `json:"id"`
	Summary string            `json:"summary"`
	Tasks   []planPreviewTask `json:"tasks"`
}

type planPreviewTask struct {
	ID             string             `json:"id"`
	Title          string             `json:"title"`
	Kind           string             `json:"kind"`
	Dependencies   []string           `json:"dependencies"`
	Complexity     string             `json:"complexity"`
	Safety         string             `json:"safety"`
	Status         string             `json:"status"`
	CanRunParallel bool               `json:"can_run_parallel"`
	Routing        planPreviewRouting `json:"routing"`
}

type planPreviewRouting struct {
	Selected *routePreviewCandidate  `json:"selected"`
	Ranked   []routePreviewCandidate `json:"ranked"`
	Rejected []routePreviewRejection `json:"rejected"`
}

type planPreviewSchedulerState struct {
	Ready     []string `json:"ready"`
	Waiting   []string `json:"waiting"`
	Blocked   []string `json:"blocked"`
	Completed []string `json:"completed"`
	Failed    []string `json:"failed"`
	Skipped   []string `json:"skipped"`
}

func buildPlanPreviewJSON(res planPreviewResult) planPreviewJSON {
	prompt := res.Prompt
	cls := res.Classification
	plan := res.Plan
	results := res.Results
	state := res.State

	out := planPreviewJSON{Prompt: prompt}

	out.Classification.Primary = string(cls.Primary)
	out.Classification.Confidence = string(cls.Confidence)
	out.Classification.Secondary = make([]string, 0, len(cls.Secondary))
	for _, k := range cls.Secondary {
		out.Classification.Secondary = append(out.Classification.Secondary, string(k))
	}
	out.Classification.RequiredCapabilities = make([]string, 0, len(cls.RequiredCapabilities))
	for _, c := range cls.RequiredCapabilities {
		out.Classification.RequiredCapabilities = append(out.Classification.RequiredCapabilities, string(c))
	}
	out.Classification.Evidence = make([]routePreviewReasonPair, 0, len(cls.Evidence))
	for _, e := range cls.Evidence {
		out.Classification.Evidence = append(out.Classification.Evidence, routePreviewReasonPair{Signal: e.Signal, Detail: e.Detail})
	}

	out.Plan.ID = plan.PlanID
	out.Plan.Summary = plan.Summary
	out.Plan.Tasks = make([]planPreviewTask, 0, len(results))
	for _, r := range results {
		t := r.Task
		pt := planPreviewTask{
			ID:             t.ID,
			Title:          t.Title,
			Kind:           string(t.TaskKind),
			Dependencies:   t.Dependencies,
			Complexity:     string(t.EstimatedComplexity),
			Safety:         string(t.SafetyLevel),
			Status:         string(r.State),
			CanRunParallel: t.CanRunParallel,
		}
		if r.Decision.Selected != nil {
			c := r.Decision.Selected
			pt.Routing.Selected = &routePreviewCandidate{
				Model:    c.Model.ID,
				Provider: string(c.Model.Provider),
				Score:    c.Score,
				Reasons:  candidateReasonsJSON(c.Reasons),
			}
		}
		pt.Routing.Ranked = make([]routePreviewCandidate, 0, len(r.Decision.Ranked))
		for _, c := range r.Decision.Ranked {
			pt.Routing.Ranked = append(pt.Routing.Ranked, routePreviewCandidate{
				Model:    c.Model.ID,
				Provider: string(c.Model.Provider),
				Score:    c.Score,
				Reasons:  candidateReasonsJSON(c.Reasons),
			})
		}
		pt.Routing.Rejected = make([]routePreviewRejection, 0, len(r.Decision.Rejected))
		for _, rej := range r.Decision.Rejected {
			pt.Routing.Rejected = append(pt.Routing.Rejected, routePreviewRejection{
				ModelID: rej.ModelID,
				Reasons: candidateReasonsJSON(rej.Reasons),
			})
		}
		out.Plan.Tasks = append(out.Plan.Tasks, pt)
	}

	out.Scheduler.Ready = taskIDs(state.ReadyQueue)
	out.Scheduler.Waiting = taskIDs(state.WaitingTasks)
	out.Scheduler.Blocked = taskIDs(state.BlockedTasks)
	out.Scheduler.Completed = taskIDs(state.CompletedTasks)
	out.Scheduler.Failed = taskIDs(state.FailedTasks)
	out.Scheduler.Skipped = taskIDs(state.SkippedTasks)

	return out
}

func taskIDs(tasks []planner.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return ids
}

func writePlanPreviewHelp(stdout io.Writer) error {
	_, err := fmt.Fprint(stdout, `Usage:
  zero plan-preview [flags] "<prompt>"
  zero plan-preview --prompt "<prompt>"

Preview the full local dry-run pipeline for a request: task classification,
execution planning, scheduler state, and per-task model routing. Nothing is
executed — no tools run, no providers are called, no sessions are created, and
no files or configuration are modified.

Flags:
  --prompt <prompt>          Prompt for the preview (alternative to positional).
  --provider <provider>      Prefer this provider as a ranking signal.
  --model <model-id>         Prefer this model if compatible.
  --allow-provider <provider>
                              Repeatable hard allowlist of providers.
  --deny-model <model-id>    Repeatable model denylist.
  --require-known-price      Reject models without known pricing.
  --max-input-cost <number>  Maximum registry input cost unit (USD per 1M tokens).
  --max-output-cost <number> Maximum registry output cost unit (USD per 1M tokens).
  --show-rejected            Show full rejected-model details in text output.
  --json                     Emit stable machine-readable JSON.
  -h, --help                 Show this help.

Examples:
  zero plan-preview "Implement OAuth login and write tests"
  zero plan-preview --json "Audit authentication for security issues"
  zero plan-preview --allow-provider openai "Refactor the provider registry"
`)
	return err
}
