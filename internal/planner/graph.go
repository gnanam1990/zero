package planner

import (
	"fmt"
	"sort"
)

// collectDependencies derives the directed edges of the execution graph from each
// task's dependency list. Edges are deduplicated and sorted for determinism.
func collectDependencies(tasks []Task) []TaskDependency {
	seen := make(map[TaskDependency]bool)
	var edges []TaskDependency
	for _, t := range tasks {
		for _, dep := range t.Dependencies {
			edge := TaskDependency{From: t.ID, To: dep}
			if seen[edge] {
				continue
			}
			seen[edge] = true
			edges = append(edges, edge)
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	return edges
}

// TopoSort returns task ids in a valid dependency order (prerequisites first).
// It returns an error if the graph contains a cycle, proving the produced plan
// is acyclic.
func TopoSort(tasks []Task) ([]string, error) {
	byID := make(map[string]Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	indeg := make(map[string]int, len(tasks))
	adj := make(map[string][]string, len(tasks))
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if _, ok := indeg[t.ID]; !ok {
			indeg[t.ID] = 0
		}
		ids = append(ids, t.ID)
		for _, dep := range t.Dependencies {
			if _, ok := byID[dep]; !ok {
				continue
			}
			adj[dep] = append(adj[dep], t.ID)
			indeg[t.ID]++
		}
	}
	sort.Strings(ids)

	// Stable queue: always pop the smallest available id.
	var queue []string
	for _, id := range ids {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)

	var order []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		order = append(order, cur)
		neighbors := append([]string(nil), adj[cur]...)
		sort.Strings(neighbors)
		for _, next := range neighbors {
			indeg[next]--
			if indeg[next] == 0 {
				queue = append(queue, next)
			}
		}
		sort.Strings(queue)
	}
	if len(order) != len(tasks) {
		return nil, fmt.Errorf("planner: cycle detected among %d tasks", len(tasks))
	}
	return order, nil
}

// Validate ensures the plan is a well-formed DAG: every dependency references a
// real task, no task depends on itself, and there are no cycles. It is also a
// guard for the "cycle impossible" guarantee.
func Validate(plan ExecutionPlan) error {
	ids := make(map[string]bool, len(plan.Tasks))
	for _, t := range plan.Tasks {
		if t.ID == "" {
			return fmt.Errorf("planner: task with empty id")
		}
		if ids[t.ID] {
			return fmt.Errorf("planner: duplicate task id %q", t.ID)
		}
		ids[t.ID] = true
	}
	for _, t := range plan.Tasks {
		for _, dep := range t.Dependencies {
			if dep == t.ID {
				return fmt.Errorf("planner: task %q depends on itself", t.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("planner: task %q depends on unknown task %q", t.ID, dep)
			}
		}
	}
	if _, err := TopoSort(plan.Tasks); err != nil {
		return err
	}
	return nil
}
