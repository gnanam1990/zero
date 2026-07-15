package taskclass

import "github.com/Gitlawb/zero/internal/modelregistry"

// Kind is a stable, factual task category. A future model router consumes these;
// the classifier never decides which model to use.
type Kind string

const (
	KindUnknown              Kind = "unknown"
	KindCodeSearch           Kind = "code_search"
	KindRepoExploration      Kind = "repo_exploration"
	KindImplementation       Kind = "implementation"
	KindBugInvestigation     Kind = "bug_investigation"
	KindDebugging            Kind = "debugging"
	KindRefactoring          Kind = "refactoring"
	KindTesting              Kind = "testing"
	KindDocumentation        Kind = "documentation"
	KindCodeReview           Kind = "code_review"
	KindSecurityReview       Kind = "security_review"
	KindArchitecturePlanning Kind = "architecture_planning"
	KindShellSystem          Kind = "shell_system"
	KindImageVisualAnalysis  Kind = "image_visual_analysis"
	KindGeneralExplanation   Kind = "general_explanation"
)

// String returns the stable string value of a Kind.
func (k Kind) String() string { return string(k) }

// Confidence is an objective signal-quality level. It is never a fabricated
// percentage.
type Confidence string

// String returns the stable string value of a Confidence.
func (c Confidence) String() string { return string(c) }

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Request is the deterministic classifier input. It deliberately carries no
// provider or model information and is decoupled from TUI/CLI types.
type Request struct {
	Prompt            string
	HasImages         bool
	RepositoryPresent bool
	RequestedTools    []string
	ExplicitMode      string
}

// Evidence records one deterministic signal that contributed to a classification.
type Evidence struct {
	// Signal identifies the deterministic rule, e.g. "exact:security
	// vulnerabilities" or "mode:security" or "image:attached".
	Signal string
	// Detail is a human-readable explanation of the signal's effect.
	Detail string
}

// Result is the deterministic classification outcome. It is advisory only:
// it grants no permissions and selects no model.
type Result struct {
	Primary              Kind
	Secondary            []Kind
	RequiredCapabilities []modelregistry.ModelCapability
	Confidence           Confidence
	Evidence             []Evidence
}
