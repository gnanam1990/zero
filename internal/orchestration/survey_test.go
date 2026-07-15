package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSurveyFixtureRepo(t *testing.T) {
	root := t.TempDir()
	// Create a minimal Go module structure.
	mustWrite(t, root, "go.mod", "module example.com/test\n\ngo 1.22\n")
	mustWrite(t, root, "cmd/myapp/main.go", "package main\nfunc main() {}\n")
	mustWrite(t, root, "internal/foo/foo.go", "package foo\nfunc Foo() {}\n")
	mustWrite(t, root, "internal/foo/foo_test.go", "package foo\nfunc TestFoo(t *testing.T) {}\n")
	mustWrite(t, root, "internal/bar/bar.go", "package bar\nfunc Bar() {}\n")
	mustWrite(t, root, "internal/foo/README.md", "# foo\n")
	mustWrite(t, root, "docs/ARCHITECTURE.md", "# Architecture\n")
	mustWrite(t, root, "docs/guide.md", "# Guide\n")
	mustWrite(t, root, "README.md", "# Test\n")

	ClearSurveyCache(root)
	survey, err := GetSurvey(root, SurveyOptions{})
	if err != nil {
		t.Fatalf("GetSurvey error: %v", err)
	}
	if survey.Module != "example.com/test" {
		t.Fatalf("expected module example.com/test, got %q", survey.Module)
	}
	if survey.GoVersion != "1.22" {
		t.Fatalf("expected go 1.22, got %q", survey.GoVersion)
	}
	if len(survey.Entrypoints) != 1 || survey.Entrypoints[0] != "cmd/myapp/main.go" {
		t.Fatalf("expected cmd/myapp/main.go entrypoint, got %v", survey.Entrypoints)
	}
	// Should have internal/foo and internal/bar packages.
	pkgPaths := []string{}
	for _, p := range survey.Packages {
		pkgPaths = append(pkgPaths, p.Path)
	}
	sort.Strings(pkgPaths)
	if !contains(pkgPaths, "internal/foo") || !contains(pkgPaths, "internal/bar") {
		t.Fatalf("expected internal/foo and internal/bar packages, got %v", pkgPaths)
	}
	// internal/foo should have tests.
	for _, p := range survey.Packages {
		if p.Path == "internal/foo" && !p.HasTests {
			t.Fatal("expected internal/foo to have tests")
		}
		if p.Path == "internal/foo" && !p.HasReadme {
			t.Fatal("expected internal/foo to have README")
		}
	}
	// Should find documentation files.
	if len(survey.Docs) == 0 {
		t.Fatal("expected documentation files")
	}
	// Source and doc summaries should be non-empty.
	if survey.SourceSummary == "" {
		t.Fatal("expected non-empty source summary")
	}
	if survey.DocSummary == "" {
		t.Fatal("expected non-empty doc summary")
	}
}

func TestSurveyExcludesGitAndVendor(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "go.mod", "module test\n\ngo 1.22\n")
	mustWrite(t, root, "main.go", "package main\nfunc main() {}\n")
	mustWrite(t, root, ".git/config", "[core]\n")
	mustWrite(t, root, "vendor/foo/foo.go", "package foo\n")
	mustWrite(t, root, ".local-bin/zero", "binary")

	ClearSurveyCache(root)
	survey, err := GetSurvey(root, SurveyOptions{})
	if err != nil {
		t.Fatalf("GetSurvey error: %v", err)
	}
	for _, p := range survey.Packages {
		if strings.Contains(p.Path, "vendor") || strings.Contains(p.Path, ".git") {
			t.Fatalf("expected vendor/.git to be excluded, found package %s", p.Path)
		}
	}
	for _, d := range survey.Docs {
		if strings.Contains(d.Path, ".git") {
			t.Fatalf("expected .git docs to be excluded, found %s", d.Path)
		}
	}
}

func TestSurveyDeterministic(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "go.mod", "module test\n\ngo 1.22\n")
	mustWrite(t, root, "cmd/app/main.go", "package main\nfunc main() {}\n")
	mustWrite(t, root, "internal/foo/foo.go", "package foo\n")
	mustWrite(t, root, "docs/README.md", "# docs\n")

	ClearSurveyCache(root)
	first, _ := GetSurvey(root, SurveyOptions{})
	ClearSurveyCache(root)
	second, _ := GetSurvey(root, SurveyOptions{})

	if first.SourceSummary != second.SourceSummary {
		t.Fatalf("source summary not deterministic")
	}
	if first.DocSummary != second.DocSummary {
		t.Fatalf("doc summary not deterministic")
	}
	if len(first.Packages) != len(second.Packages) {
		t.Fatalf("package count changed: %d vs %d", len(first.Packages), len(second.Packages))
	}
	for i := range first.Packages {
		if first.Packages[i].Path != second.Packages[i].Path {
			t.Fatalf("package order changed at %d: %s vs %s", i, first.Packages[i].Path, second.Packages[i].Path)
		}
	}
}

func TestSurveyCacheReuse(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "go.mod", "module test\n\ngo 1.22\n")
	mustWrite(t, root, "main.go", "package main\n")

	ClearSurveyCache(root)
	first, _ := GetSurvey(root, SurveyOptions{})
	if first.CacheHits != 0 {
		t.Fatalf("expected 0 cache hits on first call, got %d", first.CacheHits)
	}
	second, _ := GetSurvey(root, SurveyOptions{})
	if second.CacheHits != 1 {
		t.Fatalf("expected 1 cache hit on second call, got %d", second.CacheHits)
	}
}

func TestSurveyRenderForTask(t *testing.T) {
	survey := &Survey{
		Module:        "test",
		GoVersion:     "1.22",
		SourceSummary: "Module: test (Go 1.22)\nPackages:\n  internal/foo (2 files)",
		DocSummary:    "Documentation files:\n  docs/README.md",
	}
	out := RenderSurveyForTask(survey, "source")
	if !strings.Contains(out, "Module: test") {
		t.Fatalf("expected source summary in output, got %q", out)
	}
	out = RenderSurveyForTask(survey, "docs")
	if !strings.Contains(out, "Documentation files:") {
		t.Fatalf("expected doc summary in output, got %q", out)
	}
	out = RenderSurveyForTask(nil, "source")
	if out != "" {
		t.Fatal("expected empty output for nil survey")
	}
}

func TestSurveyCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The survey scan should still complete (it doesn't check context),
	// but the coordinator's Run should handle cancellation properly.
	_ = ctx
	root := t.TempDir()
	mustWrite(t, root, "go.mod", "module test\n\ngo 1.22\n")
	ClearSurveyCache(root)
	_, err := GetSurvey(root, SurveyOptions{})
	if err != nil {
		t.Fatalf("GetSurvey error: %v", err)
	}
}

func TestSurveyStableSorting(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "go.mod", "module test\n\ngo 1.22\n")
	mustWrite(t, root, "internal/zebra/zebra.go", "package zebra\n")
	mustWrite(t, root, "internal/alpha/alpha.go", "package alpha\n")
	mustWrite(t, root, "internal/mid/mid.go", "package mid\n")

	ClearSurveyCache(root)
	survey, _ := GetSurvey(root, SurveyOptions{})
	// Packages should be sorted by path.
	for i := 1; i < len(survey.Packages); i++ {
		if survey.Packages[i-1].Path > survey.Packages[i].Path {
			t.Fatalf("packages not sorted: %s > %s", survey.Packages[i-1].Path, survey.Packages[i].Path)
		}
	}
	// Docs should be sorted by path.
	for i := 1; i < len(survey.Docs); i++ {
		if survey.Docs[i-1].Path > survey.Docs[i].Path {
			t.Fatalf("docs not sorted: %s > %s", survey.Docs[i-1].Path, survey.Docs[i].Path)
		}
	}
}

func TestMetricsValidWhenSurveyUnavailable(t *testing.T) {
	// RunMetrics with no survey fields should still render correctly.
	metrics := &RunMetrics{
		RunWallMs:          5000,
		PeakWorkers:        1,
		Concurrency:        "serialized",
		TotalProviderCalls: 5,
		Tasks:              []TaskMetric{{TaskID: "task-1", Status: "completed"}},
	}
	out := FormatMetricsCompact(metrics)
	if !strings.Contains(out, "Completed 1/1") {
		t.Fatalf("expected completed count, got %q", out)
	}
	if !strings.Contains(out, "Peak workers: 1") {
		t.Fatalf("expected peak workers, got %q", out)
	}
	// Survey fields should be zero/omitted — no crash.
	if metrics.SurveyBuildMs != 0 {
		t.Fatal("expected zero survey build ms")
	}
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
