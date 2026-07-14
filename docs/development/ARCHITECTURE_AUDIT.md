# Zero Architecture Audit

> Grounded source audit of the Zero coding-agent repository.
> Branch: `audit/agent-loop-baseline` — commit `80c39aa` (docs baseline).
> Companion baseline: `docs/development/LOCAL_BASELINE.md` (all tests green).
> Scope: no production code modified; all findings cite file:line evidence.

## 1. Executive Summary

Zero is a Go CLI coding agent (~292k LOC across 1,119 Go files, 82 packages,
4,571 test functions) with three user surfaces — interactive TUI, headless
`zero exec`, and an ACP editor bridge — all converging on a single provider-neutral
agent loop (`internal/agent.Run`). The architecture is well-documented
(`docs/HOW_ZERO_WORKS.md`) and unusually test-heavy for its size (~140k LOC of
tests, 4571 test functions). The core risk is **not** correctness of the happy
path — it is **concentration of complexity** in a few giant files/functions and a
**cancellation gap** that lets shell sub-processes outlive a cancelled run.

Highest-priority risks (detail in sections 6–9):

1. **Shell command cancellation not propagated** — `exec_command.go` builds its
   own `context.Background()` instead of the run `ctx`, so Ctrl+C / run-cancel
   does not kill the child process (`internal/tools/exec_command.go:567`, `:575`).
2. **`agent.Run` is a 637-line god-function**; `model.updateModel` is a 1,444-line
   state machine; `loop.go` (2,965 LOC) and `model.go` (5,296 LOC) are god-objects.
3. **Production `panic` calls (6) with no top-level recovery in the agent loop**
   can crash the whole process (notably `internal/tui/model.go:720` at startup).
4. **58 `exec.Command` sites**, many rooted in `context.Background()`, create a
   broad cancellation/process-leak surface across subsystems.
5. **Overlapping provider/model packages** (`providercatalog`, `providerhealth`,
   `providermodelcatalog`, `providermodeldiscovery`, `providers`) suggest
   duplicated capability discovery that warrants consolidation.

**No confirmed P0 (security/data-loss) defect** was found. The cancellation gap is
the closest to a safety-critical issue and is flagged as a **P0 candidate pending
runtime verification** (see §6).

## 2. Architecture Overview

Zero is built on a four-layer "spine" (per `docs/HOW_ZERO_WORKS.md`):

- **Surface** — TUI (`internal/tui`), headless exec (`internal/cli/exec.go`),
  ACP bridge (`internal/acp`). All call the same `agent.Run`.
- **Agent loop** — `internal/agent.Run` turns, tool execution, permissions,
  compaction, retries, final answers.
- **Capabilities** — providers (`internal/providers/*`, `internal/zeroruntime`),
  tools (`internal/tools`), sandbox (`internal/sandbox`).
- **Persistence** — sessions (`internal/sessions`), config (`internal/config`).

Providers implement a single interface (`internal/zeroruntime`):

```go
type Provider interface {
    StreamCompletion(ctx context.Context, request CompletionRequest) (<-chan StreamEvent, error)
}
```

The model never mutates the workspace directly; it proposes tool calls that Zero
validates (permission metadata + sandbox engine), executes via the registry,
redacts, and appends as conversation context. This separation is the project's
central safety design and is implemented consistently.

## 3. Package Map

Measured locally (production Go files only for the size column):

| Package | Role | Prod LOC (top file) | Go files |
| --- | --- | --- | --- |
| `cmd/zero` | Tiny entrypoint → `internal/cli.Run` | — | 1 |
| `internal/cli` | Command parsing, provider/registry build, TUI/exec/ACP launch | `app.go` 1377 | 114 |
| `internal/tui` | Bubble Tea model/update/view, transcript, pickers, wizards | `model.go` 5296 | 210 |
| `internal/agent` | Core loop, tool exec, permission wiring, compaction, diagnostics | `loop.go` 2965 | 61 |
| `internal/zeroruntime` | Provider-neutral message/tool/stream/usage/image types | — | 9 |
| `internal/providers` (+`anthropic/openai/gemini/...`) | Provider adapters | — | many |
| `internal/tools` | Tool interface, registry, core + local-control tools | `exec_command.go` 987 | 97 |
| `internal/sandbox` | Permission policy, path/network checks, grant store, platform backends | `runner.go` 965 | 79 |
| `internal/sessions` | Append-only `metadata.json` + `events.jsonl` store | `store.go` 1093 | 17 |
| `internal/config` | User/project config resolution, active provider, preferences | `resolver.go` 1113 | 30 |
| `internal/mcp` | MCP server config, client runtime, permission store | `network_client.go` 896 | 28 |
| `internal/specialist` / `internal/swarm` | Sub-agent / team orchestration as tools | `builtin.go` | 31 / 22 |
| `internal/plugins` / `internal/skills` / `internal/hooks` | Extension surfaces | `plugins.go` 961 | 26 / — / — |
| `internal/acp` | Editor bridge (JSON-RPC) over the agent loop | `agent.go` | — |
| `internal/streamjson` | Machine-readable protocol for `zero exec` | — | — |

`cmd/` also contains auxiliary binaries: `zero-linux-sandbox`, `zero-seccomp`,
`zero-windows-command-runner`, `zero-windows-sandbox-setup`, `zero-perf-bench`,
`zero-pr-review`, `zero-release`.

## 4. Runtime Flow

1. **Startup** — `cmd/zero` → `cli.Run` resolves config (`internal/config`),
   builds the provider (`internal/providers.New`), builds the tool registry
   (core + config-gated local-control + specialists + MCP + plugins), creates the
   sandbox engine and session store, then launches the TUI (or exec/ACP path).
2. **Interactive turn** — TUI `model.updateModel` validates submit, starts
   `runAgentWithOptions` as a `tea.Cmd` with a cancellable `runCtx`, and calls
   `agent.Run` with callbacks (`OnText`, `OnToolCall`, `OnToolResult`,
   `OnPermission`, `OnAskUser`, `OnUsage`). Callbacks become Bubble Tea messages;
   the agent loop is the runtime source of truth.
3. **Headless exec** — same `agent.Run`; events serialized to text/JSON/stream-JSON
   by `internal/streamjson`. Requires a completion signal so a no-tool response is
   not treated as success.
4. **ACP** — JSON-RPC bridge (`internal/acp`) over `agent.Run`; currently records
   conversational messages while streaming tool/permission updates.
5. **Agent loop** (`internal/agent/loop.go:112` `Run`) — each turn: build prompt →
   partition visible tools → maybe compact → `Provider.StreamCompletion` → collect
   stream → if tool calls, execute through registry+sandbox+hooks → append result →
   repeat until final answer or stop reason. Guards: malformed/empty turns,
   max-turns fallback, context-limit reactive compaction, stream retry.
6. **Shutdown** — `cancelRun` cancels `runCtx`; deferred `runPermissions.cleanup()`
   and `postEditDiagnostics` no-op on nil. Session events flushed by the recorder.

## 5. Strengths

- **Single convergence point.** All three surfaces use one agent loop — minimal
  divergence of behavior between interactive and headless.
- **Safety-first tool model.** Model proposes; Zero validates + executes + redacts.
  Permission metadata and sandbox policy are separate, well-isolated checks.
- **Excellent test density.** 4,571 test functions; the full suite passed at
  baseline including `-race` on `internal/agent`.
- **Strong documentation.** `docs/HOW_ZERO_WORKS.md` accurately maps concepts to
  packages; `AGENTS.md` mandates checks before commit.
- **Provider-neutral types.** `internal/zeroruntime` keeps the loop ignorant of
  provider specifics; adapters are isolated in `internal/providers/*`.
- **Durable, local sessions.** Append-only `events.jsonl` enables resume/fork/rewind
  without a remote service — privacy-preserving by design.
- **Context compaction is conservative.** On summarizer failure it keeps original
  messages rather than dropping history (per docs and `compaction.go:442`).

## 6. P0 Findings

> No defect meeting the strict P0 definition (security breach, data loss, or
> critical execution failure) was **confirmed** from source alone. The item below
> is the strongest **P0 candidate** and should be runtime-verified before the
> refactor proceeds, because it sits on a safety boundary (process lifecycle).

### P0-CANDIDATE-1 — Shell sub-process survives run cancellation (safety boundary)
- **Severity:** P0 candidate (critical execution risk on the safety boundary)
- **File path:** `internal/tools/exec_command.go:567` and `:575`
- **Symbol:** `execCommandTool.startSession`
- **Evidence:**
  ```go
  commandCtx, cancel := context.WithCancel(context.Background())   // line 567
  monitor := zeroSandbox.StartDenialMonitor(context.Background(), plan.MonitorTag) // line 575
  ```
  `startSession` receives no run `ctx`; it derives `commandCtx` from
  `context.Background()`. The tool's `Run(ctx, ...)` parameter (`exec_command.go:475`)
  is never threaded into the command's lifecycle. Cancellation flows only through
  the locally-scoped `cancel()`, which is invoked by `command.Wait()` completion
  and error paths — not by run cancellation.
- **Why it matters:** When a user cancels a run (Ctrl+C / `cancelRun`), the agent
  loop's context is cancelled, but the spawned shell keeps running until it exits
  on its own. A long-lived or destructive command (e.g., a watch-rebuild loop,
  `rm`) can continue mutating the workspace after the user believes the run stopped.
  This undercuts Zero's core "side effects are visible/controlled" safety promise.
- **Recommended fix:** Thread the run `ctx` into `startSession` and use
  `context.WithCancel(ctx)`; propagate `ctx` to `StartDenialMonitor`. Keep a
  short grace timeout, then `command.Process.Kill()` on forced cancel.
- **Tests required:** Unit test asserting that cancelling `ctx` terminates the
  child process within a bounded time (use a long-sleep command); integration test
  via `zero exec` SIGINT mid-command.
- **Behaviour could change:** Yes — commands will actually be killed on cancel
  (currently they are not). This is the intended behaviour.

## 7. P1 Findings

### P1-1 — `agent.Run` is a 637-line god-function
- **Severity:** P1
- **File path:** `internal/agent/loop.go:112` (`Run`, lines 112–748)
- **Symbol:** `Run`
- **Evidence:** `awk` function-length measurement: `Run` spans 637 lines; the file
  holds 59 top-level/functions but the loop body alone is 636 lines.
- **Why it matters:** The core reliability-critical routine mixes prompt assembly,
  tool partitioning, permission/sandbox orchestration, compaction, retries, and
  final-answer logic. Large functions resist testing, review, and safe change—
  exactly the risk the refactor targets.
- **Recommended fix:** Extract cohesive units: `buildTurnContext`, `executeTurn`,
  `handleToolCalls`, `finalizeTurn`. Keep `Run` as an orchestrator with a clear
  per-turn `for` loop calling these.
- **Tests required:** No new behaviour; ensure existing `internal/agent` tests stay
  green after extraction (they currently pass with `-race`).
- **Behaviour could change:** No (refactor only).

### P1-2 — `model.updateModel` is a 1,444-line state machine
- **Severity:** P1
- **File path:** `internal/tui/model.go:?` (`updateModel`, measured 1,444 lines)
- **Symbol:** `model.updateModel`
- **Evidence:** `awk` measurement of `internal/tui/model.go` shows `updateModel`
  at 1,444 lines (file total 5,296 LOC). The TUI file is the single largest file
  in the repo by a wide margin.
- **Why it matters:** The entire UI event dispatch lives in one function. Changes
  to any surface behaviour risk regressions elsewhere; it is the hardest file to
  review or test in isolation.
- **Recommended fix:** Decompose by message family (prompt/compose, tool/permission,
  picker/modal, lifecycle). Consider a small reducer-per-feature pattern.
- **Tests required:** Golden/snapshot or headless TUI tests for each message family;
  current TUI has 123 test files but `updateModel` itself is exercised only
  end-to-end.
- **Behaviour could change:** No (refactor only).

### P1-3 — Production `panic` calls with no top-level recovery in the loop
- **Severity:** P1
- **File path:** `internal/tui/model.go:720`, `internal/usage/tracker.go:91`,
  `internal/specialist/builtin.go:40`, `internal/reasoning/catalog.go:55`,
  `internal/sandbox/windows_runner.go:699`, `internal/perfbench/perfbench.go:194`
- **Symbol:** `panic(...)`
- **Evidence:** 6 production `panic(` calls. Two are at **startup/initialization**:
  `model.go:720` panics if `modelregistry.DefaultRegistry()` errors; `usage/tracker.go:91`
  panics on the same error inside `NewTracker`. `Run` has **no** enclosing
  `recover()` (the only `recover` in `loop.go` is `compactionState.recover`, a
  method name, not panic recovery; repo-wide `recover()` count is 5, none in the
  agent loop or TUI dispatch).
- **Why it matters:** A panic in a tool, provider adapter, or registry lookup
  crashes the entire process with no graceful shutdown, no session flush, and no
  user-facing error. For a tool whose value is safe side-effect management, an
  uncaught panic during a run is a serious reliability gap.
- **Recommended fix:** Replace initialization panics with returned errors
  (`newModel`/`NewTracker` can fail fast with a clean message). Add a top-level
  `defer recover()` boundary in `Run` (and the TUI run command) that records the
  panic to the session and returns a structured error.
- **Tests required:** Tests that inject a registry error and assert a clean error
  (not a panic); a fault-injection test that a panicking tool returns an error
  result instead of crashing.
- **Behaviour could change:** Yes — failures become reported errors instead of
  process crashes (strictly better, but callers must handle the error).

### P1-4 — Broad `exec.Command` usage rooted in `context.Background()`
- **Severity:** P1
- **File path:** 58 `exec.Command` sites across subsystems (see §12); representative:
  `internal/imageinput/clipboard.go` (6), `internal/specialist/exec.go:217`
  (`ctx = context.Background()`), `internal/agenteval/*`
- **Symbol:** `exec.Command` / `context.Background()`
- **Evidence:** `rg` count: 58 `exec.Command` (non-test). Only ~30 use
  `exec.CommandContext`. Several subsystems re-root context to `context.Background()`
  (e.g., `internal/specialist/exec.go:217`, `internal/agenteval/run.go:42`).
- **Why it matters:** Sub-agents (`specialist`) and eval runs that discard the
  parent context cannot be cancelled by the parent run; orphaned processes and
  leaked goroutines accumulate. This amplifies P0-CANDIDATE-1 across the system.
- **Recommended fix:** Audit every `exec.Command` to prefer `exec.CommandContext(ctx, ...)`
  with the nearest cancellable context; remove ad-hoc `context.Background()` resets
  except where a genuinely detached, lifecycle-owned process is intended.
- **Tests required:** Cancellation tests for specialist/eval subprocesses.
- **Behaviour could change:** Yes for orphaned processes (they will now be killed).

## 8. P2 Findings

### P2-1 — God-object files (`model.go` 5,296, `loop.go` 2,965, `rendering.go` 2,499)
- **Severity:** P2
- **File path:** `internal/tui/model.go`, `internal/agent/loop.go`,
  `internal/tui/rendering.go`
- **Evidence:** LOC ranking (production): 5296 / 2965 / 2499. These three files
  alone are ~10.7k of 152.6k prod LOC.
- **Why it matters:** Concentrated ownership friction; high merge-conflict and
  review cost; discourages incremental improvement (compounds P1-1/P1-2).
- **Recommended fix:** Split along feature lines (see §16). Pair with the P1
  extractions.
- **Tests required:** Keep existing tests green.
- **Behaviour could change:** No.

### P2-2 — Overlapping provider/model discovery packages
- **Severity:** P2
- **File path:** `internal/providercatalog`, `internal/providerhealth`,
  `internal/providermodelcatalog`, `internal/providermodeldiscovery`,
  `internal/providers`
- **Evidence:** Five `provider*` packages plus `modelregistry`; responsibility
  boundaries (`catalog` vs `providermodelcatalog` vs `providermodeldiscovery`) are
  not obvious from names and overlap on "what models exist for a provider".
- **Why it matters:** Risk of duplicated model-resolution logic and inconsistent
  results between surfaces (TUI picker vs `zero exec` vs ACP).
- **Recommended fix:** Map each package's responsibility; consolidate model
  discovery into one source of truth behind `modelregistry`.
- **Tests required:** Cross-package consistency tests comparing resolved models.
- **Behaviour could change:** Possible (if duplicates diverge today).

### P2-3 — Mutable package-level synchronized state
- **Severity:** P2 (mostly safe, verify)
- **File path:** `internal/specialist/accounting.go:23` (`accountingMu`),
  `internal/oauth/manager.go:42` (`refreshLocks map[string]*sync.Mutex`),
  `internal/sandbox/scope.go:18`, `internal/swarm/scheduler.go:89/124`
- **Evidence:** 90 production `sync.Mutex/RWMutex/Map` declarations; a few are
  package-level mutable singletons (e.g., `accountingMu`, `refreshLocks`).
- **Why it matters:** Global shared state complicates testing and can cause
  cross-run interference (e.g., accounting counters, OAuth refresh locks) when
  multiple agents run in one process (swarm/daemon).
- **Recommended fix:** Prefer per-instance state; for genuine globals, document
  lifetime and add tests under concurrency.
- **Tests required:** `-race` tests exercising concurrent agent runs.
- **Behaviour could change:** No (internal only).

### P2-4 — `context.Background()` in `Run`-reaching helpers
- **Severity:** P2
- **File path:** `internal/agent/loop.go` helpers calling out to subprocess tools;
  `internal/tools/exec_command.go:543` (`collect(context.Background(), time.Second)`)
- **Evidence:** `exec_command.go:543` creates a fresh background context for a
  secondary `collect` after `ctx.Err() != nil`. This is a deliberate fallback to
  drain remaining output, but it means post-cancel reads are unbounded by the run.
- **Why it matters:** Minor; but it shows the cancellation model is inconsistent
  (one path uses `ctx`, another discards it).
- **Recommended fix:** Bound the fallback drain with a fixed timeout (already
  `time.Second`) and document why background is acceptable there.
- **Tests required:** None required.
- **Behaviour could change:** No.

## 9. P3 Findings

### P3-1 — Legacy/duplicate shell tool (`bash` vs `exec_command`)
- **Severity:** P3
- **File path:** `internal/tools/bash.go` ("legacy bash" per README/docs);
  `internal/tools/exec_command.go`
- **Evidence:** README lists "Shell tools: `exec_command`, `write_stdin`, legacy
  `bash`". Two shell entrypoints increase the permission/sandbox test matrix.
- **Why it matters:** Maintenance overhead and a wider attack surface for sandbox
  rules that must cover both.
- **Recommended fix:** Deprecate `bash` behind a flag; consolidate on `exec_command`.
- **Tests required:** Ensure feature parity before removal.
- **Behaviour could change:** Yes for users still invoking `bash`.

### P3-2 — `modelregistry` vs `providers` model-ID normalization split
- **Severity:** P3
- **File path:** `internal/modelregistry`, `internal/providers`
- **Evidence:** Model ID/base-URL normalization spans both; `providers.New`
  "normalizes API model IDs and base URLs" (per docs).
- **Why it matters:** Two places to update when a provider changes its model names.
- **Recommended fix:** Centralize normalization in `modelregistry`.
- **Tests required:** Round-trip tests for known model IDs.
- **Behaviour could change:** No.

### P3-3 — Low golden-test coverage for rendering
- **Severity:** P3
- **File path:** `internal/tui/rendering.go` (2,499 LOC); only 4 golden test files
  repo-wide
- **Evidence:** `rg` count: 4 files use `golden`. Rendering is the largest pure
  logic surface with little snapshot coverage.
- **Why it matters:** Refactoring `updateModel`/`rendering` without golden snapshots
  risks silent visual regressions.
- **Recommended fix:** Add golden snapshots for card/transcript rendering before
  the P1-2 decomposition.
- **Tests required:** New golden files.
- **Behaviour could change:** No.

## 10. Large-File Analysis

Top 25 production Go files by LOC (excluding `*_test.go`):

```
5296  internal/tui/model.go
2965  internal/agent/loop.go
2499  internal/tui/rendering.go
1949  internal/tui/provider_wizard.go
1742  internal/tui/onboarding.go
1485  internal/tui/transcript_selection.go
1377  internal/cli/app.go
1276  internal/tui/assistant_markdown.go
1239  internal/cli/exec.go
1113  internal/config/resolver.go
1093  internal/sessions/store.go
1047  internal/tui/view.go
1039  internal/hooks/hooks.go
1038  internal/tui/picker.go
1006  internal/cli/workflows.go
 987  internal/tools/exec_command.go
 971  internal/cli/mcp_config.go
 965  internal/sandbox/runner.go
 965  internal/release/release.go
 961  internal/plugins/plugins.go
 958  internal/tools/local_browser.go
 946  internal/tui/sidebar.go
 896  internal/mcp/network_client.go
 893  internal/dictation/download.go
```

The top 3 (`model.go`, `loop.go`, `rendering.go`) are the concentration risk from
§8 P2-1 and §7 P1-1/P1-2. Note the TUI owns 8 of the top 25 files — UI surface
complexity dominates codebase size.

Largest packages by file count: `tui` (210), `cli` (114), `tools` (97),
`sandbox` (79), `agent` (61). Packages with most test files: `tui` (123),
`cli` (66), `agent` (45), `tools` (44), `sandbox` (35) — test density tracks
complexity, which is healthy.

## 11. Error and Panic Analysis

- **Panics (production):** 6 total. Two at init (`model.go:720`, `usage/tracker.go:91`)
  panic on `modelregistry.DefaultRegistry()` failure. Others are invariant
  violations (`specialist/builtin.go:40`, `reasoning/catalog.go:55`,
  `sandbox/windows_runner.go:699`, `perfbench/perfbench.go:194`). No `recover()`
  guards the agent loop or TUI dispatch, so any panic is fatal (see P1-3).
- **Error handling:** `Run` uses named returns `(result Result, err error)`; the
  loop returns structured `Result`/`stopReason` for non-fatal stops and an `error`
  only for fatal path failures. This is reasonable, but the 637-line body makes the
  many `return` sites hard to audit. `recover()` appears 5 times repo-wide
  (`observability/crash.go`, `imageinput/pdf.go` x2, `swarm/member.go`,
  `notify/notify.go`) — all localized, none covering the core loop.
- **Recommendation:** Convert init panics to errors (P1-3); add a recovery boundary
  in `Run` and the TUI run command.

## 12. Side-Effect Boundary Analysis

Production side-effect primitives (excluding tests):

- **`exec.Command` (58 sites):** concentrated in `imageinput/clipboard.go` (6),
  `release.go` (3), `perfbench` (4), `specialist/exec.go` (2), `skills/install.go`
  (2), `plugins/install.go` (2). Many lack `CommandContext` (see P1-4).
- **Filesystem writes/deletes:** `os.WriteFile`/`os.RemoveAll`/`os.Remove`/
  `os.MkdirAll`/`os.Chmod` concentrated in `release.go` (13),
  `skills/install.go` (8), `sandbox/windows_acl_apply_windows.go` (7),
  `plugins/install.go` (7), `update/extract.go` (6), `securefile/securefile.go`
  (6), `sessions/rewind.go` (5), `sessions/store.go` (4), `background/manager.go`
  (6). Install/update/rewind paths are the heaviest mutators — and the most
  dangerous (data loss if `os.RemoveAll` runs on a wrong path).
- **Security-relevant:** `internal/securefile` (6 side-effect calls) and
  `internal/sandbox` windows ACL code are the sensitive boundaries; both are
  platform-gated and should stay behind the sandbox engine.
- **Recommendation:** Route all workspace-mutating `os.*` calls through the sandbox
  engine's scope check; add path-containment assertions before any `os.RemoveAll`
  in `sessions/rewind.go` and `update/apply.go`.

## 13. Concurrency Analysis

- **Agent loop:** single goroutine per `Run` (streamed via callbacks); tool
  execution is sequential by default with opt-in `parallel_tools.go` (guarded by a
  local `callbackMutex`) and off-path `async_diagnostics.go` (guarded by `mu`).
- **Shared mutable state:** 90 `sync.Mutex/RWMutex/Map` declarations. Most are
  per-instance and fine; package-level singletons (`accountingMu`, OAuth
  `refreshLocks`) need concurrency tests under multiple in-process agents (swarm/
  daemon). Current `internal/agent` passed `-race` at baseline — good signal.
- **Cancellation:** **Primary risk (P0-CANDIDATE-1 / P1-4).** `exec_command.go`
  roots the command context in `context.Background()` and `specialist`/`agenteval`
  reset the context, so parent cancellation does not reach many child processes.
- **Goroutine leaks:** `background/manager.go` and `daemon/server.go` manage
  long-lived goroutines; verify they observe `ctx` cancellation on shutdown (out
  of scope of this read, flagged for runtime verification).
- **Recommendation:** Add a cancellation contract test in CI: cancel the run ctx and
  assert all spawned processes exit within a timeout.

## 14. Configuration Analysis

- **Resolution:** `internal/config/resolver.go` (1,113 LOC) centralizes user/project
  config, active provider, preferences, sandbox/tool settings. `AGENTS.md`/`ZERO.md`
  are injected as prompt context (capped 8 KiB/file, 32 KiB total).
- **Validation:** Unknown-field detection exists (`internal/config/unknownfields.go`
  with `legacyJSONAliases`) — good forward/backward compat handling.
- **Risk:** `resolver.go` is large (1,113 LOC) and interleaves IO, defaults, and
  merging; a mis-merge could silently change effective policy. No evidence of a
  defect, but it is a high-blast-radius file.
- **Recommendation:** Keep `resolver.go` covered by tests; add explicit policy
  assertions (e.g., "sandbox deny wins over tool allow").

## 15. Test Architecture

- **Density:** 4,571 test functions across 1,119 Go files; ~140k LOC of tests.
  Baseline run: all packages `ok`, 0 FAIL, including `go test -race ./internal/agent/...`.
- **Style:** Tests live next to source (`foo_test.go`); `internal/tui` has 123 test
  files (likely table/unit). Golden snapshots used in only 4 files — rendering
  (`rendering.go`, 2,499 LOC) is under-covered for a refactor (P3-3).
- **Coverage gaps to close before refactor:**
  - Cancellation contract (P0-CANDIDATE-1, P1-4) — currently untested.
  - Panic recovery boundary (P1-3) — fault-injection test missing.
  - `Run` decomposition (P1-1) — must keep `internal/agent` green under `-race`.
- **CI:** `.github/workflows/ci.yml` gates on `gofmt`, `go vet`, `go test ./...`,
  build, smoke; separate `security` job runs `govulncheck` (hard gate),
  `deadcode` and `golangci-lint` (advisory). Benchmarks via `zero-perf-bench`.

## 16. Recommended Refactor Order

1. **(Safety) Close the cancellation gap** — P0-CANDIDATE-1 + P1-4: thread `ctx`
   into `exec_command.startSession` and add a cancellation contract test. Lowest
   risk, highest safety payoff. Do this *before* touching the loop.
2. **(Reliability) Remove init panics + add `Run` recovery** — P1-3.
3. **(Maintainability) Decompose `agent.Run`** — P1-1: extract
   `buildTurnContext`/`executeTurn`/`handleToolCalls`/`finalizeTurn`.
4. **(Maintainability) Add golden snapshots for rendering** — P3-3, prerequisite to
   decomposing `updateModel`.
5. **(Maintainability) Decompose `model.updateModel`** — P1-2.
6. **(Cleanup) Consolidate provider/model discovery** — P2-2/P3-2.
7. **(Cleanup) Deprecate legacy `bash` tool** — P3-1.

## 17. First Recommended Implementation Task

**Task: Propagate run context to shell execution and add a cancellation test.**

- **File:** `internal/tools/exec_command.go`
- **Change:** In `startSession` (line 565), accept the run `ctx` and replace
  `context.WithCancel(context.Background())` (line 567) and the
  `StartDenialMonitor(context.Background(), ...)` (line 575) with
  `context.WithCancel(ctx)`. Add a bounded grace period + `Process.Kill()` on
  forced cancellation. Also review `exec_command.go:543` fallback drain (document
  the 1s bound).
- **Why first:** It is the highest safety-payoff, lowest-blast-radius change; it
  does not require restructuring `Run`, and it establishes the cancellation
  contract test that later refactors (specialist/eval, P1-4) will reuse.
- **Tests:** New `internal/tools/exec_command_test.go` case: start a command that
  sleeps, cancel `ctx`, assert the child process exits within a timeout
  (use `exec_test` / `golang.org/x/sys` process polling or a sentinel file).
- **Behaviour change:** Commands are now actually killed on run cancel (intended).
- **Verification:** `go test ./internal/tools/... -race` and the existing
  `go test -race ./internal/agent/...` must stay green.

---

*Audit method: all findings derived from local source inspection (`find`, `wc`,
`rg`, `awk`, `go list`) and the repository's own docs. No external AI APIs were
used. Claims without direct file:line evidence are labelled as assumptions or
"requires runtime verification". `docs/development/LOCAL_BASELINE.md` was preserved
untouched.* 
