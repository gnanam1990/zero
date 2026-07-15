# planner

Deterministic execution-graph planner for Zero.

`planner` sits between the task classifier (`internal/taskclass`) and the model
router (`internal/modelrouter`). It converts one user request into a
**deterministic execution graph (DAG)** of tasks. It performs **no execution**,
**no LLM calls**, **no provider calls**, and **no automatic AI planning**. Every
decision is made by fixed, auditable rules.

## Responsibility

Given a `PlannerInput` (prompt, classification, repository presence, available
tools), the planner produces an `ExecutionPlan`:

- `PlanID` — deterministic id derived from the inputs (FNV-1a hash; never a
  timestamp or random source).
- `Summary` — one-line description of the graph.
- `Tasks` — ordered `Task` nodes.
- `Dependencies` — explicit directed edges (`From` depends on `To`).
- `Metadata` — deterministic facts (primary kind, confidence, repo presence,
  tool count, task count).

It does **not** call the router, providers, `agent.Run`, or the CLI.

## Task taxonomy

`TaskKind` is a small fixed set: `implementation`, `repository_search`,
`code_review`, `security_review`, `architecture`, `documentation`, `testing`,
`test_execution`, `debugging`, `refactoring`, `shell_operation`,
`image_analysis`, `unknown`. Classifier kinds are mapped to these
deterministically via `mapKind`.

## Task lifecycle

Every task is created in exactly one status: `planned`. No other lifecycle state
exists at this stage — execution/scheduling belongs to a future integrator.

Each `Task` carries: `ID`, `Title`, `TaskKind`, `Description`,
`RequiredCapabilities` (factual `modelregistry.ModelCapability` values, reused
from the registry vocabulary), `Dependencies`, `Priority`, `CanRunParallel`,
`EstimatedComplexity`, `SafetyLevel`, `Status`.

## Graph / dependency model

The graph is a DAG. Tasks may depend on earlier tasks. Edges come from fixed
rules, for example:

- `implement … and write tests` → Implementation → Testing (depends on
  Implementation).
- `… and run tests` → adds TestExecution depending on Testing.
- `search … then implement` → RepositorySearch → Implementation (depends on
  search).
- `review security` → a single SecurityReview task.
- `search the docs … and search the code` → two independent RepositorySearch
  tasks.

`collectDependencies` derives edges from each task's `Dependencies`. `TopoSort`
(Kahn's algorithm) returns a valid prerequisite-first order and **fails on a
cycle**; `Validate` rejects self-dependencies, unknown dependencies, duplicate
task ids, and cycles. Because edges only ever point from a later task to an
earlier one, cycles are impossible by construction, and `Validate`/`TopoSort`
guard that guarantee.

## Parallel model

`CanRunParallel` marks tasks that *could* eventually run simultaneously. The
planner only **marks** them; it never executes. Currently a multi-domain search
request ("search docs and search code") produces sibling `repository_search`
tasks with `CanRunParallel = true` and no inter-dependencies.

## Complexity model

`EstimatedComplexity` is a simple, non-AI ordinal: `small`, `medium`, `large`,
`unknown`. It is derived from the task kind (e.g. architecture → large; search →
small) with a prompt-length adjustment for implementation tasks.

## Safety model

`SafetyLevel` is `safe`, `needs_approval`, or `dangerous`:

- `safe`: code review, security review, documentation, testing, test execution,
  repository search, image analysis, architecture.
- `needs_approval`: implementation, refactoring, debugging, unknown.
- `dangerous`: shell operations whose prompt contains destructive keywords
  (`delete`, `remove`, `rm`, `kill`, `reset`, `chmod`, `uninstall`, `format`,
  `truncate`, …); otherwise a shell operation is `needs_approval`.

## Future router integration

A future step can feed `Task.RequiredCapabilities` (and the task kind) into
`internal/modelrouter` to select a model per task. The planner intentionally
only emits capabilities and does not select models.

## Future execution integration

A future executor can consume `ExecutionPlan.Tasks` in `TopoSort` order, honoring
`Dependencies`, `CanRunParallel`, `Priority`, and `SafetyLevel` (e.g. pausing for
`needs_approval`/`dangerous` before acting). The planner remains a pure,
side-effect-free producer of that graph.
