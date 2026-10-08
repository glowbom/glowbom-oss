package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestChatOAuthPersistsForNextOpenCodeStart(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	authFile := filepath.Join(dataHome, "opencode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, []byte(`{"xai":{"type":"api","key":"other-provider"},"openai":{"type":"api","key":"old-openai"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	credential := openCodeOpenAIOAuthCredential{AccessToken: "test-access", RefreshToken: "test-refresh", AccountID: "selected-workspace", ExpiresAtReferenceSeconds: 2000000000}
	if err := persistChatOpenAIOAuth(credential); err != nil {
		t.Fatal(err)
	}
	paths, err := userOpenCodeRuntimePaths()
	if err != nil || paths.AuthFile != authFile {
		t.Fatal("normal startup must use the saved credential store")
	}
	if got := openAICredentialTypeFromAuthFile(paths.AuthFile); got != "oauth" {
		t.Fatal("OAuth connection was not available after restarting", got)
	}
	data, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]map[string]any
	if json.Unmarshal(data, &stored) != nil || stored["xai"]["key"] != "other-provider" || stored["openai"]["access"] != "test-access" || stored["openai"]["refresh"] != "test-refresh" || stored["openai"]["accountId"] != "selected-workspace" {
		t.Fatal("credential update did not preserve both connections")
	}
	info, err := os.Stat(authFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file should remain private")
	}
}

func TestChatOAuthRefusesExternalRuntime(t *testing.T) {
	t.Setenv("OPENCODE_URL", "http://external.invalid")
	r := httptest.NewRequest("POST", "/opencode/auth/openai/oauth/start", strings.NewReader(`{"useOpenCodeConfig":true}`))
	w := httptest.NewRecorder()
	openCodeOpenAIOAuthStartHandler(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "external OpenCode") {
		t.Fatal("external runtime should have explicit connection instructions", w.Code, w.Body.String())
	}
	if err := connectChatOpenAIOAuth("", openCodeOpenAIOAuthCredential{}); err == nil {
		t.Fatal("external runtime must not connect local credentials")
	}
}

func TestProviderRefreshChecksAuthenticationAndIdleState(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "local-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	for _, test := range []struct {
		name, token, status string
		want                int
	}{
		{name: "unauthenticated", status: `{}`, want: 401},
		{name: "busy", token: "local-token", status: `{"active":{"type":"busy"}}`, want: 409},
		{name: "idle", token: "local-token", status: `{}`, want: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			disposed := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.token == "" {
					t.Error("unauthenticated refresh reached OpenCode")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /session/status":
					_, _ = w.Write([]byte(test.status))
				case "POST /instance/dispose":
					disposed = true
					_, _ = w.Write([]byte(`true`))
				default:
					t.Error("unexpected refresh request", r.URL.Path)
				}
			}))
			defer server.Close()
			chat := &chatService{directory: t.TempDir(), serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
			r := httptest.NewRequest("POST", "/settings/providers/refresh", nil)
			r.Header.Set("Authorization", "Bearer "+test.token)
			w := httptest.NewRecorder()
			providerRefreshHandler(chat)(w, r)
			if w.Code != test.want || disposed != (test.want == 200) {
				t.Fatal("unexpected refresh", w.Code, disposed)
			}
		})
	}
}

func TestProviderConnectionSavesThroughOpenCode(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "local-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	t.Setenv("OPENCODE_URL", "")
	for _, scenario := range []struct {
		provider     string
		refreshFails bool
	}{
		{"openrouter", false}, {"openrouter", true}, {"opencode-go", false},
	} {
		refreshFails := scenario.refreshFails
		t.Run(scenario.provider+"/"+map[bool]string{false: "refreshed", true: "saved but refresh failed"}[refreshFails], func(t *testing.T) {
			calls := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				switch r.Method + " " + r.URL.Path {
				case "GET /session/status":
					_, _ = w.Write([]byte(`{}`))
				case "PUT /auth/" + scenario.provider:
					var value map[string]string
					if json.NewDecoder(r.Body).Decode(&value) != nil || value["type"] != "api" || value["key"] != "test-key" {
						t.Error("incorrect credential payload")
					}
					_, _ = w.Write([]byte(`true`))
				case "POST /instance/dispose":
					if refreshFails {
						http.Error(w, "test-key", 500)
					} else {
						_, _ = w.Write([]byte(`true`))
					}
				default:
					t.Error("unexpected request", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			chat := &chatService{directory: t.TempDir(), serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
			r := httptest.NewRequest("POST", "/settings/providers/connect", strings.NewReader(`{"provider":"`+scenario.provider+`","apiKey":" test-key "}`))
			r.Header.Set("Authorization", "Bearer local-token")
			w := httptest.NewRecorder()
			providerConnectionHandler(chat)(w, r)
			var result struct {
				Saved     bool `json:"saved"`
				Refreshed bool `json:"refreshed"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Saved || result.Refreshed == refreshFails || strings.Contains(w.Body.String(), "test-key") {
				t.Fatal("unexpected connection response", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(calls, []string{"GET /session/status", "PUT /auth/" + scenario.provider, "POST /instance/dispose"}) {
				t.Fatal("incorrect request order", calls)
			}
		})
	}
}

func TestProviderConnectionRejectsInvalidRequestsBeforePreparation(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "local-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	t.Setenv("OPENCODE_URL", "")
	chat := &chatService{prepare: func() error { t.Fatal("invalid request prepared OpenCode"); return nil }}
	for _, test := range []struct {
		name, method, body, token, endpoint string
		want                                int
	}{
		{name: "no token", method: "POST", body: `{"provider":"openai","apiKey":"test-key"}`, want: 401},
		{name: "wrong token", method: "POST", body: `{}`, token: "wrong-token", want: 401},
		{name: "wrong method", method: "GET", token: "local-token", want: 405},
		{name: "unknown provider", method: "POST", body: `{"provider":"../auth/openai","apiKey":"test-key"}`, token: "local-token", want: 400},
		{name: "missing key", method: "POST", body: `{"provider":"openai"}`, token: "local-token", want: 400},
		{name: "multiline key", method: "POST", body: `{"provider":"openai","apiKey":"a\nb"}`, token: "local-token", want: 400},
		{name: "oversized body", method: "POST", body: `{"provider":"openai","apiKey":"` + strings.Repeat("a", 17000) + `"}`, token: "local-token", want: 400},
		{name: "external server", method: "POST", body: `{"provider":"openai","apiKey":"test-key"}`, token: "local-token", endpoint: "http://external.invalid", want: 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("OPENCODE_URL", test.endpoint)
			r := httptest.NewRequest(test.method, "/settings/providers/connect", strings.NewReader(test.body))
			r.Header.Set("Authorization", "Bearer "+test.token)
			w := httptest.NewRecorder()
			providerConnectionHandler(chat)(w, r)
			if w.Code != test.want || strings.Contains(w.Body.String(), "test-key") {
				t.Fatal("unexpected rejection", w.Code, w.Body.String())
			}
		})
	}
}

func TestProviderConnectionDoesNotInterruptChatOrExposeProviderErrors(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "local-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	t.Setenv("OPENCODE_URL", "")
	for _, busy := range []bool{true, false} {
		t.Run(map[bool]string{true: "busy chat", false: "upstream error"}[busy], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/session/status" {
					if busy {
						_, _ = w.Write([]byte(`{"session":{"type":"busy"}}`))
					} else {
						_, _ = w.Write([]byte(`{}`))
					}
					return
				}
				if busy || r.URL.Path != "/auth/openai" {
					t.Error("unexpected mutation", r.URL.Path)
				}
				http.Error(w, `{"error":"rejected test-key"}`, 400)
			}))
			defer server.Close()
			chat := &chatService{directory: t.TempDir(), serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
			r := httptest.NewRequest("POST", "/settings/providers/connect", strings.NewReader(`{"provider":"openai","apiKey":"test-key"}`))
			r.Header.Set("Authorization", "Bearer local-token")
			w := httptest.NewRecorder()
			providerConnectionHandler(chat)(w, r)
			want := 502
			if busy {
				want = 409
			}
			if w.Code != want || strings.Contains(w.Body.String(), "test-key") {
				t.Fatal("unexpected failure", w.Code, w.Body.String())
			}
		})
	}
}
