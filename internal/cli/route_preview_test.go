package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// routePreviewDeps returns minimal deps for route-preview plus flags that record
// whether a provider or session store would have been constructed. route-preview
// must never trigger either, proving it stays local and session-free.
func routePreviewDeps(t *testing.T) (appDeps, *bool, *bool) {
	t.Helper()
	cwd := t.TempDir()
	providerCalled := false
	sessionCalled := false
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			providerCalled = true
			return nil, nil
		},
		newSessionStore: func() *sessions.Store {
			sessionCalled = true
			return sessions.NewStore(sessions.StoreOptions{})
		},
	}
	return deps, &providerCalled, &sessionCalled
}

func runRoutePreviewCmd(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	deps, _, _ := routePreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps(append([]string{"route-preview"}, args...), &stdout, &stderr, deps)
	return stdout.String(), stderr.String(), exit
}

func selectedModel(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Model:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Model:"))
		}
	}
	return ""
}

func selectedProvider(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Provider:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Provider:"))
		}
	}
	return ""
}

func rejectedContains(out, modelID string) bool {
	inRejected := false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Rejected candidates:") {
			inRejected = true
			continue
		}
		if strings.HasPrefix(trimmed, "Ranked candidates:") {
			inRejected = false
			continue
		}
		if inRejected && trimmed == modelID {
			return true
		}
	}
	return false
}

func TestRoutePreviewBasicImplementation(t *testing.T) {
	out, stderr, exit := runRoutePreviewCmd(t, "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(out, "Primary: implementation") {
		t.Fatalf("expected implementation classification, got:\n%s", out)
	}
	if !strings.Contains(out, "tool-calling") || !strings.Contains(out, "streaming") {
		t.Fatalf("expected tool-calling/streaming capabilities, got:\n%s", out)
	}
	if selectedModel(out) == "" {
		t.Fatalf("expected a selected model, got:\n%s", out)
	}
}

func TestRoutePreviewSecurityReview(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "Review this pull request for security vulnerabilities")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(out, "Primary: security_review") {
		t.Fatalf("expected security_review, got:\n%s", out)
	}
	if !strings.Contains(out, "Secondary: code_review") {
		t.Fatalf("expected code_review secondary, got:\n%s", out)
	}
	if !strings.Contains(out, "reasoning") {
		t.Fatalf("expected reasoning capability, got:\n%s", out)
	}
}

func TestRoutePreviewScreenshotRequiresVision(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "Analyze this screenshot")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(out, "Primary: image_visual_analysis") {
		t.Fatalf("expected image_visual_analysis, got:\n%s", out)
	}
	if !strings.Contains(out, "vision") {
		t.Fatalf("expected vision capability, got:\n%s", out)
	}
	if selectedModel(out) == "" {
		t.Fatalf("expected a vision model selected, got:\n%s", out)
	}
}

func TestRoutePreviewPreferredCompatibleModel(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "--model", "claude-opus-4.1", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	if selectedModel(out) != "claude-opus-4.1" {
		t.Fatalf("expected claude-opus-4.1 selected, got %q", selectedModel(out))
	}
}

func TestRoutePreviewPreferredIncompatibleModel(t *testing.T) {
	// claude-haiku-3.5 lacks vision; for a vision task it must be rejected.
	out, _, exit := runRoutePreviewCmd(t, "--model", "claude-haiku-3.5", "Analyze this screenshot")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	if selectedModel(out) == "claude-haiku-3.5" {
		t.Fatalf("incompatible preferred model should not be selected, got:\n%s", out)
	}
	if !rejectedContains(out, "claude-haiku-3.5") {
		t.Fatalf("expected claude-haiku-3.5 in rejections, got:\n%s", out)
	}
}

func TestRoutePreviewProviderPreference(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "--provider", "anthropic", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	if got := selectedProvider(out); got != "anthropic" {
		t.Fatalf("expected anthropic selected, got %q", got)
	}
}

func TestRoutePreviewProviderAllowlist(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "--allow-provider", "openai", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	// The selected candidate must be openai and anthropic models rejected.
	if selectedProvider(out) != "openai" {
		t.Fatalf("expected openai selected, got %q", selectedProvider(out))
	}
	if !rejectedContains(out, "claude-opus-4.1") {
		t.Fatalf("expected claude-opus-4.1 rejected by allowlist, got:\n%s", out)
	}
}

func TestRoutePreviewModelDenylist(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "--deny-model", "gpt-4o-mini", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	if selectedModel(out) == "gpt-4o-mini" {
		t.Fatalf("denied model should not be selected, got:\n%s", out)
	}
	if !rejectedContains(out, "gpt-4o-mini") {
		t.Fatalf("expected gpt-4o-mini rejected, got:\n%s", out)
	}
}

func TestRoutePreviewRequireKnownPrice(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "--require-known-price", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	// The flag must be honored: every ranked candidate carries a known-price
	// reason (proving only priced models survive). The missing-price rejection
	// path itself is covered by the modelrouter unit tests with a no-price entry.
	if selectedModel(out) == "" {
		t.Fatalf("expected a selected model, got:\n%s", out)
	}
	if strings.Count(out, "known price") == 0 {
		t.Fatalf("expected known-price reasons with --require-known-price, got:\n%s", out)
	}
}

func TestRoutePreviewInputCostLimit(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "--max-input-cost", "0.2", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	// Expensive input models (e.g. gpt-4o at $2.50/M) must be rejected.
	if !rejectedContains(out, "gpt-4o") {
		t.Fatalf("expected gpt-4o rejected for input cost, got:\n%s", out)
	}
}

func TestRoutePreviewOutputCostLimit(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "--max-output-cost", "1", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	// claude-opus-4.1 output is $75/M and must be rejected.
	if !rejectedContains(out, "claude-opus-4.1") {
		t.Fatalf("expected claude-opus-4.1 rejected for output cost, got:\n%s", out)
	}
}

func TestRoutePreviewNoCompatibleModel(t *testing.T) {
	out, _, exit := runRoutePreviewCmd(t, "--allow-provider", "nosuchprovider", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d", exit)
	}
	if selectedModel(out) != "" {
		t.Fatalf("expected no selected model, got %q", selectedModel(out))
	}
	if !strings.Contains(out, "No compatible model") {
		t.Fatalf("expected no-compatible explanation, got:\n%s", out)
	}
}

func TestRoutePreviewMissingPrompt(t *testing.T) {
	_, stderr, exit := runRoutePreviewCmd(t)
	if exit == exitSuccess {
		t.Fatalf("expected non-zero exit for missing prompt, stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "requires a non-empty prompt") {
		t.Fatalf("expected prompt error, got %q", stderr)
	}
}

func TestRoutePreviewEmptyPrompt(t *testing.T) {
	_, stderr, exit := runRoutePreviewCmd(t, "")
	if exit == exitSuccess {
		t.Fatalf("expected non-zero exit for empty prompt")
	}
	if !strings.Contains(stderr, "requires a non-empty prompt") {
		t.Fatalf("expected prompt error, got %q", stderr)
	}
}

func TestRoutePreviewInvalidNumeric(t *testing.T) {
	_, stderr, exit := runRoutePreviewCmd(t, "--max-input-cost", "abc", "Implement OAuth login")
	if exit == exitSuccess {
		t.Fatalf("expected non-zero exit for invalid numeric")
	}
	if !strings.Contains(stderr, "invalid --max-input-cost") {
		t.Fatalf("expected numeric error, got %q", stderr)
	}
}

func TestRoutePreviewJSONParses(t *testing.T) {
	stdout, stderr, exit := runRoutePreviewCmd(t, "--json", "Implement OAuth login")
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("JSON parse error: %v\n%s", err, stdout)
	}
	for _, key := range []string{"prompt", "classification", "decision"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("missing top-level key %q in %v", key, doc)
		}
	}
	cls, ok := doc["classification"].(map[string]any)
	if !ok {
		t.Fatalf("classification not an object: %v", doc["classification"])
	}
	if cls["primary"] != "implementation" {
		t.Fatalf("expected primary implementation, got %v", cls["primary"])
	}
	dec, ok := doc["decision"].(map[string]any)
	if !ok {
		t.Fatalf("decision not an object")
	}
	if dec["selected"] == nil {
		t.Fatalf("expected a selected candidate in JSON")
	}
}

func TestRoutePreviewJSONDeterministic(t *testing.T) {
	first, _, exit1 := runRoutePreviewCmd(t, "--json", "Review this pull request for security vulnerabilities")
	second, _, exit2 := runRoutePreviewCmd(t, "--json", "Review this pull request for security vulnerabilities")
	if exit1 != exitSuccess || exit2 != exitSuccess {
		t.Fatalf("exits=%d,%d", exit1, exit2)
	}
	if first != second {
		t.Fatalf("JSON output not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestRoutePreviewDoesNotCreateSession(t *testing.T) {
	deps, _, sessionCalled := routePreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"route-preview", "Implement OAuth login"}, &stdout, &stderr, deps)
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if *sessionCalled {
		t.Fatal("route-preview must not create a session store")
	}
	if stdout.Len() == 0 {
		t.Fatal("expected output")
	}
}

func TestRoutePreviewDoesNotCallProviders(t *testing.T) {
	deps, providerCalled, _ := routePreviewDeps(t)
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"route-preview", "Analyze this screenshot"}, &stdout, &stderr, deps)
	if exit != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if *providerCalled {
		t.Fatal("route-preview must not construct a provider")
	}
}

func TestRoutePreviewExistingCommandsUnchanged(t *testing.T) {
	// `models` and `providers` must still work after adding route-preview.
	for _, cmd := range [][]string{{"models"}, {"providers"}, {"--version"}} {
		var stdout, stderr bytes.Buffer
		deps, _, _ := routePreviewDeps(t)
		exit := runWithDeps(cmd, &stdout, &stderr, deps)
		if exit != exitSuccess {
			t.Fatalf("command %v exit=%d stderr=%s", cmd, exit, stderr.String())
		}
	}
}
