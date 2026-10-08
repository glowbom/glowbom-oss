package main

import (
	"context"
	"regexp"
	"strings"
	"sync"
)

var acpToolIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,79}$`)

// ACP names identify tools. Some agents, including Cline, instead put the
// identifier before ": " in the title. Human prose is not a tool identifier.
func acpBuildPermissionTool(name *string, title string) string {
	tool := ""
	if name != nil {
		tool = *name
	} else {
		tool, _, _ = strings.Cut(title, ": ")
	}
	if !acpToolIdentifier.MatchString(tool) {
		return ""
	}
	return tool
}

type acpBuildPermissionScope struct {
	session string
	kind    string
	tool    string
}

// This state belongs to one build, even when an agent conversation is resumed.
// Grants never reach agent configuration or the saved run history.
type acpBuildApprovals struct {
	mu      sync.Mutex
	ctx     context.Context
	closed  bool
	allowed map[acpBuildPermissionScope]bool
}

func newACPBuildApprovals(ctx context.Context) *acpBuildApprovals {
	return &acpBuildApprovals{ctx: ctx, allowed: make(map[acpBuildPermissionScope]bool)}
}

func (a *acpBuildApprovals) active() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.closed && a.ctx.Err() == nil
}

func (a *acpBuildApprovals) permits(scope acpBuildPermissionScope) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.closed && a.ctx.Err() == nil && a.allowed[scope]
}

func (a *acpBuildApprovals) remember(scope acpBuildPermissionScope) bool {
	if a == nil || scope.tool == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.ctx.Err() != nil {
		return false
	}
	a.allowed[scope] = true
	return true
}

func (a *acpBuildApprovals) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	clear(a.allowed)
}
