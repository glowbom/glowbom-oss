package main

import (
	"context"
	"errors"
	"strings"
	"sync"
)

type buildPermissionContextKey struct{}

// Approval state belongs to one live build, never its saved agent conversation.
type buildPermissionPolicy struct {
	mu        sync.Mutex
	ctx       context.Context
	project   string
	driver    string
	session   string
	enabled   bool
	closed    bool
	claimed   map[string]bool
	confirmed map[string]bool
	arming    *buildPermissionChange
	changed   chan struct{}
}

type buildPermissionChange struct {
	policy   *buildPermissionPolicy
	previous bool
	done     chan struct{}
}

func validBuildPermissionMode(mode string) bool { return mode == "" || mode == "ask" || mode == "all" }

func normalizedBuildPermissionMode(mode string) string {
	if mode == "all" {
		return "all"
	}
	return "ask"
}

func newBuildPermissionPolicy(ctx context.Context, driver, project, mode string) *buildPermissionPolicy {
	if ctx == nil {
		return nil
	}
	root, err := codexInputProject(project)
	if err != nil || !validBuildPermissionMode(mode) {
		return nil
	}
	policy := &buildPermissionPolicy{ctx: ctx, driver: normalizedBuildDriver(driver), project: root,
		enabled: mode == "all", claimed: map[string]bool{}, confirmed: map[string]bool{}, changed: make(chan struct{})}
	context.AfterFunc(ctx, policy.close)
	return policy
}

func withBuildPermissionPolicy(ctx context.Context, policy *buildPermissionPolicy) context.Context {
	return context.WithValue(ctx, buildPermissionContextKey{}, policy)
}

func buildPermissionPolicyFromContext(ctx context.Context) *buildPermissionPolicy {
	if ctx == nil {
		return nil
	}
	policy, _ := ctx.Value(buildPermissionContextKey{}).(*buildPermissionPolicy)
	return policy
}

func buildPermissionChanged(ctx context.Context) <-chan struct{} {
	p := buildPermissionPolicyFromContext(ctx)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active() {
		return nil
	}
	return p.changed
}

func (p *buildPermissionPolicy) notify() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *buildPermissionPolicy) active() bool { return p != nil && !p.closed && p.ctx.Err() == nil }

func (p *buildPermissionPolicy) matches(driver, project, session string) bool {
	if p == nil || session == "" || len(session) > 512 || strings.ContainsAny(session, "\r\n\x00") {
		return false
	}
	root, err := codexInputProject(project)
	if err != nil || root != p.project || normalizedBuildDriver(driver) != p.driver {
		return false
	}
	switch p.driver {
	case "opencode", "codex", "acp", "claude-code", "cursor":
	default:
		return false
	}
	return true
}

// Caller holds the policy mutex. The first validated permission fixes its session.
func (p *buildPermissionPolicy) bind(session string) bool {
	if !p.active() {
		return false
	}
	if p.session == "" {
		p.session = session
	}
	return p.session == session
}

func buildPermissionAllAvailable(ctx context.Context, driver, project, session string, onceOffered bool) bool {
	p := buildPermissionPolicyFromContext(ctx)
	if !onceOffered || !p.matches(driver, project, session) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return ctx.Err() == nil && p.bind(session)
}

func buildPermissionAllEnabled(ctx context.Context, driver, project, session string) bool {
	p := buildPermissionPolicyFromContext(ctx)
	if !p.matches(driver, project, session) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return ctx.Err() == nil && p.bind(session) && (p.enabled || p.arming != nil)
}

// Reserve before sending. An uncertain provider reply cannot trigger an automatic retry.
// A later permission waits for the explicitly selected current reply to succeed.
func buildPermissionClaim(ctx context.Context, driver, project, session, id string) bool {
	p := buildPermissionPolicyFromContext(ctx)
	if id == "" || len(id) > 160 || strings.ContainsAny(id, "\r\n\x00") || !p.matches(driver, project, session) {
		return false
	}
	for {
		p.mu.Lock()
		if ctx.Err() != nil || !p.bind(session) {
			p.mu.Unlock()
			return false
		}
		if pending := p.arming; pending != nil {
			p.mu.Unlock()
			select {
			case <-pending.done:
			case <-ctx.Done():
				return false
			case <-p.ctx.Done():
				return false
			}
			continue
		}
		if !p.enabled || p.claimed[id] || len(p.claimed) >= 4096 {
			p.mu.Unlock()
			return false
		}
		p.claimed[id] = true
		p.mu.Unlock()
		return true
	}
}

func buildPermissionClaimed(ctx context.Context, driver, project, session, id string) bool {
	p := buildPermissionPolicyFromContext(ctx)
	if !p.matches(driver, project, session) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return ctx.Err() == nil && p.active() && p.session == session && p.claimed[id]
}

func buildPermissionReserve(ctx context.Context, driver, project, session, id string) bool {
	p := buildPermissionPolicyFromContext(ctx)
	if id == "" || len(id) > 160 || strings.ContainsAny(id, "\r\n\x00") || !p.matches(driver, project, session) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil || !p.bind(session) || len(p.claimed) >= 4096 && !p.claimed[id] {
		return false
	}
	p.claimed[id] = true
	return true
}

// Reserved means attempted; only an acknowledged response is settled.
func buildPermissionConfirm(ctx context.Context, driver, project, session, id string) bool {
	p := buildPermissionPolicyFromContext(ctx)
	if !p.matches(driver, project, session) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil || !p.active() || p.session != session || !p.claimed[id] {
		return false
	}
	if !p.confirmed[id] {
		p.confirmed[id] = true
		p.notify()
	}
	return true
}

func buildPermissionConfirmed(ctx context.Context, driver, project, session, id string) bool {
	p := buildPermissionPolicyFromContext(ctx)
	if !p.matches(driver, project, session) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return ctx.Err() == nil && p.active() && p.session == session && p.confirmed[id]
}

func beginBuildPermissionAll(ctx context.Context, driver, project, session, id string) (*buildPermissionChange, error) {
	p := buildPermissionPolicyFromContext(ctx)
	if id == "" || len(id) > 160 || strings.ContainsAny(id, "\r\n\x00") || !p.matches(driver, project, session) {
		return nil, errors.New("This build approval is unavailable.")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil || !p.bind(session) || p.arming != nil || len(p.claimed) >= 4096 && !p.claimed[id] {
		return nil, errors.New("This build approval is unavailable.")
	}
	change := &buildPermissionChange{policy: p, previous: p.enabled, done: make(chan struct{})}
	p.arming = change
	p.claimed[id] = true
	return change, nil
}

func (change *buildPermissionChange) finish(success bool) {
	if change == nil {
		return
	}
	p := change.policy
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.arming != change {
		return
	}
	if p.active() {
		p.enabled = success || change.previous
	}
	p.arming = nil
	close(change.done)
	p.notify()
}

func (p *buildPermissionPolicy) mode() string {
	if p == nil {
		return "ask"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active() && p.enabled {
		return "all"
	}
	return "ask"
}

func (p *buildPermissionPolicy) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed, p.enabled = true, false
	clear(p.claimed)
	clear(p.confirmed)
	if p.arming != nil {
		close(p.arming.done)
		p.arming = nil
	}
	p.notify()
}

func companionPermissionOffered(pending []byte, response string) bool {
	public := companionPublicPending(pending, "permission")
	if public == nil {
		return false
	}
	if choices, ok := public["availableResponses"].([]string); ok {
		for _, choice := range choices {
			if choice == response {
				return true
			}
		}
		return false
	}
	return response == "once" || response == "reject"
}
