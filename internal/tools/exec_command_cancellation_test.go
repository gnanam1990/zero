package tools

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestExecCommandRunContextCancellationKillsChildProcess verifies that
// cancelling the parent run context propagated to exec_command.Run terminates
// a still-running child process and removes its background session from the
// manager.
//
// Suspected bug (see docs/development/ARCHITECTURE_AUDIT.md P0-CANDIDATE-1):
// execCommandTool.startSession derives the child's context from
// context.Background() (exec_command.go:567) instead of the run ctx passed to
// Run. The only production path that calls session.terminate() after Run
// returns is run()'s in-collect ctx-cancelled branch; a long-running command
// that yields "still running" and whose run ctx is cancelled afterwards is
// never terminated, so the child survives.
//
// This test exercises the SAME production path (exec_command tool.Run) with a
// real, long-running local child. On the current code it FAILS, which is the
// intended evidence confirming the bug. Do not weaken it and do not fix the
// implementation here.
func TestExecCommandRunContextCancellationKillsChildProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-liveness check via Signal(0) is unix-specific; cancellation contract is verified on macOS/Linux")
	}

	root := t.TempDir()
	manager := newExecSessionManager()
	execTool := NewScopedExecCommandTool(root, nil, manager)

	// A long-running child that outlives this test's cancel window many times
	// over, so a still-alive process after cancel can only mean it was NOT
	// killed by cancellation (not that it exited naturally).
	const commandText = "sleep 120"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	startResult := execTool.Run(ctx, map[string]any{
		"cmd":           commandText,
		"yield_time_ms": 300, // short: Run returns "still running" quickly
	})
	elapsed := time.Since(start)

	if startResult.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", startResult.Status, startResult.Output)
	}
	sessionIDStr, ok := startResult.Meta["session_id"]
	if !ok || sessionIDStr == "" {
		t.Fatalf("expected running session metadata, got %#v output=%q", startResult.Meta, startResult.Output)
	}
	sessionID, err := strconv.Atoi(sessionIDStr)
	if err != nil {
		t.Fatalf("session_id is not numeric: %v", err)
	}

	// Tool must return promptly (it yields after 300ms, not after the 120s
	// command finishes).
	if elapsed > 5*time.Second {
		t.Fatalf("exec_command.Run took %v to return for a 120s command; expected ~yield window, not the command lifetime", elapsed)
	}

	// Capture the live child process from the in-package session so we can
	// verify its OS liveness directly. Done before cancellation.
	session, ok := manager.get(sessionID)
	if !ok {
		t.Fatalf("session %d not found in manager after start", sessionID)
	}
	if session.command == nil || session.command.Process == nil {
		t.Fatalf("no child process tracked for session %d", sessionID)
	}
	pid := session.command.Process.Pid
	if !processAliveUnix(pid) {
		t.Fatalf("child process %d not alive immediately after start", pid)
	}

	// The operation under test: cancel the parent run context. The correct
	// behavior is that this propagates to the child and removes the session.
	cancel()

	// Poll a bounded window for the correct behavior.
	deadline := time.Now().Add(3 * time.Second)
	childDied := false
	sessionGone := false
	for time.Now().Before(deadline) {
		if !processAliveUnix(pid) {
			childDied = true
		}
		if _, stillRegistered := manager.get(sessionID); !stillRegistered {
			sessionGone = true
		}
		if childDied && sessionGone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Safety net: always reap the child we started, regardless of outcome, so
	// the test never leaks a 120s sleep process. Targets only our PID.
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
		manager.stopAll()
	})

	if !childDied {
		t.Errorf("CHILD PROCESS %d SURVIVED run-context cancellation (bug confirmed): command %q", pid, commandText)
	}
	if !sessionGone {
		t.Errorf("background exec session %d remained registered after run-context cancellation (bug confirmed)", sessionID)
	}
}

// processAliveUnix reports whether a Unix process with the given PID exists,
// using signal 0 (no-op) which fails with ESRCH when the process is gone. This
// works on both macOS and Linux without extra dependencies.
func processAliveUnix(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
