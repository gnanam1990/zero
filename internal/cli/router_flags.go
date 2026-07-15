package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// routerFlagOptions holds the router-related flags shared by route-preview and
// plan-preview. They are pure routing hints/signals and never touch providers,
// sessions, networks, or execution.
type routerFlagOptions struct {
	provider          string
	model             string
	allowProviders    []string
	denyModels        []string
	requireKnownPrice bool
	maxInputCost      *float64
	maxOutputCost     *float64
}

// tryParseRouterFlag attempts to consume a router-related flag at args[i]. If
// arg is a router flag it mutates opts, advances the index past any flag value,
// and returns (true, nextIndex, nil). Otherwise it returns (false, i, nil) so
// the caller handles its own flags. An execUsageError is returned for malformed
// flag values.
func tryParseRouterFlag(arg string, args []string, i int, opts *routerFlagOptions) (bool, int, error) {
	switch {
	case arg == "--require-known-price":
		opts.requireKnownPrice = true
		return true, i, nil
	case arg == "--provider":
		value, next, err := nextFlagValue(args, i, arg)
		if err != nil {
			return true, i, err
		}
		opts.provider = value
		return true, next, nil
	case strings.HasPrefix(arg, "--provider="):
		value, err := requiredInlineFlagValue(arg, "--provider")
		if err != nil {
			return true, i, err
		}
		opts.provider = value
		return true, i, nil
	case arg == "--model":
		value, next, err := nextFlagValue(args, i, arg)
		if err != nil {
			return true, i, err
		}
		opts.model = value
		return true, next, nil
	case strings.HasPrefix(arg, "--model="):
		value, err := requiredInlineFlagValue(arg, "--model")
		if err != nil {
			return true, i, err
		}
		opts.model = value
		return true, i, nil
	case arg == "--allow-provider":
		value, next, err := nextFlagValue(args, i, arg)
		if err != nil {
			return true, i, err
		}
		opts.allowProviders = append(opts.allowProviders, value)
		return true, next, nil
	case strings.HasPrefix(arg, "--allow-provider="):
		value, err := requiredInlineFlagValue(arg, "--allow-provider")
		if err != nil {
			return true, i, err
		}
		opts.allowProviders = append(opts.allowProviders, value)
		return true, i, nil
	case arg == "--deny-model":
		value, next, err := nextFlagValue(args, i, arg)
		if err != nil {
			return true, i, err
		}
		opts.denyModels = append(opts.denyModels, value)
		return true, next, nil
	case strings.HasPrefix(arg, "--deny-model="):
		value, err := requiredInlineFlagValue(arg, "--deny-model")
		if err != nil {
			return true, i, err
		}
		opts.denyModels = append(opts.denyModels, value)
		return true, i, nil
	case arg == "--max-input-cost":
		value, next, err := nextFlagValue(args, i, arg)
		if err != nil {
			return true, i, err
		}
		f, perr := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if perr != nil {
			return true, i, execUsageError{fmt.Sprintf("invalid --max-input-cost %q: expected a number", value)}
		}
		opts.maxInputCost = &f
		return true, next, nil
	case strings.HasPrefix(arg, "--max-input-cost="):
		f, err := parseInlineFloat(arg, "--max-input-cost")
		if err != nil {
			return true, i, err
		}
		opts.maxInputCost = &f
		return true, i, nil
	case arg == "--max-output-cost":
		value, next, err := nextFlagValue(args, i, arg)
		if err != nil {
			return true, i, err
		}
		f, perr := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if perr != nil {
			return true, i, execUsageError{fmt.Sprintf("invalid --max-output-cost %q: expected a number", value)}
		}
		opts.maxOutputCost = &f
		return true, next, nil
	case strings.HasPrefix(arg, "--max-output-cost="):
		f, err := parseInlineFloat(arg, "--max-output-cost")
		if err != nil {
			return true, i, err
		}
		opts.maxOutputCost = &f
		return true, i, nil
	}
	return false, i, nil
}

func parseInlineFloat(arg, flag string) (float64, error) {
	raw, err := requiredInlineFlagValue(arg, flag)
	if err != nil {
		return 0, err
	}
	f, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if perr != nil {
		return 0, execUsageError{fmt.Sprintf("invalid %s %q: expected a number", flag, raw)}
	}
	return f, nil
}
