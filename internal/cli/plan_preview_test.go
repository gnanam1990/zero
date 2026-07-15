package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/planner"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/taskclass"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// planPreviewDeps returns minimal deps plus spies that record whether a provider
// or session store would have been constructed. plan-preview must never trigger
// either, proving it stays local and session-free.
func planPreviewDeps(t *testing.T) (appDeps, *bool, *bool) {
	t.Helper()
	cwd := t.TempDir()
	providerCalled := false
	sessionCalled := false
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			providerCalled = true
			return nil, nil
		},
		newSessionStore: func() *sessions.Store {
			sessionCalled = true
			return sessions.NewStore(sessions.StoreOptions{})
		},
	}
	return deps, &providerCalled, &sessionCalled
}

// runPlanPreviewCmd runs plan-preview and requests JSON output (the common case
// for assertions). Use runPlanPreviewTextCmd for text-output tests.
func runPlanPreviewCmd(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	args = append([]string{"--json"}, args...)
	deps, _, _ := planPreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps(append([]string{"plan-preview"}, args...), &stdout, &stderr, deps)
	return stdout.String(), stderr.String(), exit
}

func runPlanPreviewTextCmd(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	deps, _, _ := planPreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps(append([]string{"plan-preview"}, args...), &stdout, &stderr, deps)
	return stdout.String(), stderr.String(), exit
}

// ---- JSON shape for assertions ----

type ppRoutingRejection struct {
	ModelID string `json:"model_id"`
	Reasons []struct {
		Signal string `json:"signal"`
		Detail string `json:"detail"`
	} `json:"reasons"`
}

type ppSelected struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

type ppTask struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Kind           string   `json:"kind"`
	Status         string   `json:"status"`
	Safety         string   `json:"safety"`
	CanRunParallel bool     `json:"can_run_parallel"`
	Dependencies   []string `json:"dependencies"`
	Routing        struct {
		Selected *ppSelected          `json:"selected"`
		Rejected []ppRoutingRejection `json:"rejected"`
	} `json:"routing"`
}

type ppPlan struct {
	ID      string   `json:"id"`
	Summary string   `json:"summary"`
	Tasks   []ppTask `json:"tasks"`
}

type ppScheduler struct {
	Ready     []string `json:"ready"`
	Waiting   []string `json:"waiting"`
	Blocked   []string `json:"blocked"`
	Completed []string `json:"completed"`
	Failed    []string `json:"failed"`
	Skipped   []string `json:"skipped"`
}

type ppDoc struct {
	Prompt         string `json:"prompt"`
	Classification struct {
		Primary string `json:"primary"`
	} `json:"classification"`
	Plan      ppPlan      `json:"plan"`
	Scheduler ppScheduler `json:"scheduler"`
}

func parsePlanPreviewJSON(t *testing.T, out string) ppDoc {
	t.Helper()
	var doc ppDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("JSON parse error: %v\n%s", err, out)
	}
	return doc
}

func taskByKind(doc ppDoc, kind string) (ppTask, bool) {
	for _, tsk := range doc.Plan.Tasks {
		if tsk.Kind == kind {
			return tsk, true
		}
	}
	return ppTask{}, false
}

func rejectionDetailContains(rej ppRoutingRejection, substr string) bool {
	for _, r := range rej.Reasons {
		if strings.Contains(r.Detail, substr) {
			return true
		}
	}
	return false
}

// ---- Tests ----

func TestPlanPreviewSingleTask(t *testing.T) {
	out, stderr, exit := runPlanPreviewCmd(t, "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	doc := parsePlanPreviewJSON(t, out)
	if len(doc.Plan.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(doc.Plan.Tasks))
	}
	if doc.Plan.Tasks[0].Status != "ready" {
		t.Fatalf("expected ready, got %q", doc.Plan.Tasks[0].Status)
	}
	if doc.Plan.Tasks[0].Safety != "needs_approval" {
		t.Fatalf("expected needs_approval, got %q", doc.Plan.Tasks[0].Safety)
	}
	if doc.Plan.Tasks[0].Routing.Selected == nil {
		t.Fatalf("expected a selected model, got:\n%s", out)
	}
	// The approval requirement is surfaced as a clear marker in text output.
	textOut, _, textExit := runPlanPreviewTextCmd(t, "Implement OAuth login")
	if textExit != exitSuccess {
		t.Fatalf("text exit=%d", textExit)
	}
	if !strings.Contains(textOut, "Approval required before execution") {
		t.Fatalf("expected approval marker in text output:\n%s", textOut)
	}
}

func TestPlanPreviewImplementationAndTestsChain(t *testing.T) {
	out, stderr, exit := runPlanPreviewCmd(t, "Implement OAuth login and write tests")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	doc := parsePlanPreviewJSON(t, out)
	if len(doc.Plan.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(doc.Plan.Tasks))
	}
	impl, ok := taskByKind(doc, "implementation")
	if !ok {
		t.Fatalf("expected implementation task, got %v", doc.Plan.Tasks)
	}
	if impl.Status != "ready" {
		t.Fatalf("expected implementation ready, got %q", impl.Status)
	}
	tests, ok := taskByKind(doc, "testing")
	if !ok {
		t.Fatalf("expected testing task, got %v", doc.Plan.Tasks)
	}
	if tests.Status != "waiting" {
		t.Fatalf("expected testing waiting, got %q", tests.Status)
	}
	if len(tests.Dependencies) != 1 || tests.Dependencies[0] != impl.ID {
		t.Fatalf("expected testing depend on implementation, got %v", tests.Dependencies)
	}
	if len(doc.Scheduler.Ready) != 1 || len(doc.Scheduler.Waiting) != 1 {
		t.Fatalf("expected 1 ready + 1 waiting, got ready=%v waiting=%v", doc.Scheduler.Ready, doc.Scheduler.Waiting)
	}
}

func TestPlanPreviewSearchPlan(t *testing.T) {
	// A pure code-search prompt classifies as repository_search and yields a
	// ready search task. (When a prompt also contains implementation keywords the
	// planner anchors on implementation instead, covered by the impl/tests chain.)
	out, stderr, exit := runPlanPreviewCmd(t, "Find all references to the session store")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	doc := parsePlanPreviewJSON(t, out)
	search, ok := taskByKind(doc, "repository_search")
	if !ok {
		t.Fatalf("expected repository_search task, got %v", doc.Plan.Tasks)
	}
	if search.Status != "ready" {
		t.Fatalf("expected search ready, got %q", search.Status)
	}
	if search.Routing.Selected == nil {
		t.Fatalf("expected a selected model for the search task")
	}
}

func TestPlanPreviewSecurityReview(t *testing.T) {
	out, stderr, exit := runPlanPreviewCmd(t, "Audit authentication for security issues")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	doc := parsePlanPreviewJSON(t, out)
	sec, ok := taskByKind(doc, "security_review")
	if !ok {
		t.Fatalf("expected security_review task, got %v", doc.Plan.Tasks)
	}
	if sec.Status != "ready" {
		t.Fatalf("expected ready, got %q", sec.Status)
	}
}

func TestPlanPreviewDestructiveShellDangerous(t *testing.T) {
	out, stderr, exit := runPlanPreviewCmd(t, "Delete the build cache")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	doc := parsePlanPreviewJSON(t, out)
	shell, ok := taskByKind(doc, "shell_operation")
	if !ok {
		t.Fatalf("expected shell_operation task, got %v", doc.Plan.Tasks)
	}
	if shell.Safety != "dangerous" {
		t.Fatalf("expected dangerous, got %q", shell.Safety)
	}
	if shell.Status != "blocked" {
		t.Fatalf("expected blocked, got %q", shell.Status)
	}
}

func TestPlanPreviewParallelSearchSiblings(t *testing.T) {
	out, stderr, exit := runPlanPreviewCmd(t, "Search the docs and search the code")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	doc := parsePlanPreviewJSON(t, out)
	searches := 0
	parallel := 0
	for _, tsk := range doc.Plan.Tasks {
		if tsk.Kind != "repository_search" {
			t.Fatalf("expected repository_search tasks only, got %q", tsk.Kind)
		}
		searches++
		if tsk.CanRunParallel {
			parallel++
		}
		if tsk.Status != "ready" {
			t.Fatalf("expected all search siblings ready, got %q", tsk.Status)
		}
	}
	if searches != 2 {
		t.Fatalf("expected 2 search siblings, got %d", searches)
	}
	if parallel != 2 {
		t.Fatalf("expected 2 parallel-capable siblings, got %d", parallel)
	}
}

func TestPlanPreviewPerTaskRoutingDiffersByCapability(t *testing.T) {
	// Implementation task requires tool-calling + streaming (no reasoning), so
	// OpenAI models survive. Security-review task requires reasoning, so OpenAI
	// models are rejected — proving each task is routed independently on its own
	// RequiredCapabilities.
	implOut, _, exit := runPlanPreviewCmd(t, "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("impl exit=%d", exit)
	}
	secOut, _, exit := runPlanPreviewCmd(t, "Audit authentication for security vulnerabilities")
	if exit != exitSuccess {
		t.Fatalf("sec exit=%d", exit)
	}
	implDoc := parsePlanPreviewJSON(t, implOut)
	implTask := implDoc.Plan.Tasks[0]
	implRejectedReasoning := false
	for _, rej := range implTask.Routing.Rejected {
		if rejectionDetailContains(rej, "reasoning") {
			implRejectedReasoning = true
		}
	}
	if implRejectedReasoning {
		t.Fatalf("implementation task should not require reasoning; rejections=%v", implTask.Routing.Rejected)
	}

	secDoc := parsePlanPreviewJSON(t, secOut)
	secTask := secDoc.Plan.Tasks[0]
	secRejectedReasoning := false
	for _, rej := range secTask.Routing.Rejected {
		if rejectionDetailContains(rej, "reasoning") {
			secRejectedReasoning = true
		}
	}
	if !secRejectedReasoning {
		t.Fatalf("security task should require reasoning; rejections=%v", secTask.Routing.Rejected)
	}
}

func TestPlanPreviewPreferredModel(t *testing.T) {
	out, _, exit := runPlanPreviewCmd(t, "--model", "claude-opus-4.1", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	doc := parsePlanPreviewJSON(t, out)
	if doc.Plan.Tasks[0].Routing.Selected == nil || doc.Plan.Tasks[0].Routing.Selected.Model != "claude-opus-4.1" {
		t.Fatalf("expected claude-opus-4.1 selected, got %+v", doc.Plan.Tasks[0].Routing.Selected)
	}
}

func TestPlanPreviewPreferredProvider(t *testing.T) {
	out, _, exit := runPlanPreviewCmd(t, "--provider", "anthropic", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	doc := parsePlanPreviewJSON(t, out)
	if doc.Plan.Tasks[0].Routing.Selected == nil || doc.Plan.Tasks[0].Routing.Selected.Provider != "anthropic" {
		t.Fatalf("expected anthropic selected, got %+v", doc.Plan.Tasks[0].Routing.Selected)
	}
}

func TestPlanPreviewProviderAllowlist(t *testing.T) {
	out, _, exit := runPlanPreviewCmd(t, "--allow-provider", "openai", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	doc := parsePlanPreviewJSON(t, out)
	sel := doc.Plan.Tasks[0].Routing.Selected
	if sel == nil || sel.Provider != "openai" {
		t.Fatalf("expected openai selected, got %+v", sel)
	}
}

func TestPlanPreviewModelDenylist(t *testing.T) {
	out, _, exit := runPlanPreviewCmd(t, "--deny-model", "gpt-4o-mini", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	doc := parsePlanPreviewJSON(t, out)
	if doc.Plan.Tasks[0].Routing.Selected != nil && doc.Plan.Tasks[0].Routing.Selected.Model == "gpt-4o-mini" {
		t.Fatalf("denied model should not be selected")
	}
	for _, rej := range doc.Plan.Tasks[0].Routing.Rejected {
		if rej.ModelID == "gpt-4o-mini" {
			return
		}
	}
	t.Fatalf("expected gpt-4o-mini in rejections")
}

func TestPlanPreviewCostConstraints(t *testing.T) {
	out, _, exit := runPlanPreviewCmd(t, "--max-input-cost", "0.2", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	doc := parsePlanPreviewJSON(t, out)
	for _, rej := range doc.Plan.Tasks[0].Routing.Rejected {
		if rej.ModelID == "gpt-4o" {
			return
		}
	}
	t.Fatalf("expected gpt-4o rejected for input cost")
}

func TestPlanPreviewNoCompatibleModelForOneTask(t *testing.T) {
	// A security_review task requires reasoning; restricting to openai (which has
	// no reasoning model) leaves that task without a compatible model. The preview
	// must NOT fail — the task stays in the plan with routing unavailable.
	out, stderr, exit := runPlanPreviewCmd(t, "--json", "--allow-provider", "openai", "Audit authentication for security vulnerabilities")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	doc := parsePlanPreviewJSON(t, out)
	if len(doc.Plan.Tasks) != 1 {
		t.Fatalf("expected task still in plan, got %d", len(doc.Plan.Tasks))
	}
	if doc.Plan.Tasks[0].Routing.Selected != nil {
		t.Fatalf("expected no selected model, got %+v", doc.Plan.Tasks[0].Routing.Selected)
	}
	if len(doc.Plan.Tasks[0].Routing.Rejected) == 0 {
		t.Fatalf("expected rejections for the incompatible task")
	}
}

func TestPlanPreviewMissingPrompt(t *testing.T) {
	_, stderr, exit := runPlanPreviewCmd(t)
	if exit == exitSuccess {
		t.Fatalf("expected non-zero exit for missing prompt")
	}
	if !strings.Contains(stderr, "requires a non-empty prompt") {
		t.Fatalf("expected prompt error, got %q", stderr)
	}
}

func TestPlanPreviewEmptyPrompt(t *testing.T) {
	_, stderr, exit := runPlanPreviewCmd(t, "")
	if exit == exitSuccess {
		t.Fatalf("expected non-zero exit for empty prompt")
	}
	if !strings.Contains(stderr, "requires a non-empty prompt") {
		t.Fatalf("expected prompt error, got %q", stderr)
	}
}

func TestPlanPreviewInvalidNumeric(t *testing.T) {
	_, stderr, exit := runPlanPreviewCmd(t, "--max-input-cost", "abc", "Implement OAuth login")
	if exit == exitSuccess {
		t.Fatalf("expected non-zero exit for invalid numeric")
	}
	if !strings.Contains(stderr, "invalid --max-input-cost") {
		t.Fatalf("expected numeric error, got %q", stderr)
	}
}

func TestPlanPreviewTextDeterministic(t *testing.T) {
	first, _, exit1 := runPlanPreviewTextCmd(t, "Implement OAuth login and write tests")
	second, _, exit2 := runPlanPreviewTextCmd(t, "Implement OAuth login and write tests")
	if exit1 != exitSuccess || exit2 != exitSuccess {
		t.Fatalf("exits=%d,%d", exit1, exit2)
	}
	if first != second {
		t.Fatalf("text output not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestPlanPreviewJSONDeterministic(t *testing.T) {
	first, _, exit1 := runPlanPreviewCmd(t, "--json", "Audit authentication for security issues")
	second, _, exit2 := runPlanPreviewCmd(t, "--json", "Audit authentication for security issues")
	if exit1 != exitSuccess || exit2 != exitSuccess {
		t.Fatalf("exits=%d,%d", exit1, exit2)
	}
	if first != second {
		t.Fatalf("JSON output not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestPlanPreviewJSONParses(t *testing.T) {
	stdout, stderr, exit := runPlanPreviewCmd(t, "--json", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	doc := parsePlanPreviewJSON(t, stdout)
	if doc.Prompt == "" {
		t.Fatalf("expected non-empty prompt in JSON")
	}
	if len(doc.Plan.Tasks) == 0 {
		t.Fatalf("expected tasks in plan JSON")
	}
	for _, tsk := range doc.Plan.Tasks {
		if tsk.Routing.Selected == nil && len(tsk.Routing.Rejected) == 0 {
			t.Fatalf("task %s has neither selected nor rejected routing", tsk.ID)
		}
	}
}

func TestPlanPreviewDoesNotCreateSession(t *testing.T) {
	deps, _, sessionCalled := planPreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"plan-preview", "Implement OAuth login"}, &stdout, &stderr, deps)
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if *sessionCalled {
		t.Fatal("plan-preview must not create a session store")
	}
	if stdout.Len() == 0 {
		t.Fatal("expected output")
	}
}

func TestPlanPreviewDoesNotCallProviders(t *testing.T) {
	deps, providerCalled, _ := planPreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"plan-preview", "Delete the build cache"}, &stdout, &stderr, deps)
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if *providerCalled {
		t.Fatal("plan-preview must not construct a provider")
	}
}

func TestPlanPreviewDoesNotExecuteTools(t *testing.T) {
	// Tools cannot run without a provider or a session; both spies staying cold
	// proves no tool execution path is reached.
	deps, providerCalled, sessionCalled := planPreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"plan-preview", "Implement OAuth login and write tests"}, &stdout, &stderr, deps)
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if *providerCalled {
		t.Fatal("plan-preview must not construct a provider (no tools can run)")
	}
	if *sessionCalled {
		t.Fatal("plan-preview must not create a session (no tools can run)")
	}
}

func TestPlanPreviewDoesNotMutatePlannerOutput(t *testing.T) {
	// planner.Plan is pure; a successful plan-preview run must not leave any
	// shared global state that changes a subsequent identical plan.
	input := plannerInputForTest(t, "Implement OAuth login")
	before, err := planForTest(input)
	if err != nil {
		t.Fatalf("plan error: %v", err)
	}
	_, _, exit := runPlanPreviewCmd(t, "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	after, err := planForTest(input)
	if err != nil {
		t.Fatalf("plan error: %v", err)
	}
	if before.PlanID != after.PlanID {
		t.Fatalf("plan ID changed after run: %s vs %s", before.PlanID, after.PlanID)
	}
	if len(before.Tasks) != len(after.Tasks) {
		t.Fatalf("task count changed after run")
	}
	for i := range before.Tasks {
		if before.Tasks[i].ID != after.Tasks[i].ID || before.Tasks[i].TaskKind != after.Tasks[i].TaskKind {
			t.Fatalf("task %d changed after run", i)
		}
	}
}

func plannerInputForTest(t *testing.T, prompt string) planner.PlannerInput {
	t.Helper()
	cls := taskclass.Classify(taskclass.Request{Prompt: prompt, RepositoryPresent: false})
	return planner.PlannerInput{
		Prompt:             prompt,
		TaskClassification: cls,
		RepositoryPresent:  false,
		AvailableTools:     nil,
	}
}

func planForTest(input planner.PlannerInput) (planner.ExecutionPlan, error) {
	return planner.Plan(input)
}

func TestPlanPreviewExistingRoutePreviewUnchanged(t *testing.T) {
	out, stderr, exit := runRoutePreviewCmd(t, "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("route-preview exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(out, "Primary: implementation") {
		t.Fatalf("route-preview regressed:\n%s", out)
	}
	// Other commands still work.
	for _, cmd := range [][]string{{"models"}, {"--version"}} {
		var o, e bytes.Buffer
		deps, _, _ := planPreviewDeps(t)
		if rc := runWithDeps(cmd, &o, &e, deps); rc != exitSuccess {
			t.Fatalf("command %v exit=%d stderr=%s", cmd, rc, e.String())
		}
	}
}
