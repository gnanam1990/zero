package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/modelrouter"
	"github.com/Gitlawb/zero/internal/taskclass"
)

// runRoutePreview is a fully local, dry-run inspector. It classifies the prompt
// with internal/taskclass, routes the result against the curated model registry
// with internal/modelrouter, and prints the decision. It performs no network
// calls, executes no tools, changes no provider/model selection, creates no
// session, and writes no config or memory.
func runRoutePreview(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, err := parseRoutePreviewArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if options.help {
		if err := writeRoutePreviewHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	prompt := strings.TrimSpace(options.prompt)
	if prompt == "" {
		return writeExecUsageError(stderr, "route-preview requires a non-empty prompt (positional or --prompt)")
	}

	repoPresent := detectRepositoryPresence(deps)

	classification := taskclass.Classify(taskclass.Request{
		Prompt:            prompt,
		HasImages:         false,
		RepositoryPresent: repoPresent,
	})

	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	entries := registry.List(modelregistry.ListOptions{IncludeDeprecated: true})

	decision, err := modelrouter.Decide(modelrouter.Request{
		Task:              classification,
		Candidates:        entries,
		PreferredProvider: options.provider,
		PreferredModel:    options.model,
		AllowedProviders:  options.allowProviders,
		DisallowedModels:  options.denyModels,
		MaxInputCost:      options.maxInputCost,
		MaxOutputCost:     options.maxOutputCost,
		RequireKnownPrice: options.requireKnownPrice,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitProvider)
	}

	if options.json {
		if err := writePrettyJSON(stdout, buildRoutePreviewJSON(prompt, classification, decision)); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	writeRoutePreviewText(stdout, prompt, classification, decision)
	return exitSuccess
}

// detectRepositoryPresence reports whether the current working directory is
// inside a git repository, detected deterministically from a local .git entry.
func detectRepositoryPresence(deps appDeps) bool {
	wd := ""
	if deps.getwd != nil {
		if p, err := deps.getwd(); err == nil {
			wd = p
		}
	}
	if wd == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(wd, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

type routePreviewOptions struct {
	routerFlagOptions
	prompt string
	json   bool
	help   bool
}

func parseRoutePreviewArgs(args []string) (routePreviewOptions, error) {
	var opts routePreviewOptions
	var ropts routerFlagOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			opts.help = true
		case arg == "--json":
			opts.json = true
		default:
			if matched, next, err := tryParseRouterFlag(arg, args, i, &ropts); matched {
				if err != nil {
					return opts, err
				}
				i = next
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return opts, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
			}
			if opts.prompt != "" {
				return opts, execUsageError{"multiple prompts provided; pass a single prompt positionally or via --prompt"}
			}
			opts.prompt = arg
		}
	}
	opts.routerFlagOptions = ropts
	return opts, nil
}

func writeRoutePreviewText(stdout io.Writer, prompt string, cls taskclass.Result, decision modelrouter.Decision) {
	var b strings.Builder

	fmt.Fprintf(&b, "Prompt: %s\n\n", prompt)

	b.WriteString("Task classification:\n")
	fmt.Fprintf(&b, "  Primary: %s\n", cls.Primary)
	if len(cls.Secondary) == 0 {
		b.WriteString("  Secondary: none\n")
	} else {
		parts := make([]string, len(cls.Secondary))
		for i, k := range cls.Secondary {
			parts[i] = string(k)
		}
		fmt.Fprintf(&b, "  Secondary: %s\n", strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, "  Confidence: %s\n", cls.Confidence)

	b.WriteString("\nRequired capabilities:\n")
	if len(cls.RequiredCapabilities) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, c := range cls.RequiredCapabilities {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}

	b.WriteString("\nClassified evidence:\n")
	if len(cls.Evidence) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, e := range cls.Evidence {
			fmt.Fprintf(&b, "  - %s: %s\n", e.Signal, e.Detail)
		}
	}

	b.WriteString("\nSelected candidate:\n")
	if decision.Selected == nil {
		b.WriteString("  (none)\n")
	} else {
		c := decision.Selected
		fmt.Fprintf(&b, "  Model: %s\n", c.Model.ID)
		fmt.Fprintf(&b, "  Provider: %s\n", c.Model.Provider)
		fmt.Fprintf(&b, "  Score: %d\n", c.Score)
	}

	b.WriteString("\nReasons:\n")
	if decision.Selected == nil {
		b.WriteString("  (none)\n")
	} else {
		for _, r := range decision.Selected.Reasons {
			fmt.Fprintf(&b, "  - %s\n", r.Detail)
		}
	}

	b.WriteString("\nRanked candidates:\n")
	if len(decision.Ranked) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for i, c := range decision.Ranked {
			fmt.Fprintf(&b, "  %d. %s [%s] score=%d\n", i+1, c.Model.ID, c.Model.Provider, c.Score)
		}
	}

	b.WriteString("\nRejected candidates:\n")
	if len(decision.Rejected) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, r := range decision.Rejected {
			fmt.Fprintf(&b, "  %s\n", r.ModelID)
			for _, reason := range r.Reasons {
				fmt.Fprintf(&b, "    - %s\n", reason.Detail)
			}
		}
	}

	if decision.NoCompatible {
		b.WriteString("\nNo compatible model was found for this task. Adjust filters or capabilities and retry.\n")
	}

	if _, err := io.WriteString(stdout, b.String()); err != nil {
		return
	}
}

// ---- JSON output ----

type routePreviewJSON struct {
	Prompt         string                     `json:"prompt"`
	Classification routePreviewClassification `json:"classification"`
	Decision       routePreviewDecision       `json:"decision"`
}

type routePreviewClassification struct {
	Primary              string                   `json:"primary"`
	Secondary            []string                 `json:"secondary"`
	Confidence           string                   `json:"confidence"`
	RequiredCapabilities []string                 `json:"required_capabilities"`
	Evidence             []routePreviewReasonPair `json:"evidence"`
}

type routePreviewReasonPair struct {
	Signal string `json:"signal"`
	Detail string `json:"detail"`
}

type routePreviewDecision struct {
	Selected *routePreviewCandidate  `json:"selected"`
	Ranked   []routePreviewCandidate `json:"ranked"`
	Rejected []routePreviewRejection `json:"rejected"`
}

type routePreviewCandidate struct {
	Model    string                   `json:"model"`
	Provider string                   `json:"provider"`
	Score    int                      `json:"score"`
	Reasons  []routePreviewReasonPair `json:"reasons"`
}

type routePreviewRejection struct {
	ModelID string                   `json:"model_id"`
	Reasons []routePreviewReasonPair `json:"reasons"`
}

func buildRoutePreviewJSON(prompt string, cls taskclass.Result, decision modelrouter.Decision) routePreviewJSON {
	out := routePreviewJSON{Prompt: prompt}
	out.Classification.Primary = string(cls.Primary)
	out.Classification.Confidence = string(cls.Confidence)
	out.Classification.Secondary = make([]string, 0, len(cls.Secondary))
	for _, k := range cls.Secondary {
		out.Classification.Secondary = append(out.Classification.Secondary, string(k))
	}
	out.Classification.RequiredCapabilities = make([]string, 0, len(cls.RequiredCapabilities))
	for _, c := range cls.RequiredCapabilities {
		out.Classification.RequiredCapabilities = append(out.Classification.RequiredCapabilities, string(c))
	}
	out.Classification.Evidence = make([]routePreviewReasonPair, 0, len(cls.Evidence))
	for _, e := range cls.Evidence {
		out.Classification.Evidence = append(out.Classification.Evidence, routePreviewReasonPair{Signal: e.Signal, Detail: e.Detail})
	}

	if decision.Selected != nil {
		c := decision.Selected
		out.Decision.Selected = &routePreviewCandidate{
			Model:    c.Model.ID,
			Provider: string(c.Model.Provider),
			Score:    c.Score,
			Reasons:  candidateReasonsJSON(c.Reasons),
		}
	}
	out.Decision.Ranked = make([]routePreviewCandidate, 0, len(decision.Ranked))
	for _, c := range decision.Ranked {
		out.Decision.Ranked = append(out.Decision.Ranked, routePreviewCandidate{
			Model:    c.Model.ID,
			Provider: string(c.Model.Provider),
			Score:    c.Score,
			Reasons:  candidateReasonsJSON(c.Reasons),
		})
	}
	out.Decision.Rejected = make([]routePreviewRejection, 0, len(decision.Rejected))
	for _, r := range decision.Rejected {
		out.Decision.Rejected = append(out.Decision.Rejected, routePreviewRejection{
			ModelID: r.ModelID,
			Reasons: candidateReasonsJSON(r.Reasons),
		})
	}
	return out
}

func candidateReasonsJSON(reasons []modelrouter.Reason) []routePreviewReasonPair {
	out := make([]routePreviewReasonPair, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, routePreviewReasonPair{Signal: r.Signal, Detail: r.Detail})
	}
	return out
}

func writeRoutePreviewHelp(stdout io.Writer) error {
	_, err := fmt.Fprint(stdout, `Usage:
  zero route-preview [flags] "<prompt>"
  zero route-preview --prompt "<prompt>"

Preview how Zero would classify a task and rank model-registry candidates for it.
This is a fully local, dry-run inspector: it performs no network calls, executes
no tools, changes no active provider or model, creates no session, and writes no
config or memory. It does not affect real Zero execution yet.

Flags:
  --provider <provider>      Prefer this provider as a ranking signal.
  --model <model-id>         Prefer this model if compatible.
  --allow-provider <provider>
                             Repeatable hard allowlist of providers.
  --deny-model <model-id>    Repeatable model denylist.
  --require-known-price      Reject models without known pricing.
  --max-input-cost <number>  Maximum registry input cost unit (USD per 1M tokens).
  --max-output-cost <number> Maximum registry output cost unit (USD per 1M tokens).
  --json                     Emit stable machine-readable JSON.
  -h, --help                 Show this help.

Examples:
  zero route-preview "Implement OAuth login"
  zero route-preview --provider anthropic "Design cloud session sync architecture"
  zero route-preview --require-known-price "Review this pull request for security vulnerabilities"
`)
	return err
}
