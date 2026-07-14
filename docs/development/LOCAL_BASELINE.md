# Local Verification Baseline — Agent-Loop Refactor

This document records the clean local verification state captured **before** any
source-code changes, as a reference point for the `audit/agent-loop-baseline`
refactor work on `internal/agent`.

## Environment

- **Date:** 2026-07-14 (local)
- **Operating system:** macOS 15.7.7 (Sequoia), Build 24G720, Darwin 24.6.0, arm64
  - Kernel: `Darwin Kernel Version 24.6.0 ... RELEASE_ARM64_T8132`
  - Host: KRATOSs-Mac-mini.local
- **Go version:** go1.26.5 (darwin/arm64)
  - `GOPATH`: `/Users/kratos/go`
  - `GOMOD`: `/Users/kratos/Documents/New OpenCode Project/zero/go.mod`
  - `GOTOOLCHAIN`: `auto`
- **Required Go version (go.mod / README):** 1.26.5+
- **Current branch:** `audit/agent-loop-baseline`
- **Current commit:** `80c39aa599b6a1caf1ce229c40efbc8157983ae9`
  - Subject: `docs: add codebase minimization guideline to AGENTS.md (#661)`

## Documented Commands (from README.md, Makefile, .github/workflows/ci.yml, docs/BENCHMARK.md)

### Test commands
- `go test ./...` — full suite (README, Makefile `test-quick`, CI `Test` step)
- `make test` — `go test ./... -race -count=1` (CI-equivalent, race detector)
- `make test-quick` — `go test ./...` (no race)
- `go run ./cmd/zero-release build` — build production binary (CI smoke)
- `go run ./cmd/zero-release smoke` — smoke test the binary (CI)

### Lint / formatting commands
- `go fmt ./...` / `make fmt` (`gofmt -w` on tracked Go files)
- `make fmt-check` — fail if any tracked Go file is not gofmt-clean
- `go vet ./...` / `make vet`
- `make lint` — `fmt-check` + `vet`
- `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --enable-only unused,ineffassign,staticcheck ./...`
  (per AGENTS.md step 2 / README / CI advisory, non-blocking in CI)

### Security checks
- `go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...` — hard gate in CI (`security` job)
- `go run golang.org/x/tools/cmd/deadcode@v0.46.0 -test=false ./...` — advisory, non-blocking in CI

### Benchmark commands
- `go run ./cmd/zero-perf-bench` — performance smoke (`--output dist/perf/perf-bench.json --ci` in CI)
- Task benchmark: `go run ./cmd/zero-perf-bench tasks --suite <json> --binary ./zero --model <m> ...`
  (see `docs/BENCHMARK.md`; requires a model + built binary, not a unit-level check)

## Commands Run (this baseline session)

| # | Command | Result |
|---|---------|--------|
| 1 | `go fmt ./...` | PASS — no files reformatted (exit 0) |
| 2 | `git diff --check` | PASS — no whitespace / diff issues (exit 0) |
| 3 | `go vet ./...` | PASS — no findings (exit 0) |
| 4 | `go test ./internal/agent/...` | PASS — `ok github.com/Gitlawb/zero/internal/agent 4.034s` |
| 5 | `go test -race ./internal/agent/...` | PASS — `ok github.com/Gitlawb/zero/internal/agent 4.766s` |
| 6 | `go test ./...` | PASS — all packages `ok`, 0 FAIL (full suite, exit 0) |

## Passed Checks

- `go fmt ./...` — clean, no changes needed
- `git diff --check` — clean
- `go vet ./...` — clean
- `go test ./internal/agent/...` — passed
- `go test -race ./internal/agent/...` — passed (no data races detected)
- `go test ./...` — passed across all packages (0 failures)

## Failed Checks

- None.

## Existing Failures

- None observed. The full `go test ./...` run reported 0 `FAIL` lines across all
  packages including the slower provider suites (`anthropic` ~122s, `openai` ~182s).

## Not Run (documented but out of scope for this baseline)

These are documented in the repo and are CI gates, but were **not** executed in
this session (the baseline task scoped the run to the 6 commands above):

- `golangci-lint` (staticcheck/unused/ineffassign)
- `govulncheck` (vulnerability scan — CI hard gate)
- `deadcode` (advisory reachability scan)
- `make lint` (fmt-check + vet) — covered by the individual `go fmt`/`go vet` runs
- `go run ./cmd/zero-perf-bench` (benchmark harness — requires a model + binary)

## Environment Limitations

- **Local OS differs from CI:** CI runs on `ubuntu-latest`, `macos-latest`,
  `windows-latest`; this baseline was captured on macOS 15.7.7 / arm64 only.
  Platform-specific behavior (sandbox helpers, terminal control) is not fully
  exercised locally.
- **GOTOOLCHAIN = auto:** matches CI practice of pinning to the go.mod toolchain
  for `govulncheck`/`deadcode`/`golangci-lint`. Local toolchain already satisfies
  go.mod (1.26.5), so no toolchain download was triggered.
- **No external AI APIs used:** the agent-loop tests passed without network/model
  access; provider integration suites exercised local/mock paths only.
- **No dependency changes:** `go mod` was not modified; `go test` used the
  existing module cache.

## Safety Assessment

The working tree was clean before and after the checks, no production code was
modified, and every executed check passed (including the `-race` suite for
`internal/agent`, the target of the planned refactor). With no pre-existing
failures at the baseline commit, **agent-loop refactoring is safe to begin**,
subject to re-running `go fmt`, `go vet`, `go test ./internal/agent/...`,
`go test -race ./internal/agent/...`, and ideally `golangci-lint`/`govulncheck`
after each change.

**Verdict: SAFE TO CONTINUE.**
