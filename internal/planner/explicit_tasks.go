package planner

import (
	"regexp"
	"strings"
)

// explicitTaskHeader matches section headers like "Task 1:", "Task 2 -",
// "Task #1:", "Step 1:" (only when it contains imperative content). The
// match is case-insensitive and allows flexible punctuation.
var explicitTaskHeader = regexp.MustCompile(`(?im)^\s*(?:Task|Step)\s*#?(\d+)\s*[:\-\.]\s*(.*)$`)

// globalConstraintPatterns are trailing lines that apply to ALL tasks, not
// just one. They are stripped from individual task bodies and re-appended
// to each extracted task prompt.
var globalConstraintPatterns = []string{
	"use read-only tools only",
	"do not ask clarifying questions",
	"do not ask questions",
	"do not modify files",
	"complete both tasks independently",
	"complete all tasks independently",
	"complete tasks independently",
	"do not modify",
	"read-only",
}

// explicitTask is one extracted task section.
type explicitTask struct {
	Number  int
	Heading string // The heading line (e.g. "Task 1: inspect documentation files...")
	Body    string // The full task body including the heading
}

// parseExplicitTasks detects whether the prompt contains explicit numbered
// task sections (Task 1:, Task 2:, etc.) and extracts them. Returns nil when
// fewer than 2 valid task sections are found — the planner then falls back to
// its existing implicit decomposition.
//
// Requirements:
//   - At least 2 valid task sections required.
//   - Each section must contain meaningful imperative content (not just a
//     heading or a numbered example).
//   - Global trailing constraints (read-only, do not modify, etc.) are
//     extracted and appended to every task body.
//   - Text before the first task heading is retained as shared context.
//   - Source order is preserved.
//   - Whitespace is normalized deterministically.
func parseExplicitTasks(prompt string) []explicitTask {
	if prompt == "" {
		return nil
	}

	// Normalize CRLF to LF.
	normalized := strings.ReplaceAll(prompt, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	lines := strings.Split(normalized, "\n")

	// Find all task header line indices.
	type headerMatch struct {
		lineIndex int
		number    int
		heading   string
	}
	var headers []headerMatch
	for i, line := range lines {
		m := explicitTaskHeader.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		num := 0
		for _, c := range m[1] {
			num = num*10 + int(c-'0')
		}
		if num < 1 {
			continue
		}
		heading := strings.TrimSpace(m[2])
		headers = append(headers, headerMatch{
			lineIndex: i,
			number:    num,
			heading:   heading,
		})
	}

	if len(headers) < 2 {
		return nil // Need at least 2 tasks to activate explicit decomposition.
	}

	// Extract shared context (text before the first task heading).
	sharedContext := ""
	if headers[0].lineIndex > 0 {
		var ctxLines []string
		for i := 0; i < headers[0].lineIndex; i++ {
			line := strings.TrimSpace(lines[i])
			if line != "" {
				ctxLines = append(ctxLines, lines[i])
			}
		}
		sharedContext = strings.Join(ctxLines, "\n")
	}

	// Extract global trailing constraints (lines after the last task heading
	// that match known constraint patterns).
	globalConstraints := extractGlobalConstraints(lines, headers[len(headers)-1].lineIndex)

	// Build each task body from its heading to the next heading (or end of
	// global constraints).
	var tasks []explicitTask
	for i, hdr := range headers {
		var endLine int
		if i+1 < len(headers) {
			endLine = headers[i+1].lineIndex
		} else {
			endLine = len(lines)
		}

		// Collect the body lines (heading + following lines until next heading).
		var bodyLines []string
		bodyLines = append(bodyLines, lines[hdr.lineIndex])
		for j := hdr.lineIndex + 1; j < endLine; j++ {
			line := lines[j]
			// Skip lines that are global constraints (they're re-appended below).
			if isGlobalConstraintLine(strings.TrimSpace(strings.ToLower(line))) {
				continue
			}
			bodyLines = append(bodyLines, line)
		}

		body := strings.Join(bodyLines, "\n")
		body = strings.TrimSpace(body)
		if body == "" {
			continue
		}

		// Validate: the body must contain meaningful imperative content.
		// Reject bodies that are just a heading with no instructions.
		if !hasImperativeContent(body) {
			continue
		}

		// Prepend shared context.
		if sharedContext != "" {
			body = sharedContext + "\n\n" + body
		}

		// Append global constraints.
		if globalConstraints != "" {
			body = body + "\n\n" + globalConstraints
		}

		tasks = append(tasks, explicitTask{
			Number:  hdr.number,
			Heading: hdr.heading,
			Body:    body,
		})
	}

	if len(tasks) < 2 {
		return nil
	}

	return tasks
}

// extractGlobalConstraints collects trailing lines after the last task heading
// that match known global constraint patterns. Returns them as a joined string.
func extractGlobalConstraints(lines []string, lastHeaderIndex int) string {
	var constraints []string
	seen := map[string]bool{}
	for i := lastHeaderIndex + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if isGlobalConstraintLine(lower) {
			if !seen[lower] {
				seen[lower] = true
				constraints = append(constraints, line)
			}
		}
	}
	return strings.Join(constraints, "\n")
}

// isGlobalConstraintLine reports whether a line matches a known global
// constraint pattern (case-insensitive, already lowercased).
func isGlobalConstraintLine(lowerLine string) bool {
	for _, pattern := range globalConstraintPatterns {
		if strings.Contains(lowerLine, pattern) {
			return true
		}
	}
	return false
}

// hasImperativeContent reports whether the task body contains meaningful
// imperative instructions beyond just a heading. Rejects bodies that are
// just a number/label with no actionable content.
func hasImperativeContent(body string) bool {
	// Must have more than just the heading line.
	lines := strings.Split(body, "\n")
	if len(lines) < 2 {
		// A single line heading might still have imperative content if the
		// heading itself contains an instruction (e.g. "Task 1: inspect
		// documentation files and summarize the documentation structure").
		// Check that it contains at least one verb-like word.
		return hasVerb(body)
	}
	// Multi-line: at least one non-empty line beyond the heading.
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return true
		}
	}
	return false
}

// hasVerb checks for common imperative verbs in task instructions.
func hasVerb(text string) bool {
	lower := strings.ToLower(text)
	verbs := []string{
		"inspect", "search", "find", "summarize", "analyze", "review",
		"implement", "create", "write", "build", "fix", "update",
		"add", "remove", "check", "test", "run", "explain",
		"describe", "list", "identify", "evaluate", "compare",
	}
	for _, v := range verbs {
		if strings.Contains(lower, v) {
			return true
		}
	}
	return false
}

// classifyExplicitTask classifies an individual explicit task body into a
// planner TaskKind. It uses keyword analysis on the task body itself, not the
// full prompt, so each task gets its own correct kind. Falls back to the
// overall primary kind when the body doesn't clearly indicate a different kind.
func classifyExplicitTask(body string, fallback TaskKind) TaskKind {
	lower := strings.ToLower(body)

	// Documentation task.
	if strings.Contains(lower, "documentation") || strings.Contains(lower, "docs") ||
		strings.Contains(lower, "readme") {
		if strings.Contains(lower, "inspect") || strings.Contains(lower, "summarize") ||
			strings.Contains(lower, "review") || strings.Contains(lower, "search") ||
			strings.Contains(lower, "find") || strings.Contains(lower, "analyze") {
			return KindRepositorySearch
		}
	}

	// Go source / code inspection task.
	if strings.Contains(lower, "go source") || strings.Contains(lower, "source files") ||
		strings.Contains(lower, "source code") || strings.Contains(lower, "code files") ||
		strings.Contains(lower, "module layout") || strings.Contains(lower, "package") {
		if strings.Contains(lower, "inspect") || strings.Contains(lower, "summarize") ||
			strings.Contains(lower, "review") || strings.Contains(lower, "search") ||
			strings.Contains(lower, "find") || strings.Contains(lower, "analyze") {
			return KindRepositorySearch
		}
	}

	// Implementation.
	if strings.Contains(lower, "implement") || strings.Contains(lower, "add ") ||
		strings.Contains(lower, "create ") || strings.Contains(lower, "build ") {
		return KindImplementation
	}

	// Testing.
	if strings.Contains(lower, "write tests") || strings.Contains(lower, "add tests") ||
		strings.Contains(lower, "test suite") {
		return KindTesting
	}

	// Refactoring.
	if strings.Contains(lower, "refactor") {
		return KindRefactoring
	}

	// Debugging.
	if strings.Contains(lower, "debug") || strings.Contains(lower, "fix ") {
		return KindDebugging
	}

	// Code review.
	if strings.Contains(lower, "code review") || strings.Contains(lower, "review this code") {
		return KindCodeReview
	}

	// Security review.
	if strings.Contains(lower, "security") && strings.Contains(lower, "review") {
		return KindSecurityReview
	}

	// Shell operation.
	if strings.Contains(lower, "run ") && (strings.Contains(lower, "shell") ||
		strings.Contains(lower, "bash") || strings.Contains(lower, "command")) {
		return KindShellOperation
	}

	// Fall back to the overall classified kind.
	return fallback
}

// titleFromHeading extracts a concise title from a task heading. If the
// heading is non-empty, it uses the heading text (truncated); otherwise it
// falls back to a generic title.
func titleFromHeading(heading string, fallback string) string {
	h := strings.TrimSpace(heading)
	if h == "" {
		return fallback
	}
	// Truncate long headings for readability.
	if len(h) > 60 {
		// Find a word boundary near the limit.
		cutoff := 60
		for cutoff > 40 && h[cutoff] != ' ' {
			cutoff--
		}
		h = h[:cutoff] + "…"
	}
	return h
}
