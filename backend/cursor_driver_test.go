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

func fakeCursor(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cursor-agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCursorArgumentsIsolateSessions(t *testing.T) {
	base := []string{"--print", "--force", "--output-format", "stream-json"}
	for _, session := range []string{"", "ses_opencode", "cursor-", "cursor-../../bad"} {
		if got := cursorArguments("", session); !reflect.DeepEqual(got, base) {
			t.Fatalf("session %q: %v", session, got)
		}
	}
	want := append(base, "--model", "model-name", "--resume", "abc-123")
	if got := cursorArguments(" model-name ", "cursor-abc-123"); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestStreamCursor(t *testing.T) {
	for _, tc := range []struct {
		name, output, exit string
		success            bool
	}{
		{"success", `{"type":"system","subtype":"init","session_id":"abc-123"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Done"}`, "0", true},
		{"missing result", `{"type":"assistant"}`, "0", false},
		{"error result", `{"type":"result","subtype":"success","is_error":true}`, "0", false},
		{"bad json", `not json`, "0", false},
		{"nonzero after success", `{"type":"result","subtype":"success"}`, "1", false},
		{"error event", `{"type":"error"}`, "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := fakeCursor(t, "cat > prompt.txt\nprintf '%s\\n' '"+tc.output+"'\nexit "+tc.exit+"\n")
			var events []map[string]interface{}
			result, err := streamCursor(context.Background(), bin, dir, cursorArguments("", ""), "literal $(touch wrong) prompt", func(e map[string]interface{}) { events = append(events, e) })
			if (err == nil) != tc.success {
				t.Fatalf("result=%q err=%v", result, err)
			}
			prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
			if err != nil || string(prompt) != "literal $(touch wrong) prompt" {
				t.Fatalf("stdin: %q %v", prompt, err)
			}
			if tc.success && (result != "Done" || len(events) != 2 || events[0]["output"] != "Session created: cursor-abc-123") {
				t.Fatalf("%q %+v", result, events)
			}
		})
	}
}

func TestCursorCancellation(t *testing.T) {
	bin := fakeCursor(t, "sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := streamCursor(ctx, bin, t.TempDir(), nil, "", func(map[string]interface{}) {})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation: %v after %s", err, time.Since(start))
	}
}

func TestCursorRefineUsesSharedHistoryAndReportsChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "glowbom.json"), []byte(`{"name":"Test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := fakeCursor(t, `cat > received-prompt.txt
printf 'new file' > changed.txt
printf '%s\n' '{"type":"result","subtype":"success","result":"Created file"}'
`)
	t.Setenv("GLOWBOM_CURSOR_BIN", bin)
	payload, _ := json.Marshal(OpenCodeAgentRequest{AgentDriver: "cursor", ProjectPath: dir, Instructions: "Update the page", PersistCurrentInstructionsToHistory: true})
	req := httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()
	openCodeRefineHandler(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) || !strings.Contains(w.Body.String(), "changed.txt") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "received-prompt.txt"))
	if !strings.Contains(string(prompt), "Update the page") {
		t.Fatal("missing instructions")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "history"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("history: %v %v", entries, err)
	}
}

func TestCursorPreflightAndHealth(t *testing.T) {
	t.Setenv("GLOWBOM_CURSOR_BIN", filepath.Join(t.TempDir(), "missing"))
	w := httptest.NewRecorder()
	cursorHealthHandler(w, httptest.NewRequest("GET", "/opencode/health?agentDriver=cursor", nil))
	if !strings.Contains(w.Body.String(), `"healthy":false`) {
		t.Fatal(w.Body.String())
	}
	for _, driver := range []string{"cursor", "unknown"} {
		body, _ := json.Marshal(OpenCodeAgentRequest{AgentDriver: driver, ProjectPath: t.TempDir()})
		w = httptest.NewRecorder()
		openCodeRefineHandler(w, httptest.NewRequest("POST", "/opencode/refine", strings.NewReader(string(body))))
		if w.Code != http.StatusServiceUnavailable && w.Code != http.StatusBadRequest {
			t.Fatal(w.Code)
		}
	}
}

func TestCursorHealthRequiresAuthenticatedJSON(t *testing.T) {
	for _, tc := range []struct {
		name, output, exit string
		healthy            bool
	}{
		{"authenticated", `{"isAuthenticated":true,"message":"private account detail"}`, "0", true},
		{"logged out with zero exit", `{"isAuthenticated":false,"status":"unauthenticated"}`, "0", false},
		{"missing auth field", `{}`, "0", false},
		{"invalid output", `private account detail`, "0", false},
		{"failed process", `{"isAuthenticated":true}`, "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GLOWBOM_CURSOR_BIN", fakeCursor(t, "printf '%s\\n' '"+tc.output+"'\nexit "+tc.exit+"\n"))
			w := httptest.NewRecorder()
			openCodeHealthHandler(w, httptest.NewRequest("GET", "/opencode/health?agentDriver=cursor", nil))
			var result struct {
				Healthy bool `json:"healthy"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Healthy != tc.healthy || strings.Contains(w.Body.String(), "private account detail") {
				t.Fatal(w.Body.String())
			}
		})
	}
}
