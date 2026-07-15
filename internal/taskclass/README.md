# taskclass

Deterministic, local task classification for Zero.

`taskclass` categorizes a user's request into a stable **task kind** and lists the
**model capabilities** that kind factually requires. It is one input to a future
model router. It does **not** select a model, call a provider, execute work, or
use an LLM, and it has no network access.

## I/O

- **Input** (`Request`): `Prompt`, `HasImages`, `RepositoryPresent`,
  `RequestedTools`, `ExplicitMode`.
- **Output** (`Result`): `Primary` kind, `Secondary` kinds, `RequiredCapabilities`,
  `Confidence`, and `Evidence` (the deterministic signals that fired).

## Task kinds

`unknown`, `code_search`, `repo_exploration`, `implementation`, `bug_investigation`,
`debugging`, `refactoring`, `testing`, `documentation`, `code_review`,
`security_review`, `architecture_planning`, `shell_system`, `image_visual_analysis`,
`general_explanation`.

## Rule order (deterministic)

1. **Explicit mode override** — highest priority primary selector.
2. **Attached image** — forces `image_visual_analysis` (vision required).
3. **Text detectors**, evaluated in fixed order, precedence decides primary.
   Exact phrases beat broad keywords. Notable orderings:
   - `security_review` never collapses into `code_review`.
   - `shell_system` beats generic `implementation`.
   - `bug_investigation` (why/cause) is distinct from `debugging` (fix).
   - Test **creation** (`write tests`) is distinct from test **execution**
     (`run the tests`).
   - Image text hints (e.g. "screenshot") are low precedence, so concrete
     actions win when present.

## Capability mapping

Capabilities reuse `internal/modelregistry.ModelCapability` (factual only —
no provider/model/price/quality). Examples: tool use, streaming, reasoning,
json-mode, vision, parallel-tool-calls. Output order follows a fixed
`capabilityOrder` so results are stable. Vision is added whenever an image is
present; parallel-tool-calls is added when ≥2 tools are requested.

## Confidence

`high` (exact phrase or image/mode), `medium` (keyword-only but supported),
`low` (unknown). Never a fabricated percentage.

## Non-goals

- No model/provider selection, no routing, no execution, no LLM, no network.
- No mutation of inputs; identical inputs always yield identical results.

## Router boundary

This package is advisory only. The future router (not part of this package)
consumes `RequiredCapabilities` and `Confidence` to choose a model; `taskclass`
grants no permissions and never decides *which* model runs.
