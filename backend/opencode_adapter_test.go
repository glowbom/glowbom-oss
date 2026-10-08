package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
)

func TestV2HealthUsesAuthenticatedInfo(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_PASSWORD", "fixture-password")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, password, ok := r.BasicAuth(); !ok || password != "fixture-password" {
			t.Error("authorization missing")
		}
		if r.URL.Path == "/health" || r.URL.Path == "/global/health" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/api/info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"version":"2.0.21","pid":1,"urls":[],"paths":{"tmp":"/tmp"}}`)
	}))
	defer server.Close()
	if running, err := probeOpenCodeServer(server.URL); !running || err != nil {
		t.Fatalf("health = %t, %v", running, err)
	}
	if openCodeProtocol(server.URL) != "v2" {
		t.Fatal("V2 protocol was not detected")
	}
}

func TestV2HealthRejectsAuthAndUnknownServer(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusOK} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" || r.URL.Path == "/global/health" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(status)
			io.WriteString(w, `{"version":"3.0.0"}`)
		}))
		running, err := probeOpenCodeServer(server.URL)
		server.Close()
		if running || err == nil {
			t.Fatalf("accepted HTTP %d", status)
		}
		if status == http.StatusUnauthorized && !errors.Is(err, errChatServerAuth) {
			t.Fatalf("auth error = %v", err)
		}
	}
}

func TestV2SDKSessionPromptAndAbort(t *testing.T) {
	var operations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operations = append(operations, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/session":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if location, ok := body["location"].(map[string]any); !ok || location["directory"] != "/fixture" {
				t.Errorf("location = %#v", body)
			}
			io.WriteString(w, `{"data":{"id":"ses_fixture","projectID":"p","time":{"created":1,"updated":1},"location":{"directory":"/fixture"}}}`)
		case "/api/session/ses_fixture/model":
			var body struct {
				Model struct{ ID, ProviderID, Variant string }
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Model.ID != "fixture-model" || body.Model.ProviderID != "fixture-provider" {
				t.Errorf("model = %#v", body)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/session/ses_fixture/prompt":
			var body struct {
				Text  string
				Files []map[string]any
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Text != "hello" {
				t.Errorf("prompt = %#v", body)
			}
			io.WriteString(w, `{"data":{"id":"msg_user","time":{"created":2}}}`)
		case "/api/experimental/session/ses_fixture/wait":
			w.WriteHeader(http.StatusNoContent)
		case "/api/session/ses_fixture/message":
			io.WriteString(w, `{"data":[{"id":"msg_old","type":"assistant","time":{"created":1},"content":[{"type":"text","text":"stale"}]},{"id":"msg_new","type":"assistant","time":{"created":3},"content":[{"type":"text","text":"answer"}]}],"cursor":{}}`)
		case "/api/session/ses_fixture/interrupt":
			io.WriteString(w, `{"interrupted":true}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	driver := NewOpenCodeDriver(server.URL)
	session, err := driver.client.Session.New(context.Background(), opencode.SessionNewParams{Directory: opencode.F("/fixture")})
	if err != nil || session.ID != "ses_fixture" {
		t.Fatalf("session = %#v, %v", session, err)
	}
	response, err := driver.sendSessionPrompt(context.Background(), session.ID, opencode.SessionPromptParams{Model: opencode.F(opencode.SessionPromptParamsModel{ProviderID: opencode.F("fixture-provider"), ModelID: opencode.F("fixture-model")}), Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{opencode.TextPartInputParam{Type: opencode.F(opencode.TextPartInputTypeText), Text: opencode.F("hello")}})})
	if err != nil || response == nil {
		t.Fatalf("prompt = %#v, %v", response, err)
	}
	if len(response.Parts) != 1 || response.Parts[0].Text != "answer" {
		t.Fatalf("parts = %#v", response.Parts)
	}
	if _, err := driver.client.Session.Abort(context.Background(), session.ID, opencode.SessionAbortParams{}); err != nil {
		t.Fatal(err)
	}
	if len(operations) != 6 {
		t.Fatalf("operations = %v", operations)
	}
}

func TestV2DenyRulesAreTranslatedAndVerified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permissions []struct{ Action, Resource, Effect string }
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Permissions) != 1 || body.Permissions[0].Action != "*" || body.Permissions[0].Resource != "*" || body.Permissions[0].Effect != "deny" {
			t.Errorf("permissions = %#v", body)
		}
		io.WriteString(w, `{"data":{"id":"ses_fixture","permissions":[{"action":"*","resource":"*","effect":"deny"}]}}`)
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	service := &chatService{serverURL: server.URL, client: openCodeHTTPClient(&http.Client{})}
	var session struct {
		ID         string
		Permission []struct{ Permission, Pattern, Action string }
	}
	err := service.json(context.Background(), "POST", "/session", map[string]any{"permission": []map[string]string{{"permission": "*", "pattern": "*", "action": "deny"}}}, &session)
	if err != nil || len(session.Permission) != 1 || session.Permission[0].Action != "deny" {
		t.Fatalf("session = %#v, %v", session, err)
	}
}

func TestV2EventsKeepSessionAndPartIdentity(t *testing.T) {
	stream := "data: {\"type\":\"session.step.started\",\"data\":{\"sessionID\":\"ses_a\",\"assistantMessageID\":\"msg_a\"}}\n\n" +
		"data: {\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"ses_a\",\"assistantMessageID\":\"msg_a\",\"ordinal\":0,\"delta\":\"hel\"}}\n\n" +
		"data: {\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"ses_b\",\"assistantMessageID\":\"msg_b\",\"ordinal\":0,\"delta\":\"other\"}}\n\n" +
		"data: {\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"ses_a\",\"assistantMessageID\":\"msg_a\",\"ordinal\":0,\"text\":\"hello\"}}\n\n" +
		"data: {\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses_a\"}}\n\n"
	body := translateOpenCodeEvents(io.NopCloser(strings.NewReader(stream)))
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"text":"hel"`) || !strings.Contains(string(data), `"text":"hello"`) || strings.Contains(string(data), "helother") || !strings.Contains(string(data), `"type":"session.idle"`) {
		t.Fatalf("stream = %s", data)
	}
}

func TestV2ProviderCatalogKeepsModelsAndVariants(t *testing.T) {
	t.Setenv("GLOWBOM_CURSOR_BIN", "/nonexistent/glowbom-cursor-fixture")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("location[directory]") != "/fixture" {
			t.Errorf("location query = %s", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/api/provider":
			io.WriteString(w, `{"data":[{"id":"test","name":"Test","activation":"auto"}]}`)
		case "/api/model":
			io.WriteString(w, `{"data":[{"id":"model","modelID":"upstream-model","providerID":"test","name":"Test model","enabled":true,"capabilities":{"tools":true,"input":["text","image"],"output":["text"]},"variants":[{"id":"high"}],"limit":{"context":128000,"output":8000}}]}`)
		default:
			t.Errorf("path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	service := &chatService{directory: "/fixture", serverURL: server.URL, client: openCodeHTTPClient(&http.Client{})}
	models, err := service.models(context.Background())
	found := false
	for _, model := range models {
		if model.ID == "test/model" && model.Images {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatalf("V2 model missing, error %v", err)
	}
	driver := NewOpenCodeDriver(server.URL)
	providers, err := driver.client.App.Providers(context.Background(), opencode.AppProvidersParams{Directory: opencode.F("/fixture")})
	if err != nil || len(providers.Providers) != 1 || providers.Providers[0].Models["model"].Name != "Test model" {
		t.Fatalf("providers = %#v, %v", providers, err)
	}
}

func TestV2PermissionReplyAndFormReply(t *testing.T) {
	var replied, answered bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_fixture/permission/request/reply":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 1 || body["decision"] != "reject" {
				t.Errorf("permission body = %#v", body)
			}
			replied = true
			w.WriteHeader(http.StatusNoContent)
		case "/api/session/ses_fixture/form/frm_fixture":
			io.WriteString(w, `{"data":{"fields":[{"key":"color","type":"string"},{"key":"features","type":"multiselect"}]}}`)
		case "/api/session/ses_fixture/form/frm_fixture/reply":
			var body struct{ Answer map[string]any }
			json.NewDecoder(r.Body).Decode(&body)
			if body.Answer["color"] != "blue" || len(body.Answer["features"].([]any)) != 2 {
				t.Errorf("form body = %#v", body)
			}
			answered = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	driver := NewOpenCodeDriver(server.URL)
	if err := driver.respondToPermission(context.Background(), "ses_fixture", "request", "reject", ""); err != nil {
		t.Fatal(err)
	}
	if err := driver.respondToQuestion(context.Background(), "ses_fixture", "frm_fixture", "", "", [][]string{{"blue"}, {"one", "two"}}, nil); err != nil {
		t.Fatal(err)
	}
	if !replied || !answered {
		t.Fatalf("permission = %t, form = %t", replied, answered)
	}
}

func TestV2VersionPreferenceRequiresAuth(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GLOWBOM_OPENCODE_VERSION", "")
	t.Setenv("OPENCODE_URL", "http://configured.example")
	for _, tc := range []struct {
		token, body string
		status      int
	}{
		{"", `{"version":"v2"}`, http.StatusUnauthorized},
		{"fixture-token", `{"version":"v3"}`, http.StatusBadRequest},
		{"fixture-token", `{"version":"v2"}`, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader(tc.body))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		agentSetupHandler()(w, req)
		if w.Code != tc.status {
			t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
		}
	}
}

func TestV1TransportPreservesRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/session" || r.URL.Query().Get("directory") != "/fixture" {
			t.Errorf("V1 request changed: %s", r.URL.String())
		}
		io.WriteString(w, `{"id":"ses_v1"}`)
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v1")
	driver := NewOpenCodeDriver(server.URL)
	value, err := driver.client.Session.New(context.Background(), opencode.SessionNewParams{Directory: opencode.F("/fixture")})
	if err != nil || value.ID != "ses_v1" {
		t.Fatalf("session = %#v, %v", value, err)
	}
}

func TestV2ChatStreamsWithToolsDenied(t *testing.T) {
	events := make(chan string, 8)
	var interrupted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			var body struct{ Permissions []map[string]string }
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Permissions) != 1 || body.Permissions[0]["effect"] != "deny" {
				t.Error("chat did not deny tools")
			}
			writeJSON(w, map[string]any{"data": map[string]any{"id": "ses_chat", "permissions": body.Permissions}})
		case "/api/event":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"server.connected\",\"data\":{}}\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case data := <-events:
					io.WriteString(w, "data: "+data+"\n\n")
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case "/api/experimental/session/ses_chat/instructions/entries/glowbom":
			var body struct{ Value string }
			json.NewDecoder(r.Body).Decode(&body)
			if body.Value != "Glowbom chat guidance" {
				t.Errorf("instructions = %#v", body)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/session/ses_chat/model":
			w.WriteHeader(http.StatusNoContent)
		case "/api/session/ses_chat/prompt":
			var body struct{ Text string }
			json.NewDecoder(r.Body).Decode(&body)
			if body.Text != "hello" {
				t.Errorf("prompt = %q", body.Text)
			}
			events <- `{"type":"session.step.started","data":{"sessionID":"ses_chat","assistantMessageID":"msg_chat"}}`
			events <- `{"type":"session.text.delta","data":{"sessionID":"ses_chat","assistantMessageID":"msg_chat","ordinal":0,"delta":"Hello"}}`
			events <- `{"type":"session.text.ended","data":{"sessionID":"ses_chat","assistantMessageID":"msg_chat","ordinal":0,"text":"Hello world"}}`
			writeJSON(w, map[string]any{"data": map[string]any{"id": "msg_user", "time": map[string]int{"created": 1}}})
		case "/api/experimental/session/ses_chat/wait":
			time.Sleep(20 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
		case "/api/session/ses_chat/message":
			io.WriteString(w, `{"data":[{"id":"msg_chat","type":"assistant","time":{"created":2},"content":[{"type":"text","text":"Hello world"}]}],"cursor":{}}`)
		case "/api/session/ses_chat/interrupt":
			interrupted = true
			writeJSON(w, map[string]bool{"interrupted": false})
		case "/api/session/ses_chat":
			if r.Method != http.MethodDelete {
				t.Error("unexpected session operation")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected route = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	service := &chatService{serverURL: server.URL, client: openCodeHTTPClient(server.Client())}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var streamed []string
	text, err := service.complete(ctx, "test/model", "Glowbom chat guidance", []map[string]any{{"type": "text", "text": "hello"}}, func(text string) { streamed = append(streamed, text) })
	if err != nil || text != "Hello world" || len(streamed) == 0 || !interrupted {
		t.Fatalf("chat = %q, streams %v, interrupted %t, error %v", text, streamed, interrupted, err)
	}
}

func TestV2AcceptedPromptIsNotRetriedWhenWaitFails(t *testing.T) {
	prompts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			prompts++
			io.WriteString(w, `{"data":{"id":"msg_fixture","time":{"created":1}}}`)
			return
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	driver := NewOpenCodeDriver(server.URL)
	_, err := driver.sendSessionPrompt(context.Background(), "ses_fixture", opencode.SessionPromptParams{Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{opencode.TextPartInputParam{Type: opencode.F(opencode.TextPartInputTypeText), Text: opencode.F("hello")}})})
	if err == nil || prompts != 1 {
		t.Fatalf("prompt submissions = %d, error %v", prompts, err)
	}
}

func TestV2OAuthManagesOnlyGlowbomCredential(t *testing.T) {
	var created, removed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/provider":
			io.WriteString(w, `{"data":[{"id":"openai","integrationID":"openai"}]}`)
		case "/api/integration":
			io.WriteString(w, `{"data":[{"id":"openai","methods":[{"id":"chatgpt-token-sharing","type":"oauth"},{"id":"chatgpt-browser","type":"oauth"}]}]}`)
		case "/api/credential/cred_glowbom_openai":
			if r.Method != http.MethodDelete {
				t.Error("unexpected credential operation")
			}
			removed = true
			w.WriteHeader(http.StatusNoContent)
		case "/api/credential":
			var body struct {
				ID, IntegrationID string
				Activate          bool
				Value             struct {
					Type, MethodID, Access, Refresh, AccountID string
					Expires                                    int64
				}
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.ID != "cred_glowbom_openai" || body.IntegrationID != "openai" || !body.Activate || body.Value.MethodID != "chatgpt-browser" || body.Value.Access != "fixture-access" || body.Value.Refresh != "fixture-refresh" || body.Value.Expires <= 0 || body.Value.AccountID != "selected-workspace" {
				t.Errorf("credential conversion failed")
			}
			created = true
			io.WriteString(w, `{"data":{"value":{"access":"response-secret-must-not-be-returned"}}}`)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	if err := syncOpenAIAuth(server.URL, "fixture-access", "fixture-refresh", 1, "selected-workspace"); err != nil {
		t.Fatal(err)
	}
	if err := clearOpenAIAuth(server.URL); err != nil {
		t.Fatal(err)
	}
	if !created || !removed {
		t.Fatalf("created = %t, removed = %t", created, removed)
	}
}
