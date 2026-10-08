package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func steerTestProject(t *testing.T, messages []chatMessage) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "glowbom.json"), []byte(`{"name":"Steer test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".glowbom")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chat.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func steerTestRequest(project, runID string, index int, role, message string) *http.Request {
	data, _ := json.Marshal(openCodeSteerRequest{ProjectPath: project, RunID: runID, MessageIndex: index, Role: role, Text: message})
	return httptest.NewRequest(http.MethodPost, "/opencode/steer", bytes.NewReader(data))
}

func TestOpenCodeSteerTargetsSavedMessageInExactActiveRun(t *testing.T) {
	project := steerTestProject(t, []chatMessage{{Role: "user", Text: "Make the welcome warmer"}, {Role: "assistant", Text: "A soft welcome would help"}})
	canonicalProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	posted := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("directory") != canonicalProject {
			t.Errorf("OpenCode directory = %q, want %q", r.URL.Query().Get("directory"), canonicalProject)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_build/prompt_async":
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
				Model map[string]string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode prompt: %v", err)
			}
			if payload.Model["providerID"] != "xai" || payload.Model["modelID"] != "grok-4.7" {
				t.Errorf("steer changed model: %#v", payload.Model)
			}
			mu.Lock()
			posted = append(posted, payload.Parts[0].Text)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_build/message":
			mu.Lock()
			current := append([]string(nil), posted...)
			mu.Unlock()
			messages := []map[string]any{}
			for i, prompt := range current {
				messages = append(messages, map[string]any{"info": map[string]any{"id": "msg_" + string(rune('a'+i)), "role": "user", "time": map[string]any{"created": 1000 + i}}, "parts": []map[string]any{{"type": "text", "text": prompt}}})
			}
			_ = json.NewEncoder(w).Encode(messages)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	run, err := registerOpenCodeSteerRun(project, "run-current", "ses_build", "xai", "grok-4.7", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.close)

	for _, req := range []openCodeSteerRequest{
		{ProjectPath: project, RunID: "run-old", MessageIndex: 1, Role: "assistant", Text: "A soft welcome would help"},
		{ProjectPath: project, RunID: "run-current", MessageIndex: 1, Role: "assistant", Text: "Changed text"},
		{ProjectPath: project, RunID: "run-current", MessageIndex: 0, Role: "assistant", Text: "Make the welcome warmer"},
	} {
		body, _ := json.Marshal(req)
		result := httptest.NewRecorder()
		openCodeSteerHandler(result, httptest.NewRequest(http.MethodPost, "/opencode/steer", bytes.NewReader(body)))
		if result.Code != http.StatusConflict {
			t.Errorf("stale or mismatched message status = %d, want 409", result.Code)
		}
	}

	result := httptest.NewRecorder()
	openCodeSteerHandler(result, steerTestRequest(project, "run-current", 1, "assistant", "A soft welcome would help"))
	if result.Code != http.StatusAccepted || !strings.Contains(result.Body.String(), `"status":"sent"`) {
		t.Fatalf("valid steer = %d %s", result.Code, result.Body.String())
	}
	mu.Lock()
	if len(posted) != 1 || !strings.Contains(posted[0], "earlier assistant reply") || !strings.Contains(posted[0], "not a new user instruction") {
		t.Errorf("assistant attribution missing: %#v", posted)
	}
	mu.Unlock()

	result = httptest.NewRecorder()
	openCodeSteerHandler(result, steerTestRequest(project, "run-current", 1, "assistant", "A soft welcome would help"))
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"status":"sent"`) {
		t.Fatalf("duplicate steer = %d %s", result.Code, result.Body.String())
	}
	mu.Lock()
	if len(posted) != 1 {
		t.Errorf("duplicate sent %d prompts, want one", len(posted))
	}
	mu.Unlock()

	run.close()
	result = httptest.NewRecorder()
	openCodeSteerHandler(result, steerTestRequest(project, "run-current", 0, "user", "Make the welcome warmer"))
	if result.Code != http.StatusConflict {
		t.Errorf("stopped run status = %d, want 409", result.Code)
	}

	newRun, err := registerOpenCodeSteerRun(project, "run-next", "ses_build", "xai", "grok-4.7", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(newRun.close)
	result = httptest.NewRecorder()
	openCodeSteerHandler(result, steerTestRequest(project, "run-current", 0, "user", "Make the welcome warmer"))
	if result.Code != http.StatusConflict {
		t.Errorf("old run after session reuse status = %d, want 409", result.Code)
	}
}

func TestOpenCodeSteerAsyncAcknowledgmentWaitsForPersistence(t *testing.T) {
	project := steerTestProject(t, []chatMessage{{Role: "user", Text: "Use blue"}})
	var mu sync.Mutex
	prompt := ""
	persisted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt_async"):
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			prompt = payload.Parts[0].Text
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/message"):
			mu.Lock()
			text, ready := prompt, persisted
			mu.Unlock()
			if !ready {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"info": map[string]any{"id": "msg_generated_by_opencode", "role": "user", "time": map[string]any{"created": 1000}}, "parts": []map[string]string{{"type": "text", "text": text}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	run, err := registerOpenCodeSteerRun(project, "run-delay", "ses_build", "", "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.close)
	go func() {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		persisted = true
		mu.Unlock()
	}()
	result := httptest.NewRecorder()
	openCodeSteerHandler(result, steerTestRequest(project, "run-delay", 0, "user", "Use blue"))
	if result.Code != http.StatusAccepted || !strings.Contains(result.Body.String(), `"status":"sent"`) {
		t.Fatalf("delayed persistence response = %d %s", result.Code, result.Body.String())
	}
	mu.Lock()
	if !strings.HasPrefix(prompt, "[Glowbom steer ") {
		t.Errorf("missing reconciliation marker: %q", prompt)
	}
	mu.Unlock()
}

func TestOpenCodeSteerIdleBarrierNudgesAndCloses(t *testing.T) {
	project := steerTestProject(t, []chatMessage{{Role: "user", Text: "Fix the color"}})
	var mu sync.Mutex
	steerText := ""
	nudgeText := ""
	finished := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt_async"):
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			if strings.Contains(payload.Parts[0].Text, "Before finishing this build") {
				nudgeText = payload.Parts[0].Text
			} else {
				steerText = payload.Parts[0].Text
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/message"):
			mu.Lock()
			steer, nudge, done := steerText, nudgeText, finished
			mu.Unlock()
			messages := []map[string]any{{"info": map[string]any{"id": "msg_initial", "role": "user", "time": map[string]any{"created": 100}}, "parts": []map[string]string{{"type": "text", "text": "Initial request"}}}}
			if steer != "" {
				messages = append(messages, map[string]any{"info": map[string]any{"id": "msg_steer", "role": "user", "time": map[string]any{"created": 200}}, "parts": []map[string]string{{"type": "text", "text": steer}}})
			}
			if nudge != "" {
				messages = append(messages, map[string]any{"info": map[string]any{"id": "msg_nudge", "role": "user", "time": map[string]any{"created": 300}}, "parts": []map[string]string{{"type": "text", "text": nudge}}})
			}
			if done {
				messages = append(messages, map[string]any{"info": map[string]any{"id": "msg_answer", "role": "assistant", "parentID": "msg_nudge", "finish": "stop", "time": map[string]any{"created": 400}}})
			}
			_ = json.NewEncoder(w).Encode(messages)
		case r.Method == http.MethodGet && r.URL.Path == "/session/status":
			mu.Lock()
			busy := nudgeText != "" && !finished
			mu.Unlock()
			if busy {
				_, _ = w.Write([]byte(`{"ses_build":{"type":"busy"}}`))
			} else {
				_, _ = w.Write([]byte(`{}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	run, err := registerOpenCodeSteerRun(project, "run-idle", "ses_build", "", "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.close)
	result := httptest.NewRecorder()
	openCodeSteerHandler(result, steerTestRequest(project, "run-idle", 0, "user", "Fix the color"))
	if result.Code != http.StatusAccepted {
		t.Fatalf("steer response = %d %s", result.Code, result.Body.String())
	}
	continueWork, err := run.continueAtIdle(context.Background())
	if err != nil || !continueWork {
		t.Fatalf("first idle = continue %t, err %v", continueWork, err)
	}
	mu.Lock()
	if nudgeText == "" {
		t.Error("idle barrier did not nudge the session")
	}
	mu.Unlock()
	// A stale idle event from the first runner must not end the build while
	// the follow-up runner is busy.
	continueWork, err = run.continueAtIdle(context.Background())
	if err != nil || !continueWork {
		t.Fatalf("stale idle during nudge = continue %t, err %v", continueWork, err)
	}
	mu.Lock()
	finished = true
	mu.Unlock()
	continueWork, err = run.continueAtIdle(context.Background())
	if err != nil || continueWork {
		t.Fatalf("second idle = continue %t, err %v", continueWork, err)
	}
	result = httptest.NewRecorder()
	openCodeSteerHandler(result, steerTestRequest(project, "run-idle", 0, "user", "Fix the color"))
	if result.Code != http.StatusConflict {
		t.Errorf("steer accepted after final idle: %d", result.Code)
	}
}

func TestOpenCodeSteerIdleBarrierReportsUnstoredAsyncMessage(t *testing.T) {
	project := steerTestProject(t, []chatMessage{{Role: "user", Text: "Use blue"}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/message") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	run, err := registerOpenCodeSteerRun(project, "run-missing", "ses_build", "", "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.close)
	run.sent["selected"] = &openCodeSteerDelivery{reference: "missing"}
	continueWork, err := run.continueAtIdle(context.Background())
	if continueWork || err == nil || !strings.Contains(err.Error(), "did not store") {
		t.Fatalf("unstored steer = continue %t, err %v", continueWork, err)
	}
	if run.accepting {
		t.Fatal("run still accepts steers after unconfirmed final idle")
	}
}

func TestOpenCodeSteerConsumptionUsesOpenCodeOrderAtSameMillisecond(t *testing.T) {
	run := &openCodeSteerRun{sent: map[string]*openCodeSteerDelivery{"selected": {reference: "ref", messageID: "msg_b"}}}
	var messages []openCodeSteerSessionMessage
	data := `[
{"info":{"id":"msg_a","role":"user","time":{"created":1000}}},
{"info":{"id":"msg_b","role":"user","time":{"created":1000}}},
{"info":{"id":"msg_answer","role":"assistant","parentID":"msg_a","finish":"stop","time":{"created":1000}}}
]`
	if err := json.Unmarshal([]byte(data), &messages); err != nil {
		t.Fatal(err)
	}
	if done, err := run.consumed(messages); done || err != nil {
		t.Fatalf("older same-millisecond answer should not consume steer: done=%t err=%v", done, err)
	}
}
