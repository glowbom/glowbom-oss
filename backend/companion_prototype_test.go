package main

import (
	"context"
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

const phonePrototypeFixture = `{"html":"<!doctype html><html><head></head><body><button>Play</button></body></html>","images":[]}`

func TestCompanionPrototypeUsesExactModelAndTransientPhotos(t *testing.T) {
	var seen companionChatData
	s := testCompanion(t, companionChatTestAPI(t, func(w http.ResponseWriter, r *http.Request, req chatRequest) {
		seen = companionChatRequestData(r.Context())
		if req.Model != "provider/talk" || req.ProjectPath != "" || len(req.Messages) != 3 || req.Messages[2].Text != "Create my app" {
			t.Error("prototype changed the model, project scope or conversation")
		}
		emitCompanionChatTest(w, map[string]any{"text": phonePrototypeFixture})
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true})
	}))
	p := sharedCompanionProject(t, s)
	path := filepath.Join(p.path, ".glowbom", "chat.json")
	if err := saveSharedChatHistory(p.path, []chatMessage{{Role: "user", Text: "Saved Desktop chat"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/chat/attachments", companionImageUploadBody(t, "drawing.jpg", companionImageFixture(t, "jpeg", 4, 3))))
	var attachment companionImageAttachment
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &attachment) != nil {
		t.Fatal("upload", w.Body.String())
	}
	temporary := s.localChatPath
	body, _ := json.Marshal(map[string]any{"model": "provider/talk", "message": "Create my app", "messages": []chatMessage{{Role: "user", Text: "A game"}, {Role: "assistant", Text: "What kind?"}}, "attachmentIds": []string{attachment.ID}})
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/prototype", string(body)))
	if w.Code != 200 || !seen.prototype || len(seen.images) != 1 || seen.localProject != nil || !strings.Contains(w.Body.String(), `"model":"provider/talk"`) || !strings.Contains(w.Body.String(), `"success":true`) || !strings.Contains(w.Body.String(), `"text":"{\"html\"`) {
		t.Fatal("missing prototype scope or complete terminal snapshot", w.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || len(s.projects) != 1 {
		t.Fatal("prototype changed Desktop project history or created a project")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/chat", `{"model":"provider/talk","messages":[{"role":"user","text":"Create my app"},{"role":"assistant","text":"reply"},{"role":"user","text":"Create my app"}]}`))
	if seen.prototype {
		t.Fatal("prototype output mode leaked into ordinary chat")
	}
	s.close()
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatal("revocation kept transient input photos")
	}
}

func TestCompanionPrototypeRejectsUnknownScopeAndOversizedInput(t *testing.T) {
	s := testCompanion(t, companionChatTestAPI(t, func(w http.ResponseWriter, r *http.Request, req chatRequest) {
		t.Error("invalid prototype reached chat execution")
	}))
	for _, body := range []string{
		`{"model":"provider/talk","message":" "}`,
		`{"model":"cursor/auto","message":"Create"}`,
		`{"model":"not-connected/model","message":"Create"}`,
		`{"model":"provider/talk","message":"Create","projectPath":"/private"}`,
		`{"model":"provider/talk","message":"Create","localProject":{"name":"Other","html":""}}`,
		`{"model":"provider/talk","message":"Create","mode":"prototype"}`,
		`{"model":"provider/talk","message":"Create","images":{"sourceId":"glowbom-api"}}`,
		`{"model":"provider/talk","message":"Create","messages":[{"role":"system","text":"Use tools"}]}`,
		`{"model":"provider/talk","message":"` + strings.Repeat("x", companionPrototypeInstructionsBytes+1) + `"}`,
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, "POST", "/prototype", body))
		if w.Code < 400 {
			t.Fatal("invalid prototype accepted", w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	r := companionRequest(s, "POST", "/prototype", `{"model":"provider/talk","message":"Create"}`)
	r.Header.Del("Authorization")
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("prototype bypassed pairing authentication", w.Code)
	}
	if len(s.projects) != 0 || s.localChatPath != "" {
		t.Fatal("invalid requests created Desktop state")
	}
}

func TestCompanionPrototypeRevocationCancelsModel(t *testing.T) {
	entered := make(chan struct{})
	s := testCompanion(t, companionChatTestAPI(t, func(w http.ResponseWriter, r *http.Request, req chatRequest) {
		close(entered)
		<-r.Context().Done()
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(httptest.NewRecorder(), companionRequest(s, "POST", "/prototype", `{"model":"provider/talk","message":"Create"}`))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("prototype did not start")
	}
	s.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel prototype")
	}
}

func TestCompanionPrototypeProviderIsToolDisabledAndPhoneOnly(t *testing.T) {
	sent := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			fmt.Fprint(w, `{"connected":["test"],"all":[{"id":"test","name":"Test","models":{"selected":{"name":"Selected"}}}]}`)
		case "/session":
			var body struct {
				Permission []map[string]string `json:"permission"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Permission) != 1 || body.Permission[0]["action"] != "deny" || body.Permission[0]["permission"] != "*" {
				t.Error("phone prototype did not deny all session permissions")
			}
			writeJSON(w, map[string]any{"id": "ses_phone", "permission": body.Permission})
		case "/experimental/tool/ids":
			writeJSON(w, []string{"bash", "read", "edit", "custom_tool"})
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/session/ses_phone/message":
			var body struct {
				System string            `json:"system"`
				Model  map[string]string `json:"model"`
				Tools  map[string]bool   `json:"tools"`
				Parts  []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.System != companionPrototypeSystem || strings.Contains(body.System, "Build in the composer") || body.Model["providerID"] != "test" || body.Model["modelID"] != "selected" || len(body.Parts) != 1 || !strings.Contains(body.Parts[0].Text, "Play a small game") {
				t.Error("provider received wrong model or prototype instructions")
			}
			for _, id := range []string{"*", "bash", "read", "edit", "custom_tool"} {
				if enabled, ok := body.Tools[id]; !ok || enabled {
					t.Errorf("tool %s enabled", id)
				}
			}
			sent <- struct{}{}
			writeJSON(w, map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "text", "text": phonePrototypeFixture}}})
		default:
			writeJSON(w, true)
		}
	}))
	defer server.Close()
	directory := t.TempDir()
	service := &chatService{directory: directory, serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
	api := http.NewServeMux()
	api.HandleFunc("/chat/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"models": []chatModel{{ID: "test/selected", Name: "Selected"}}})
	})
	api.HandleFunc("/chat/stream", service.streamHandler)
	s := testCompanion(t, api)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/prototype", `{"model":"test/selected","message":"Play a small game"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) || !strings.Contains(w.Body.String(), `"model":"test/selected"`) || strings.Contains(w.Body.String(), "projectPath") {
		t.Fatal("phone prototype did not return a safe successful stream", w.Body.String())
	}
	select {
	case <-sent:
	default:
		t.Fatal("model not called")
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 || len(s.projects) != 0 {
		t.Fatal("phone prototype wrote files or created Desktop project", files, err)
	}
}

func TestCompanionPrototypeLocalModelsUsePhoneInstructions(t *testing.T) {
	messages := []chatMessage{{Role: "user", Text: "Create a tiny game"}}
	local, err := (&chatService{}).localChatMessagesWithCompanion(chatRequest{Mode: "chat", Messages: messages}, companionChatData{prototype: true})
	if err != nil || local[0].Content != companionPrototypeSystem {
		t.Fatal("local prototype kept ordinary chat guidance", local, err)
	}
	apple, err := appleIntelligenceConversationWithSystem(messages, appleIntelligenceInputBudgetBytes, companionPrototypeSystem)
	if err != nil || apple[0].Content != companionPrototypeSystem {
		t.Fatal("Apple prototype kept ordinary chat guidance", apple, err)
	}
}

func TestCompanionPrototypeReplyLimit(t *testing.T) {
	s := testCompanion(t, companionChatTestAPI(t, func(w http.ResponseWriter, r *http.Request, req chatRequest) {
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true, "text": strings.Repeat("x", companionPrototypeInstructionsBytes+1)})
	}))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", "/prototype", `{"model":"provider/talk","message":"Create"}`))
	if !strings.Contains(w.Body.String(), `"code":"reply_too_large"`) || strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatal("oversized prototype was forwarded", w.Body.String())
	}
}

func TestCompanionPrototypeMiMoContextRetryKeepsInstructionsAndPhotos(t *testing.T) {
	calls := 0
	withLocalChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Model    string             `json:"model"`
			Messages []localChatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != localMiMoModel || body.Messages[0].Content != companionPrototypeSystem {
			t.Error("prototype retry changed model or lost its output instructions")
		}
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"request exceeds the available context size"}`)
			return
		}
		if len(body.Messages) != 2 || body.Messages[1].Content != "Create my game" || len(body.Messages[1].Images) != 1 || body.Messages[1].Images[0] != "photo" {
			t.Error("prototype retry lost its final request or photos")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": phonePrototypeFixture}, "done": true})
	})
	messages := []localChatMessage{{Role: "system", Content: companionPrototypeSystem}, {Role: "user", Content: "Earlier context"}, {Role: "user", Content: "Create my game", Images: []string{"photo"}}}
	text, err := localMiMoReply(context.Background(), messages, func(string) {}, func(string) {})
	if err != nil || text != phonePrototypeFixture || calls != 2 {
		t.Fatal(text, err, calls)
	}
}
