package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withLocalChatServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(h)
	before := localChatBaseURL
	localChatBaseURL = server.URL
	t.Cleanup(func() { localChatBaseURL = before; server.Close() })
}
func configureTestMiMo(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := saveLocalAIState(localAIState{localMiMoModel: {"name": "MiMo"}}); err != nil {
		t.Fatal(err)
	}
}
func TestLocalChatBudgetKeepsLatestMessageWhole(t *testing.T) {
	latest := "Hello 世界"
	messages := []chatMessage{{Role: "user", Text: strings.Repeat("old", 5000)}, {Role: "assistant", Text: "old answer"}, {Role: "user", Text: "Recent question"}, {Role: "assistant", Text: "Recent answer", Reasoning: "private reasoning"}, {Role: "user", Text: latest}}
	result, err := localChatConversation(messages, strings.Repeat("project", 1000))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "private reasoning") || strings.Contains(string(data), "old answer") || result[len(result)-1].Content != latest || result[1].Role != "user" {
		t.Fatal(string(data))
	}
	used := 0
	for _, m := range result {
		used += len(m.Content) + 32
	}
	if used > localChatInputBytes {
		t.Fatal("unbounded conversation", used)
	}
	if _, err := localChatConversation([]chatMessage{{Role: "user", Text: strings.Repeat("界", 2000)}}, ""); err == nil {
		t.Fatal("silently cropped oversized message")
	}
	if _, err := localChatConversation(nil, ""); err == nil {
		t.Fatal("accepted no messages")
	}
}
func TestLocalChatWithLargeProjectNeverSendsPrototype(t *testing.T) {
	configureTestMiMo(t)
	project := translationProject(t)
	html := "<!doctype html><html>" + strings.Repeat("PRIVATE_PROTOTYPE_CODE", 200000) + "</html>"
	if err := os.WriteFile(filepath.Join(project, "prototype", "index.html"), []byte(html), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	withLocalChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Model    string             `json:"model"`
			Messages []localChatMessage `json:"messages"`
			Tools    json.RawMessage    `json:"tools"`
			Think    bool               `json:"think"`
			Options  map[string]int     `json:"options"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad request")
		}
		data, _ := json.Marshal(body.Messages)
		if r.URL.Path != "/api/chat" || body.Model != localMiMoModel || len(body.Tools) > 0 || body.Think || body.Options["num_ctx"] != 8192 || body.Options["num_predict"] != 768 || strings.Contains(string(data), "PRIVATE_PROTOTYPE_CODE") || strings.Contains(string(data), "old conversation") || body.Messages[len(body.Messages)-1].Content != "howdy" {
			t.Error("invalid bounded chat", string(data))
		}
		fmt.Fprintln(w, `{"message":{"content":"Howdy!"},"done":true,"done_reason":"stop"}`)
	})
	req := chatRequest{ProjectPath: project, Mode: "chat", Model: localAIProvider + "/" + localMiMoModel, Messages: []chatMessage{{Role: "user", Text: strings.Repeat("old conversation ", 2000)}, {Role: "assistant", Text: "old answer"}, {Role: "user", Text: "howdy"}}}
	data, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	(&chatService{prepare: func() error { t.Fatal("MiMo chat started OpenCode"); return nil }}).streamHandler(w, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(data)))
	if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), `"success":true`) || !strings.Contains(w.Body.String(), "Howdy!") {
		t.Fatal(w.Code, w.Body.String())
	}
	saved, _ := os.ReadFile(filepath.Join(project, "prototype", "index.html"))
	if string(saved) != html {
		t.Fatal("chat changed the prototype")
	}
}
func TestLocalChatAttachmentsKeepUploadBoundary(t *testing.T) {
	uploads := t.TempDir()
	s := &chatService{uploads: uploads}
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(uploads, "test.png")
	os.WriteFile(path, imageBytes.Bytes(), 0600)
	req := chatRequest{Messages: []chatMessage{{Role: "user", Text: "Describe this"}}, AttachmentPaths: []string{path}}
	messages, err := s.localChatMessages(req)
	if err != nil || len(messages[len(messages)-1].Images) != 1 || messages[len(messages)-1].Images[0] != base64.StdEncoding.EncodeToString(imageBytes.Bytes()) {
		t.Fatal(messages, err)
	}
	other := filepath.Join(t.TempDir(), "private.png")
	os.WriteFile(other, imageBytes.Bytes(), 0600)
	req.AttachmentPaths = []string{other}
	if _, err := s.localChatMessages(req); err == nil {
		t.Fatal("accepted file outside uploads")
	}
}
func TestLocalChatContextRetryRetainsLatestImages(t *testing.T) {
	calls := 0
	withLocalChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Messages []localChatMessage `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if calls == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"request (4463 tokens) exceeds the available context size (4096 tokens), try increasing it"}`)
			return
		}
		if len(body.Messages) != 2 || body.Messages[0].Content != localChatSystem || body.Messages[1].Content != "current" || len(body.Messages[1].Images) != 1 {
			t.Error("lost latest request", body)
		}
		fmt.Fprintln(w, `{"message":{"content":"Reply"},"done":true}`)
	})
	messages := []localChatMessage{{Role: "system", Content: "summary"}, {Role: "user", Content: "old"}, {Role: "assistant", Content: "old reply"}, {Role: "user", Content: "current", Images: []string{"image"}}}
	text, err := localMiMoReply(context.Background(), messages, func(string) {}, func(string) {})
	if err != nil || text != "Reply" || calls != 2 {
		t.Fatal(text, err, calls)
	}
}
func TestLocalChatRejectsFailedAndTruncatedStreams(t *testing.T) {
	for _, response := range []string{
		`{"message":{"content":"partial"},"done":false}`,
		`{"message":{"content":""},"done":true}`,
		`{"message":{"content":"partial"},"done":true,"done_reason":"length"}`,
		`{"message":{"tool_calls":[{}]},"done":true}`,
		`{"error":"private secret provider failure"}`,
	} {
		t.Run(response, func(t *testing.T) {
			withLocalChatServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, response) })
			_, err := streamLocalMiMo(context.Background(), []localChatMessage{{Role: "user", Content: "hi"}}, func(string) {})
			if err == nil || strings.Contains(err.Error(), "private secret") {
				t.Fatal("unsafe completion", err)
			}
		})
	}
}
func TestChatContextErrorIsReadable(t *testing.T) {
	for _, raw := range []string{
		`{"error":{"code":400,"message":"request (4463 tokens) exceeds the available context size (4096 tokens), try increasing it","type":"exceed_context_size_error"}}`,
		`{"data":{"message":"{\"error\":{\"type\":\"exceed_context_size_error\"}}"}}`,
	} {
		err := decodeChatProviderError([]byte(raw), 400)
		message := chatFailureMessage(err, localAIProvider+"/"+localMiMoModel)
		if chatFailureCode(err) != "context_limit" || strings.Contains(message, "{") || !strings.Contains(message, "shorter") {
			t.Fatal(err, message)
		}
	}
}
