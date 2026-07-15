# Execution Scheduler

`internal/scheduler` consumes a `planner.ExecutionPlan` and produces a
deterministic `ExecutionState`. The scheduler **only decides task states** — it
never executes tasks, never calls providers, never touches files, and never
mutates the `ExecutionPlan` it was given.

It is the final link in the `feature/zero-roadmap` chain:

```
modelregistry → taskclass → planner → modelrouter → route-preview → scheduler
```

## What it does

Given a static DAG of tasks, the scheduler computes, for every task, one of:

| State      | Meaning                                                              |
|------------|---------------------------------------------------------------------|
| `planned`  | task is known but its derived state has not been computed           |
| `ready`    | all dependencies completed (a `needs_approval` task is still Ready) |
| `running`  | reserved for a future executor; the scheduler never sets this       |
| `waiting`  | at least one dependency is not yet completed                        |
| `blocked`  | hard-blocked: `SafetyDangerous` (approval-gated but not dangerous tasks stay Ready) |
| `completed`| set via `MarkCompleted`                                             |
| `failed`   | set via `MarkFailed`                                                |
| `skipped`  | set via `MarkSkipped`                                               |

Explicit terminal transitions (`completed` / `failed` / `skipped`) always win.
Otherwise the state is derived from dependency completion and risk level: a
`dangerous` task is `blocked`; a `needs_approval` task is still `ready` (the
approval requirement is a runtime gate, not a scheduler block); a task whose
dependencies are all complete is `ready`; otherwise `waiting`.

## State machine

```
        ┌─────────────────────────────────────────────────┐
        │                                                 │
planner │                                          MarkCompleted
  tasks │                                          MarkFailed
        ▼                                          MarkSkipped
   ┌────────┐   deps done   ┌───────┐  approval  ┌─────────┐
   │planned │ ────────────▶ │ ready │ ─────────▶ │ blocked │
   └────────┘               └───────┘            └─────────┘
        │                      │
        │ deps pending         │ NextReady / ReadyParallel (report only)
        ▼                      ▼
   ┌─────────┐            (future executor runs the task)
   │ waiting │            then MarkCompleted / MarkFailed / MarkSkipped
   └─────────┘
```

`running` is intentionally **not** set by the scheduler: scheduling and
execution are separate concerns. A future executor would call `NextReady`,
mark a task `running`, and on completion call `MarkCompleted` / `MarkFailed`.

## Public API

```go
s, err := scheduler.NewScheduler(plan) // validates the graph up front
ready, ok := s.NextReady()             // highest-priority ready task
parallel := s.ReadyParallel()          // ready tasks with CanRunParallel
_ = s.MarkCompleted(id)
_ = s.MarkFailed(id)
_ = s.MarkSkipped(id)
state := s.State()                     // ready/blocked/completed/waiting/...
s.Reset()                              // drop all transitions
```

`NewScheduler` rejects invalid graphs (duplicate ids, self/unknown
dependencies, cycles) by delegating to `planner.Validate`.

## Determinism

- `State()` and `NextReady()` order tasks by `Priority` (descending), then by
  `ID` (ascending). Repeated calls with the same transitions return identical
  results.
- The scheduler keeps its own copy of the plan's tasks, so the caller's
  `ExecutionPlan` is never mutated.

## Out of scope

This package does **not**: execute tasks, resolve or call model providers,
manage sessions, modify files, or render any UI. Those remain the
responsibilities of the executor and other components.
