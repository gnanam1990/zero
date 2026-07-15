package taskclass

import (
	"reflect"
	"sort"
	"testing"

	"github.com/Gitlawb/zero/internal/modelregistry"
)

// want describes the expected classification outcome for a case.
type want struct {
	primary    Kind
	secondary  []Kind
	confidence Confidence
	mustCaps   []modelregistry.ModelCapability
	noCaps     []modelregistry.ModelCapability
}

type tc struct {
	name string
	req  Request
	want want
}

func cases() []tc {
	return []tc{
		{
			name: "security_review does not collapse into code_review",
			req:  Request{Prompt: "run a security review on the auth module and check for vulnerabilities"},
			want: want{
				primary:    KindSecurityReview,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
				noCaps:     []modelregistry.ModelCapability{modelregistry.ModelCapabilityVision},
			},
		},
		{
			name: "code_review exact phrase",
			req:  Request{Prompt: "review this pull request for style issues"},
			want: want{
				primary:    KindCodeReview,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
			},
		},
		{
			name: "shell/system beats generic implementation",
			req:  Request{Prompt: "delete the build artifacts directory and clean the workspace"},
			want: want{
				primary:    KindShellSystem,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling},
				noCaps:     []modelregistry.ModelCapability{modelregistry.ModelCapabilityVision},
			},
		},
		{
			name: "bug_investigation distinct from implementation",
			req:  Request{Prompt: "investigate why the login endpoint returns 500 for valid tokens"},
			want: want{
				primary:    KindBugInvestigation,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
			},
		},
		{
			name: "debugging distinct from bug_investigation",
			req:  Request{Prompt: "fix the failing migration that breaks the test database"},
			want: want{
				primary:    KindDebugging,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
			},
		},
		{
			name: "test creation distinct from execution",
			req:  Request{Prompt: "write tests for the payment service"},
			want: want{
				primary:    KindTesting,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
			},
		},
		{
			name: "test execution distinct from creation",
			req:  Request{Prompt: "run the full test suite and report failures"},
			want: want{
				primary:    KindTesting,
				confidence: ConfidenceHigh,
			},
		},
		{
			name: "implementation keyword",
			req:  Request{Prompt: "implement a retry policy for the http client"},
			want: want{
				primary:    KindImplementation,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityStreaming},
			},
		},
		{
			name: "refactoring keyword",
			req:  Request{Prompt: "refactor the user store to use the new repository pattern"},
			want: want{
				primary:    KindRefactoring,
				confidence: ConfidenceHigh,
			},
		},
		{
			name: "image attached forces vision",
			req:  Request{Prompt: "what is wrong with this layout", HasImages: true},
			want: want{
				primary:    KindImageVisualAnalysis,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityVision},
			},
		},
		{
			name: "image text hint lower precedence than shell",
			req:  Request{Prompt: "delete this screenshot from the uploads folder"},
			want: want{
				primary:    KindShellSystem,
				secondary:  []Kind{KindImageVisualAnalysis},
				confidence: ConfidenceMedium,
			},
		},
		{
			name: "repo exploration",
			req:  Request{Prompt: "explore the repository to understand the module layout"},
			want: want{
				primary:    KindRepoExploration,
				confidence: ConfidenceHigh,
			},
		},
		{
			name: "code search",
			req:  Request{Prompt: "find where the config is loaded at startup"},
			want: want{
				primary:    KindCodeSearch,
				confidence: ConfidenceHigh,
			},
		},
		{
			name: "documentation",
			req:  Request{Prompt: "update the readme with the new setup steps"},
			want: want{
				primary:    KindDocumentation,
				confidence: ConfidenceHigh,
			},
		},
		{
			name: "architecture planning",
			req:  Request{Prompt: "design an architecture for the multi-tenant billing system"},
			want: want{
				primary:    KindArchitecturePlanning,
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityJSONMode, modelregistry.ModelCapabilityStreaming},
			},
		},
		{
			name: "general explanation",
			req:  Request{Prompt: "explain how the event loop works in node"},
			want: want{
				primary:    KindGeneralExplanation,
				confidence: ConfidenceHigh,
			},
		},
		{
			name: "unknown stays unknown",
			req:  Request{Prompt: "zzxqwk the qwjb thingy"},
			want: want{
				primary:    KindUnknown,
				confidence: ConfidenceLow,
			},
		},
		{
			name: "explicit mode override",
			req:  Request{Prompt: "delete the auth module", ExplicitMode: "security"},
			want: want{
				primary:    KindSecurityReview,
				secondary:  []Kind{KindShellSystem},
				confidence: ConfidenceHigh,
			},
		},
		{
			name: "multiple tools imply parallel calls",
			req:  Request{Prompt: "investigate why the compile fails and run the tests", RequestedTools: []string{"search", "bash"}},
			want: want{
				primary:    KindBugInvestigation,
				secondary:  []Kind{KindTesting},
				confidence: ConfidenceHigh,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityParallelToolCalls},
			},
		},
		{
			name: "keyword-only falls to medium confidence",
			req:  Request{Prompt: "some review of the code perhaps"},
			want: want{
				primary:    KindCodeReview,
				confidence: ConfidenceMedium,
				mustCaps:   []modelregistry.ModelCapability{modelregistry.ModelCapabilityToolCalling, modelregistry.ModelCapabilityReasoning, modelregistry.ModelCapabilityStreaming},
			},
		},
	}
}

func hasCaps(got []modelregistry.ModelCapability, want []modelregistry.ModelCapability) bool {
	set := map[modelregistry.ModelCapability]bool{}
	for _, c := range got {
		set[c] = true
	}
	for _, c := range want {
		if !set[c] {
			return false
		}
	}
	return true
}

func TestClassifyTable(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.req)
			if got.Primary != c.want.primary {
				t.Errorf("primary = %q, want %q", got.Primary, c.want.primary)
			}
			if got.Confidence != c.want.confidence {
				t.Errorf("confidence = %q, want %q", got.Confidence, c.want.confidence)
			}
			if c.want.secondary != nil && !reflect.DeepEqual(got.Secondary, c.want.secondary) {
				t.Errorf("secondary = %v, want %v", got.Secondary, c.want.secondary)
			}
			for _, cap := range c.want.mustCaps {
				if !hasCaps(got.RequiredCapabilities, []modelregistry.ModelCapability{cap}) {
					t.Errorf("missing required capability %q in %v", cap, got.RequiredCapabilities)
				}
			}
			for _, cap := range c.want.noCaps {
				if hasCaps(got.RequiredCapabilities, []modelregistry.ModelCapability{cap}) {
					t.Errorf("unexpected capability %q present in %v", cap, got.RequiredCapabilities)
				}
			}
			// Determinism of evidence ordering within a single run is implied;
			// duplicates are checked below.
		})
	}
}

func TestDeterminism(t *testing.T) {
	inputs := []Request{
		{Prompt: "investigate why the build fails and run the tests", RequestedTools: []string{"search", "bash"}},
		{Prompt: "review this pull request for style issues"},
		{Prompt: "delete this screenshot from the uploads folder"},
		{Prompt: "zzxqwk the qwjb thingy"},
	}
	for _, in := range inputs {
		first := Classify(in)
		for i := 0; i < 20; i++ {
			again := Classify(in)
			if !reflect.DeepEqual(first, again) {
				t.Errorf("Classify not deterministic for %q:\n first=%+v\n again=%+v", in.Prompt, first, again)
			}
		}
	}
}

func TestNoDuplicates(t *testing.T) {
	for _, c := range cases() {
		got := Classify(c.req)
		seen := map[Kind]bool{}
		for _, k := range got.Secondary {
			if seen[k] {
				t.Errorf("duplicate secondary kind %q for %q", k, c.req.Prompt)
			}
			seen[k] = true
		}
		seenCap := map[modelregistry.ModelCapability]bool{}
		for _, cp := range got.RequiredCapabilities {
			if seenCap[cp] {
				t.Errorf("duplicate capability %q for %q", cp, c.req.Prompt)
			}
			seenCap[cp] = true
		}
	}
}

func TestStableCapabilityOrder(t *testing.T) {
	got := Classify(Request{Prompt: "investigate why the build fails and run the tests", RequestedTools: []string{"search", "bash"}})
	order := append([]modelregistry.ModelCapability(nil), capabilityOrder...)
	sort.SliceStable(order, func(i, j int) bool { return false })
	// capabilityOrder is already sorted; verify the result follows it.
	idx := map[modelregistry.ModelCapability]int{}
	for i, c := range capabilityOrder {
		idx[c] = i
	}
	for i := 1; i < len(got.RequiredCapabilities); i++ {
		if idx[got.RequiredCapabilities[i]] < idx[got.RequiredCapabilities[i-1]] {
			t.Errorf("capability order not stable: %v", got.RequiredCapabilities)
		}
	}
}

func TestNoInputMutation(t *testing.T) {
	tools := []string{"search", "bash"}
	req := Request{Prompt: "investigate why the build fails", RequestedTools: tools}
	_ = Classify(req)
	if len(tools) != 2 || tools[0] != "search" || tools[1] != "bash" {
		t.Errorf("input RequestedTools mutated: %v", tools)
	}
	// Run multiple classifications; ensure tools slice content is untouched.
	for i := 0; i < 5; i++ {
		_ = Classify(req)
	}
	if tools[0] != "search" || tools[1] != "bash" {
		t.Errorf("input RequestedTools mutated after repeated calls: %v", tools)
	}
}

func TestConfidenceLevels(t *testing.T) {
	if got := Classify(Request{Prompt: "explain how the event loop works in node"}).Confidence; got != ConfidenceHigh {
		t.Errorf("exact phrase should be high, got %q", got)
	}
	if got := Classify(Request{Prompt: "some review of the code perhaps"}).Confidence; got != ConfidenceMedium {
		t.Errorf("keyword-only should be medium, got %q", got)
	}
	if got := Classify(Request{Prompt: "zzxqwk the qwjb thingy"}).Confidence; got != ConfidenceLow {
		t.Errorf("unknown should be low, got %q", got)
	}
}
