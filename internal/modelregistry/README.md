# Model Capability Registry

`internal/modelregistry` is the **single source of truth for the stable, factual
capabilities of every model Zero supports**. Future systems build on top of it:

- Multi-Model Router
- Embedded LLM
- Planning
- Parallel Workers
- Tool Calling
- Cost Optimizer
- Provider Selection
- Fallback
- Auto Routing

## What the registry owns

Pure, stable, factual model metadata — no behavior, no decisions:

- **Identity**: `ID` (canonical), `DisplayName`, `APIModel`, `Aliases`,
  `MatchPatterns`, `Description`.
- **Provider**: `Provider` (primary kind), `APIProviders` (runtime adapters).
- **Context**: `ContextLimits{ContextWindow, MaxOutputTokens}`.
- **Capabilities** (`[]ModelCapability`): `chat`, `streaming`, `tool-calling`,
  `vision`, `json-mode`, `reasoning`, `system-prompt`, `prompt-cache`,
  `long-context`, `parallel-tool-calls`, `thinking-tokens`, `image-generation`,
  `embeddings`, `audio`.
- **Reasoning**: `ReasoningEfforts`, `DefaultReasoningEffort`.
- **Pricing**: `ModelCost` (currency, per-1M rates, tiers, source + verification
  date) — the registry records pricing; it does not compute spend policy.
- **Lifecycle**: `Status` (active / preview / deprecated), `Deprecation`,
  `UpgradeTargetID`.

## What the registry does NOT own

These belong to other packages and are intentionally out of scope:

- **Routing / selection / scoring / fallback policy** — those live in the
  future router, cost optimizer, planner, etc. The registry only exposes
  capabilities; it never chooses.
- **Provider transport & auth metadata** — `OAuth`, `Local`, API formats, base
  URLs, and auth env vars live in `internal/providercatalog`. The registry
  references a provider by `ProviderKind` only; it does not duplicate provider
  metadata. Consequently, "OpenAI-compatible" and "OAuth" are provider
  properties, not per-model facts, and are not stored on `ModelEntry`.
- **Subjective tiers** — quality / coding / reasoning / latency rankings are not
  recorded; they are policy inputs for a router, not stable model facts.
- **Language-quality claims** — not maintained; too broad to assert truthfully.
- **Live model discovery / catalog overlays** — `internal/providermodeldiscovery`,
  `internal/providermodelcatalog`, and the models.dev overlay feed the curated
  catalog; the registry consumes the result.
- **Terminal / UI rendering**, sandboxing, and provider clients.

## Canonical model identity

- The **model `ID` is the canonical identity and registry key**. A model is
  registered under its `ID`, its `APIModel`, every `Alias`, and every
  `MatchPatterns` entry — all normalized to lower case.
- **Provider-qualified forms** (e.g. `openai:gpt-4.1`) are *aliases*, not
  separate identities. Resolution normalizes the input and looks it up.
- **Duplicate canonical IDs are rejected** at `NewRegistry`. The `ID` namespace
  is global: two models from different providers may not share an `ID`.
- `Provider` is an *attribute* of the model, not part of the key.
- Fetched entries are deep-cloned on registration, so mutating a value returned
  by `Get`/`List` never affects the registry.

## Validation rules

`ModelEntry.Validate()` runs at `NewRegistry` construction:

- `ID`, `DisplayName`, `APIModel` required; at least one non-blank `Alias`.
- `Provider` must be a valid **primary** kind (openai / anthropic / google);
  `openai-compatible` is a runtime adapter, not a primary provider.
- `ContextWindow > 0`, `MaxOutputTokens > 0`, `MaxOutputTokens <= ContextWindow`.
- At least one capability; every capability must be a known `ModelCapability`.
- `ReasoningEfforts` valid; `DefaultReasoningEffort` must be supported.
- `ModelCost` in `USD` / `per_1m_tokens`, non-negative, with a `Source` and a
  `SourceLastVerified` date (YYYY-MM-DD).
- `Status` known; a `Deprecation` rule requires a resolvable `FallbackID`.
- `APIProviders` must be known runtime kinds and must include the primary.
- **Capability combinations** (factual contradictions only):
  - an `embeddings` model must not also declare `vision`, `tool-calling`,
    `reasoning`, `streaming`, `image-generation`, or `audio`;
  - a `reasoning` model must expose at least one reasoning effort or
    `thinking-tokens`.

`NewRegistry` additionally rejects duplicate canonical IDs and any
`Deprecation`/`UpgradeTargetID` that does not resolve to a known model.

## How future routing code should consume it

Depend on the concrete `Registry` (or define a narrow interface at the call
site) and **read** capabilities to make decisions elsewhere:

```go
reg, _ := modelregistry.DefaultRegistry()
if entry, ok := reg.Resolve("gpt-4.1"); ok {
    if entry.Supports(modelregistry.ModelCapabilityToolCalling) {
        // hand off to the router / tool-calling system — selection happens there
    }
}
```

Routing policy, scoring, fallback, and auto-routing are **intentionally outside
this package** so the registry stays a deterministic, testable source of facts
that every consumer can rely on without pulling in selection logic.

## Construction

```go
reg, err := modelregistry.DefaultRegistry()        // curated catalog
reg, err := modelregistry.NewRegistry(entries)     // your own entries (validated)
```
