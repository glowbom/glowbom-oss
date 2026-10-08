package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openAIOAuthTestToken(claims string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
}

func TestOpenAIOAuthAccountClaimsMatchOpenCode(t *testing.T) {
	access := openAIOAuthTestToken(`{"https://api.openai.com/auth":{"chatgpt_account_id":"access-account"}}`)
	for _, tc := range []struct{ name, idToken, want string }{
		{"ID token first", openAIOAuthTestToken(`{"https://api.openai.com/auth":{"chatgpt_account_id":"id-account"}}`), "id-account"},
		{"top level first", openAIOAuthTestToken(`{"chatgpt_account_id":"top-account","https://api.openai.com/auth":{"chatgpt_account_id":"nested-account"},"organizations":[{"id":"organization"}]}`), "top-account"},
		{"nested before organizations", openAIOAuthTestToken(`{"https://api.openai.com/auth":{"chatgpt_account_id":"nested-account"},"organizations":[{"id":"organization"}]}`), "nested-account"},
		{"first organization", openAIOAuthTestToken(`{"organizations":[{"id":"first"},{"id":"second"}]}`), "first"},
		{"do not choose another organization", openAIOAuthTestToken(`{"organizations":[{"id":""},{"id":"second"}]}`), "access-account"},
		{"invalid ID token", "not-a-token", "access-account"},
		{"missing ID token", "", "access-account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := openAIAccountIDFromTokens(tc.idToken, access); got != tc.want {
				t.Fatalf("account=%q, want %q", got, tc.want)
			}
		})
	}
	if got := openAIAccountIDFromTokens("opaque-access", openAIOAuthTestToken(`{}`)); got != "" {
		t.Fatal("invented an account for a token without account claims")
	}
}

func TestOpenAIOAuthExchangeCarriesSelectedAccountWithoutLeakingErrors(t *testing.T) {
	access := openAIOAuthTestToken(`{"chatgpt_account_id":"access-account"}`)
	idToken := openAIOAuthTestToken(`{"chatgpt_account_id":"id-account"}`)
	success, _ := json.Marshal(map[string]any{"access_token": access, "id_token": idToken, "refresh_token": "fixture-refresh", "expires_in": 3600})
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"success", 200, string(success)},
		{"rejected", 400, `{"error":"private-token-do-not-return"}`},
		{"malformed", 200, `{"access_token": "private-token-do-not-return", broken}`},
		{"oversized", 200, strings.Repeat("private-token-do-not-return", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := http.DefaultTransport
			calls := 0
			http.DefaultTransport = projectIconTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.URL.String() != openAIOAuthIssuer+"/oauth/token" {
					t.Fatal("unexpected OAuth exchange request")
				}
				return codexImageTestResponse(tc.status, tc.body), nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			credential, err := exchangeOpenAIOAuthCodeForCredential("fixture-code", "fixture-verifier", "http://localhost/callback")
			if calls != 1 {
				t.Fatal("OAuth exchange was replayed")
			}
			if tc.name == "success" {
				if err != nil || credential.AccountID != "id-account" || credential.AccessToken != access || credential.RefreshToken != "fixture-refresh" {
					t.Fatal("OAuth exchange lost the selected account or credential")
				}
			} else if err == nil || strings.Contains(err.Error(), "private-token-do-not-return") {
				t.Fatal("OAuth failure exposed provider diagnostics")
			}
		})
	}
}

func TestOpenAIOAuthAccountSurvivesSyncAndPersistence(t *testing.T) {
	token := openAIOAuthTestToken(`{"chatgpt_account_id":"token-account"}`)
	for _, tc := range []struct{ name, access, selected, want string }{
		{"selected workspace", token, "selected-workspace", "selected-workspace"},
		{"token fallback", token, "", "token-account"},
		{"opaque without account", "opaque-access", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/auth/openai" || json.NewDecoder(r.Body).Decode(&payload) != nil {
					t.Error("invalid live auth sync")
				}
				_, _ = w.Write([]byte(`true`))
			}))
			defer server.Close()
			if err := syncOpenAIAuth(server.URL, tc.access, "fixture-refresh", 1, tc.selected); err != nil {
				t.Fatal(err)
			}
			account, _ := payload["accountId"].(string)
			if account != tc.want || payload["access"] != tc.access {
				t.Fatal("live sync changed the selected account")
			}
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, []byte(`{"openai":{"type":"oauth","access":"old-access","accountId":"old-account"},"xai":{"type":"api","key":"fixture-other-key"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			credential := openCodeOpenAIOAuthCredential{AccessToken: tc.access, RefreshToken: "fixture-refresh", AccountID: tc.selected}
			if err := persistOpenAIOAuthToAuthFile(path, credential); err != nil {
				t.Fatal(err)
			}
			stored, ok, err := readOpenAIOAuthCredentialFromOpenCodeAuthFile(path)
			if err != nil || !ok || stored.AccountID != tc.want || stored.AccessToken != tc.access {
				t.Fatal("saved connection lost the account or kept the previous user's account")
			}
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "fixture-other-key") {
				t.Fatal("saving OAuth removed another provider")
			}
		})
	}
}

func TestOpenAIOAuthImportsKeepExplicitWorkspaceBeforeTokenClaims(t *testing.T) {
	access := openAIOAuthTestToken(`{"chatgpt_account_id":"access-account"}`)
	idToken := openAIOAuthTestToken(`{"chatgpt_account_id":"id-account"}`)
	for _, tc := range []struct {
		name, want string
		codex      bool
		fields     map[string]any
	}{
		{"OpenCode selected", "selected", false, map[string]any{"accountId": "selected"}},
		{"OpenCode alias", "selected", false, map[string]any{"account_id": "selected"}},
		{"OpenCode fallback", "access-account", false, map[string]any{}},
		{"Codex selected", "selected", true, map[string]any{"account_id": "selected", "id_token": idToken}},
		{"Codex ID token", "id-account", true, map[string]any{"id_token": idToken}},
		{"Codex access token", "access-account", true, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "openai"
			tc.fields["type"], tc.fields["access"] = "oauth", access
			read := readOpenAIOAuthCredentialFromOpenCodeAuthFile
			if tc.codex {
				key, tc.fields["access_token"] = "tokens", access
				read = readOpenAIOAuthCredentialFromCodexAuthPath
			}
			data, _ := json.Marshal(map[string]any{key: tc.fields})
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			credential, ok, err := read(path)
			if err != nil || !ok || credential.AccountID != tc.want {
				t.Fatal("import changed the selected account")
			}
		})
	}
}

func TestOpenAIOAuthSyncFailureHidesResponseCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"access":"private-token-do-not-return"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	err := syncOpenAIAuth(server.URL, "fixture-access", "fixture-refresh", 1)
	if err == nil || strings.Contains(err.Error(), "private-token-do-not-return") {
		t.Fatal("auth sync exposed a credential response")
	}
}
