package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalAIConfigPreservesExistingProviders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	config, err := localAIModelConfig("maternion/mimo-v2.6:9b", []string{"completion", "vision"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveLocalAIState(localAIState{"maternion/mimo-v2.6:9b": config}); err != nil {
		t.Fatal(err)
	}
	path, _ := localAIStatePath()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("settings must be private", err)
	}
	env, err := addLocalAIOpenCodeConfig([]string{`OPENCODE_CONFIG_CONTENT={"theme":"system","provider":{"ollama":{"options":{"baseURL":"http://custom:1234/v1"}},"other":{"name":"Keep me"}}}`})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(envValue(env, "OPENCODE_CONFIG_CONTENT")), &result); err != nil {
		t.Fatal(err)
	}
	providers := result["provider"].(map[string]any)
	if result["theme"] != "system" || providers["other"] == nil || providers["ollama"].(map[string]any)["options"].(map[string]any)["baseURL"] != "http://custom:1234/v1" {
		t.Fatal("existing configuration changed")
	}
	local := providers[localAIProvider].(map[string]any)
	if local["models"].(map[string]any)["maternion/mimo-v2.6:9b"] == nil {
		t.Fatal("missing local model")
	}
	for _, raw := range []string{"invalid", "null", "[]"} {
		if _, err := addLocalAIOpenCodeConfig([]string{"OPENCODE_CONFIG_CONTENT=" + raw}); err == nil {
			t.Fatalf("accepted invalid config %q", raw)
		}
	}
}

func TestLocalAIModelCapabilities(t *testing.T) {
	if _, err := localAIModelConfig("embedding", []string{"embedding"}); err == nil {
		t.Fatal("embedding model accepted for chat")
	}
	for _, vision := range []bool{false, true} {
		caps := []string{"completion"}
		if vision {
			caps = append(caps, "vision", "tools")
		}
		config, err := localAIModelConfig("test", caps)
		if err != nil || config["attachment"] != vision || config["tool_call"] != vision {
			t.Fatal(config, err)
		}
	}
}

func TestLocalAIStatusAndAuthentication(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GLOWBOM_SERVER_TOKEN", "test-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"z:1","size":6000},{"name":"a:1","size":4000}]}`))
	}))
	defer server.Close()
	service := newLocalAIService(nil)
	service.endpoint, service.client = server.URL, server.Client()
	if err := saveLocalAIState(localAIState{"a:1": {"name": "A"}}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		w := httptest.NewRecorder()
		service.handler(w, httptest.NewRequest(method, "/settings/local-ai", strings.NewReader(`{"model":"a:1"}`)))
		if w.Code != 401 || calls != 0 {
			t.Fatal("unauthenticated access", w.Code, calls)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/settings/local-ai", nil)
	r.Header.Set("Authorization", "Bearer test-token")
	service.handler(w, r)
	var status localAIStatus
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || w.Code != 200 || !status.Running || len(status.Models) != 2 {
		t.Fatal(w.Body.String())
	}
	if status.Models[0].Name != "a:1" || !status.Models[0].Connected || status.Models[1].Connected {
		t.Fatal(status.Models)
	}
	for _, body := range []string{`{`, `{}`, `{"model":""}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/settings/local-ai", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-token")
		service.handler(w, r)
		if w.Code != 400 {
			t.Fatal("invalid input accepted", body, w.Code)
		}
	}
	server.Close()
	status, err := service.status(context.Background())
	if err != nil || status.Running || len(status.Models) != 0 {
		t.Fatal("offline status", status, err)
	}
}

func TestLocalAIRefusesExternalOpenCodeAndUnknownModels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENCODE_URL", "http://external.invalid")
	service := newLocalAIService(nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"models":[]}`)) }))
	defer server.Close()
	service.endpoint, service.client = server.URL, server.Client()
	if err := service.connect(context.Background(), "anything"); err == nil || !strings.Contains(err.Error(), "external") {
		t.Fatal(err)
	}
	state, err := readLocalAIState()
	if err != nil || len(state) != 0 {
		t.Fatal("external connection mutated settings")
	}
}

func TestLocalAIMiMoCannotGenerate(t *testing.T) {
	service := &chatService{}
	for _, mode := range []string{"prototype", "translation"} {
		body := `{"model":"glowbom-ollama/maternion/mimo-v2.6:9b","mode":"` + mode + `","messages":[{"role":"user","text":"Build"}],"stack":{"id":"swiftui","name":"SwiftUI","description":"Native Apple application"}}`
		w := httptest.NewRecorder()
		service.streamHandler(w, httptest.NewRequest("POST", "/chat/stream", strings.NewReader(body)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "Chat only") {
			t.Fatal(mode, w.Code, w.Body.String())
		}
	}
}

func TestLocalAIConnectTestsThroughOpenCode(t *testing.T) {
	for _, scenario := range []string{"ready", "busy", "empty-reply", "reload-fails", "unknown", "cloud"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OPENCODE_URL", "")
			bin := t.TempDir()
			executable := "opencode"
			if runtime.GOOS == "windows" {
				executable += ".exe"
			}
			if err := os.WriteFile(filepath.Join(bin, executable), []byte("test executable placeholder"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			reloads, messages := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/tags":
					io.WriteString(w, `{"models":[{"name":"test:1","size":100}]}`)
				case "/api/show":
					if scenario == "cloud" {
						io.WriteString(w, `{"capabilities":["completion"],"remote_host":"https://ollama.com"}`)
					} else {
						io.WriteString(w, `{"capabilities":["completion"]}`)
					}
				case "/session/status":
					if scenario == "busy" {
						io.WriteString(w, `{"active":{"type":"busy"}}`)
					} else {
						io.WriteString(w, `{}`)
					}
				case "/provider":
					io.WriteString(w, `{"connected":["glowbom-ollama"],"all":[{"id":"glowbom-ollama","name":"Local","models":{"test:1":{"name":"Test"}}}]}`)
				case "/session":
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil || !strings.Contains(fmt.Sprint(body["permission"]), "deny") {
						t.Error("test session did not deny tools")
					}
					io.WriteString(w, `{"id":"test-session","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`)
				case "/experimental/tool/ids":
					io.WriteString(w, `["bash","read"]`)
				case "/event":
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {}\n\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				case "/session/test-session/message":
					messages++
					var body struct {
						Model map[string]string   `json:"model"`
						Tools map[string]bool     `json:"tools"`
						Parts []map[string]string `json:"parts"`
					}
					if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model["providerID"] != localAIProvider || body.Model["modelID"] != "test:1" || body.Tools["*"] || body.Tools["bash"] || len(body.Parts) != 1 || body.Parts[0]["text"] != "Say hello in one short sentence." {
						t.Error("test escaped restricted greeting", body)
					}
					if scenario == "empty-reply" {
						io.WriteString(w, `{"parts":[]}`)
					} else {
						io.WriteString(w, `{"parts":[{"type":"text","text":"Hello!"}]}`)
					}
				default:
					io.WriteString(w, `true`)
				}
			}))
			defer server.Close()
			chat := &chatService{directory: t.TempDir(), serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
			service := newLocalAIService(chat)
			service.endpoint, service.client = server.URL, server.Client()
			service.reload = func() error {
				reloads++
				if scenario == "reload-fails" {
					return errors.New("private error")
				}
				return nil
			}
			name := "test:1"
			if scenario == "unknown" {
				name = "missing:1"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := service.connect(ctx, name)
			if (err == nil) != (scenario == "ready") {
				t.Fatalf("unexpected result %v", err)
			}
			state, readErr := readLocalAIState()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "busy" || scenario == "unknown" || scenario == "cloud" {
				if reloads != 0 || messages != 0 || len(state) != 0 {
					t.Fatal("blocked connection changed state")
				}
			} else if reloads != 1 || state["test:1"] == nil {
				t.Fatal("connection not saved and reloaded")
			}
			if scenario == "ready" || scenario == "empty-reply" {
				if messages != 1 {
					t.Fatal("connection was not verified through OpenCode")
				}
			}
		})
	}
}
