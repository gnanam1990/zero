# modelrouter

Deterministic, local model-router foundation for Zero.

`modelrouter` consumes a `taskclass.Result`, a set of `modelregistry.ModelEntry`
candidates, and runtime routing preferences, then returns a **ranked set of model
candidates with explainable scores**. It is an advisory input to a future model
router/executor. It does **not** select a model for execution, call any provider,
measure latency, fetch prices, mutate configuration, authorize tools, start
workers, retry, execute fallbacks, or use an LLM.

## I/O contract

- **Input** (`Request`): `Task` (classification), `Candidates`, `PreferredProvider`,
  `PreferredModel`, `AllowedProviders`, `DisallowedModels`, `LocalOnly` +
  `IsLocal` predicate, `MaxInputCost`/`MaxOutputCost`, `RequireKnownPrice`.
- **Output** (`Decision`): `Selected *Candidate`, `Ranked []Candidate`,
  `Rejected []Rejection`, `NoCompatible bool`.
- **Candidate**: `Model`, integer `Score`, `Reasons []Reason`.
- **Rejection**: `ModelID`, `Reasons []Reason`. All scoring is deterministic
  integer/ordinal; there are no fabricated probabilities.

## Hard filters (rejection reasons)

A candidate is rejected when any of these hold (reasons listed in deterministic
order):

1. `invalid` — model entry missing id/capabilities or has an invalid
   lifecycle status / negative price (a minimal, router-safe check; the full
   registry `Validate` is intentionally not required so custom candidates
   without full pricing metadata are accepted).
2. `capability-missing` — a required capability from the task is absent.
3. `provider-disallowed` — primary provider (and all `APIProviders`) not in
   `AllowedProviders` (when set).
4. `model-disallowed` — id or alias is in `DisallowedModels`.
5. `local-only` — `LocalOnly` set but the `IsLocal` predicate returns false.
6. `price-missing` — `RequireKnownPrice` set but the model has no price.
7. `cost-input` / `cost-output` — a **known** price exceeds the corresponding
   limit.
8. `lifecycle-deprecated` — deprecated and not explicitly preferred while an
   active alternative exists (see Lifecycle).

Missing price is **never** treated as free: cost limits and the cost ranking
tie-breaker only apply to candidates with known price.

## Deterministic ranking

Surviving candidates are scored with integer weights (higher ranks higher):

1. Explicit `PreferredModel` (if compatible) — dominant bonus.
2. `PreferredProvider` match — ranking signal only, never a filter.
3. Exact capability fit (no unnecessary extras) — bonus; each extra capability
   is a small penalty (tie-breaker, not a filter).
4. Lower known cost — small penalty (cheaper ranks higher), only when priced.
5. Final tie-break: original candidate/registry order, then model ID ascending.

No subjective quality tiers, coding scores, vendor favoritism, live latency,
historical metrics, or hidden provider defaults are used.

## Capability matching

Required capabilities come from `Task.RequiredCapabilities` (already derived by
`taskclass` from factual signals). The router requires **every** mandatory
capability, preserves deterministic capability ordering, explains each missing
capability, ignores unknown capability values (so they can never silently reject
all models), and never infers features from model names.

## Cost semantics

- Uses only existing `modelregistry.ModelCost` types. No currency conversion, no
  external fetch, no assumed-free missing price.
- Input and output costs are compared independently against their limits.
- Unknown price is allowed unless `RequireKnownPrice` is set.
- Source/date metadata (`Cost.Source`, `Cost.SourceLastVerified`) is preserved
  and reported in reasons; it is never modified.

## Lifecycle handling

- `active` / `preview` models are always eligible (subject to other filters).
- An unknown lifecycle status is treated as invalid and rejected.
- `deprecated` models are rejected **unless** they are the explicitly preferred
  model **or** no non-deprecated candidate exists. When kept, the deprecation
  `UpgradeTargetID`/`FallbackID` is reported in a reason — never automatically
  applied. There is no hidden model substitution.

## Local-only handling

The `modelregistry` entry has **no stable local/remote property**, so the router
does not guess locality from provider names. `LocalOnly` is enforced only via a
caller-supplied `IsLocal func(ModelEntry) bool` predicate. Requesting
`LocalOnly` without `IsLocal` is a contradictory constraint and `Decide` returns
an error. This avoids reintroducing speculative hosting fields into the registry.

## Explainability

Every selected, ranked, and rejected model carries stable `Reason` entries
(`Signal` + `Detail`). Reason ordering is fixed per outcome (capability → fit →
provider → preferred model → local → cost → tie-break for rankings; filter order
above for rejections).

## What the router does NOT do

No provider calls, no live latency, no price fetching, no config mutation, no
tool authorization, no worker spawning, no retries, no fallback execution, no
learning from history, no subjective model superiority claims, and no model
selection for execution.

## Integration boundary

This package is not wired into `agent.Run`, the TUI model picker, `zero exec`,
provider construction, or config resolution. It is independently testable; a
future prompt will add an opt-in integration path that consumes `Decision` to
actually pick and invoke a model.
