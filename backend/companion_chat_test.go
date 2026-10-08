package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func companionChatTestAPI(t *testing.T, run func(http.ResponseWriter, *http.Request, chatRequest)) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"models": []chatModel{
			{ID: "provider/talk", Name: "Talk", Provider: "Provider", Images: true, Build: true},
			{ID: "local-ai/maternion/mimo-v2.6:9b", Name: "MiMo", Build: false},
			{ID: "cursor/auto", Name: "Cursor", Build: true},
			{ID: "acp/agent", Name: "ACP", Build: true},
			{ID: "claude-code/sonnet", Name: "Claude Code", Build: true},
			{ID: "opencode/free", Name: "Free", Build: true},
		}})
	})
	mux.HandleFunc("/chat/stream", func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Mode != "chat" || request.Images != nil || request.Stack != nil {
			t.Error("companion escaped chat-only request boundary")
		}
		run(w, r, request)
	})
	return mux
}

func emitCompanionChatTest(w http.ResponseWriter, event any) {
	w.Header().Set("Content-Type", "text/event-stream")
	data, _ := json.Marshal(event)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()
}

func TestCompanionChatCatalogAndStrictScope(t *testing.T) {
	s := testCompanion(t, companionChatTestAPI(t, func(w http.ResponseWriter, r *http.Request, req chatRequest) { t.Fatal("invalid request reached chat") }))
	p := sharedCompanionProject(t, s)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "GET", "/chat/models", ""))
	var models struct {
		Models []chatModel `json:"models"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &models) != nil || len(models.Models) != 2 || models.Models[0].Build {
		t.Fatal("chat catalog included coding-only agents or lost chat-only local model", w.Body.String())
	}
	for _, test := range []struct{ path, body string }{
		{"/projects/unknown/chat", `{"model":"provider/talk","message":"Hi"}`},
		{"/projects/" + p.ID + "/chat", `{"model":"cursor/auto","message":"Hi"}`},
		{"/projects/" + p.ID + "/chat", `{"model":"provider/talk","message":"Hi","mode":"prototype"}`},
		{"/chat", `{"model":"provider/talk","messages":[{"role":"user","text":"Hi"}],"projectPath":"/private"}`},
		{"/chat", `{"model":"provider/talk","messages":[{"role":"system","text":"Hi"}]}`},
	} {
		w = httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, "POST", test.path, test.body))
		if w.Code < 400 {
			t.Fatal("invalid chat scope accepted", test.path, w.Body.String())
		}
	}
}

func TestCompanionChatAgentStatusStaysWithinSharedProject(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	s.jobs["other"] = &companionJob{id: "other", kind: "build", projectID: "other-project", status: "waiting", agentDriver: "cursor"}
	if state := s.projectChatAgentState("shared"); state.Status != "idle" {
		t.Fatal("chat borrowed an unrelated project's build status", state)
	}
	s.jobs["shared"] = &companionJob{id: "shared", kind: "build", projectID: "shared", status: "running", agentDriver: "opencode"}
	if state := s.projectChatAgentState("shared"); state.Status != "running" || state.Driver != "opencode" || state.SteerAvailable {
		t.Fatal("chat promised unsupported control of the active build", state)
	}
}

func TestCompanionChatSharedHistoryAndStaleDesktopSave(t *testing.T) {
	var project companionProject
	s := testCompanion(t, companionChatTestAPI(t, func(w http.ResponseWriter, r *http.Request, req chatRequest) {
		if req.ProjectPath != project.path || len(req.Messages) != 3 || req.Messages[2].Text != "Phone request" {
			t.Error("missing shared conversation")
		}
		emitCompanionChatTest(w, map[string]any{"status": "Thinking", "projectPath": "/private/secret", "providerCredential": "secret"})
		emitCompanionChatTest(w, map[string]any{"text": "Reply"})
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true, "text": "Reply", "projectPath": project.path})
	}))
	project = sharedCompanionProject(t, s)
	base := []chatMessage{{Role: "user", Text: "Desktop request"}, {Role: "assistant", Text: "Desktop reply"}}
	if err := saveSharedChatHistory(project.path, base); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/projects/"+project.ID+"/chat", `{"model":"provider/talk","message":"Phone request"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"historySaved":true`) || strings.Contains(w.Body.String(), project.path) || strings.Contains(w.Body.String(), "private/secret") || strings.Contains(w.Body.String(), "Credential") {
		t.Fatal("unsafe or incomplete stream", w.Body.String())
	}
	saved, err := readSharedChatHistory(project.path)
	if err != nil || len(saved) != 4 || saved[3].Text != "Reply" {
		t.Fatal("phone turn not saved", saved, err)
	}
	service := &chatService{}
	callSave := func(messages []chatMessage) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"projectPath": project.path, "messages": messages})
		result := httptest.NewRecorder()
		service.historyHandler(result, httptest.NewRequest("POST", "/chat/history", strings.NewReader(string(body))))
		return result
	}
	stale := append(append([]chatMessage{}, base...), chatMessage{Role: "user", Text: "Stale Desktop request"})
	if result := callSave(stale); result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "Reopen the project") {
		t.Fatal("stale Desktop save erased completed phone turn", result.Body.String())
	}
	latest := append(saved, chatMessage{Role: "user", Text: "Current Desktop request"})
	if result := callSave(latest); result.Code != 200 {
		t.Fatal("synchronized Desktop cannot save", result.Body.String())
	}
	if result := callSave([]chatMessage{}); result.Code != 200 {
		t.Fatal("explicit New conversation cannot clear the completed chat", result.Body.String())
	}
	if result := callSave(base); result.Code != 200 {
		t.Fatal("new conversation retains an obsolete phone guard", result.Body.String())
	}
}

func TestCompanionChatLocalContextAndPhotosStayTransient(t *testing.T) {
	var seen companionChatData
	s := testCompanion(t, companionChatTestAPI(t, func(w http.ResponseWriter, r *http.Request, req chatRequest) {
		seen = companionChatRequestData(r.Context())
		if req.ProjectPath != "" {
			t.Error("local phone chat used a Desktop project")
		}
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true, "text": "Local reply"})
	}))
	p := sharedCompanionProject(t, s)
	before, _ := os.ReadDir(p.path)
	image := companionImageFixture(t, "jpeg", 4, 3)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/chat/attachments", companionImageUploadBody(t, "reference.jpg", image)))
	var attachment companionImageAttachment
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &attachment) != nil || attachment.ProjectID != "local-chat" {
		t.Fatal("local upload", w.Body.String())
	}
	temporary := s.localChatPath
	body, _ := json.Marshal(map[string]any{"model": "provider/talk", "messages": []chatMessage{{Role: "user", Text: "Explain my local app"}}, "localProject": companionChatLocalProject{Name: "Local app", Prompt: "Original idea", HTML: "<!doctype html><html>phone only</html>"}, "attachmentIds": []string{attachment.ID}})
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/chat", string(body)))
	if w.Code != 200 || seen.localProject == nil || seen.localProject.Prompt != "Original idea" || len(seen.images) != 1 || !strings.Contains(companionChatReference(seen), "phone only") {
		t.Fatal("local context missing", w.Body.String())
	}
	after, _ := os.ReadDir(p.path)
	if len(before) != len(after) || len(s.projects) != 1 {
		t.Fatal("local chat modified or created a Desktop project")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/projects/"+p.ID+"/chat", `{"model":"provider/talk","message":"Use other project photo","attachmentIds":["`+attachment.ID+`"]}`))
	if w.Code != http.StatusGone {
		t.Fatal("local handle accepted for another project", w.Body.String())
	}
	s.close()
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatal("revocation kept transient phone photos")
	}
}

func TestCompanionChatRevocationCancelsAndReleasesProject(t *testing.T) {
	entered := make(chan struct{})
	s := testCompanion(t, companionChatTestAPI(t, func(w http.ResponseWriter, r *http.Request, req chatRequest) {
		close(entered)
		<-r.Context().Done()
	}))
	p := sharedCompanionProject(t, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(httptest.NewRecorder(), companionRequest(s, "POST", "/projects/"+p.ID+"/chat", `{"model":"provider/talk","message":"Keep request"}`))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("chat did not start")
	}
	s.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pairing revocation did not cancel chat")
	}
	sharedChatHistory.Lock()
	active := sharedChatHistory.active[p.path]
	sharedChatHistory.Unlock()
	if active {
		t.Fatal("canceled chat kept the active project guard")
	}
	data, err := os.ReadFile(filepath.Join(p.path, ".glowbom", "chat.json"))
	if err != nil || !strings.Contains(string(data), "Keep request") {
		t.Fatal("cancellation lost user request")
	}
	if _, err := beginCompanionProjectChat(p.path, "Keep request"); err != nil {
		t.Fatal("retry blocked after cancellation", err)
	}
	endCompanionProjectChat(p.path)
}
