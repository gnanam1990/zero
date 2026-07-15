package planner

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/taskclass"
)

// kindCapabilities maps each planner task kind to the factual model capabilities
// it requires. It reuses modelregistry.ModelCapability (no provider/model/price
// data) and mirrors the capability sets used by taskclass and modelrouter.
var kindCapabilities = map[TaskKind][]modelregistry.ModelCapability{
	KindImplementation:   {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
	KindRepositorySearch: {modelregistry.ModelCapabilityToolCalling},
	KindCodeReview:       {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
	KindSecurityReview:   {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
	KindArchitecture:     {modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityJSONMode, modelregistry.ModelCapabilityStreaming},
	KindDocumentation:    {modelregistry.ModelCapabilityStreaming},
	KindTesting:          {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
	KindTestExecution:    {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
	KindDebugging:        {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
	KindRefactoring:      {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
	KindShellOperation:   {modelregistry.ModelCapabilityToolCalling},
	KindImageAnalysis:    {modelregistry.ModelCapabilityVision},
	KindUnknown:          {},
}

// mapKind converts a classifier kind into the planner's taxonomy deterministically.
func mapKind(k taskclass.Kind) TaskKind {
	switch k {
	case taskclass.KindImplementation:
		return KindImplementation
	case taskclass.KindCodeSearch, taskclass.KindRepoExploration:
		return KindRepositorySearch
	case taskclass.KindCodeReview:
		return KindCodeReview
	case taskclass.KindSecurityReview:
		return KindSecurityReview
	case taskclass.KindArchitecturePlanning:
		return KindArchitecture
	case taskclass.KindDocumentation:
		return KindDocumentation
	case taskclass.KindTesting:
		return KindTesting
	case taskclass.KindBugInvestigation, taskclass.KindDebugging:
		return KindDebugging
	case taskclass.KindRefactoring:
		return KindRefactoring
	case taskclass.KindShellSystem:
		return KindShellOperation
	case taskclass.KindImageVisualAnalysis:
		return KindImageAnalysis
	default:
		return KindUnknown
	}
}

// taskID derives a deterministic task identifier from its creation index.
func taskID(index int) string {
	return "task-" + strconv.Itoa(index+1)
}

// Plan converts one user request into a deterministic execution graph. It never
// calls a provider, an LLM, or the router, and performs no execution.
func Plan(input PlannerInput) (ExecutionPlan, error) {
	prompt := strings.TrimSpace(input.Prompt)
	primary := mapKind(input.TaskClassification.Primary)

	// Explicit multi-task decomposition: when the prompt contains at least two
	// numbered task sections (Task 1:, Task 2:, etc.), each with meaningful
	// imperative content, produce one planner.Task per section. This takes
	// precedence over the implicit classification-based decomposition so a
	// prompt with explicit independent tasks doesn't collapse into a single
	// task based on the classifier's keyword precedence.
	if explicitTasks := parseExplicitTasks(prompt); len(explicitTasks) >= 2 {
		b := &taskBuilder{}
		for _, et := range explicitTasks {
			taskKind := classifyExplicitTask(et.Body, primary)
			title := titleFromHeading(et.Heading, titleFor(taskKind, et.Body))
			t := b.add(taskKind, title, et.Body, nil)
			_ = t // index not needed — all are independent
		}
		// All explicit tasks are independent and read-only by default.
		for i := range b.tasks {
			b.tasks[i].CanRunParallel = true
		}

		tasks := finalizeTasks(b.tasks, prompt)
		plan := ExecutionPlan{
			PlanID:       planID(input),
			Summary:      summaryFor(primary, tasks),
			Tasks:        tasks,
			Dependencies: collectDependencies(tasks),
			Metadata:     buildMetadata(input, primary, tasks),
		}
		if err := Validate(plan); err != nil {
			return ExecutionPlan{}, err
		}
		return plan, nil
	}

	// Normalize: a request that implements *and* tests something should anchor
	// the graph on the implementation, which the test tasks then depend on. The
	// classifier may rank "write tests" above "implement", so we re-anchor here
	// deterministically from prompt keywords.
	if (primary == KindTesting || primary == KindTestExecution) && mentionsImplementation(prompt) {
		primary = KindImplementation
	}

	b := &taskBuilder{}
	mainIdx := b.add(primary, titleFor(primary, prompt), descriptionFor(primary, prompt), nil)

	// Rule: a search request mentioning multiple domains fans out into parallel,
	// independent search tasks (e.g. "search the docs and search the code").
	if primary == KindRepositorySearch {
		if domains := detectSearchDomains(prompt); len(domains) >= 2 {
			b.tasks = nil
			for _, d := range domains {
				b.add(KindRepositorySearch, "Search "+d, "Repository search across "+d, nil)
			}
			for i := range b.tasks {
				b.tasks[i].CanRunParallel = true
			}
		}
	}

	// Rule: an implementation request that also asks for tests produces a testing
	// task that depends on the implementation; running tests depends on testing.
	if primary == KindImplementation {
		testGen := mentionsTestGeneration(prompt)
		testExec := mentionsTestExecution(prompt)
		if testGen {
			genIdx := b.add(KindTesting, "Write tests", "Generate tests covering the implementation", []string{taskID(mainIdx)})
			if testExec {
				b.add(KindTestExecution, "Run tests", "Execute the test suite", []string{taskID(genIdx)})
			}
		} else if testExec {
			b.add(KindTestExecution, "Run tests", "Execute the test suite", []string{taskID(mainIdx)})
		}
	}

	// Rule: a search request that also asks to implement produces an
	// implementation task that depends on the search.
	if primary == KindRepositorySearch && len(b.tasks) == 1 && mentionsImplementation(prompt) {
		b.add(KindImplementation, "Implement changes", "Implement based on search findings", []string{taskID(mainIdx)})
	}

	tasks := finalizeTasks(b.tasks, prompt)

	plan := ExecutionPlan{
		PlanID:       planID(input),
		Summary:      summaryFor(primary, tasks),
		Tasks:        tasks,
		Dependencies: collectDependencies(tasks),
		Metadata:     buildMetadata(input, primary, tasks),
	}
	if err := Validate(plan); err != nil {
		return ExecutionPlan{}, err
	}
	return plan, nil
}

// taskBuilder accumulates tasks during rule application. Dependencies are stored
// by index reference and resolved to IDs during finalize.
type taskBuilder struct {
	tasks []Task
}

func (b *taskBuilder) add(kind TaskKind, title, desc string, deps []string) int {
	idx := len(b.tasks)
	b.tasks = append(b.tasks, Task{
		TaskKind:     kind,
		Title:        title,
		Description:  desc,
		Dependencies: deps,
	})
	return idx
}

func finalizeTasks(raw []Task, prompt string) []Task {
	out := make([]Task, len(raw))
	ids := make([]string, len(raw))
	for i := range raw {
		ids[i] = taskID(i)
	}
	for i := range raw {
		t := raw[i]
		t.ID = ids[i]
		t.Priority = 100 - i*10
		t.Status = StatusPlanned
		t.EstimatedComplexity = estimateComplexity(t.TaskKind, prompt)
		t.SafetyLevel = estimateSafety(t.TaskKind, prompt)
		t.RequiredCapabilities = kindCapabilities[t.TaskKind]
		t.Dependencies = cleanDependencies(t.Dependencies, t.ID, ids)
		out[i] = t
	}
	return out
}

// cleanDependencies dedupes, drops self-references and unknown ids, and sorts for
// deterministic ordering.
func cleanDependencies(deps []string, selfID string, validIDs []string) []string {
	valid := make(map[string]bool, len(validIDs))
	for _, id := range validIDs {
		valid[id] = true
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		if d == selfID || !valid[d] || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func estimateComplexity(k TaskKind, prompt string) Complexity {
	switch k {
	case KindArchitecture:
		return ComplexityLarge
	case KindSecurityReview, KindRefactoring, KindDebugging:
		return ComplexityMedium
	case KindRepositorySearch, KindCodeReview, KindDocumentation, KindTesting, KindTestExecution, KindImageAnalysis:
		return ComplexitySmall
	case KindImplementation:
		switch {
		case len(prompt) > 200:
			return ComplexityLarge
		case len(prompt) > 80:
			return ComplexityMedium
		default:
			return ComplexitySmall
		}
	default:
		return ComplexityUnknown
	}
}

func estimateSafety(k TaskKind, prompt string) SafetyLevel {
	if k == KindShellOperation {
		if isDestructive(prompt) {
			return SafetyDangerous
		}
		return SafetyNeedsApproval
	}
	switch k {
	case KindImplementation, KindRefactoring, KindDebugging, KindUnknown:
		return SafetyNeedsApproval
	default:
		return SafetySafe
	}
}

// isDestructive reports whether a shell prompt contains destructive keywords.
func isDestructive(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, kw := range []string{"delete", "remove", "rm -rf", "rm ", "kill", "reset", "chmod", "uninstall", "format", "truncate", "drop table"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// ---- prompt rule helpers ----

func mentionsImplementation(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, kw := range []string{"implement", "add ", "create ", "build ", "develop"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func mentionsTestGeneration(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, kw := range []string{"write tests", "add tests", "create tests", "tests for", "with tests", "and test", "generate tests"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func mentionsTestExecution(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, kw := range []string{"run tests", "run the tests", "execute tests", "test suite", "run the full test suite"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// detectSearchDomains returns the deterministic, ordered set of search domains
// mentioned in a prompt that also contains the word "search".
func detectSearchDomains(prompt string) []string {
	lower := strings.ToLower(prompt)
	if !strings.Contains(lower, "search") {
		return nil
	}
	type domain struct {
		keyword string
		label   string
	}
	candidates := []domain{
		{"docs", "documentation"},
		{"documentation", "documentation"},
		{"readme", "documentation"},
		{"code", "code"},
		{"repo", "code"},
		{"source", "code"},
		{"api", "api"},
		{"database", "database"},
		{"db ", "database"},
		{"config", "config"},
		{"settings", "config"},
		{"files", "files"},
		{"file system", "files"},
	}
	seen := make(map[string]bool)
	var out []string
	for _, c := range candidates {
		if strings.Contains(lower, c.keyword) && !seen[c.label] {
			seen[c.label] = true
			out = append(out, c.label)
		}
	}
	return out
}

// ---- titles / summaries / metadata ----

func titleFor(k TaskKind, prompt string) string {
	switch k {
	case KindImplementation:
		return "Implement changes"
	case KindRepositorySearch:
		return "Search the repository"
	case KindCodeReview:
		return "Review code"
	case KindSecurityReview:
		return "Security review"
	case KindArchitecture:
		return "Design architecture"
	case KindDocumentation:
		return "Write documentation"
	case KindTesting:
		return "Write tests"
	case KindTestExecution:
		return "Run tests"
	case KindDebugging:
		return "Debug issue"
	case KindRefactoring:
		return "Refactor code"
	case KindShellOperation:
		return "Run shell operation"
	case KindImageAnalysis:
		return "Analyze image"
	default:
		return "Clarify request"
	}
}

func descriptionFor(k TaskKind, prompt string) string {
	base := "Task derived deterministically from the user request."
	if strings.TrimSpace(prompt) == "" {
		return "Empty prompt: no actionable request could be classified."
	}
	return base
}

func summaryFor(primary TaskKind, tasks []Task) string {
	if len(tasks) == 0 {
		return "empty plan"
	}
	if len(tasks) == 1 {
		return fmt.Sprintf("Single %s task", primary)
	}
	return fmt.Sprintf("%d tasks starting with %s", len(tasks), primary)
}

func buildMetadata(input PlannerInput, primary TaskKind, tasks []Task) map[string]string {
	tools := append([]string(nil), input.AvailableTools...)
	sort.Strings(tools)
	meta := map[string]string{
		"primary_kind":       string(primary),
		"confidence":         string(input.TaskClassification.Confidence),
		"repository_present": strconv.FormatBool(input.RepositoryPresent),
		"tool_count":         strconv.Itoa(len(input.AvailableTools)),
		"task_count":         strconv.Itoa(len(tasks)),
		"prompt_present":     strconv.FormatBool(strings.TrimSpace(input.Prompt) != ""),
	}
	return meta
}

// planID is deterministic: identical inputs always yield the same id. It never
// includes a timestamp or random source.
func planID(input PlannerInput) string {
	secondary := make([]string, 0, len(input.TaskClassification.Secondary))
	for _, s := range input.TaskClassification.Secondary {
		secondary = append(secondary, string(s))
	}
	sort.Strings(secondary)
	tools := append([]string(nil), input.AvailableTools...)
	sort.Strings(tools)

	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(input.Prompt))))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(string(input.TaskClassification.Primary)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.Join(secondary, ",")))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatBool(input.RepositoryPresent)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.Join(tools, ",")))
	return fmt.Sprintf("plan-%016x", h.Sum64())
}
