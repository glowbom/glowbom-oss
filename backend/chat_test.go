package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatStreamingAndQuota(t *testing.T) {
	for _, quota := range []bool{false, true} {
		t.Run(fmt.Sprint(quota), func(t *testing.T) {
			events := make(chan string, 10)
			var aborted atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("directory") != "/isolated" {
					t.Error("wrong working directory")
				}
				switch r.URL.Path {
				case "/session":
					var body struct {
						Permission []map[string]string `json:"permission"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					if len(body.Permission) != 1 || body.Permission[0]["permission"] != "*" || body.Permission[0]["action"] != "deny" {
						t.Error("missing deny-all permissions")
					}
					writeJSON(w, map[string]any{"id": "ses_test", "permission": body.Permission})
				case "/experimental/tool/ids":
					writeJSON(w, []string{"bash", "read", "edit", "custom_tool"})
				case "/event":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {}\n\n")
					w.(http.Flusher).Flush()
					for {
						select {
						case event := <-events:
							fmt.Fprintf(w, "data: %s\n\n", event)
							w.(http.Flusher).Flush()
						case <-r.Context().Done():
							return
						}
					}
				case "/session/ses_test/message":
					var body struct {
						Tools map[string]bool   `json:"tools"`
						Model map[string]string `json:"model"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					for _, id := range []string{"*", "bash", "read", "edit", "custom_tool"} {
						enabled, ok := body.Tools[id]
						if !ok || enabled {
							t.Errorf("tool %s not denied", id)
						}
					}
					if body.Model["providerID"] != "test" || body.Model["modelID"] != "model" {
						t.Error("wrong model")
					}
					if quota {
						events <- `{"type":"session.status","properties":{"sessionID":"ses_test","status":{"type":"retry","message":"Free usage exceeded"}}}`
						<-r.Context().Done()
						return
					}
					events <- `{"type":"session.status","properties":{"sessionID":"ses_test","status":{"type":"retry","attempt":1,"message":"The provider is temporarily unavailable"}}}`
					events <- `{"type":"message.updated","properties":{"info":{"id":"msg_u","role":"user","sessionID":"ses_test"}}}`
					events <- `{"type":"message.part.updated","properties":{"part":{"id":"prt_u","type":"text","text":"private prompt","messageID":"msg_u","sessionID":"ses_test"}}}`
					events <- `{"type":"message.updated","properties":{"info":{"id":"msg_a","role":"assistant","sessionID":"ses_test"}}}`
					events <- `{"type":"message.part.updated","properties":{"part":{"id":"prt_a","type":"text","text":"Hello","messageID":"msg_a","sessionID":"ses_test"}}}`
					events <- `{"type":"message.part.delta","properties":{"sessionID":"ses_test","messageID":"msg_a","partID":"prt_a","field":"text","delta":" world"}}`
					time.Sleep(50 * time.Millisecond)
					writeJSON(w, map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "text", "text": "Hello world"}}})
				case "/session/ses_test/abort":
					aborted.Store(true)
					writeJSON(w, true)
				default:
					writeJSON(w, true)
				}
			}))
			defer server.Close()
			service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var frames []string
			text, err := service.complete(ctx, "test/model", "Chat", nil, func(text string) { frames = append(frames, text) })
			cancel()
			if quota {
				if err == nil || !strings.Contains(err.Error(), "Free usage exceeded") {
					t.Fatal(err)
				}
			} else {
				if err != nil || text != "Hello world" {
					t.Fatal(text, err)
				}
				if len(frames) == 0 || frames[len(frames)-1] != "Hello world" {
					t.Fatal("missing streamed text", frames)
				}
				for _, f := range frames {
					if strings.Contains(f, "private") {
						t.Fatal("user text displayed as assistant")
					}
				}
			}
			if !aborted.Load() {
				t.Fatal("session not aborted on cleanup")
			}
		})
	}
}

func TestChatPrototypeValidationAndHistory(t *testing.T) {
	root, err := createSketchProject(t.TempDir(), "Sketch")
	if err != nil {
		t.Fatal(err)
	}
	html := "<!doctype html><html><body>First</body></html>"
	if err := saveChatPrototype(root, html, "", []chatMessage{{Role: "user", Text: "Build"}}, "test/model", nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"Here is your app", "<!doctype html><html>unfinished"} {
		if err := saveChatPrototype(root, bad, html, nil, "test/model", nil); err == nil {
			t.Fatal("accepted incomplete output")
		}
	}
	if err := saveChatPrototype(root, "<!doctype html><html>new</html>", "stale", nil, "test/model", nil); err == nil {
		t.Fatal("overwrote newer prototype")
	}
	saved, _ := os.ReadFile(filepath.Join(root, "prototype/index.html"))
	if string(saved) != html {
		t.Fatal("prototype corrupted")
	}
	versions, _ := os.ReadDir(filepath.Join(root, ".glowbom/prototypes"))
	if len(versions) != 1 {
		t.Fatal("missing history")
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(root, "outside"))
	if _, err := chatWriteDirectory(root, "outside/nested"); err == nil {
		t.Fatal("followed output symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "nested")); !os.IsNotExist(err) {
		t.Fatal("wrote outside project")
	}
}

func TestChatModelsHideDisconnectedProviders(t *testing.T) {
	t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
	rememberCursorModels(nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"connected":["connected"],"all":[{"id":"connected","name":"Connected","models":{"vision":{"name":"Vision","capabilities":{"input":{"image":true}}},"text":{"name":"Text"}}},{"id":"other","models":{"hidden":{"name":"Hidden"}}}]}`)
	}))
	defer server.Close()
	service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client()}
	models, err := service.models(context.Background())
	if err != nil || len(models) != 2 {
		t.Fatal(models, err)
	}
	if models[0].Images || !models[1].Images {
		t.Fatal("incorrect image capabilities", models)
	}
}

func TestMiMoPrototypeAndTranslationAreChatOnly(t *testing.T) {
	service := &chatService{}
	for _, mode := range []string{"prototype", "translation"} {
		req := chatRequest{Model: "ollama/maternion/mimo-v2.6:9b", Mode: mode, Messages: []chatMessage{{Role: "user", Text: "Build an app"}}}
		if mode == "translation" {
			req.Stack = &chatStack{ID: "swiftui", Name: "SwiftUI", Description: "Native Apple application"}
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		service.streamHandler(recorder, httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(data)))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Chat only") {
			t.Fatalf("%s: %d %q", mode, recorder.Code, recorder.Body.String())
		}
	}
}

func TestChatRejectsAttachmentsOutsideUploadRoot(t *testing.T) {
	service := &chatService{uploads: t.TempDir()}
	file := filepath.Join(t.TempDir(), "private.png")
	os.WriteFile(file, []byte("private"), 0600)
	if _, err := service.imagePart(file); err == nil {
		t.Fatal("read arbitrary attachment path")
	}
}

func TestChatProviderErrorMessages(t *testing.T) {
	for _, tt := range []struct {
		name, raw, model, code, contains string
	}{
		{"free restriction", `{"name":"APIError","data":{"message":"OpenCode's free tier can only be used from within OpenCode","statusCode":403,"responseHeaders":{"authorization":"secret"},"responseBody":"private"}}`, "opencode/big-pickle", "provider_restricted", "Build"},
		{"subscription quota", `{"name":"APIError","data":{"message":"The usage limit has been reached","statusCode":429,"responseHeaders":{"account":"private"}}}`, "openai/gpt-5.6-sol-fast", "usage_limit", "Codex allowance"},
		{"provider credits", `{"error":{"message":"Insufficient credits","statusCode":402}}`, "opencode/gpt-5.6-luna", "usage_limit", "usage or credit limit"},
		{"authentication", `{"error":{"message":"Expired connection","statusCode":401}}`, "openai/model", "provider_auth", "Reconnect the provider"},
		{"ordinary error", `{"name":"APIError","data":{"message":"Model is unavailable","responseHeaders":{"secret":"private"}}}`, "test/model", "provider_error", "Model is unavailable"},
		{"unrecognized envelope", `{"responseHeaders":{"authorization":"secret"},"responseBody":"private"}`, "test/model", "provider_error", "could not complete"},
		{"gateway HTML", `<html>private diagnostic from proxy</html>`, "test/model", "provider_error", "could not complete"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := decodeChatProviderError([]byte(tt.raw), 0)
			if code := chatFailureCode(err); code != tt.code {
				t.Fatalf("code = %q, want %q", code, tt.code)
			}
			message := chatFailureMessage(err, tt.model)
			if !strings.Contains(message, tt.contains) {
				t.Fatalf("message = %q, want %q", message, tt.contains)
			}
			for _, private := range []string{"private", "secret", "responseHeaders", "statusCode"} {
				if strings.Contains(message, private) {
					t.Fatalf("exposed provider envelope: %s", message)
				}
			}
		})
	}
}

func TestChatSessionErrorUsesPublicMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			writeJSON(w, map[string]any{"id": "ses_error", "permission": []map[string]string{{"permission": "*", "pattern": "*", "action": "deny"}}})
		case "/experimental/tool/ids":
			writeJSON(w, []string{"read", "bash"})
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/session/ses_error/message":
			fmt.Fprint(w, `{"info":{"error":{"name":"APIError","data":{"message":"OpenCode's free tier can only be used from within OpenCode","responseHeaders":{"private":"secret"}}}},"parts":[]}`)
		default:
			writeJSON(w, true)
		}
	}))
	defer server.Close()
	service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := service.complete(ctx, "opencode/big-pickle", "Chat", nil, func(string) {})
	if err == nil || chatFailureCode(err) != "provider_restricted" {
		t.Fatal(err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatal("provider envelope reached the caller", err)
	}
}

func TestChatRequestUsesServerBasicAuth(t *testing.T) {
	for _, username := range []string{"", "glowbom-test"} {
		t.Run("username="+username, func(t *testing.T) {
			t.Setenv("OPENCODE_SERVER_USERNAME", username)
			t.Setenv("OPENCODE_SERVER_PASSWORD", "fixture-server-password")
			expectedUsername := username
			if expectedUsername == "" {
				expectedUsername = "opencode"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != expectedUsername || password != "fixture-server-password" {
					t.Error("missing or incorrect local OpenCode Basic authorization")
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				writeJSON(w, true)
			}))
			defer server.Close()
			service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client()}
			response, err := service.request(context.Background(), http.MethodGet, "/provider", nil)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
		})
	}
}

func TestChatLocalServerUnauthorizedIsNotProviderAuth(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_PASSWORD", "fixture-wrong-password")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unauthorized: private server diagnostic", http.StatusUnauthorized)
	}))
	defer server.Close()
	service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client()}
	_, err := service.request(context.Background(), http.MethodGet, "/provider", nil)
	if err == nil || chatFailureCode(err) != "opencode_server_auth" {
		t.Fatal("local authentication failure was not identified", err)
	}
	message := chatFailureMessage(err, "openai/test")
	if !strings.Contains(message, "local OpenCode server") || !strings.Contains(message, "glowbom start") || strings.Contains(message, "private") {
		t.Fatal("incorrect server authentication guidance", message)
	}
	providerErr := decodeChatProviderError([]byte(`{"name":"APIError","data":{"message":"Expired provider login","statusCode":401}}`), 0)
	if chatFailureCode(providerErr) != "provider_auth" || !strings.Contains(chatFailureMessage(providerErr, "openai/test"), "Reconnect the provider") {
		t.Fatal("provider authentication failure was confused with server authentication")
	}
}
