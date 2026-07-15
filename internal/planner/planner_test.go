package planner

import (
	"reflect"
	"testing"

	"github.com/Gitlawb/zero/internal/taskclass"
)

func input(prompt string, primary taskclass.Kind, secondary ...taskclass.Kind) PlannerInput {
	return PlannerInput{
		Prompt:             prompt,
		TaskClassification: taskclass.Result{Primary: primary, Secondary: secondary, Confidence: taskclass.ConfidenceHigh},
		RepositoryPresent:  true,
		AvailableTools:     []string{"bash", "search"},
	}
}

func findTask(plan ExecutionPlan, id string) (Task, bool) {
	for _, t := range plan.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

func TestSingleTask(t *testing.T) {
	plan, err := Plan(input("implement oauth login", taskclass.KindImplementation))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(plan.Tasks))
	}
	if plan.Tasks[0].TaskKind != KindImplementation {
		t.Fatalf("expected implementation, got %q", plan.Tasks[0].TaskKind)
	}
	if plan.Tasks[0].Status != StatusPlanned {
		t.Fatalf("expected planned status, got %q", plan.Tasks[0].Status)
	}
}

func TestMultipleTasksWithDependency(t *testing.T) {
	plan, err := Plan(input("implement oauth login and write tests", taskclass.KindImplementation))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(plan.Tasks))
	}
	impl, ok := findTask(plan, "task-1")
	if !ok || impl.TaskKind != KindImplementation {
		t.Fatalf("task-1 should be implementation, got %+v", impl)
	}
	test, ok := findTask(plan, "task-2")
	if !ok || test.TaskKind != KindTesting {
		t.Fatalf("task-2 should be testing, got %+v", test)
	}
	if !reflect.DeepEqual(test.Dependencies, []string{"task-1"}) {
		t.Fatalf("testing should depend on implementation, got %v", test.Dependencies)
	}
}

func TestDependencyGraphThreeDeep(t *testing.T) {
	plan, err := Plan(input("implement oauth login and write tests and run tests", taskclass.KindImplementation))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(plan.Tasks))
	}
	exec, _ := findTask(plan, "task-3")
	if exec.TaskKind != KindTestExecution {
		t.Fatalf("task-3 should be test_execution, got %q", exec.TaskKind)
	}
	if !reflect.DeepEqual(exec.Dependencies, []string{"task-2"}) {
		t.Fatalf("test execution should depend on testing, got %v", exec.Dependencies)
	}
	order, err := TopoSort(plan.Tasks)
	if err != nil {
		t.Fatalf("topo sort failed: %v", err)
	}
	// task-1 must precede task-2 which must precede task-3.
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if !(pos["task-1"] < pos["task-2"] && pos["task-2"] < pos["task-3"]) {
		t.Fatalf("unexpected topological order: %v", order)
	}
}

func TestParallelGraph(t *testing.T) {
	plan, err := Plan(input("search the docs for auth and search the code for auth", taskclass.KindRepoExploration))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("expected 2 parallel tasks, got %d", len(plan.Tasks))
	}
	for _, tk := range plan.Tasks {
		if tk.TaskKind != KindRepositorySearch {
			t.Fatalf("expected repository_search, got %q", tk.TaskKind)
		}
		if !tk.CanRunParallel {
			t.Fatalf("task %q should be marked parallel", tk.ID)
		}
		if len(tk.Dependencies) != 0 {
			t.Fatalf("parallel tasks must have no dependencies, got %v", tk.Dependencies)
		}
	}
}

func TestSearchThenImplement(t *testing.T) {
	plan, err := Plan(input("search auth then implement it", taskclass.KindRepoExploration))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(plan.Tasks))
	}
	impl, _ := findTask(plan, "task-2")
	if impl.TaskKind != KindImplementation {
		t.Fatalf("task-2 should be implementation, got %q", impl.TaskKind)
	}
	if !reflect.DeepEqual(impl.Dependencies, []string{"task-1"}) {
		t.Fatalf("implementation should depend on search, got %v", impl.Dependencies)
	}
}

func TestEmptyPrompt(t *testing.T) {
	plan, err := Plan(input("", taskclass.KindUnknown))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("expected 1 task for empty prompt, got %d", len(plan.Tasks))
	}
	if plan.Tasks[0].TaskKind != KindUnknown {
		t.Fatalf("expected unknown task, got %q", plan.Tasks[0].TaskKind)
	}
}

func TestAmbiguousPrompt(t *testing.T) {
	plan, err := Plan(input("do something vaguely weird", taskclass.KindUnknown))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].TaskKind != KindUnknown {
		t.Fatalf("expected single unknown task, got %+v", plan.Tasks)
	}
}

func TestStableOrdering(t *testing.T) {
	p := input("implement oauth login and write tests", taskclass.KindImplementation)
	first, err := Plan(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, err := Plan(p)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(first.Tasks) != len(again.Tasks) {
			t.Fatalf("task count changed: %d vs %d", len(first.Tasks), len(again.Tasks))
		}
		for j := range first.Tasks {
			if first.Tasks[j].ID != again.Tasks[j].ID || first.Tasks[j].TaskKind != again.Tasks[j].TaskKind {
				t.Fatalf("ordering not stable at %d: %+v vs %+v", j, first.Tasks[j], again.Tasks[j])
			}
		}
	}
}

func TestDeterministicIDs(t *testing.T) {
	p := input("implement oauth login and write tests", taskclass.KindImplementation)
	first, err := Plan(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := Plan(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.PlanID != second.PlanID {
		t.Fatalf("PlanID not deterministic: %q vs %q", first.PlanID, second.PlanID)
	}
	if first.PlanID == "" {
		t.Fatalf("PlanID should not be empty")
	}
	ids1 := make([]string, len(first.Tasks))
	ids2 := make([]string, len(second.Tasks))
	for i, t1 := range first.Tasks {
		ids1[i] = t1.ID
	}
	for i, t2 := range second.Tasks {
		ids2[i] = t2.ID
	}
	if !reflect.DeepEqual(ids1, ids2) {
		t.Fatalf("task IDs not deterministic: %v vs %v", ids1, ids2)
	}
}

func TestNoDuplicateDependencies(t *testing.T) {
	// Direct unit check of dependency cleaning.
	cleaned := cleanDependencies([]string{"task-1", "task-1", "task-2", "task-2"}, "task-3", []string{"task-1", "task-2", "task-3"})
	if !reflect.DeepEqual(cleaned, []string{"task-1", "task-2"}) {
		t.Fatalf("expected deduped sorted deps, got %v", cleaned)
	}
	// Self-reference and unknown ids dropped.
	cleaned = cleanDependencies([]string{"task-3", "ghost", "task-1"}, "task-3", []string{"task-1", "task-3"})
	if !reflect.DeepEqual(cleaned, []string{"task-1"}) {
		t.Fatalf("expected self/unknown dropped, got %v", cleaned)
	}

	// Integration: no planned task should ever carry duplicate dependencies.
	plan, err := Plan(input("implement oauth login and write tests", taskclass.KindImplementation))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tk := range plan.Tasks {
		seen := map[string]bool{}
		for _, d := range tk.Dependencies {
			if seen[d] {
				t.Fatalf("task %q has duplicate dependency %q", tk.ID, d)
			}
			seen[d] = true
		}
	}
}

func TestCycleImpossible(t *testing.T) {
	// A manually constructed cyclic plan must be rejected by Validate.
	cyclic := ExecutionPlan{
		PlanID: "plan-cycle",
		Tasks: []Task{
			{ID: "task-1", TaskKind: KindImplementation, Dependencies: []string{"task-2"}, Status: StatusPlanned},
			{ID: "task-2", TaskKind: KindTesting, Dependencies: []string{"task-1"}, Status: StatusPlanned},
		},
	}
	if err := Validate(cyclic); err == nil {
		t.Fatal("expected validation error for cyclic plan")
	}
	if _, err := TopoSort(cyclic.Tasks); err == nil {
		t.Fatal("expected topo sort error for cyclic plan")
	}

	// Plans produced by Plan must never contain a cycle.
	plan, err := Plan(input("implement oauth login and write tests", taskclass.KindImplementation))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := TopoSort(plan.Tasks); err != nil {
		t.Fatalf("Plan produced a cycle: %v", err)
	}
}

func TestRepeatabilityDeepEqual(t *testing.T) {
	p := input("search the docs for auth and search the code for auth", taskclass.KindRepoExploration)
	first, err := Plan(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := Plan(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("plans not deeply equal:\n%+v\n%+v", first, second)
	}
}

func TestSafetyClassification(t *testing.T) {
	dangerous, err := Plan(input("delete the build cache directory", taskclass.KindShellSystem))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dangerous.Tasks[0].SafetyLevel != SafetyDangerous {
		t.Fatalf("destructive shell op should be dangerous, got %q", dangerous.Tasks[0].SafetyLevel)
	}

	safe, err := Plan(input("review this pull request", taskclass.KindCodeReview))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe.Tasks[0].SafetyLevel != SafetySafe {
		t.Fatalf("code review should be safe, got %q", safe.Tasks[0].SafetyLevel)
	}
}

func TestIntegrationWithTaskclass(t *testing.T) {
	cls := taskclass.Classify(taskclass.Request{Prompt: "Implement OAuth login and write tests", RepositoryPresent: true})
	plan, err := Plan(PlannerInput{
		Prompt:             "Implement OAuth login and write tests",
		TaskClassification: cls,
		RepositoryPresent:  true,
		AvailableTools:     []string{"bash", "search"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := Validate(plan); err != nil {
		t.Fatalf("integration plan invalid: %v", err)
	}
	if len(plan.Tasks) < 2 {
		t.Fatalf("expected at least implementation + testing, got %d", len(plan.Tasks))
	}
}
