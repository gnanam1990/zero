package taskclass

import (
	"strings"

	"github.com/Gitlawb/zero/internal/modelregistry"
)

// capabilityOrder fixes the emitted order of RequiredCapabilities so results are
// deterministic across runs.
var capabilityOrder = []modelregistry.ModelCapability{
	modelregistry.ModelCapabilityToolCalling,
	modelregistry.ModelCapabilityStreaming,
	modelregistry.ModelCapabilityJSONMode,
	modelregistry.ModelCapabilityVision,
	modelregistry.ModelCapabilityReasoning,
	modelregistry.ModelCapabilityParallelToolCalls,
	modelregistry.ModelCapabilityThinkingTokens,
	modelregistry.ModelCapabilityImageGeneration,
	modelregistry.ModelCapabilityEmbeddings,
	modelregistry.ModelCapabilityAudio,
}

// kindCapabilities maps each kind to the factual model capabilities a task of
// that kind requires. It uses only stable, factual requirements — never
// provider, model id, price, or quality tiers.
var kindCapabilities = map[Kind][]modelregistry.ModelCapability{
	KindCodeSearch:           {modelregistry.ModelCapabilityToolCalling},
	KindRepoExploration:      {modelregistry.ModelCapabilityToolCalling},
	KindImplementation:       {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
	KindBugInvestigation:     {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
	KindDebugging:            {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
	KindRefactoring:          {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
	KindTesting:              {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
	KindDocumentation:        {modelregistry.ModelCapabilityStreaming},
	KindCodeReview:           {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
	KindSecurityReview:       {modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
	KindArchitecturePlanning: {modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityJSONMode, modelregistry.ModelCapabilityStreaming},
	KindShellSystem:          {modelregistry.ModelCapabilityToolCalling},
	KindImageVisualAnalysis:  {modelregistry.ModelCapabilityVision},
	KindGeneralExplanation:   {modelregistry.ModelCapabilityStreaming},
	KindUnknown:              {},
}

// detector describes one deterministic text rule.
type detector struct {
	kind       Kind
	precedence int // higher wins primary selection among text matches
	phrases    []string
	keywords   []string
}

// detectors are evaluated in slice order; that order fixes the Evidence order.
// Precedence (not slice order) decides the primary kind when several match.
var detectors = []detector{
	{
		kind:       KindSecurityReview,
		precedence: 100,
		phrases: []string{
			"security vulnerabilities", "security vulnerability", "security flaw",
			"security issue", "security review", "security audit", "audit for security",
			"audit this code for security", "vulnerabilities",
		},
		keywords: []string{"security", "vulnerabilities", "audit"},
	},
	{
		kind:       KindShellSystem,
		precedence: 95,
		phrases: []string{
			"delete the", "remove the", "rm -rf", "mkdir", "install dependencies",
			"npm install", "pip install", "apt install", "brew install", "docker ",
			"start the server", "restart the", "kill ", "run the build", "make clean",
			"git reset --hard", "chmod", "move the", "copy the", "format the disk",
			"uninstall",
		},
		keywords: []string{"delete", "remove", "install", "uninstall", "restart", "kill"},
	},
	{
		kind:       KindBugInvestigation,
		precedence: 90,
		phrases: []string{
			"investigate why", "why is", "why this", "why does", "why the",
			"root cause", "figure out why", "what is causing", "what caused",
		},
		keywords: []string{"investigate", "root cause"},
	},
	{
		kind:       KindDebugging,
		precedence: 88,
		phrases: []string{
			"fix this", "fix the", "fix a", "debug", "fix failing", "resolve the bug",
			"troubleshoot",
		},
		keywords: []string{"debug", "fix"},
	},
	{
		kind:       KindRefactoring,
		precedence: 80,
		phrases:    []string{"refactor", "restructure", "clean up the code", "improve the structure"},
		keywords:   []string{"refactor"},
	},
	{
		kind:       KindTesting,
		precedence: 78,
		phrases: []string{
			"write tests", "add tests", "create tests", "tests for", "test for",
			"run the tests", "run tests", "execute the tests", "test suite",
			"run the full test suite", "full test suite", "unit tests",
		},
		keywords: []string{"tests", "test", "testing"},
	},
	{
		kind:       KindImplementation,
		precedence: 75,
		phrases: []string{
			"implement", "add a", "add an", "create a", "create an", "build a",
			"build an", "write a function", "develop", "add support for", "introduce",
		},
		keywords: []string{"implement", "add", "create", "build", "develop"},
	},
	{
		kind:       KindCodeReview,
		precedence: 70,
		phrases: []string{
			"review this pull request", "review the pull request", "review this code",
			"review the code", "code review", "pull request review", "review this pr",
			"review the pr", "pr review",
		},
		keywords: []string{"review", "pull request", "pr"},
	},
	{
		kind:       KindArchitecturePlanning,
		precedence: 65,
		phrases: []string{
			"design an architecture", "design the architecture", "architecture for",
			"architect", "system design", "technical design", "plan the architecture",
			"design a system",
		},
		keywords: []string{"architecture", "design", "plan"},
	},
	{
		kind:       KindRepoExploration,
		precedence: 60,
		phrases: []string{
			"explore the repository", "explore the repo", "understand the codebase",
			"how the codebase", "walk through the code",
		},
		keywords: []string{"explore", "codebase"},
	},
	{
		kind:       KindCodeSearch,
		precedence: 58,
		phrases: []string{
			"find where", "where is", "where are", "locate", "find the implementation",
			"search for", "find the function", "find usages",
		},
		keywords: []string{"find", "search", "locate"},
	},
	{
		kind:       KindDocumentation,
		precedence: 55,
		phrases: []string{
			"update the readme", "update the docs", "write documentation",
			"document this", "readme", "update documentation", "add comments",
		},
		keywords: []string{"readme", "documentation", "docs"},
	},
	{
		// Text-only image detection (e.g. "analyze this screenshot"). Kept low
		// precedence so concrete actions (shell/security/…) win when present;
		// an attached image (Request.HasImages) is handled separately and forces
		// the image kind to primary.
		kind:       KindImageVisualAnalysis,
		precedence: 45,
		phrases: []string{
			"screenshot", "this image", "this picture", "this diagram", "this photo",
			"analyze this screenshot", "visual analysis", "image analysis",
		},
		keywords: []string{"screenshot", "image", "picture", "diagram", "visual"},
	},
	{
		kind:       KindGeneralExplanation,
		precedence: 50,
		phrases: []string{
			"explain", "how does", "how do", "what is", "what are", "why does",
			"tell me about", "describe",
		},
		keywords: []string{"explain", "how", "what"},
	},
}

// normalize lowercases the text and collapses all non-alphanumeric runs to single
// spaces, returning the normalized string and its individual words. No regular
// expressions are used, so there is no risk of pathological backtracking.
func normalize(text string) (string, []string) {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	collapsed := strings.Join(strings.Fields(b.String()), " ")
	return collapsed, strings.Fields(b.String())
}

func containsPhrase(normalized, phrase string) bool {
	if phrase == "" {
		return false
	}
	np, _ := normalize(phrase)
	return np != "" && strings.Contains(normalized, np)
}

func containsKeyword(words []string, keyword string) bool {
	for _, w := range words {
		if w == keyword {
			return true
		}
	}
	return false
}

// resolveExplicitMode maps a caller-supplied mode string to a Kind. An empty or
// unrecognized mode yields (KindUnknown, false).
func resolveExplicitMode(mode string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "security", "security_review", "sec":
		return KindSecurityReview, true
	case "review", "code_review", "cr":
		return KindCodeReview, true
	case "plan", "planning", "architecture", "design":
		return KindArchitecturePlanning, true
	case "implement", "implementation", "impl":
		return KindImplementation, true
	case "debug", "debugging":
		return KindDebugging, true
	case "investigate", "bug", "bug_investigation":
		return KindBugInvestigation, true
	case "test", "testing":
		return KindTesting, true
	case "docs", "documentation":
		return KindDocumentation, true
	case "explore", "search", "code_search":
		return KindCodeSearch, true
	case "refactor":
		return KindRefactoring, true
	case "shell", "system":
		return KindShellSystem, true
	case "explain", "explanation", "general":
		return KindGeneralExplanation, true
	case "image", "vision":
		return KindImageVisualAnalysis, true
	default:
		return KindUnknown, false
	}
}

type match struct {
	kind       Kind
	precedence int
	exact      bool // matched an exact phrase (high confidence) vs a keyword
}

// Classify returns a deterministic task classification for the given request.
// It never selects a model, never calls a provider, and never executes anything.
func Classify(req Request) Result {
	text, words := normalize(req.Prompt)
	matched := make([]match, 0, len(detectors))
	evidence := make([]Evidence, 0, 8)

	// 1. Attached-image signal (strong, deterministic): requires vision.
	if req.HasImages {
		matched = append(matched, match{kind: KindImageVisualAnalysis, precedence: 97, exact: true})
		evidence = append(evidence, Evidence{
			Signal: "image:attached",
			Detail: "request carries attached images; visual analysis requires vision",
		})
	}

	// 2. Explicit mode override: highest-priority primary selector.
	if modeKind, ok := resolveExplicitMode(req.ExplicitMode); ok {
		matched = append(matched, match{kind: modeKind, precedence: 1 << 30, exact: true})
		evidence = append(evidence, Evidence{
			Signal: "mode:" + modeKind.String(),
			Detail: "explicit mode override selected " + modeKind.String(),
		})
	}

	// 3. Text detectors, in fixed slice order (fixes Evidence ordering).
	for _, d := range detectors {
		exact := false
		for _, p := range d.phrases {
			if containsPhrase(text, p) {
				exact = true
				matched = append(matched, match{kind: d.kind, precedence: d.precedence, exact: true})
				evidence = append(evidence, Evidence{
					Signal: "exact:" + p,
					Detail: "matched phrase " + quote(p) + " -> " + d.kind.String(),
				})
				break
			}
		}
		if !exact {
			for _, k := range d.keywords {
				if containsKeyword(words, k) {
					matched = append(matched, match{kind: d.kind, precedence: d.precedence, exact: false})
					evidence = append(evidence, Evidence{
						Signal: "keyword:" + k,
						Detail: "matched keyword " + quote(k) + " -> " + d.kind.String(),
					})
					break
				}
			}
		}
	}

	// Requested-tool signal: multiple independent tools justify parallel calls.
	if len(req.RequestedTools) >= 2 {
		evidence = append(evidence, Evidence{
			Signal: "tools:multiple",
			Detail: "multiple requested tools imply parallel tool calls",
		})
	} else if len(req.RequestedTools) == 1 {
		evidence = append(evidence, Evidence{
			Signal: "tools:single",
			Detail: "a tool was requested",
		})
	}

	primary, hasExact := selectPrimary(req, matched)
	secondary := selectSecondary(primary, matched)
	caps := collectCapabilities(primary, secondary, req)
	conf := confidence(hasExact, matched, primary)

	if primary == KindUnknown {
		evidence = append(evidence, Evidence{
			Signal: "unknown",
			Detail: "no recognized signals; classified as unknown",
		})
	}

	return Result{
		Primary:              primary,
		Secondary:            secondary,
		RequiredCapabilities: caps,
		Confidence:           conf,
		Evidence:             evidence,
	}
}

// selectPrimary applies the documented precedence: explicit mode > attached
// image > highest-precedence text match.
func selectPrimary(req Request, matched []match) (Kind, bool) {
	best := KindUnknown
	bestPrec := -1
	hasExact := false
	for _, m := range matched {
		if m.kind == best && m.precedence > bestPrec {
			bestPrec = m.precedence
			hasExact = hasExact || m.exact
		}
		if m.precedence > bestPrec {
			best = m.kind
			bestPrec = m.precedence
			hasExact = m.exact
		}
	}
	return best, hasExact
}

// selectSecondary returns the matched kinds other than the primary, ordered by
// precedence desc then kind name asc for determinism, with no duplicates.
func selectSecondary(primary Kind, matched []match) []Kind {
	seen := map[Kind]bool{primary: true}
	type ranked struct {
		kind       Kind
		precedence int
	}
	var ranks []ranked
	for _, m := range matched {
		if m.kind == primary || seen[m.kind] {
			continue
		}
		seen[m.kind] = true
		ranks = append(ranks, ranked{kind: m.kind, precedence: m.precedence})
	}
	// Stable, deterministic order: precedence desc, then kind name asc.
	for i := 0; i < len(ranks); i++ {
		for j := i + 1; j < len(ranks); j++ {
			if ranks[j].precedence > ranks[i].precedence ||
				(ranks[j].precedence == ranks[i].precedence && ranks[j].kind < ranks[i].kind) {
				ranks[i], ranks[j] = ranks[j], ranks[i]
			}
		}
	}
	out := make([]Kind, 0, len(ranks))
	for _, r := range ranks {
		out = append(out, r.kind)
	}
	return out
}

// collectCapabilities unions the capabilities of the primary and secondary kinds
// plus vision (when an image is involved) and parallel tool calls (when multiple
// tools are requested). Output follows capabilityOrder for determinism.
func collectCapabilities(primary Kind, secondary []Kind, req Request) []modelregistry.ModelCapability {
	set := map[modelregistry.ModelCapability]bool{}
	add := func(caps []modelregistry.ModelCapability) {
		for _, c := range caps {
			set[c] = true
		}
	}
	if caps, ok := kindCapabilities[primary]; ok {
		add(caps)
	}
	for _, k := range secondary {
		add(kindCapabilities[k])
	}
	if req.HasImages {
		set[modelregistry.ModelCapabilityVision] = true
	}
	if len(req.RequestedTools) >= 2 {
		set[modelregistry.ModelCapabilityParallelToolCalls] = true
	}
	out := make([]modelregistry.ModelCapability, 0, len(set))
	for _, c := range capabilityOrder {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}

// confidence derives from signal quality, never from a fabricated score.
func confidence(hasExact bool, matched []match, primary Kind) Confidence {
	if primary == KindUnknown {
		return ConfidenceLow
	}
	if hasExact {
		return ConfidenceHigh
	}
	return ConfidenceMedium
}

func quote(s string) string {
	if len(s) > 40 {
		return s[:37] + "..."
	}
	return s
}
