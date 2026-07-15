package modelrouter

import (
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/taskclass"
)

// Request is the deterministic router input. It carries no provider clients,
// secrets, or network configuration, and is decoupled from CLI/TUI types.
type Request struct {
	// Task is the classification produced by internal/taskclass. Its
	// RequiredCapabilities drive hard capability filtering.
	Task taskclass.Result

	// Candidates are the factual model registry entries to consider. The router
	// never mutates this slice or the entries within it.
	Candidates []modelregistry.ModelEntry

	// PreferredProvider, when set, is a ranking signal only (not a hard filter).
	PreferredProvider string

	// PreferredModel, when set, is an explicit user/runtime choice. If it passes
	// the hard filters it is ranked first.
	PreferredModel string

	// AllowedProviders, when non-empty, restricts candidates to these providers.
	// An entry is allowed if its primary Provider or any APIProviders member
	// matches. Empty means "allow all providers".
	AllowedProviders []string

	// DisallowedModels lists model IDs (or aliases) that must be rejected.
	DisallowedModels []string

	// LocalOnly restricts candidates to local models. Because the registry has
	// no stable local/remote property, the caller must supply IsLocal; without
	// it LocalOnly cannot be applied reliably and Decide returns an error.
	LocalOnly bool
	IsLocal   func(modelregistry.ModelEntry) bool

	// MaxInputCost / MaxOutputCost are optional upper bounds (USD per 1M tokens)
	// on the candidate's base input/output price. They only apply when the
	// candidate has a known price; missing price is never treated as free.
	MaxInputCost  *float64
	MaxOutputCost *float64

	// RequireKnownPrice rejects any candidate whose price is unknown/missing.
	RequireKnownPrice bool
}

// Reason is one deterministic, human-readable explanation for a candidate's
// rank or a rejection. Signal identifies the deterministic rule; Detail gives
// context.
type Reason struct {
	Signal string
	Detail string
}

// Candidate is a surviving, scored model with its explanatory reasons.
type Candidate struct {
	Model   modelregistry.ModelEntry
	Score   int
	Reasons []Reason
}

// Rejection records a model that failed a hard filter, with reasons.
type Rejection struct {
	ModelID string
	Reasons []Reason
}

// Decision is the deterministic router outcome. Selected points at the top
// ranked candidate (nil when none passed). NoCompatible is true when candidates
// were supplied but every one was rejected.
type Decision struct {
	Selected     *Candidate
	Ranked       []Candidate
	Rejected     []Rejection
	NoCompatible bool
}
