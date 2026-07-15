package orchestration

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Gitlawb/zero/internal/repomap"
	"github.com/Gitlawb/zero/internal/workspaceindex"
)

// Survey is a pre-computed, immutable repository overview shared between
// parallel read-only orchestration tasks. Built once per run; never mutated.
type Survey struct {
	Root          string
	BuiltAt       time.Time
	BuildMs       int64
	CacheHits     int
	Module        string
	GoVersion     string
	Entrypoints   []string
	Packages      []SurveyPackage
	Docs          []SurveyDoc
	KeyFiles      []SurveyKeyFile
	SourceSummary string
	DocSummary    string
	Truncated     bool
}

type SurveyPackage struct {
	Name      string
	Path      string
	FileCount int
	HasTests  bool
	HasReadme bool
}

type SurveyDoc struct {
	Path     string
	Category string
}

type SurveyKeyFile struct {
	Path     string
	Area     string
	Priority int
}

// SurveyOptions configures the survey scan.
type SurveyOptions struct {
	MaxFiles int
	MaxDepth int
}

// surveyCache memoizes a Survey per root so parallel workers share the same
// immutable snapshot. Safe for concurrent use.
var surveyCache sync.Map

type cachedSurvey struct {
	survey *Survey
	err    error
}

// GetSurvey returns a cached survey for root, or builds one if absent.
// Concurrent callers for the same root share the same result.
func GetSurvey(root string, opts SurveyOptions) (*Survey, error) {
	key := root
	if cached, ok := surveyCache.Load(key); ok {
		cs := cached.(*cachedSurvey)
		cs.survey.CacheHits++
		return cs.survey, cs.err
	}
	survey, err := buildSurvey(root, opts)
	surveyCache.Store(key, &cachedSurvey{survey: survey, err: err})
	return survey, err
}

// ClearSurveyCache drops the cached survey for a root (or all when root == "").
func ClearSurveyCache(root string) {
	if root == "" {
		surveyCache.Range(func(key, _ any) bool {
			surveyCache.Delete(key)
			return true
		})
		return
	}
	surveyCache.Delete(root)
}

func buildSurvey(root string, opts SurveyOptions) (*Survey, error) {
	start := time.Now()
	if root == "" {
		return &Survey{BuiltAt: start, BuildMs: 0}, nil
	}

	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = workspaceindex.DefaultMaxFiles
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = workspaceindex.DefaultMaxDepth
	}

	snap, err := repomap.Scan(root, repomap.Options{MaxFiles: maxFiles, MaxDepth: maxDepth})
	if err != nil {
		return nil, fmt.Errorf("survey scan: %w", err)
	}

	survey := &Survey{
		Root:      root,
		BuiltAt:   start,
		Truncated: snap.Truncated,
	}

	// Module info from go.mod.
	survey.Module, survey.GoVersion = readGoMod(root)

	// Classify files into packages, docs, and key files.
	pkgMap := map[string]*SurveyPackage{}
	var docs []SurveyDoc
	var keyFiles []SurveyKeyFile
	var goFiles []string
	var readmePaths []string

	for _, f := range snap.Files {
		rel := f.Path
		ext := f.Extension
		if ext == "" {
			ext = strings.ToLower(path.Ext(rel))
		}

		// Go source files → packages.
		if ext == ".go" && !strings.HasSuffix(rel, "_test.go") {
			pkgPath := path.Dir(rel)
			if pkgPath == "." || pkgPath == "/" {
				pkgPath = ""
			}
			pkg, ok := pkgMap[pkgPath]
			if !ok {
				pkg = &SurveyPackage{Name: path.Base(pkgPath), Path: pkgPath}
				pkgMap[pkgPath] = pkg
			}
			pkg.FileCount++
			goFiles = append(goFiles, rel)
		}

		// Test files.
		if ext == ".go" && strings.HasSuffix(rel, "_test.go") {
			pkgPath := path.Dir(rel)
			if pkg, ok := pkgMap[pkgPath]; ok {
				pkg.HasTests = true
			} else {
				pkg = &SurveyPackage{Name: path.Base(pkgPath), Path: pkgPath, HasTests: true}
				pkgMap[pkgPath] = pkg
			}
		}

		// Track README files for second pass (package may not exist yet).
		base := strings.ToLower(path.Base(rel))
		if base == "readme.md" || base == "readme.txt" || base == "readme" {
			readmePaths = append(readmePaths, path.Dir(rel))
		}

		// Documentation files.
		if isDocFile(rel, ext) {
			docs = append(docs, SurveyDoc{Path: rel, Category: docCategory(rel)})
		}

		// Key source entrypoints.
		if area, prio, ok := classifyKeyFile(rel); ok {
			keyFiles = append(keyFiles, SurveyKeyFile{Path: rel, Area: area, Priority: prio})
		}
	}

	// Second pass: mark packages with READMEs (deferred so order doesn't matter).
	for _, dir := range readmePaths {
		if pkg, ok := pkgMap[dir]; ok {
			pkg.HasReadme = true
		}
	}

	// Sort packages by path.
	for _, pkg := range pkgMap {
		survey.Packages = append(survey.Packages, *pkg)
	}
	sort.Slice(survey.Packages, func(i, j int) bool {
		return survey.Packages[i].Path < survey.Packages[j].Path
	})

	// Sort docs by path.
	survey.Docs = docs
	sort.Slice(survey.Docs, func(i, j int) bool {
		return survey.Docs[i].Path < survey.Docs[j].Path
	})

	// Sort key files by priority then path.
	survey.KeyFiles = keyFiles
	sort.Slice(survey.KeyFiles, func(i, j int) bool {
		if survey.KeyFiles[i].Priority != survey.KeyFiles[j].Priority {
			return survey.KeyFiles[i].Priority < survey.KeyFiles[j].Priority
		}
		return survey.KeyFiles[i].Path < survey.KeyFiles[j].Path
	})

	// Entrypoints from cmd/.
	for _, f := range snap.Files {
		if strings.HasPrefix(f.Path, "cmd/") && strings.HasSuffix(f.Path, "main.go") {
			survey.Entrypoints = append(survey.Entrypoints, f.Path)
		}
	}
	sort.Strings(survey.Entrypoints)

	// Build summaries.
	survey.SourceSummary = renderSourceSummary(survey)
	survey.DocSummary = renderDocSummary(survey)

	survey.BuildMs = time.Since(start).Milliseconds()
	return survey, nil
}

func readGoMod(root string) (module, goVersion string) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			module = strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
		if strings.HasPrefix(line, "go ") {
			goVersion = strings.TrimSpace(strings.TrimPrefix(line, "go "))
		}
	}
	return module, goVersion
}

func isDocFile(rel, ext string) bool {
	base := strings.ToLower(path.Base(rel))
	if base == "readme.md" || base == "readme" || base == "readme.txt" {
		return true
	}
	if ext == ".md" || ext == ".rst" || ext == ".txt" {
		// Only count docs/ directory or root-level markdown.
		if strings.HasPrefix(rel, "docs/") || !strings.Contains(rel, "/") {
			return true
		}
	}
	return false
}

func docCategory(rel string) string {
	if strings.HasPrefix(rel, "docs/") {
		sub := strings.TrimPrefix(rel, "docs/")
		if idx := strings.Index(sub, "/"); idx > 0 {
			return sub[:idx]
		}
		return "root"
	}
	return "root"
}

func classifyKeyFile(rel string) (area string, priority int, ok bool) {
	base := strings.ToLower(path.Base(rel))
	dir := path.Dir(rel)

	// Entrypoints.
	if strings.HasPrefix(rel, "cmd/") && strings.HasSuffix(rel, "main.go") {
		return "entrypoint", 1, true
	}

	// Key agent files.
	if strings.HasPrefix(dir, "internal/agent/") && (base == "loop.go" || base == "types.go" || base == "run.go") {
		return "agent", 2, true
	}

	// Tools.
	if strings.HasPrefix(dir, "internal/tools/") && (base == "registry.go" || base == "tools.go") {
		return "tools", 3, true
	}

	// Orchestration.
	if strings.HasPrefix(dir, "internal/orchestration/") {
		return "orchestration", 2, true
	}
	if strings.HasPrefix(dir, "internal/cli/") && strings.Contains(base, "orchestrat") {
		return "orchestration", 2, true
	}

	// Planner/scheduler/router.
	for _, pkg := range []string{"planner", "scheduler", "modelrouter", "taskclass", "executor"} {
		if strings.HasPrefix(dir, "internal/"+pkg+"/") {
			return pkg, 3, true
		}
	}

	// Sessions.
	if strings.HasPrefix(dir, "internal/sessions/") && base == "store.go" {
		return "sessions", 3, true
	}

	// Providers.
	if strings.HasPrefix(dir, "internal/providers/") {
		return "providers", 4, true
	}

	// Sandbox.
	if strings.HasPrefix(dir, "internal/sandbox/") {
		return "sandbox", 4, true
	}

	// MCP.
	if strings.HasPrefix(dir, "internal/mcp/") {
		return "mcp", 4, true
	}

	return "", 0, false
}

func renderSourceSummary(s *Survey) string {
	var b strings.Builder
	if s.Module != "" {
		fmt.Fprintf(&b, "Module: %s", s.Module)
		if s.GoVersion != "" {
			fmt.Fprintf(&b, " (Go %s)", s.GoVersion)
		}
		b.WriteString("\n")
	}
	if len(s.Entrypoints) > 0 {
		b.WriteString("Entrypoints:\n")
		for _, ep := range s.Entrypoints {
			fmt.Fprintf(&b, "  %s\n", ep)
		}
	}
	if len(s.Packages) > 0 {
		b.WriteString("Packages:\n")
		for _, pkg := range s.Packages {
			flags := ""
			if pkg.HasTests {
				flags += " +tests"
			}
			if pkg.HasReadme {
				flags += " +readme"
			}
			fmt.Fprintf(&b, "  %s (%d files%s)\n", pkg.Path, pkg.FileCount, flags)
		}
	}
	if len(s.KeyFiles) > 0 {
		b.WriteString("Key source files:\n")
		for _, kf := range s.KeyFiles {
			fmt.Fprintf(&b, "  %s [%s]\n", kf.Path, kf.Area)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderDocSummary(s *Survey) string {
	if len(s.Docs) == 0 {
		return "No documentation files found."
	}
	var b strings.Builder
	b.WriteString("Documentation files:\n")
	for _, doc := range s.Docs {
		fmt.Fprintf(&b, "  %s (%s)\n", doc.Path, doc.Category)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderSurveyForTask renders the survey as a compact context block injected
// into a read-only task prompt. The source parameter selects which view to
// render: "source" for Go source surveys, "docs" for documentation surveys,
// "all" for the full overview.
func RenderSurveyForTask(s *Survey, view string) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Repository survey (pre-computed, read-only):\n")
	b.WriteString(strings.Repeat("─", 40))
	b.WriteString("\n")

	switch view {
	case "source":
		if s.SourceSummary != "" {
			b.WriteString(s.SourceSummary)
			b.WriteString("\n")
		}
	case "docs":
		if s.DocSummary != "" {
			b.WriteString(s.DocSummary)
			b.WriteString("\n")
		}
	default:
		if s.SourceSummary != "" {
			b.WriteString(s.SourceSummary)
			b.WriteString("\n")
		}
		if s.DocSummary != "" {
			b.WriteString("\n")
			b.WriteString(s.DocSummary)
			b.WriteString("\n")
		}
	}

	b.WriteString("\nUse this survey as a starting index. You may still use grep, glob, read_file, and other read-only tools for deeper evidence.\n")
	return b.String()
}

// Suppress unused import warnings for parser/token/ast — they're used in
// future per-file analysis but not yet in the initial version.
var _ = ast.File{}
var _ = token.NewFileSet
var _ = parser.ParseFile
var _ = runtime.GOOS
