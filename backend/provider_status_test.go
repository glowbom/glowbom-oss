package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type providerStatusTransport func(*http.Request) (*http.Response, error)

func (transport providerStatusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func providerStatusFixture(t *testing.T, auth, catalog string, code int) (*chatService, *int) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("OPENCODE_URL", "")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	state := getOpenCodeOpenAIAuthState()
	t.Cleanup(func() {
		openCodeOpenAIAuthStateCache.mu.Lock()
		openCodeOpenAIAuthStateCache.state = state
		openCodeOpenAIAuthStateCache.mu.Unlock()
	})
	setOpenCodeOpenAIAuthState("opencode-config", "")
	if auth != "" {
		paths, err := userOpenCodeRuntimePaths()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(paths.AuthFile), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.AuthFile, []byte(auth), 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	chat := &chatService{
		directory: "fixture-chat", serverURL: "http://127.0.0.1:1",
		prepare: func() error { t.Fatal("status check must not prepare OpenCode"); return nil },
		client: &http.Client{Transport: providerStatusTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != http.MethodGet || r.URL.Path != "/provider" || r.URL.Query().Get("directory") != "fixture-chat" {
				t.Fatal("status check made an unexpected request", r.Method, r.URL.Path)
			}
			return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(catalog))}, nil
		})},
	}
	return chat, &calls
}

const providerStatusConnectedCatalog = `{"connected":["openai"],"all":[{"id":"openai","models":{"gpt-test":{"status":"active"}}}]}`

func TestProviderStatusDistinguishesUserCredentialsAndModels(t *testing.T) {
	for _, test := range []struct {
		name, auth, catalog, credential string
		connected, models               bool
	}{
		{name: "user OAuth", auth: `{"openai":{"type":"oauth","access":"private-token"}}`, catalog: providerStatusConnectedCatalog, credential: "oauth", connected: true, models: true},
		{name: "user API key", auth: `{"openai":{"type":"api","key":"private-key"}}`, catalog: providerStatusConnectedCatalog, credential: "api", connected: true, models: true},
		{name: "configured provider without saved auth", catalog: providerStatusConnectedCatalog, credential: "none", connected: true, models: true},
		{name: "missing", catalog: `{"connected":[],"all":[]}`, credential: "none"},
		{name: "model catalog alone is not a connection", catalog: `{"connected":[],"all":[{"id":"openai","models":{"gpt-test":{}}}]}`, credential: "none"},
		{name: "saved OAuth is independent of runtime", auth: `{"openai":{"type":"oauth","access":"private-token"}}`, catalog: `{"connected":[],"all":[]}`, credential: "oauth"},
		{name: "deprecated models unavailable", auth: `{"openai":{"type":"api","key":"private-key"}}`, catalog: `{"connected":["openai"],"all":[{"id":"openai","models":{"old":{"status":"deprecated"}}}]}`, credential: "api", connected: true},
		{name: "other provider models do not count", catalog: `{"connected":["openai","xai"],"all":[{"id":"xai","models":{"some-model":{}}}]}`, credential: "none", connected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat, calls := providerStatusFixture(t, test.auth, test.catalog, http.StatusOK)
			status := readProviderConnectionStatus(t.Context(), chat)
			if status.OpenAI.CredentialType != test.credential || status.OpenAI.Connected != test.connected || status.OpenAI.ModelsAvailable != test.models || status.OpenAI.Source != "opencode" || !status.ModelsChecked || *calls != 1 {
				t.Fatalf("unexpected redacted status: %+v, calls %d", status, *calls)
			}
			encoded, err := json.Marshal(status)
			if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "auth.json") {
				t.Fatal("status must not expose credentials or local paths")
			}
		})
	}
}

func TestProviderStatusDoesNotMixLegacyRuntimeWithUserCredentials(t *testing.T) {
	for _, userAuth := range []string{"", `{"openai":{"type":"oauth","access":"user-token"}}`} {
		t.Run(map[bool]string{true: "legacy only", false: "separate user connection"}[userAuth == ""], func(t *testing.T) {
			chat, calls := providerStatusFixture(t, userAuth, providerStatusConnectedCatalog, http.StatusOK)
			t.Setenv("HOME", t.TempDir())
			legacyPaths, err := glowbomOpenCodeRuntimePaths()
			if err != nil {
				t.Fatal(err)
			}
			if err := persistOpenAIOAuthToAuthFile(legacyPaths.AuthFile, openCodeOpenAIOAuthCredential{AccessToken: "legacy-token"}); err != nil {
				t.Fatal(err)
			}
			setOpenCodeOpenAIAuthState("codex-jwt", "")
			status := readProviderConnectionStatus(t.Context(), chat)
			want := "none"
			if userAuth != "" {
				want = "oauth"
			}
			if status.OpenAI.CredentialType != want || status.OpenAI.Connected || status.OpenAI.ModelsAvailable || status.ModelsChecked || *calls != 0 {
				t.Fatalf("legacy runtime was mistaken for user configuration: %+v, calls %d", status, *calls)
			}
		})
	}
}

func TestProviderStatusExternalDoesNotInferLocalAccount(t *testing.T) {
	chat, calls := providerStatusFixture(t, `{"openai":{"type":"oauth","access":"local-secret"}}`, providerStatusConnectedCatalog, http.StatusOK)
	t.Setenv("OPENCODE_URL", "http://external.invalid")
	setOpenCodeOpenAIAuthState("codex-jwt", "")
	status := readProviderConnectionStatus(t.Context(), chat)
	if status.OpenAI.Source != "external" || status.OpenAI.CredentialType != "unknown" || !status.OpenAI.Connected || !status.OpenAI.ModelsAvailable || !status.ModelsChecked || *calls != 1 {
		t.Fatalf("external status inferred a local account or ignored its own catalog: %+v", status)
	}
}

func TestProviderStatusFailedCatalogRemainsUnknown(t *testing.T) {
	for _, test := range []struct {
		name, catalog string
		code          int
	}{
		{name: "unavailable", catalog: `{"error":"private-provider-token"}`, code: http.StatusServiceUnavailable},
		{name: "invalid JSON", catalog: `not JSON`, code: http.StatusOK},
		{name: "null catalog", catalog: `null`, code: http.StatusOK},
		{name: "missing catalog fields", catalog: `{}`, code: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat, calls := providerStatusFixture(t, `{"openai":{"type":"oauth","access":"private-token"}}`, test.catalog, test.code)
			status := readProviderConnectionStatus(t.Context(), chat)
			if status.OpenAI.CredentialType != "oauth" || status.OpenAI.Connected || status.OpenAI.ModelsAvailable || status.ModelsChecked || *calls != 1 {
				t.Fatalf("failed check was mistaken for a missing or confirmed connection: %+v", status)
			}
		})
	}
}

func TestProviderStatusRequiresAuthenticationAndReadOnlyMethod(t *testing.T) {
	chat, calls := providerStatusFixture(t, `{"openai":{"type":"api","key":"private-key"}}`, providerStatusConnectedCatalog, http.StatusOK)
	t.Setenv("GLOWBOM_SERVER_TOKEN", "local-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	for _, test := range []struct {
		method, token string
		want          int
	}{
		{method: "GET", want: http.StatusUnauthorized},
		{method: "POST", token: "local-token", want: http.StatusMethodNotAllowed},
		{method: "GET", token: "local-token", want: http.StatusOK},
	} {
		before := *calls
		r := httptest.NewRequest(test.method, "/settings/providers/status", nil)
		r.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		providerStatusHandler(chat)(w, r)
		if w.Code != test.want || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "private-key") {
			t.Fatal("unexpected status response", w.Code)
		}
		if w.Code != http.StatusOK && *calls != before {
			t.Fatal("rejected request inspected OpenCode")
		}
	}
}
