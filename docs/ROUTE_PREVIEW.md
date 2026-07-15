# `zero route-preview`

Local, dry-run inspector for Zero's task classification and model-router
decisions. It shows how Zero would classify a prompt and rank the curated model
registry for it — without ever touching a model provider.

## Purpose

`route-preview` answers two questions, fully offline:

1. **How would Zero classify this task?** via `internal/taskclass`.
2. **Which models would the router rank, and why?** via `internal/modelrouter`.

It is an inspection tool only. It does **not** affect real Zero execution yet.

## Examples

```bash
zero route-preview "Implement OAuth login"

zero route-preview "Review this pull request for security vulnerabilities"

zero route-preview --provider anthropic \
  "Design cloud session sync architecture"

zero route-preview --require-known-price \
  "Review this pull request for security vulnerabilities"

zero route-preview --json "Analyze this screenshot"
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
| `--json` | Emit stable machine-readable JSON. |
| `-h`, `--help` | Show help. |

A prompt is required (positional or `--prompt`). Flags may be repeated where
noted; everything else rejects duplicates/contradictions with a non-zero exit
code.

## Classification output

```
Task classification:
  Primary: security_review
  Secondary: code_review
  Confidence: high

Required capabilities:
  - tool-calling
  - streaming
  - reasoning

Classified evidence:
  - exact:security vulnerabilities: matched phrase security vulnerabilities -> security_review
  ...
```

The classification is produced by `internal/taskclass.Classify` from the prompt
and a deterministically detected repository presence (a local `.git` entry in the
working directory). No images are attached and no LLM is used.

## Ranking output

```
Selected candidate:
  Model: claude-haiku-4.5
  Provider: anthropic
  Score: <integer>

Reasons:
  - satisfies required capabilities: tool-calling, streaming, reasoning
  - includes N unnecessary capability(ies)
  - known price input=$1.00 output=$5.00 source=...

Ranked candidates:
  1. claude-haiku-4.5 [anthropic] score=-230
  ...

Rejected candidates:
  <model id>
    - missing required capability: reasoning
    - provider not in allowed set: ...
    - model is explicitly disallowed: ...
```

Scoring is deterministic and integer-based (documented in
`internal/modelrouter/README.md`): explicit preferred model dominates, then
preferred provider, then exact capability fit, then lower known cost, then
registry order as a final tie-break. No subjective quality tiers or live metrics
are used.

When **no compatible model** exists, the command prints a clear explanation and
all rejection reasons, and exits `0` (it is a preview, not a failure).

## JSON output

```json
{
  "prompt": "...",
  "classification": {
    "primary": "security_review",
    "secondary": ["code_review"],
    "confidence": "high",
    "required_capabilities": ["tool-calling", "streaming", "reasoning"],
    "evidence": [{ "signal": "...", "detail": "..." }]
  },
  "decision": {
    "selected": { "model": "...", "provider": "...", "score": 0, "reasons": [...] },
    "ranked": [ ... ],
    "rejected": [ { "model_id": "...", "reasons": [...] } ]
  }
}
```

Output is stable and deterministic (field order and slice order are fixed).

## Local-only behavior

`LocalOnly` is intentionally **not** exposed. The router's local-only handling
requires a runtime predicate (the registry has no stable local/remote property),
which this command does not currently supply. As a result, `--local` is not a
flag here.

## Limitations

- Reads the curated model registry (`internal/modelregistry.DefaultRegistry`).
  It does **not** fetch models from configured provider profiles or discovery
  APIs, and it does not include runtime-only models beyond those already in the
  local registry.
- Prices come from the registry (and the existing CLI `models.dev` overlay cache,
  an offline local cache — not a network call). Treat them as advisory.
- Deprecated models are reported and may appear as rejected unless they are the
  explicit preference or the only candidate. Upgrade targets are reported, never
  auto-applied.
- This command does **not** change the active provider or model, does not execute
  tools, does not create a session, does not persist memory, and does not emit
  telemetry.

## Explicit non-goals (this command does not affect real execution yet)

`route-preview` is read-only. A future opt-in integration path will consume the
`Decision` to actually select and invoke a model; until then it is purely an
inspector.
