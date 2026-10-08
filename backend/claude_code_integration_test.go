package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeCodeCatalogWithoutOtherAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '{\"loggedIn\":true}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", path)
	t.Setenv("GLOWBOM_CURSOR_BIN", filepath.Join(t.TempDir(), "missing"))
	rememberCursorModels(nil)
	t.Cleanup(func() { rememberCursorModels(nil) })
	withCodexModels(t, nil)
	service := &chatService{prepare: func() error { return errors.New("OpenCode missing") }}
	w := httptest.NewRecorder()
	service.modelsHandler(w, httptest.NewRequest(http.MethodGet, "/chat/models", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"claude-code/default"`) || !strings.Contains(w.Body.String(), `"build":true`) {
		t.Fatal("Claude Code depends on another agent", w.Code, w.Body.String())
	}
}

func TestBuildOnlyCLIModelsCannotEnterChat(t *testing.T) {
	for _, mode := range []string{"chat", "prototype", "translation"} {
		for _, model := range []string{"claude-code/sonnet", "cursor/auto"} {
			req := chatRequest{Mode: mode, Model: model, Messages: []chatMessage{{Role: "user", Text: "Build this"}}}
			if err := validateChatRequest(req); err == nil || !strings.Contains(err.Error(), "only available in Build") {
				t.Fatal("build-only model accepted", mode, model, err)
			}
		}
	}
	req := chatRequest{Mode: "chat", Model: "provider/model", Messages: []chatMessage{{Role: "user", Text: "How is it going?"}}, AgentState: &chatAgentState{Status: "running", Driver: "claude-code"}}
	if err := validateChatRequest(req); err != nil {
		t.Fatal("chat cannot describe the active Claude worker", err)
	}
}

func TestClaudeCodeBookGenerationIsDisabled(t *testing.T) {
	service := &chatService{prepare: func() error { t.Fatal("started a different agent"); return nil }}
	if _, err := bookVisualModel(context.Background(), service, "claude-code/sonnet"); err == nil {
		t.Fatal("Claude Code model allowed for sketch generation")
	}
}

func TestClaudeCodeSessionRestoresOnlyLocally(t *testing.T) {
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	project := t.TempDir()
	p := companionProject{ID: companionProjectID(project), path: project}
	job := m.newRun(p, "desktop", OpenCodeAgentRequest{AgentDriver: "claude-code", Model: "sonnet"})
	session := claudeCodeSessionPrefix + "12345678-1234-1234-1234-123456789abc"
	job.mu.Lock()
	job.agentSessionID, job.status, job.finishedAt = session, "completed", time.Now().UTC().Format(time.RFC3339Nano)
	job.mu.Unlock()
	m.saveRun(job)
	restored := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(restored.Shutdown)
	restored.loadProjectRuns(project)
	run := restored.run(job.id)
	if run == nil || run.snapshot(true)["sessionID"] != session || run.snapshot(true)["model"] != "claude-code/sonnet" {
		t.Fatal("Claude Code session did not restore")
	}
	if _, exists := run.snapshot(false)["sessionID"]; exists {
		t.Fatal("private CLI session leaked to the companion")
	}
}
