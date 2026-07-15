package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// runExecOrchestrationPreview runs `zero exec --orchestration-preview` (optionally
// with --json) using the session/provider spy deps from plan_preview_test.go. It
// returns the spies so a test can assert the preview constructed neither a
// provider nor a session store.
func runExecOrchestrationPreview(t *testing.T, jsonOut bool, args ...string) (string, string, int, *bool, *bool) {
	t.Helper()
	deps, providerCalled, sessionCalled := planPreviewDeps(t)
	full := []string{"exec", "--orchestration-preview"}
	if jsonOut {
		full = append(full, "--json")
	}
	full = append(full, args...)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps(full, &stdout, &stderr, deps)
	return stdout.String(), stderr.String(), exit, providerCalled, sessionCalled
}

type execPreviewDoc struct {
	Mode        string          `json:"mode"`
	Executed    bool            `json:"executed"`
	PlanPreview planPreviewJSON `json:"plan_preview"`
}

func TestExecOrchestrationPreviewTextBasic(t *testing.T) {
	stdout, stderr, exit, _, _ := runExecOrchestrationPreview(t, false, "Refactor the provider registry")
	if exit != exitSuccess {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitSuccess, exit, stderr)
	}
	out := stdout
	if !strings.Contains(out, "ORCHESTRATION PREVIEW — no tasks will be executed") {
		t.Fatalf("missing preview banner in output:\n%s", out)
	}
	if !strings.Contains(out, "Plan:") {
		t.Fatalf("missing plan section in output:\n%s", out)
	}
	if !strings.Contains(out, "Preview complete. No provider was called and no task was executed.") {
		t.Fatalf("missing preview footer in output:\n%s", out)
	}
	if len(stderr) != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
}

func TestExecOrchestrationPreviewJSONShape(t *testing.T) {
	stdout, stderr, exit, _, _ := runExecOrchestrationPreview(t, true, "Refactor the provider registry")
	if exit != exitSuccess {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitSuccess, exit, stderr)
	}
	var doc execPreviewDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("JSON parse error: %v\n%s", err, stdout)
	}
	if doc.Mode != "orchestration-preview" {
		t.Fatalf("expected mode orchestration-preview, got %q", doc.Mode)
	}
	if doc.Executed {
		t.Fatalf("expected executed=false in preview")
	}
	if doc.PlanPreview.Prompt != "Refactor the provider registry" {
		t.Fatalf("unexpected prompt: %q", doc.PlanPreview.Prompt)
	}
	if doc.PlanPreview.Plan.ID == "" {
		t.Fatalf("expected a plan id in preview JSON")
	}
	if len(doc.PlanPreview.Plan.Tasks) == 0 {
		t.Fatalf("expected at least one planned task in preview JSON")
	}
}

func TestExecOrchestrationPreviewNoProviderOrSession(t *testing.T) {
	_, stderr, exit, providerCalled, sessionCalled := runExecOrchestrationPreview(t, true, "Refactor the provider registry")
	if exit != exitSuccess {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitSuccess, exit, stderr)
	}
	if *providerCalled {
		t.Fatal("preview constructed a provider; it must stay offline")
	}
	if *sessionCalled {
		t.Fatal("preview constructed a session store; it must stay session-free")
	}
}

func TestExecOrchestrationPreviewRejectsExecutionFlags(t *testing.T) {
	cases := [][]string{
		{"--skip-permissions-unsafe", "Refactor the provider registry"},
		{"--allow-escalation", "Refactor the provider registry"},
		{"--self-correct", "Refactor the provider registry"},
		{"--worktree", "Refactor the provider registry"},
		{"--use-spec", "Refactor the provider registry"},
		{"--list-tools"},
		{"--resume", "Refactor the provider registry"},
		{"--fork", "abc", "Refactor the provider registry"},
		{"--no-completion-gate", "Refactor the provider registry"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c, " "), func(t *testing.T) {
			_, stderr, exit, _, _ := runExecOrchestrationPreview(t, false, c...)
			if exit != exitUsage {
				t.Fatalf("expected exit %d (usage), got %d; stderr=%s", exitUsage, exit, stderr)
			}
			if !strings.Contains(stderr, "cannot be combined") {
				t.Fatalf("expected combination rejection, got stderr=%s", stderr)
			}
		})
	}
}

func TestExecOrchestrationPreviewRouterFlagsRequirePreview(t *testing.T) {
	cases := [][]string{
		{"--provider", "openai", "Refactor the provider registry"},
		{"--allow-provider", "openai", "Refactor the provider registry"},
		{"--deny-model", "gpt-4", "Refactor the provider registry"},
		{"--require-known-price", "Refactor the provider registry"},
		{"--max-input-cost", "1", "Refactor the provider registry"},
		{"--max-output-cost", "1", "Refactor the provider registry"},
		{"--show-rejected", "Refactor the provider registry"},
		{"--json", "Refactor the provider registry"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c, " "), func(t *testing.T) {
			_, stderr, exit := runExecRaw(t, c...)
			if exit != exitUsage {
				t.Fatalf("expected exit %d (usage), got %d; stderr=%s", exitUsage, exit, stderr)
			}
			if !strings.Contains(stderr, "require --orchestration-preview") {
				t.Fatalf("expected router-flag-requires-preview error, got stderr=%s", stderr)
			}
		})
	}
}

// runExecRaw runs a plain `zero exec` (no --orchestration-preview) with the
// session/provider spy deps, for testing that router flags are rejected outside
// the preview and that a normal exec is untouched.
func runExecRaw(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	deps, _, _ := planPreviewDeps(t)
	full := append([]string{"exec"}, args...)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps(full, &stdout, &stderr, deps)
	return stdout.String(), stderr.String(), exit
}

func TestExecOrchestrationPreviewMissingPrompt(t *testing.T) {
	_, stderr, exit, _, _ := runExecOrchestrationPreview(t, false)
	if exit != exitUsage {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitUsage, exit, stderr)
	}
	if !strings.Contains(stderr, "Prompt required") {
		t.Fatalf("expected prompt-required error, got stderr=%s", stderr)
	}
}

func TestExecOrchestrationPreviewFilePrompt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(p, []byte("Refactor the provider registry"), 0644); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}
	stdout, stderr, exit, _, _ := runExecOrchestrationPreview(t, true, "--file", p)
	if exit != exitSuccess {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitSuccess, exit, stderr)
	}
	var doc execPreviewDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("JSON parse error: %v\n%s", err, stdout)
	}
	if doc.PlanPreview.Prompt != "Refactor the provider registry" {
		t.Fatalf("file prompt not read; got %q", doc.PlanPreview.Prompt)
	}
}

func TestExecOrchestrationPreviewPromptFlag(t *testing.T) {
	stdout, stderr, exit, _, _ := runExecOrchestrationPreview(t, true, "--prompt", "Refactor the provider registry")
	if exit != exitSuccess {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitSuccess, exit, stderr)
	}
	var doc execPreviewDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("JSON parse error: %v\n%s", err, stdout)
	}
	if doc.PlanPreview.Prompt != "Refactor the provider registry" {
		t.Fatalf("prompt flag not read; got %q", doc.PlanPreview.Prompt)
	}
}

func TestExecOrchestrationPreviewModelPreference(t *testing.T) {
	stdout, stderr, exit, _, _ := runExecOrchestrationPreview(t, true, "-m", "gpt-4o", "Refactor the provider registry")
	if exit != exitUsage && exit != exitSuccess {
		t.Fatalf("unexpected exit %d; stderr=%s", exit, stderr)
	}
	if exit != exitSuccess {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitSuccess, exit, stderr)
	}
	var doc execPreviewDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("JSON parse error: %v\n%s", err, stdout)
	}
	if doc.PlanPreview.Prompt != "Refactor the provider registry" {
		t.Fatalf("unexpected prompt %q", doc.PlanPreview.Prompt)
	}
}

func TestExecOrchestrationPreviewInvalidMaxInputCost(t *testing.T) {
	_, stderr, exit, _, _ := runExecOrchestrationPreview(t, false, "--max-input-cost", "notanumber", "Refactor the provider registry")
	if exit != exitUsage {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitUsage, exit, stderr)
	}
	if !strings.Contains(stderr, "--max-input-cost") {
		t.Fatalf("expected max-input-cost parse error, got stderr=%s", stderr)
	}
}

func TestExecOrchestrationPreviewNoOrchestratedFlag(t *testing.T) {
	// --orchestrated is now a real flag, but it cannot be combined with
	// --orchestration-preview (mutual exclusion), which yields a usage error.
	_, stderr, exit, _, _ := runExecOrchestrationPreview(t, false, "--orchestrated", "Refactor the provider registry")
	if exit != exitUsage {
		t.Fatalf("expected exit %d, got %d; stderr=%s", exitUsage, exit, stderr)
	}
	if !strings.Contains(stderr, "cannot be combined") {
		t.Fatalf("expected mutual-exclusion error for --orchestrated with preview, got stderr=%s", stderr)
	}
}

func TestExecOrchestrationPreviewFlagNotDefault(t *testing.T) {
	opts, _, err := parseExecArgs([]string{"Refactor the provider registry"})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if opts.orchestrationPreview {
		t.Fatal("preview enabled without --orchestration-preview")
	}
	opts2, _, err := parseExecArgs([]string{"--orchestration-preview", "Refactor the provider registry"})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !opts2.orchestrationPreview {
		t.Fatal("--orchestration-preview did not enable the preview")
	}
}

func TestExecWithoutPreviewDoesNotEmitPreviewBanner(t *testing.T) {
	// A normal exec (no preview flag) must run its usual path and must NOT emit
	// the preview banner; it should reach provider configuration and fail there
	// because the spy deps have no provider configured.
	deps, _, _ := planPreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"exec", "Refactor the provider registry"}, &stdout, &stderr, deps)
	if strings.Contains(stdout.String(), "ORCHESTRATION PREVIEW") {
		t.Fatalf("normal exec emitted the preview banner; preview branch was not gated:\n%s", stdout.String())
	}
	if exit == exitSuccess {
		t.Fatalf("expected normal exec to fail without a provider, got exit 0")
	}
}

func TestExecOrchestrationPreviewHelpDocumentsFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exit := Run([]string{"exec", "--help"}, &stdout, &stderr)
	if exit != exitSuccess {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	for _, want := range []string{
		"--orchestration-preview",
		"no execution, no provider, no session",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected exec help to contain %q, got %q", want, stdout.String())
		}
	}
}

func TestExecOrchestrationPreviewEquivalenceWithPlanPreview(t *testing.T) {
	prompt := "Refactor the provider registry"
	deps, _, _ := planPreviewDeps(t)

	var eStdout, eStderr bytes.Buffer
	eExit := runWithDeps([]string{"exec", "--orchestration-preview", "--json", prompt, "--provider", "openai"}, &eStdout, &eStderr, deps)
	if eExit != exitSuccess {
		t.Fatalf("exec preview failed: exit=%d stderr=%s", eExit, eStderr.String())
	}
	var ew execWrapper
	if err := json.Unmarshal(eStdout.Bytes(), &ew); err != nil {
		t.Fatalf("exec preview JSON parse error: %v\n%s", err, eStdout.String())
	}

	var pStdout, pStderr bytes.Buffer
	pExit := runWithDeps([]string{"plan-preview", "--json", prompt, "--provider", "openai"}, &pStdout, &pStderr, deps)
	if pExit != exitSuccess {
		t.Fatalf("plan-preview failed: exit=%d stderr=%s", pExit, pStderr.String())
	}
	var pw planPreviewJSON
	if err := json.Unmarshal(pStdout.Bytes(), &pw); err != nil {
		t.Fatalf("plan-preview JSON parse error: %v\n%s", err, pStdout.String())
	}

	if !reflect.DeepEqual(ew.PlanPreview, pw) {
		t.Fatalf("exec --orchestration-preview and plan-preview produced different results for the same prompt+flags:\n exec=%+v\n plan=%+v", ew.PlanPreview, pw)
	}
}

type execWrapper struct {
	Mode        string          `json:"mode"`
	Executed    bool            `json:"executed"`
	PlanPreview planPreviewJSON `json:"plan_preview"`
}
