package planner

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/taskclass"
)

const reproPrompt = `Search the entire repository source code and documentation.

Task 1: inspect documentation files and summarize the documentation structure, major topics, architecture docs, extension docs, and orchestration-related docs.

Task 2: inspect Go source files and summarize the module layout, CLI entrypoints, agent loop, tools, providers, sandbox, sessions, MCP, specialists, planner, router, scheduler, executor, and orchestration packages.

Use read-only tools only.
Do not ask clarifying questions.
Do not modify files.
Complete both tasks independently.`

func TestParseExplicitTasksReproPrompt(t *testing.T) {
	tasks := parseExplicitTasks(reproPrompt)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 explicit tasks, got %d", len(tasks))
	}
	if tasks[0].Number != 1 || tasks[1].Number != 2 {
		t.Fatalf("expected task numbers 1 and 2, got %d and %d", tasks[0].Number, tasks[1].Number)
	}
}

func TestParseExplicitTasksTwoSections(t *testing.T) {
	prompt := `Search the repository.

Task 1: inspect documentation files and summarize the structure.

Task 2: inspect Go source files and summarize the module layout.

Use read-only tools only.
Do not modify files.`

	tasks := parseExplicitTasks(prompt)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestParseExplicitTasksHashSyntax(t *testing.T) {
	prompt := `Task #1: inspect documentation.
Task #2: inspect source code.
Use read-only tools only.`

	tasks := parseExplicitTasks(prompt)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestParseExplicitTasksDashSyntax(t *testing.T) {
	prompt := `Task 1 - inspect documentation files.
Task 2 - inspect Go source files.
Use read-only tools only.`

	tasks := parseExplicitTasks(prompt)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestParseExplicitTasksCRLF(t *testing.T) {
	prompt := "Search the repo.\r\n\r\nTask 1: inspect docs.\r\n\r\nTask 2: inspect source.\r\n\r\nUse read-only tools only.\r\n"

	tasks := parseExplicitTasks(prompt)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestParseExplicitTasksGlobalConstraintsInherited(t *testing.T) {
	prompt := `Search the repo.

Task 1: inspect documentation files and summarize.

Task 2: inspect Go source files and summarize.

Use read-only tools only.
Do not modify files.
Complete both tasks independently.`

	tasks := parseExplicitTasks(prompt)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	for _, task := range tasks {
		if !strings.Contains(strings.ToLower(task.Body), "use read-only tools only") {
			t.Fatalf("task %d body missing global constraint 'use read-only tools only': %q", task.Number, task.Body)
		}
		if !strings.Contains(strings.ToLower(task.Body), "do not modify files") {
			t.Fatalf("task %d body missing global constraint 'do not modify files': %q", task.Number, task.Body)
		}
		if !strings.Contains(strings.ToLower(task.Body), "complete both tasks independently") {
			t.Fatalf("task %d body missing global constraint 'complete both tasks independently': %q", task.Number, task.Body)
		}
	}
}

func TestParseExplicitTasksSourceOrder(t *testing.T) {
	prompt := `Task 2: inspect Go source files and summarize the module layout.

Task 1: inspect documentation files and summarize the documentation structure.

Use read-only tools only.`

	tasks := parseExplicitTasks(prompt)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	// Source order, not numeric order.
	if tasks[0].Number != 2 || tasks[1].Number != 1 {
		t.Fatalf("expected source order [2, 1], got [%d, %d]", tasks[0].Number, tasks[1].Number)
	}
}

func TestParseExplicitTasksSingleHeadingDoesNotActivate(t *testing.T) {
	prompt := `Task 1: inspect documentation files and summarize the structure.
Do some other work too.`

	tasks := parseExplicitTasks(prompt)
	if tasks != nil {
		t.Fatalf("expected nil for single task heading, got %d tasks", len(tasks))
	}
}

func TestParseExplicitTasksNumberedExamplesNotMisclassified(t *testing.T) {
	// "Task 1" used as a numbered example, not an instruction.
	prompt := `The model should handle:
1. Simple prompts
2. Complex prompts
3. Edge cases

For example, given task 1, the model should respond with a summary.`

	tasks := parseExplicitTasks(prompt)
	if tasks != nil {
		t.Fatalf("expected nil for numbered examples, got %d tasks", len(tasks))
	}
}

func TestParseExplicitTasksEmptySectionsRejected(t *testing.T) {
	prompt := `Task 1:
Task 2:
Use read-only tools only.`

	tasks := parseExplicitTasks(prompt)
	if tasks != nil {
		t.Fatalf("expected nil for empty sections, got %d tasks", len(tasks))
	}
}

func TestParseExplicitTasksSharedContextPrepended(t *testing.T) {
	prompt := `Search the entire repository source code and documentation.

Task 1: inspect documentation files and summarize the structure.

Task 2: inspect Go source files and summarize the module layout.

Use read-only tools only.`

	tasks := parseExplicitTasks(prompt)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	for _, task := range tasks {
		if !strings.Contains(task.Body, "Search the entire repository") {
			t.Fatalf("task %d missing shared context: %q", task.Number, task.Body[:100])
		}
	}
}

// --- Planner tests ---

func TestPlanExplicitTwoTasks(t *testing.T) {
	cls := taskclass.Classify(taskclass.Request{Prompt: reproPrompt, RepositoryPresent: true})
	plan, err := Plan(PlannerInput{
		Prompt:             reproPrompt,
		TaskClassification: cls,
		RepositoryPresent:  true,
	})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(plan.Tasks))
	}
	for _, task := range plan.Tasks {
		if !task.CanRunParallel {
			t.Fatalf("task %s should be parallel", task.ID)
		}
		if task.SafetyLevel != SafetySafe {
			t.Fatalf("task %s should be safe, got %s", task.ID, task.SafetyLevel)
		}
		if len(task.Dependencies) != 0 {
			t.Fatalf("task %s should have no deps, got %v", task.ID, task.Dependencies)
		}
		if task.TaskKind != KindRepositorySearch {
			t.Fatalf("task %s should be repository_search, got %s", task.ID, task.TaskKind)
		}
	}
}

func TestPlanExplicitDeterministicIDs(t *testing.T) {
	cls := taskclass.Classify(taskclass.Request{Prompt: reproPrompt, RepositoryPresent: true})
	input := PlannerInput{
		Prompt:             reproPrompt,
		TaskClassification: cls,
		RepositoryPresent:  true,
	}
	first, _ := Plan(input)
	second, _ := Plan(input)
	if first.PlanID != second.PlanID {
		t.Fatalf("PlanID not deterministic: %s vs %s", first.PlanID, second.PlanID)
	}
	if len(first.Tasks) != len(second.Tasks) {
		t.Fatalf("task count changed: %d vs %d", len(first.Tasks), len(second.Tasks))
	}
	for i := range first.Tasks {
		if first.Tasks[i].ID != second.Tasks[i].ID {
			t.Fatalf("task %d ID changed: %s vs %s", i, first.Tasks[i].ID, second.Tasks[i].ID)
		}
		if first.Tasks[i].Title != second.Tasks[i].Title {
			t.Fatalf("task %d title changed: %q vs %q", i, first.Tasks[i].Title, second.Tasks[i].Title)
		}
	}
}

func TestPlanImplicitUnchanged(t *testing.T) {
	// A simple implicit search prompt should NOT use explicit decomposition.
	prompt := "Search the docs and search the code"
	cls := taskclass.Classify(taskclass.Request{Prompt: prompt, RepositoryPresent: true})
	plan, err := Plan(PlannerInput{
		Prompt:             prompt,
		TaskClassification: cls,
		RepositoryPresent:  true,
	})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("expected 2 implicit tasks, got %d", len(plan.Tasks))
	}
}

func TestPlanExplicitRegressionPrompt(t *testing.T) {
	// The exact reproduction prompt must produce exactly 2 tasks.
	cls := taskclass.Classify(taskclass.Request{Prompt: reproPrompt, RepositoryPresent: true})
	plan, err := Plan(PlannerInput{
		Prompt:             reproPrompt,
		TaskClassification: cls,
		RepositoryPresent:  true,
	})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("REGRESSION: expected 2 tasks for the reproduction prompt, got %d", len(plan.Tasks))
	}
	// Both should be ready and parallel.
	for _, task := range plan.Tasks {
		if task.SafetyLevel != SafetySafe {
			t.Fatalf("task %s safety should be safe, got %s", task.ID, task.SafetyLevel)
		}
		if !task.CanRunParallel {
			t.Fatalf("task %s should be parallel", task.ID)
		}
	}
}
