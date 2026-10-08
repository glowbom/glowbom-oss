package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestCompanionModelCatalogMatchesConfiguredDesktopConnections(t *testing.T) {
	acpSettingsFixture(t)
	t.Setenv("GLOWBOM_CURSOR_BIN", filepath.Join(t.TempDir(), "missing-cursor"))
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", filepath.Join(t.TempDir(), "missing-claude"))
	cursorModelCache.mu.Lock()
	previousModels, previousAt := cursorModelCache.models, cursorModelCache.at
	cursorModelCache.models = []chatModel{{ID: "cursor/auto", Name: "Auto", Provider: "Cursor"}}
	cursorModelCache.at = time.Now()
	cursorModelCache.mu.Unlock()
	t.Cleanup(func() {
		cursorModelCache.mu.Lock()
		cursorModelCache.models, cursorModelCache.at = previousModels, previousAt
		cursorModelCache.mu.Unlock()
	})
	withCodexModels(t, []chatModel{
		{ID: "codex/future-primary", Name: "Future primary", Provider: "Codex", Build: true, Images: true,
			ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high"},
		{ID: "codex/future-secondary", Name: "Future secondary", Provider: "Codex", Build: true},
	})
	marker := filepath.Join(t.TempDir(), "agent-started")
	executable := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	profiles := []acpProfile{}
	for index := 1; index <= 3; index++ {
		profiles = append(profiles, acpProfile{ID: fmt.Sprintf("acp-%d", index), Name: fmt.Sprintf("Configured agent %d", index),
			Command: executable, Args: []string{"private-launch-argument"}, Model: "private-model-choice"})
	}
	if err := saveACPProfiles(profiles); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider" {
			t.Errorf("unexpected catalog request %s", r.URL.Path)
		}
		io.WriteString(w, `{"connected":["provider","opencode"],"all":[{"id":"provider","name":"Connected provider","models":{"code":{"tool_call":true},"chat-only":{"tool_call":false}}},{"id":"opencode","name":"OpenCode Zen","models":{"build-only":{"tool_call":true}}},{"id":"disconnected","models":{"hidden":{"tool_call":true}}}]}`)
	}))
	defer provider.Close()
	service := &chatService{directory: t.TempDir(), serverURL: provider.URL, client: provider.Client(), prepare: func() error { return nil }}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/models", service.modelsHandler)
	s := testCompanion(t, mux)
	for _, test := range []struct {
		path string
		want []string
	}{
		{"/models", []string{"acp/acp-1", "acp/acp-2", "acp/acp-3", "codex/future-primary", "codex/future-secondary", "cursor/auto", "opencode/build-only", "provider/code"}},
		{"/chat/models", []string{"codex/future-primary", "codex/future-secondary", "provider/chat-only", "provider/code"}},
	} {
		t.Run(test.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, companionRequest(s, http.MethodGet, test.path, ""))
			var response struct {
				Models []chatModel `json:"models"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			ids := []string{}
			for _, model := range response.Models {
				ids = append(ids, model.ID)
				if model.ID == "codex/future-primary" && (!model.Images || !reflect.DeepEqual(model.ReasoningEfforts, []string{"low", "high"}) || model.DefaultReasoningEffort != "high") {
					t.Fatal("Codex public capabilities changed", model)
				}
				if strings.HasPrefix(model.ID, "acp/") && (model.Provider != "ACP" || !model.Build || !strings.HasPrefix(model.Name, "Configured agent ")) {
					t.Fatal("saved ACP identity lost", model)
				}
			}
			if !reflect.DeepEqual(ids, test.want) {
				t.Fatalf("catalog=%v want=%v", ids, test.want)
			}
			for _, private := range []string{executable, "private-launch-argument", "private-model-choice"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatal("private ACP configuration crossed pairing boundary")
				}
			}
		})
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("catalog discovery launched a configured ACP executable")
	}
}

func TestCompanionLargeCatalogKeepsConfiguredConnections(t *testing.T) {
	canonical := []chatModel{}
	for _, provider := range []string{"experiential", "openai"} {
		for index := 0; index < 600; index++ {
			canonical = append(canonical, chatModel{ID: fmt.Sprintf("%s/model-%03d", provider, index), Name: "Provider model", Build: true})
		}
	}
	for index := 0; index < 80; index++ {
		canonical = append(canonical, chatModel{ID: fmt.Sprintf("cursor/model-%03d", index), Name: "Cursor model", Build: true})
	}
	required := []string{"codex/future-primary", "codex/future-secondary", "acp/acp-1", "acp/acp-2", "acp/acp-3"}
	for _, id := range required {
		canonical = append(canonical, chatModel{ID: id, Name: "Saved connection", Build: true})
	}
	const selected = "openai/model-599"
	chatCalls := 0
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/stream" {
			chatCalls++
			var request chatRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != selected {
				t.Error("sampled catalog changed the selected chat model")
			}
			emitCompanionChatTest(w, map[string]any{"done": true, "success": true, "text": "Continued with the selected model."})
			return
		}
		writeJSON(w, map[string]any{"models": canonical})
	}))
	for _, path := range []string{"/models", "/chat/models"} {
		t.Run(path, func(t *testing.T) {
			var previous []string
			for pass := 0; pass < 2; pass++ {
				w := httptest.NewRecorder()
				s.ServeHTTP(w, companionRequest(s, http.MethodGet, path, ""))
				var response struct {
					Models []chatModel `json:"models"`
				}
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
					t.Fatal(w.Code, w.Body.String())
				}
				if len(response.Models) != 500 {
					t.Fatal("paired catalog escaped or failed to fill its existing bound", len(response.Models))
				}
				ids := []string{}
				for _, model := range response.Models {
					ids = append(ids, model.ID)
				}
				if !sort.StringsAreSorted(ids) {
					t.Fatal("catalog order changed")
				}
				index := sort.SearchStrings(ids, selected)
				if index < len(ids) && ids[index] == selected {
					t.Fatal("fixture selection was not outside the display sample")
				}
				for _, id := range required {
					shouldInclude := path == "/models" || strings.HasPrefix(id, "codex/")
					found := sort.SearchStrings(ids, id)
					if (found < len(ids) && ids[found] == id) != shouldInclude {
						t.Fatal("large provider catalog hid a saved connection or exposed a Build-only connection", id, path)
					}
				}
				for _, provider := range []string{"experiential/", "openai/"} {
					represented := false
					for _, id := range ids {
						represented = represented || strings.HasPrefix(id, provider)
					}
					if !represented {
						t.Fatal("connected provider disappeared", provider)
					}
				}
				if pass == 1 && !reflect.DeepEqual(ids, previous) {
					t.Fatal("catalog depended on runtime return order")
				}
				previous = ids
				for left, right := 0, len(canonical)-1; left < right; left, right = left+1, right-1 {
					canonical[left], canonical[right] = canonical[right], canonical[left]
				}
			}
		})
	}
	project := sharedCompanionProject(t, s)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPut, "/projects/"+project.ID+"/build-model", `{"model":"`+selected+`"}`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), selected) {
		t.Fatal("display sampling rejected an available build model", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/models", ""))
	var response struct {
		Models []chatModel `json:"models"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Models) != 500 || !strings.Contains(w.Body.String(), selected) {
		t.Fatal("selected build choice disappeared from the bounded catalog", w.Code, len(response.Models))
	}
	if err := s.validateModel(context.Background(), "openai/unavailable"); err == nil {
		t.Fatal("unconfigured model passed full catalog validation")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/chat", `{"model":"`+selected+`","messages":[{"role":"user","text":"Continue our idea"}]}`))
	if w.Code != http.StatusOK || chatCalls != 1 || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatal("display sampling rejected an available chat model", w.Code, chatCalls, w.Body.String())
	}
}

func TestCompanionCatalogPartialDeadlinePreservesCancellation(t *testing.T) {
	for _, path := range []string{"/models", "/chat/models"} {
		for _, mode := range []string{"deadline", "canceled", "revoked", "invalid", "failed"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				var s *companionSession
				s = testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "revoked" {
						s.cancel()
					}
					<-r.Context().Done()
					switch mode {
					case "invalid":
						io.WriteString(w, "invalid catalog")
					case "failed":
						http.Error(w, "runtime failed", http.StatusBadGateway)
					default:
						writeJSON(w, map[string]any{"models": []chatModel{{ID: "codex/future-model", Build: true}}})
					}
				}))
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				if mode == "canceled" {
					cancel()
				}
				r := companionRequest(s, http.MethodGet, path, "").WithContext(ctx)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if mode == "deadline" {
					if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "codex/future-model") {
						t.Fatal("valid partial catalog was hidden", w.Code, w.Body.String())
					}
				} else if w.Code < 400 || strings.Contains(w.Body.String(), "codex/future-model") {
					t.Fatal("canceled, revoked or invalid catalog was accepted", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestCompanionNativeBuildDispatchMatchesDesktop(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-test-token")
	cases := []struct{ id, driver, dispatched string }{
		{"acp/acp-1", "acp", "acp/acp-1"},
		{"acp/acp-2", "acp", "acp/acp-2"},
		{"acp/acp-3", "acp", "acp/acp-3"},
		{"codex/future-model", "codex", "future-model"},
		{"cursor/auto", "cursor", "auto"},
		{"claude-code/sonnet", "claude-code", "sonnet"},
		{"provider/nested/model", "opencode", "provider/nested/model"},
	}
	for _, test := range cases {
		t.Run(test.id, func(t *testing.T) {
			received := make(chan OpenCodeAgentRequest, 1)
			s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/chat/models":
					writeJSON(w, map[string]any{"models": []chatModel{{ID: test.id, Name: "Selected", Build: true}}})
				case "/opencode/refine":
					var request OpenCodeAgentRequest
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						t.Error("invalid dispatched build")
					}
					received <- request
					emitCompanionChatTest(w, map[string]any{"done": true, "success": true})
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			project := sharedCompanionProject(t, s)
			body, _ := json.Marshal(map[string]any{"instructions": "Build the selected project", "model": test.id})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+project.ID+"/build", string(body)))
			var snapshot struct {
				AgentDriver string `json:"agentDriver"`
				Model       string `json:"model"`
			}
			if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &snapshot) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			if snapshot.AgentDriver != test.driver || snapshot.Model != test.id {
				t.Fatal("public build identity changed", snapshot)
			}
			select {
			case request := <-received:
				if request.ProjectPath != project.path || request.AgentDriver != test.driver || request.Model != test.dispatched || request.OpenAIAuthMode != "opencode-config" || request.MediaGenerationPolicy != "skip" {
					t.Fatal("selected model or shared project escaped existing Desktop build boundary", request)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("build did not reach the selected worker")
			}
		})
	}
}

func TestCompanionCodexChatUsesExactModelWithoutToolsOrFallback(t *testing.T) {
	const model = "codex/future-chat-model"
	withCodexModels(t, []chatModel{{ID: model, Name: "Future model", Provider: "Codex", Build: true}})
	calls := 0
	withCodexTurn(t, func(ctx context.Context, options codexRunOptions, emit func(codexRPCMessage) error, request func(codexRPCMessage) (any, error)) (string, error) {
		calls++
		if options.Model != "future-chat-model" || !options.ChatOnly || !strings.Contains(options.Instructions, "Tools are disabled") {
			t.Fatal("Codex companion turn lost model or tool restriction", options)
		}
		if _, err := request(codexRPCMessage{Method: "item/commandExecution/requestApproval"}); err == nil {
			t.Fatal("chat allowed a coding approval")
		}
		return "", emit(codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","phase":"final_answer","text":"Talk through the idea."}}`))
	})
	service := &chatService{directory: t.TempDir(), prepare: func() error { t.Fatal("Codex fell back to OpenCode"); return nil }}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"models": []chatModel{{ID: model, Name: "Future model", Build: true}}})
	})
	mux.HandleFunc("/chat/stream", service.streamHandler)
	s := testCompanion(t, mux)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/chat", `{"model":"`+model+`","messages":[{"role":"user","text":"Discuss a new idea"}]}`))
	if w.Code != http.StatusOK || calls != 1 || !strings.Contains(w.Body.String(), `"text":"Talk through the idea."`) || !strings.Contains(w.Body.String(), `"model":"`+model+`"`) || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatal("Codex chat did not stream selected model", w.Code, calls, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/chat", `{"model":"codex/unavailable","messages":[{"role":"user","text":"Discuss"}]}`))
	if w.Code < 400 || calls != 1 {
		t.Fatal("unavailable Codex model silently fell back", w.Code, calls)
	}
}

func TestCompanionACPBuildKeepsDesktopWorkerLane(t *testing.T) {
	started, stopped := make(chan string, 3), make(chan string, 3)
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/models" {
			writeJSON(w, map[string]any{"models": []chatModel{{ID: "acp/acp-1", Build: true}, {ID: "acp/acp-2", Build: true}, {ID: "codex/future-model", Build: true}}})
			return
		}
		var request OpenCodeAgentRequest
		if r.URL.Path != "/opencode/refine" || json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("unexpected build worker request")
			return
		}
		started <- request.Model
		<-r.Context().Done()
		stopped <- request.Model
	}))
	project := sharedCompanionProject(t, s)
	start := func(model string, want int) string {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/"+project.ID+"/build", `{"instructions":"Build","model":"`+model+`"}`))
		if w.Code != want {
			t.Fatal("worker lane changed", model, w.Code, w.Body.String())
		}
		if want != http.StatusAccepted {
			return ""
		}
		var snapshot struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(w.Body.Bytes(), &snapshot) != nil || snapshot.ID == "" {
			t.Fatal("accepted build has no ID")
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("accepted worker did not start")
		}
		return snapshot.ID
	}
	first := start("acp/acp-1", http.StatusAccepted)
	start("acp/acp-2", http.StatusConflict)
	start("codex/future-model", http.StatusAccepted)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodDelete, "/builds/"+first, ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"canceled"`) {
		t.Fatal("ACP cancel failed", w.Code, w.Body.String())
	}
	start("acp/acp-2", http.StatusAccepted)
	s.cancel()
	for index := 0; index < 3; index++ {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("canceled ACP/Codex worker remained active")
		}
	}
}
