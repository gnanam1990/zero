package executor

import (
	"context"
	"errors"
	"strings"
)

var mutatingTools = map[string]bool{
	"write_file":    true,
	"edit_file":     true,
	"create_file":   true,
	"delete_file":   true,
	"rename_file":   true,
	"patch_file":    true,
	"write":         true,
	"edit":          true,
	"create":        true,
	"delete":        true,
	"apply_patch":   true,
	"notebook_edit": true,
}

var commandTools = map[string]bool{
	"shell":    true,
	"bash":     true,
	"sh":       true,
	"exec":     true,
	"command":  true,
	"run":      true,
	"terminal": true,
}

var readTools = map[string]bool{
	"read_file":       true,
	"read":            true,
	"grep":            true,
	"glob":            true,
	"search":          true,
	"repo_explore":    true,
	"code_review":     true,
	"security_review": true,
	"web_fetch":       true,
	"read_url":        true,
	"ls":              true,
	"cat":             true,
}

// toolCategory classifies a tool by name for completion-gate logic.
func toolCategory(name string) ToolCategory {
	n := strings.ToLower(strings.TrimSpace(name))
	if mutatingTools[n] {
		return CategoryMutating
	}
	if commandTools[n] {
		return CategoryCommand
	}
	if readTools[n] {
		return CategoryRead
	}
	return CategoryOther
}

// ClassifyTool exposes the same name-based classification used by the completion
// gate so other packages (e.g. the orchestrated task tool-policy) share one
// source of truth for what a tool can do.
func ClassifyTool(name string) ToolCategory {
	return toolCategory(name)
}

// (ToolCategory).String satisfies fmt.Stringer for render/logging.
func (c ToolCategory) String() string { return string(c) }

// isCancellation reports whether err indicates the run was stopped by signal or
// context cancellation.
func isCancellation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"interrupted", "cancelled", "canceled", "signal", "context canceled", "deadline exceeded"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// isPermissionError reports whether err is a permission/approval denial.
func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"permission", "approval", "denied", "approve", "not allowed"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
