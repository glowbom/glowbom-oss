package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCodingAgentExecutableFindsInstallerLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("OPENCODE_URL", "")
	agent, _ := codingAgentByID("opencode")
	// The resolver also searches system directories outside PATH. Give this
	// fixture its own binary name so a real installation cannot satisfy it.
	agent.binary = "glowbom-fixture-" + filepath.Base(filepath.Dir(home))
	name := agent.binary
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	found, err := codingAgentExecutable(agent)
	if err != nil || found != bin {
		t.Fatalf("found %q err %v", found, err)
	}
}

func TestOpenCodeURLCountsAsInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("OPENCODE_URL", "")
	agent, _ := codingAgentByID("opencode")
	agent.binary = "glowbom-fixture-" + filepath.Base(filepath.Dir(t.TempDir()))
	if codingAgentInstalled(agent) {
		t.Fatal("missing OpenCode was reported as installed")
	}
	t.Setenv("OPENCODE_URL", "http://127.0.0.1:4096")
	if !codingAgentInstalled(agent) {
		t.Fatal("configured OpenCode server was reported missing")
	}
}

func TestClaudeCodeSetupUsesConfiguredExecutable(t *testing.T) {
	agent, ok := codingAgentByID("claude-code")
	if !ok || agent.name != "Claude Code" {
		t.Fatalf("Claude Code setup entry missing: %+v", agent)
	}
	path := filepath.Join(t.TempDir(), "claude")
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", path)
	if codingAgentInstalled(agent) {
		t.Fatal("missing configured Claude Code executable accepted")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if !codingAgentInstalled(agent) {
		t.Fatal("configured Claude Code executable was not found")
	}
	if err := installCodingAgent(t.Context(), agent.id); err != nil {
		t.Fatalf("installed Claude Code should not require installation: %v", err)
	}
}

func TestClaudeCodeSetupProvidesManualInstallation(t *testing.T) {
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", filepath.Join(t.TempDir(), "missing-claude"))
	err := installCodingAgent(t.Context(), "claude-code")
	problem, ok := err.(agentSetupProblem)
	if !ok || !strings.Contains(problem.message, "code.claude.com/docs/en/setup") {
		t.Fatalf("missing Claude Code should provide manual setup instructions: %v", err)
	}
}

func TestAgentSetupStatusRequiresAuth(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	recorder := httptest.NewRecorder()
	agentSetupHandler()(recorder, httptest.NewRequest(http.MethodGet, "/settings/agents", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status returned %d", recorder.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/settings/agents", nil)
	request.Header.Set("Authorization", "Bearer token")
	recorder = httptest.NewRecorder()
	agentSetupHandler()(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status was not returned: %d %s", recorder.Code, recorder.Body.String())
	}
	var status agentSetupStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	wanted := []string{"opencode", "cursor", "claude-code", "codex"}
	if len(status.Agents) != len(wanted) {
		t.Fatalf("expected four coding agents: %+v", status.Agents)
	}
	for index, id := range wanted {
		if status.Agents[index].ID != id {
			t.Fatalf("agent %d = %q, want %q", index, status.Agents[index].ID, id)
		}
	}
}

func TestAgentSetupInstallRejectsUnknownAgent(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	request := httptest.NewRequest(http.MethodPost, "/settings/agents/install", strings.NewReader(`{"id":"goose"}`))
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	agentSetupInstallHandler()(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "goose") {
		t.Fatalf("unknown agent was not refused safely: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDownloadInstallScriptRejectsOtherHosts(t *testing.T) {
	if _, err := downloadInstallScript(t.Context(), "https://example.com/install"); err == nil {
		t.Fatal("installer host was not restricted")
	}
}
