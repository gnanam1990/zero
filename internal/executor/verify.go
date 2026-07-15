package executor

import "context"

// NoVerifier is the default verifier used when verification is not configured.
// It reports "not_available" so the completion gate never silently claims a pass.
func NoVerifier(_ context.Context, _ string, _ []string) VerificationOutcome {
	return VerificationOutcome{Status: "not_available"}
}
