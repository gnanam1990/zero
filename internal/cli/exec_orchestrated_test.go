package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/executor"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/verify"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// --- fakes ---

type fakeProvider struct{}

func (fakeProvider) StreamCompletion(ctx context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent)
	close(ch)
	return ch, nil
}

type fakeRunner struct {
	result executor.TaskExecutionResult
}

func (f fakeRunner) RunTask(ctx context.Context, req executor.TaskExecutionRequest) (executor.TaskExecutionResult, error) {
	return f.result, nil
}

func fakeVerifyPassed(ctx context.Context, root string, changed []string) executor.VerificationOutcome {
	return executor.VerificationOutcome{Status: "passed", Total: 1, Passed: 1}
}

func newOrchestratedTestDeps(t *testing.T, tmp string, runner executor.Runner, verifier executor.Verifier, outputFormat execOutputFormat) orchestratedOnceDeps {
	reg, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("modelregistry: %v", err)
	}
	fakeProfile := config.ProviderProfile{Name: "fake", Provider: "fake", Model: "fake-model"}
	return orchestratedOnceDeps{
		options: execOptions{outputFormat: outputFormat, autonomy: "low"},
		stdout:  &strings.Builder{},
		stderr:  &strings.Builder{},
		deps: appDeps{
			getwd: func() (string, error) { return tmp, nil },
			newProvider: func(config.ProviderProfile) (agent.Provider, error) {
				return fakeProvider{}, nil
			},
			newSessionStore: func() *sessions.Store {
				return sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
			},
			detectVerifyPlan: func(string) (verify.Plan, error) { return verify.Plan{}, nil },
			runVerify: func(context.Context, verify.Plan, verify.RunOptions) verify.Report {
				return verify.Report{}
			},
			skillsDir: func() string { return "" },
		},
		workspaceRoot: tmp,
		trustRoot:     tmp,
		registry:      newCoreRegistry(tmp),
		modelRegistry: reg,
		resolved: config.ResolvedConfig{
			ActiveProvider: "fake",
			Providers:      []config.ProviderProfile{fakeProfile},
			Provider:       fakeProfile,
			MaxTurns:       10,
		},
		permissionMode: agent.PermissionModeAuto,
		sessionTitle:   "test",
		prompt:         "add a utility function to parse durations from strings",
		runner:         runner,
		verifier:       verifier,
	}
}

func TestRunOrchestratedOnceText(t *testing.T) {
	tmp := t.TempDir()
	runner := fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "Added parseDuration in util.go"},
		FinalAnswer: "Added parseDuration in util.go",
		ToolEvents:  []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}},
	}}
	od := newOrchestratedTestDeps(t, tmp, runner, fakeVerifyPassed, execOutputText)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{
		"ORCHESTRATED EXECUTION — one task only",
		"Selected task:",
		"Routing:",
		"Execution (completed)",
		"Verification: passed",
		"Stopped after one task by --orchestrated-once.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report missing %q\n---\n%s", want, out)
		}
	}
}

func TestRunOrchestratedOnceJSON(t *testing.T) {
	tmp := t.TempDir()
	runner := fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "done"},
		FinalAnswer: "done",
		ToolEvents:  []executor.ToolEvent{{Name: "write_file", Kind: "mutating"}},
	}}
	od := newOrchestratedTestDeps(t, tmp, runner, fakeVerifyPassed, execOutputJSON)
	code := runOrchestratedOnce(od)
	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d", code, exitSuccess)
	}
	out := od.stdout.(*strings.Builder).String()
	for _, want := range []string{"\"mode\": \"orchestrated-once\"", "\"selected_task\"", "\"scheduler\"", "\"verification\""} {
		if !strings.Contains(out, want) {
			t.Errorf("json report missing %q\n---\n%s", want, out)
		}
	}
}

func TestRunOrchestratedOnceIncomplete(t *testing.T) {
	tmp := t.TempDir()
	// No tool action and no "feature exists" evidence -> incomplete.
	runner := fakeRunner{result: executor.TaskExecutionResult{
		AgentResult: agent.Result{FinalAnswer: "I think it is done"},
		FinalAnswer: "I think it is done",
	}}
	od := newOrchestratedTestDeps(t, tmp, runner, fakeVerifyPassed, execOutputText)
	code := runOrchestratedOnce(od)
	if code != exitIncomplete {
		t.Fatalf("exit code = %d, want %d (incomplete)", code, exitIncomplete)
	}
}

// --- parse tests ---

func TestParseOrchestratedOnceAccepted(t *testing.T) {
	opts, help, err := parseExecArgs([]string{"--orchestrated-once", "do the thing"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if help || !opts.orchestratedOnce {
		t.Fatalf("expected orchestratedOnce=true, help=%v", help)
	}
}

func TestParseOrchestratedOnceRejectsPreview(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--orchestrated-once", "--orchestration-preview", "x"})
	if err == nil {
		t.Fatal("expected error combining --orchestrated-once with --orchestration-preview")
	}
}

func TestParseOrchestratedOnceRejectsResumeForkWorktreeSpecListTools(t *testing.T) {
	for _, extra := range []string{"--resume", "--fork", "--worktree", "--use-spec", "--list-tools"} {
		args := []string{"--orchestrated-once", extra, "x"}
		if extra == "--resume" || extra == "--fork" {
			args = []string{"--orchestrated-once", extra, "sess", "x"}
		}
		_, _, err := parseExecArgs(args)
		if err == nil {
			t.Errorf("expected error combining --orchestrated-once with %s", extra)
		}
	}
}

func TestParseOrchestratedOnceAllowsRouterFlags(t *testing.T) {
	opts, _, err := parseExecArgs([]string{"--orchestrated-once", "--provider", "openai", "--deny-model", "x", "do it"})
	if err != nil {
		t.Fatalf("router flags should be allowed with --orchestrated-once: %v", err)
	}
	if opts.routerProvider != "openai" {
		t.Errorf("routerProvider = %q, want openai", opts.routerProvider)
	}
	if len(opts.denyModels) != 1 {
		t.Errorf("denyModels = %v, want [x]", opts.denyModels)
	}
}

func TestParseRouterFlagsRejectedWithoutOrchestration(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--provider", "openai", "do it"})
	if err == nil {
		t.Fatal("expected router flags to be rejected without --orchestration-preview or --orchestrated-once")
	}
}
