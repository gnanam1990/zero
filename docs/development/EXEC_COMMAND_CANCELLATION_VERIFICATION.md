# Exec-Command Cancellation Verification

**Branch:** `test/exec-command-cancellation` (base `80c39aa`)
**Date:** 2026-07-14 (local)
**Platform:** macOS 15.7.7 (Sequoia), arm64 — Darwin 24.6.0
**Authoritative test:** `internal/tools/exec_command_cancellation_test.go`
**Companion docs:** `docs/development/ARCHITECTURE_AUDIT.md` (P0-CANDIDATE-1),
`docs/development/LOCAL_BASELINE.md`

## Suspected code path

The agent runs shell commands through `exec_command`. The lifecycle:

1. `execCommandTool.Run(ctx, args)` → `run(ctx, args, engine)`
   (`internal/tools/exec_command.go:475`).
2. `run` parses args and calls
   `tool.startSession(commandText, absoluteCwd, relativeCwd, ttyRequested, engine, sandboxPermissions)`
   (`exec_command.go:536`). **`ctx` is NOT passed to `startSession`.**
3. `startSession` creates the child context from `context.Background()`, not the
   run context:
   ```go
   // internal/tools/exec_command.go:567
   commandCtx, cancel := context.WithCancel(context.Background())
   ```
   and again at line 575:
   ```go
   monitor := zeroSandbox.StartDenialMonitor(context.Background(), plan.MonitorTag)
   ```
4. `buildBashCommand(commandCtx, ...)` builds the process with
   `exec.CommandContext(commandCtx, ...)` (`internal/tools/bash.go:285`). So the
   child's lifetime is bound to `commandCtx` (a `context.Background()` child), not
   to the run `ctx`.
5. `hardenProcessLifetime` wires `command.Cancel` to `kill -<pgid> SIGKILL`
   (`internal/tools/bash_proc_unix.go:34`), so cancelling `commandCtx` *would* kill
   the process group — but `commandCtx` is only cancelled via `session.terminate()`,
   which calls `session.cancel()`.
6. `session.terminate()` is reachable in exactly two production paths:
   - `run()`'s post-`collect` branch, **only while `ctx.Err() != nil` during the
     yield window** (`exec_command.go:541-546`);
   - the wait goroutine, which fires only when the **process exits on its own**
     (`exec_command.go:601-612`);
   - plus explicit `StopExecSession` / `StopAllExecSessions`.
7. **There is no path that terminates a still-running background session when the
   parent run `ctx` is cancelled after `Run` has already returned** "still running".

So a long-running command that yields "still running" and whose run context is
cancelled afterwards (exactly the user-cancels-a-run scenario) leaves the child
process and its registered session alive until the command exits naturally.

## Test design

`TestExecCommandRunContextCancellationKillsChildProcess` (new test file
`internal/tools/exec_command_cancellation_test.go`):

- Builds the tool through the **same production constructor**
  `NewScopedExecCommandTool(root, nil, manager)` with a fresh
  `newExecSessionManager()`, so the execution path is identical to production.
- Starts a real, long-running local child: `sleep 120` (120s ≫ the 3s cancel
  window, so a still-alive process can only mean "not killed by cancellation",
  never "exited naturally").
- Calls `execTool.Run(ctx, {cmd: "sleep 120", yield_time_ms: 300})` with a
  cancellable `ctx`. The short yield makes `Run` return promptly with
  `session_id` ("still running"), exactly like a background dev server.
- Captures the child PID directly from the in-package session
  (`session.command.Process.Pid`) — no fragile PID-file parsing, and the PID is
  unique to this run.
- Asserts the tool returned promptly (elapsed < 5s, well under the 120s command).
- **Cancels the parent `ctx`.**
- Polls a bounded 3s window checking two correct-behavior conditions:
  - the OS child process is no longer alive (`Signal(0)` on macOS/Linux), and
  - the session is no longer registered in the manager.
- `t.Cleanup` always reaps the specific child by PID and calls `manager.stopAll()`,
  so the test never leaks a process and never touches unrelated ones.
- Skips cleanly on Windows (the `Signal(0)` liveness probe is Unix-specific).

This is a **regression test asserting the corrected behavior**. On the current
(unfixed) code it fails — which is the intended evidence.

## Exact command executed

```bash
cd "/Users/kratos/Documents/New OpenCode Project/zero"
gofmt -w internal/tools/exec_command_cancellation_test.go
go test ./internal/tools/... -run 'Cancellation|Cancel|ExecCommand' -count=1 -v
go test -race ./internal/tools/... -run 'Cancellation|Cancel|ExecCommand' -count=1 -v
git diff --check
```

## Actual result

`TestExecCommandRunContextCancellationKillsChildProcess` **FAILED** in both runs:

- non-race: `--- FAIL: TestExecCommandRunContextCancellationKillsChildProcess (3.32s)`
- race:     `--- FAIL: TestExecCommandRunContextCancellationKillsChildProcess (3.33s)`

Failing assertions (verbatim):

```
exec_command_cancellation_test.go:117: CHILD PROCESS 21964 SURVIVED run-context cancellation (bug confirmed): command "sleep 120"
exec_command_cancellation_test.go:120: background exec session 1000 remained registered after run-context cancellation (bug confirmed)
```

Both conditions failed: the OS child process was still alive 3s after `ctx` was
cancelled, and the session was still registered in the manager. All other
`Cancellation|Cancel|ExecCommand` tests in the package passed; no unrelated test
regressed.

## Whether the child survived cancellation

**Yes.** After cancelling the run context, the `sleep 120` child process (PID
21964 in the observed run) remained running and the background session (id 1000)
remained registered. The command's `Context` is `context.Background()`-derived
(`exec_command.go:567`), and nothing calls `session.terminate()` once `Run` has
returned, so cancellation does not reach the child.

## Whether the issue is confirmed

**Confirmed by runtime test.** The audit's P0-CANDIDATE-1 is now a confirmed
defect: `exec_command` does not propagate run-context cancellation to a
long-running child process. The failure was reproduced deterministically on
macOS; the same code path and `context.Background()` root cause apply on Linux
(the only platform difference is the Windows `Cancel` body in
`bash_proc_windows.go`, which is still driven by the same `commandCtx`).

## Platform limitations

- Verified on **macOS (arm64)**. Linux shares the identical code path
  (`exec_command.go:567`, `bash.go:285`, `bash_proc_unix.go`); the behavior is
  expected to reproduce there.
- **Windows not exercised**: `processAliveUnix` uses `Signal(0)`, which is
  Unix-specific, so the test skips on Windows. The underlying root cause
  (`commandCtx` from `context.Background()`) is platform-independent, but a
  Windows-specific liveness probe would be needed for full coverage.
- The test reaps its own child by PID in `t.Cleanup`; no unrelated process is
  killed, and no dependency was added or modified.

## Recommended production fix

Thread the run `ctx` into `startSession` and use it as the child context:

```go
// internal/tools/exec_command.go
func (tool execCommandTool) startSession(
    ctx context.Context,                 // ADD
    commandText string, absoluteCwd string, relativeCwd string,
    ttyRequested bool, engine *zeroSandbox.Engine,
    sandboxPermissions SandboxPermissionOverride,
) (*execSession, error) {
    id := tool.manager.allocateID()
    commandCtx, cancel := context.WithCancel(ctx)   // was: context.Background()
    ...
    monitor := zeroSandbox.StartDenialMonitor(ctx, plan.MonitorTag) // was: context.Background()
    ...
}
```

and update the single caller in `run()` (line 536) to pass `ctx`. Keep the
existing `run()` post-`collect` `terminate()` branch as a backstop. Optionally add
a bounded grace period + `command.Process.Kill()` on forced cancel. This requires
no public-API change (the signature of the unexported `startSession` is internal).

## Recommended regression test

`TestExecCommandRunContextCancellationKillsChildProcess` (this task) is the
regression test. Once the fix lands it must **pass** (child dies and session is
removed within the bounded window). Keep it in the `Cancellation|Cancel|ExecCommand`
run group and run it under `-race`. Add a Windows sibling using a Windows-appropriate
liveness probe (e.g. `exec.Task`/`syscall` process-exit wait or `golang.org/x/sys/windows`)
to close the platform gap noted above.

---

# Fix Applied (implementation task)

## Root cause

`execCommandTool.startSession` created the child process context from
`context.Background()` instead of the active run context:

```go
// internal/tools/exec_command.go (before)
commandCtx, cancel := context.WithCancel(context.Background())   // line 567
...
monitor := zeroSandbox.StartDenialMonitor(context.Background(), plan.MonitorTag) // line 575
```

The child's lifetime was therefore bound to a `Background`-derived context. The
only production path that called `session.terminate()` (which cancels that
context and, via `hardenProcessLifetime`'s `command.Cancel`, kills the process
group) was `run()`'s post-`collect` branch — and that branch fires **only while
`ctx.Err() != nil` during the yield window**. A long-running command that yields
"still running" and whose parent run context is cancelled afterwards (the
user-cancels-a-run scenario) was never terminated, leaving the child process and
its registered background session alive until the command exited on its own.

## Exact fix

`internal/tools/exec_command.go`:

1. `startSession` now takes the run `ctx` and derives the child context from it
   (falling back to `context.Background()` only if `ctx == nil`, preserving
   pre-existing no-context callers):

   ```go
   func (tool execCommandTool) startSession(ctx context.Context, commandText string, ...) (*execSession, error) {
       parentCtx := ctx
       if parentCtx == nil {
           parentCtx = context.Background()
       }
       commandCtx, cancel := context.WithCancel(parentCtx)   // was: context.Background()
       ...
       monitor := zeroSandbox.StartDenialMonitor(parentCtx, plan.MonitorTag) // was: context.Background()
       ...
   }
   ```

2. The single caller in `run()` (line ~536) passes `ctx`:
   `tool.startSession(ctx, commandText, ...)`.

3. `removeCompletedLater` now also receives `parentCtx` and drops the session
   from the registry **promptly when the run context is cancelled**, while still
   honouring the normal `completedSessionRetention` (30s) for naturally-finished
   sessions so they stay listable:

   ```go
   func (manager *execSessionManager) removeCompletedLater(session *execSession, parentCtx context.Context) {
       go func() {
           select {
           case <-session.done:
           case <-parentCtx.Done():
           }
           if retention := manager.completedRetention; retention > 0 && !session.doneClosed() {
               timer := time.NewTimer(retention)
               select {
               case <-session.done:
               case <-timer.C:
               }
           }
           manager.remove(session.id)
       }()
   }
   ```

No public API changed; only the unexported `startSession` signature and the
`removeCompletedLater` signature changed (both internal). Foreground commands,
PTY/non-PTY execution, session listing/stopping, output collection, exit-status
handling, and explicit user stops (`write_stdin` `\x03` → `session.terminate()`)
are all preserved — explicit stops still go through `terminate()` and keep the
30s retention for history.

## Test before fix

`TestExecCommandRunContextCancellationKillsChildProcess` **FAILED** (both normal
and `-race` runs, ~3.3s):

```
exec_command_cancellation_test.go:117: CHILD PROCESS 21964 SURVIVED run-context cancellation (bug confirmed): command "sleep 120"
exec_command_cancellation_test.go:120: background exec session 1000 remained registered after run-context cancellation (bug confirmed)
```

## Test after fix

`TestExecCommandRunContextCancellationKillsChildProcess` **PASSES** (0.33s),
both normal and `-race`. The full `internal/tools` suite, `internal/agent`
suite, `go vet ./internal/tools/...`, `git diff --check`, and the full
`go test ./...` suite all pass with zero failures.

## CLI verification steps

```bash
mkdir -p /tmp/zero-cancel-verification
cd /tmp/zero-cancel-verification

# Build a local binary (separate task):
#   go build -o .local-bin/zero ./cmd/zero

# Start a long-running command through the exec-command tool path, auto-approving
# the shell so the command actually runs headless:
.local-bin/zero exec --auto high --skip-permissions-unsafe \
  --enabled-tools exec_command --max-turns 3 \
  "Run exactly this shell command and nothing else: sleep 120" &

# Confirm the child is running:
pgrep -fl 'sleep 120'        # -> shows a real `sleep 120` child (e.g. pid 25625)

# Cancel the active Zero run:
kill -INT <zero-pid>

# After zero exits, confirm cleanup:
pgrep -fl 'sleep 120'        # -> only the (exited) zero line, if matched; no child
```

## CLI verification result

- A real `sleep 120` child process was observed running (pid 25625) after
  `exec_command` started it.
- `kill -INT` on the Zero process cancelled the run; Zero **exited within ~1s**
  and reported `Interrupted` / `exit_code: -1` for the session.
- `pgrep -fl 'sleep 120'` after cancellation showed **no remaining `sleep 120`
  child process** — it was killed by run-context cancellation.
- No panic, no goroutine/race failure reported by the test suite.
- Only the Zero process started for this verification was signalled; no unrelated
  processes were killed.

## Platform tested

- **macOS 15.7.7 (Sequoia), arm64** — Darwin 24.6.0. Both the unit regression
  test and the CLI runtime check passed here.

## Remaining platform risks

- **Windows not runtime-verified.** The regression test skips on Windows (its
  `Signal(0)` liveness probe is Unix-specific). The fix applies identically
  there — `commandCtx` is now derived from the run `ctx`, and
  `bash_proc_windows.go`'s `command.Cancel` is driven by that same context — but
  a Windows-specific liveness probe + run would be needed to close the gap.
- **Linux not runtime-verified in this session**, but it shares the identical
  execution path (`exec_command.go`, `bash.go`, `bash_proc_unix.go`); the fix is
  expected to behave the same as on macOS.
- The `StartDenialMonitor` denial monitor (macOS `log stream` subprocess) is now
  also scoped to the run context, so it is reaped together with the run; this is
  a strict improvement and was exercised on macOS without issue.

