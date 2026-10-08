package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testClaudeCodeSession = "5eb64b3c-c0fc-4f20-a38d-3bbfdc106a28"

func fakeClaudeCode(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeCodeArgumentsIsolateSessionsAndKeepPermissions(t *testing.T) {
	base := []string{"--print", "--verbose", "--output-format", "stream-json", "--permission-mode", "auto", "--permission-prompts", "none", "--model", "default"}
	for _, session := range []string{"", "ses_opencode", "cursor-" + testClaudeCodeSession, "codex:" + testClaudeCodeSession, testClaudeCodeSession, "claude-code:", "claude-code:../../bad"} {
		if got := claudeCodeArguments("", session); !reflect.DeepEqual(got, base) {
			t.Fatalf("session %q: %v", session, got)
		}
	}
	want := append(append([]string(nil), base...), "--resume", testClaudeCodeSession)
	want[len(base)-1] = "sonnet"
	if got := claudeCodeArguments(" claude-code/sonnet ", claudeCodeSessionPrefix+testClaudeCodeSession); !reflect.DeepEqual(got, want) {
		t.Fatalf("resume: %v", got)
	}
	for _, model := range []string{"haiku", "claude-code/haiku", "claude-haiku-4-5"} {
		got := claudeCodeArguments(model, "")
		if got[5] != "acceptEdits" || strings.Contains(strings.Join(got, " "), "skip-permissions") || strings.Contains(strings.Join(got, " "), "allowedTools") {
			t.Fatalf("permissions: %v", got)
		}
	}
	for _, model := range []string{"../escape", "cursor/sonnet", "--dangerously-skip-permissions", "claude-code/sonnet\n--continue"} {
		if got := claudeCodeCLIModel(model); got != "" {
			t.Fatalf("accepted invalid model %q", got)
		}
	}
}

func TestClaudeCodeExecutableHonorsExplicitOverride(t *testing.T) {
	binary := fakeClaudeCode(t, "exit 0\n")
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", binary)
	if got, err := claudeCodeExecutable(); err != nil || got != binary {
		t.Fatalf("override: %q %v", got, err)
	}
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", filepath.Join(t.TempDir(), "missing"))
	if _, err := claudeCodeExecutable(); err == nil {
		t.Fatal("invalid explicit override fell back to another executable")
	}
}

func TestClaudeCodeHealthAndModelsRequireAuthenticatedJSON(t *testing.T) {
	for _, tc := range []struct {
		name, output, exit string
		healthy            bool
	}{
		{"authenticated", `{"loggedIn":true,"email":"private-account-detail"}`, "0", true},
		{"logged out with zero exit", `{"loggedIn":false}`, "0", false},
		{"missing auth field", `{}`, "0", false},
		{"invalid output", `private-account-detail`, "0", false},
		{"failed process", `{"loggedIn":true}`, "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", fakeClaudeCode(t, "[ \"$1\" = auth ] && [ \"$2\" = status ] && [ \"$3\" = --json ] || exit 3\nprintf '%s\\n' '"+tc.output+"'\nprintf '%s\\n' private-account-detail >&2\nexit "+tc.exit+"\n"))
			w := httptest.NewRecorder()
			claudeCodeHealthHandler(w, httptest.NewRequest("GET", "/opencode/health?agentDriver=claude-code", nil))
			var result struct {
				Healthy bool `json:"healthy"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Healthy != tc.healthy || strings.Contains(w.Body.String(), "private-account-detail") {
				t.Fatal(w.Body.String())
			}
			models := listClaudeCodeModels(context.Background())
			if !tc.healthy {
				if len(models) != 0 {
					t.Fatalf("unauthenticated models: %+v", models)
				}
				return
			}
			if len(models) != 4 {
				t.Fatalf("models: %+v", models)
			}
			for i, alias := range []string{"default", "sonnet", "opus", "haiku"} {
				if models[i].ID != "claude-code/"+alias || models[i].Provider != "Claude Code" || !models[i].Build || models[i].Images {
					t.Fatalf("model: %+v", models[i])
				}
			}
		})
	}
}

func TestStreamClaudeCode(t *testing.T) {
	for _, tc := range []struct {
		name, output, exit string
		success            bool
	}{
		{"success", `{"type":"system","subtype":"init","session_id":"` + testClaudeCodeSession + `","model":"claude-sonnet-test","apiKeySource":"private-account-detail"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working"},{"type":"tool_use","input":{"secret":"private-account-detail"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"private-account-detail"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Done"}`, "0", true},
		{"result only", `{"type":"result","subtype":"success","result":"Done"}`, "0", true},
		{"missing result", `{"type":"assistant"}`, "0", false},
		{"error result", `{"type":"result","subtype":"success","is_error":true}`, "0", false},
		{"bad json", `private-account-detail`, "0", false},
		{"nonzero after success", `{"type":"result","subtype":"success"}`, "1", false},
		{"error event", `{"type":"error","error":"private-account-detail"}`, "0", false},
		{"startup notices", `{"type":"system","subtype":"hook_progress","message":"private-account-detail"}
{"type":"result","subtype":"success","result":"Done"}`, "0", true},
		{"malformed assistant", `{"type":"assistant","message":"private-account-detail"}`, "0", false},
		{"permission denied in result", `{"type":"result","subtype":"success","result":"Done","permission_denials":[{"tool_name":"Bash","tool_input":{"command":"private-account-detail"}}]}`, "0", false},
		{"permission denied in stream", `{"type":"system","subtype":"permission_denied","message":"private-account-detail"}
{"type":"result","subtype":"success","result":"Done"}`, "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := fakeClaudeCode(t, "cat > prompt.txt\nprintf '%s\\n' private-account-detail >&2\nprintf '%s\\n' '"+tc.output+"'\nexit "+tc.exit+"\n")
			var events []map[string]interface{}
			result, err := streamClaudeCode(context.Background(), bin, dir, claudeCodeArguments("", ""), "literal $(touch wrong) prompt", func(e map[string]interface{}) { events = append(events, e) })
			if (err == nil) != tc.success {
				t.Fatalf("result=%q err=%v", result, err)
			}
			if strings.HasPrefix(tc.name, "permission denied") && (err == nil || !strings.Contains(err.Error(), "permission was denied")) {
				t.Fatalf("permission denial was not explained: %v", err)
			}
			prompt, readErr := os.ReadFile(filepath.Join(dir, "prompt.txt"))
			if readErr != nil || string(prompt) != "literal $(touch wrong) prompt" {
				t.Fatalf("stdin: %q %v", prompt, readErr)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "wrong")); !os.IsNotExist(statErr) {
				t.Fatal("prompt was interpreted by a shell")
			}
			encoded, _ := json.Marshal(events)
			if strings.Contains(string(encoded), "private-account-detail") || (err != nil && strings.Contains(err.Error(), "private-account-detail")) {
				t.Fatal("raw diagnostic or tool input leaked")
			}
			if tc.name == "success" && (result != "Done" || len(events) != 4 || events[0]["output"] != "Session created: "+claudeCodeSessionPrefix+testClaudeCodeSession || events[1]["output"] != "Claude Code model: claude-sonnet-test") {
				t.Fatalf("%q %+v", result, events)
			}
			if tc.name == "result only" && (len(events) != 1 || events[0]["output"] != "Done") {
				t.Fatalf("result-only output: %+v", events)
			}
		})
	}
}

func TestClaudeCodeCancellation(t *testing.T) {
	bin := fakeClaudeCode(t, "sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := streamClaudeCode(ctx, bin, t.TempDir(), nil, "", func(map[string]interface{}) {})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation: %v after %s", err, time.Since(start))
	}
}

func TestClaudeCodeRefineUsesSharedHistoryAndReportsChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "glowbom.json"), []byte(`{"name":"Test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := fakeClaudeCode(t, `cat > received-prompt.txt
printf 'new file' > changed.txt
printf '%s\n' '{"type":"system","subtype":"init","session_id":"`+testClaudeCodeSession+`"}'
printf '%s\n' '{"type":"result","subtype":"success","result":"Created file"}'
`)
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", bin)
	payload, _ := json.Marshal(OpenCodeAgentRequest{AgentDriver: "claude-code", Model: "claude-code/sonnet", ProjectPath: dir, Instructions: "Update the page", PersistCurrentInstructionsToHistory: true})
	req := httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()
	openCodeRefineHandler(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) || !strings.Contains(w.Body.String(), "changed.txt") || !strings.Contains(w.Body.String(), "Session created: "+claudeCodeSessionPrefix+testClaudeCodeSession) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "received-prompt.txt"))
	if !strings.Contains(string(prompt), "Update the page") || !strings.Contains(string(prompt), "Preserve unrelated changes") {
		t.Fatal("missing instructions")
	}
	entries, err := loadProjectHistoryEntries(dir)
	if err != nil || len(entries) != 1 || entries[0].Contributor != "Claude Code" || entries[0].Provider != "claude-code" || entries[0].Model != "claude-code/sonnet" || entries[0].Status != "completed" || entries[0].OutputSummary != "Created file" || entries[0].RunID == "" {
		t.Fatalf("history: %+v %v", entries, err)
	}
	if !strings.Contains(strings.Join(entries[0].ChangedFiles, "\n"), "changed.txt") {
		t.Fatalf("missing changed files in history: %+v", entries[0])
	}
}

func TestClaudeCodeRefineReportsPartialChangesOnPermissionDenial(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", fakeClaudeCode(t, `cat > /dev/null
printf 'partial changes' > partial.txt
printf '%s\n' '{"type":"result","subtype":"success","result":"Done","permission_denials":[{"tool_name":"Bash"}]}'
`))
	w := httptest.NewRecorder()
	w.Header().Set("X-Glowbom-Run-ID", "test-run")
	called := false
	status, _ := runClaudeCodeRefine(w, httptest.NewRequest("POST", "/opencode/refine", nil), OpenCodeAgentRequest{ProjectPath: dir, Model: "haiku"}, "Update the page", func(status, summary string, changed []string) {
		called = true
		if status != "failed" || !strings.Contains(summary, "permission was denied") || !reflect.DeepEqual(changed, []string{"partial.txt"}) {
			t.Fatalf("completion: %q %q %v", status, summary, changed)
		}
	})
	if status != "failed" || !called || !strings.Contains(w.Body.String(), `"success":false`) || !strings.Contains(w.Body.String(), `"runId":"test-run"`) {
		t.Fatalf("%q %s", status, w.Body.String())
	}
}

func TestClaudeCodeRefineRejectsInvalidModel(t *testing.T) {
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", fakeClaudeCode(t, "exit 0\n"))
	w := httptest.NewRecorder()
	status, _ := runClaudeCodeRefine(w, httptest.NewRequest("POST", "/opencode/refine", nil), OpenCodeAgentRequest{ProjectPath: t.TempDir(), Model: "cursor/sonnet"}, "Update", nil)
	if status != "failed" || w.Code != http.StatusBadRequest {
		t.Fatalf("%q %d", status, w.Code)
	}
}
