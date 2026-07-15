# `zero plan-preview`

Local, dry-run inspector that connects Zero's existing local-only modules
end to end and shows the full planned execution — **without executing anything**.

```
Prompt
  → task classification   (internal/taskclass)
  → execution planning     (internal/planner)
  → scheduler state        (internal/scheduler)
  → per-task model routing (internal/modelrouter)
  → human-readable preview
```

It is an inspection tool only. It performs **no network calls, executes no
tools, calls no providers, creates no sessions, and modifies no files or
configuration**.

## Purpose

`plan-preview` answers, fully offline:

1. **How would Zero classify this task?** via `internal/taskclass`.
2. **What execution graph would the planner build?** via `internal/planner`.
3. **What is the scheduler's initial state for every task?** via
   `internal/scheduler` (Ready / Waiting / Blocked / Completed / Failed / Skipped).
4. **Which model would the router pick for each individual task?** via
   `internal/modelrouter`, using each task's own `RequiredCapabilities`.

## Examples

```bash
zero plan-preview "Implement OAuth login and write tests"

zero plan-preview --prompt "Refactor the provider registry"

zero plan-preview --json "Audit authentication for security issues"

zero plan-preview --show-rejected "Implement OAuth login"
```

## Flags

| Flag | Meaning |
| --- | --- |
| `--prompt "<text>"` | Supply the prompt as a flag instead of positionally. |
| `--provider <provider>` | Prefer this provider as a **ranking signal** (never a hard filter). |
| `--model <model-id>` | Prefer this model if it is compatible with the task. |
| `--allow-provider <provider>` | Repeatable hard allowlist of providers. |
| `--deny-model <model-id>` | Repeatable model denylist (matches id or alias). |
| `--require-known-price` | Reject candidates whose price is unknown/missing. |
| `--max-input-cost <number>` | Maximum registry input cost unit (USD per 1M tokens). |
| `--max-output-cost <number>` | Maximum registry output cost unit (USD per 1M tokens). |
| `--show-rejected` | Show full rejected-model details in text output. |
| `--json` | Emit stable machine-readable JSON. |
| `-h`, `--help` | Show help. |

A prompt is required (positional or `--prompt`). The router-related flags are
shared with `zero route-preview`; execution, approval, worker-count,
provider-health, and network flags are intentionally **not** exposed (this is a
preview, not a run).

## Pipeline

1. Classify the prompt with `taskclass.Classify`.
2. Produce an `ExecutionPlan` with `planner.Plan`.
3. Validate the plan (`planner.Validate`).
4. Build a scheduler with `scheduler.NewScheduler`.
5. Read the **initial** scheduler state (`Scheduler.State()`).
6. For every planned task, convert it into an independent routing request using
   the task's `RequiredCapabilities`, call `modelrouter.Decide`, and attach the
   decision to the preview.
7. Tasks are **never** transitioned to `Running` or `Completed`. The scheduler
   stays in its initial derived state; the preview reflects what *would* run.

## Task dependencies

The planner produces a deterministic DAG. A task is `waiting` until all of its
`Dependencies` are `completed`; the scheduler derives this without executing
anything. Example (implementation + tests):

```
1. Implement changes        Kind: implementation  Status: ready
2. Write tests              Kind: testing         Status: waiting
     Dependencies:
       - task-1
```

## Scheduler states

| State | Meaning |
| --- | --- |
| `ready` | all dependencies completed (a `needs_approval` task is still `ready`) |
| `waiting` | at least one dependency is not yet completed |
| `blocked` | hard-blocked: `dangerous` |
| `completed` / `failed` / `skipped` | only reachable via an explicit transition (never set by a preview) |

A `needs_approval` task is schedulable (`ready`) and is flagged with
**"Approval required before execution."** A `dangerous` task is `blocked` and
also carries that approval marker.

## Per-task routing

Each task is routed **independently** on its own `RequiredCapabilities` — the
original prompt is not used for routing. If no compatible model exists for a
task, the task stays in the plan, its routing is marked unavailable, and the
rejection reasons are shown; the preview does **not** fail unless the request
itself is invalid (e.g. a contradictory constraint or non-numeric cost flag).

## Parallel display

- Tasks currently `ready` are listed in the scheduler summary.
- Tasks marked `CanRunParallel` are shown per task (`Parallel: yes/no`) and
  summarized as `Parallel tasks (ready now):`.
- Dependency groups are implicit in each task's `Dependencies` list.

`ReadyParallel()` is intentionally **not** called in a way that could mutate
state; the preview reads the read-only `State()` snapshot.

## Approval indicators

Tasks with `needs_approval` or `dangerous` safety print
`Approval required before execution.` and the scheduler summary repeats the
approval notice when any task is blocked. This command does **not** request or
store approval.

## JSON output

```json
{
  "prompt": "...",
  "classification": { "primary": "...", "secondary": [], "confidence": "...", "required_capabilities": [], "evidence": [] },
  "plan": {
    "id": "...",
    "summary": "...",
    "tasks": [
      {
        "id": "...",
        "title": "...",
        "kind": "...",
        "dependencies": [],
        "complexity": "...",
        "safety": "...",
        "status": "...",
        "can_run_parallel": false,
        "routing": { "selected": {}, "ranked": [], "rejected": [] }
      }
    ]
  },
  "scheduler": {
    "ready": [], "waiting": [], "blocked": [], "completed": [], "failed": [], "skipped": []
  }
}
```

Output is stable and deterministic: tasks follow planner order, ranked/rejected
candidates follow the router's deterministic ordering, and scheduler buckets
follow priority-then-ID ordering. Internal mutable scheduler objects are never
serialized directly.

## Local-only behavior

`plan-preview` reads the curated model registry
(`internal/modelregistry.DefaultRegistry`) and never contacts providers or
discovery APIs. Prices come from the registry (and the offline `models.dev`
overlay cache, not a network call). No session store is constructed, no tool is
executed, and `agent.Run` is never invoked.

## Limitations

- Planning and routing are **deterministic and advisory**. They do not prove a
  task will succeed; they describe what Zero would schedule and route.
- The preview does not execute tasks, so `Running` is never observed here, and
  `Completed` / `Failed` / `Skipped` are only present if a later executor
  transitions them (out of scope for this command).
- Deprecated models are reported and may appear as rejected unless they are the
  explicit preference or the only candidate.
- This command does **not** change the active provider or model, does not write
  configuration or memory, and does not emit telemetry.

## Explicit non-goals

`plan-preview` is read-only. It does not affect real execution; a future
executor would consume the `ExecutionPlan` and `ExecutionState` to actually run
tasks.

## `zero exec --orchestration-preview`

The same dry-run pipeline is exposed inline on the real `exec` argument path, so
you can preview the orchestration of a prompt **before** launching a run — using
the exact same prompt and router constraints you would pass to `exec`:

```bash
zero exec --orchestration-preview "Implement OAuth login and write tests"

zero exec --orchestration-preview --json --provider openai "Refactor the provider registry"

zero exec --orchestration-preview --show-rejected --deny-model gpt-4 "Audit authentication"
```

Behavior:

- The preview renders from the actual `exec` prompt resolution (positional,
  `--prompt`, `--file`, or stream-json input) and then returns **before** any
  provider is constructed, any session is created, or `agent.Run` is invoked.
- A normal `zero exec` (without the flag) is byte-identical to before — the flag
  is the only branch that touches the preview path.
- The router flags (`--provider`, `--model`, `--allow-provider`, `--deny-model`,
  `--require-known-price`, `--max-input-cost`, `--max-output-cost`, `--json`,
  `--show-rejected`) are shared with `plan-preview` via `routerFlagOptions` /
  `tryParseRouterFlag`. `exec`'s `-m/--model` is read as a **routing preference**
  (not a runtime override) during the preview.
- Text output is prefixed with `ORCHESTRATION PREVIEW — no tasks will be
  executed` and ends with `Preview complete. No provider was called and no task
  was executed.`
- `--json` emits the same `plan_preview` JSON used by `plan-preview`, wrapped in
  `{"mode":"orchestration-preview","executed":false,"plan_preview":{...}}`.
- The preview cannot be combined with execution/session flags:
  `--skip-permissions-unsafe`, `--allow-escalation`, `--self-correct`,
  `--worktree`, `--use-spec`, `--list-tools`, `--resume`, `--fork`,
  `--no-completion-gate`. Used without `--orchestration-preview`, the router
  preview flags are rejected (they would otherwise be silently ignored).

Both commands call the shared `buildPlanPreview`, so they always produce
equivalent results for the same prompt and constraints.
