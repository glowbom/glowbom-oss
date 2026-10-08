package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func codexImageTestJWT(account string, expires int64) string {
	body, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}, "exp": expires})
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func codexImageTestOpenCode(t *testing.T, account string, expires int64, refresh string) string {
	t.Helper()
	credential, err := json.Marshal(map[string]any{"openai": map[string]any{
		"type": "oauth", "access": codexImageTestJWT(account, expires), "refresh": refresh, "expires": expires * 1000,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(credential)
}

func codexImageTestFallback(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	original := codexImageCodexAuthFilePath
	codexImageCodexAuthFilePath = func() string { return path }
	t.Cleanup(func() { codexImageCodexAuthFilePath = original })
	return path
}

func codexImageTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestCodexImageSubscriptionCredentialPrecedence(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	for _, tc := range []struct {
		name      string
		opencode  string
		fallback  string
		account   string
		available bool
	}{
		{
			name: "current OpenCode OAuth precedes Codex", opencode: codexImageTestOpenCode(t, "opencode-account", future, "opencode-refresh"),
			fallback: `{"tokens":{"access_token":"codex-access","account_id":"codex-account"}}`, account: "opencode-account", available: true,
		},
		{
			name: "current Codex avoids expired OpenCode refresh", opencode: codexImageTestOpenCode(t, "opencode-account", 1, "opencode-refresh"),
			fallback: `{"tokens":{"access_token":"codex-access","account_id":"codex-account"}}`, account: "codex-account", available: true,
		},
		{
			name: "platform API key is not a subscription", opencode: `{"openai":{"type":"api","key":"secret-platform-key"}}`,
			fallback: `{}`, available: false,
		},
		{
			name: "expired token without refresh unavailable", opencode: codexImageTestOpenCode(t, "expired-account", 1, ""),
			fallback: `{}`, available: false,
		},
		{
			name: "renewable expired token available without network", opencode: codexImageTestOpenCode(t, "expired-account", 1, "refresh"),
			fallback: `{}`, account: "expired-account", available: true,
		},
		{
			name: "missing account unavailable without refresh", opencode: `{"openai":{"type":"oauth","access":"opaque-access"}}`,
			fallback: `{}`, available: false,
		},
		{
			name: "OpenCode accountId is preserved", opencode: `{"openai":{"type":"oauth","access":"opaque-access","accountId":"stored-account"}}`,
			fallback: `{}`, account: "stored-account", available: true,
		},
		{
			name: "OpenCode account_id is preserved", opencode: `{"openai":{"type":"oauth","access":"opaque-access","account_id":"stored-account"}}`,
			fallback: `{}`, account: "stored-account", available: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateProjectIconCredentials(t, tc.opencode)
			codexImageTestFallback(t, tc.fallback)
			mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
				t.Fatal("discovery performed a network request")
				return nil, nil
			})
			credential, ok := codexImageSubscription()
			if ok != tc.available || credential.AccountID != tc.account {
				t.Fatalf("availability=%v account=%q, want %v %q", ok, credential.AccountID, tc.available, tc.account)
			}
		})
	}
}

func TestCodexImageCredentialJWTExpiryAndAccount(t *testing.T) {
	token := codexImageTestJWT("jwt-account", 1)
	contents, _ := json.Marshal(map[string]any{"openai": map[string]any{
		"type": "oauth", "access": token, "account_id": "old-account", "expires": time.Now().Add(time.Hour).UnixMilli(),
	}})
	isolateProjectIconCredentials(t, string(contents))
	codexImageTestFallback(t, `{}`)
	credential, ok := readCodexImageCredential(projectIconAuthFileCandidates()[0], false)
	if !ok || credential.AccountID != "jwt-account" || credential.Expires != 1000 {
		t.Fatalf("JWT claims were not used: account=%q expiry=%d", credential.AccountID, credential.Expires)
	}
	if _, ok := codexImageSubscription(); ok {
		t.Fatal("stale file expiry hid an expired token")
	}
}

func TestCodexImageCodexSelectedAccountAndIDTokenFallback(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	for _, tc := range []struct {
		name    string
		tokens  map[string]any
		account string
	}{
		{"preserve selected Codex workspace", map[string]any{"access_token": codexImageTestJWT("token-account", future), "account_id": "selected-account"}, "selected-account"},
		{"fall back to ID token account", map[string]any{"access_token": "opaque-access", "id_token": codexImageTestJWT("id-token-account", future)}, "id-token-account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			body, _ := json.Marshal(map[string]any{"tokens": tc.tokens})
			codexImageTestFallback(t, string(body))
			credential, ok := codexImageSubscription()
			if !ok || credential.AccountID != tc.account {
				t.Fatalf("selected account was not preserved: %q", credential.AccountID)
			}
		})
	}
}

func TestCodexImageRefreshPreservesOpenCodeRecords(t *testing.T) {
	oldToken := codexImageTestJWT("account", 1)
	newToken := codexImageTestJWT("account", time.Now().Add(time.Hour).Unix())
	contents, _ := json.Marshal(map[string]any{
		"openai": map[string]any{"type": "oauth", "access": oldToken, "refresh": "old-refresh", "account_id": "account", "accountId": "account", "custom": "keep-me", "expires": 1000},
		"xai":    map[string]any{"type": "oauth", "access": "unrelated-xai-access", "refresh": "unrelated-xai-refresh"},
	})
	isolateProjectIconCredentials(t, string(contents))
	codexImageTestFallback(t, `{}`)
	count := 0
	mockProjectIconProvider(t, func(req *http.Request) (*http.Response, error) {
		count++
		if req.URL.String() != "https://auth.openai.com/oauth/token" || req.Method != http.MethodPost {
			t.Fatalf("wrong refresh route: %s", req.URL)
		}
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if req.Form.Get("grant_type") != "refresh_token" || req.Form.Get("refresh_token") != "old-refresh" || req.Form.Get("client_id") != openAIOAuthClientID() {
			t.Fatal("incorrect refresh fields")
		}
		body, _ := json.Marshal(map[string]any{"access_token": newToken, "refresh_token": "rotated-refresh", "expires_in": 3600})
		return codexImageTestResponse(200, string(body)), nil
	})
	credential, err := resolveCodexImageSubscription(context.Background())
	if err != nil || credential.Bearer != newToken || credential.Refresh != "rotated-refresh" || credential.AccountID != "account" || count != 1 {
		t.Fatalf("refresh failed: count=%d err=%v", count, err)
	}
	data, err := os.ReadFile(credential.AuthFile)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["xai"]["access"] != "unrelated-xai-access" || stored["xai"]["refresh"] != "unrelated-xai-refresh" || stored["openai"]["custom"] != "keep-me" || stored["openai"]["accountId"] != "account" {
		t.Fatal("refresh changed unrelated credentials or account metadata")
	}
	info, err := os.Stat(credential.AuthFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("refreshed credentials are not private")
	}
	if _, err := resolveCodexImageSubscription(context.Background()); err != nil || count != 1 {
		t.Fatal("current credentials refreshed a second time")
	}
}

func TestCodexImageRefreshPreservesCodexMetadata(t *testing.T) {
	isolateProjectIconCredentials(t)
	path := codexImageTestFallback(t, `{"tokens":{"access_token":"old-access","refresh_token":"old-refresh","account_id":"account","id_token":"old-id-token","expires_at":1000,"custom":"keep"},"last_refresh":"old","custom_top":"keep-top","OPENAI_API_KEY":null}`)
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{"access_token": codexImageTestJWT("different-token-account", time.Now().Add(time.Hour).Unix()), "refresh_token": "new-refresh", "expires_in": 3600})
		return codexImageTestResponse(200, string(body)), nil
	})
	credential, err := resolveCodexImageSubscription(context.Background())
	if err != nil || credential.AccountID != "account" || credential.AuthFile != path {
		t.Fatalf("Codex refresh failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(data, &auth); err != nil {
		t.Fatal(err)
	}
	var tokens map[string]any
	if err := json.Unmarshal(auth["tokens"], &tokens); err != nil {
		t.Fatal(err)
	}
	if tokens["account_id"] != "account" || tokens["id_token"] != "old-id-token" || tokens["custom"] != "keep" || string(auth["custom_top"]) != `"keep-top"` || string(auth["OPENAI_API_KEY"]) != "null" || string(auth["last_refresh"]) == `"old"` {
		t.Fatal("Codex refresh lost unrelated metadata")
	}
}

func TestCodexImageRefreshNeverRestoresChangedCredentials(t *testing.T) {
	for _, replacement := range []string{
		`{}`,
		`{"openai":{"type":"api","key":"new-api-key"}}`,
		`{"openai":{"type":"oauth","access":"old-access","refresh":"old-refresh","account_id":"new-account","expires":1000}}`,
	} {
		t.Run(replacement, func(t *testing.T) {
			isolateProjectIconCredentials(t, `{"openai":{"type":"oauth","access":"old-access","refresh":"old-refresh","account_id":"account","expires":1000}}`)
			codexImageTestFallback(t, `{}`)
			path := projectIconAuthFileCandidates()[0]
			mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
				if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
					t.Fatal(err)
				}
				return codexImageTestResponse(200, `{"access_token":"refreshed-access","refresh_token":"rotated-refresh","expires_in":3600}`), nil
			})
			_, err := resolveCodexImageSubscription(context.Background())
			var failure *codexImageFailure
			if !errors.As(err, &failure) || failure.Code != "save" {
				t.Fatalf("changed credentials were restored: %v", err)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil || string(data) != replacement {
				t.Fatal("refresh overwrote a new login, sign-out, or account choice")
			}
		})
	}
}

func TestCodexImageRefreshUsesConcurrentRotationWithoutRetry(t *testing.T) {
	isolateProjectIconCredentials(t, codexImageTestOpenCode(t, "account", 1, "old-refresh"))
	codexImageTestFallback(t, `{}`)
	path := projectIconAuthFileCandidates()[0]
	newAccess := codexImageTestJWT("account", time.Now().Add(time.Hour).Unix())
	count := 0
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		count++
		data, _ := json.Marshal(map[string]any{"openai": map[string]any{"type": "oauth", "access": newAccess, "refresh": "concurrent-refresh"}})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return codexImageTestResponse(400, `{"error":"refresh-token-secret"}`), nil
	})
	credential, err := resolveCodexImageSubscription(context.Background())
	if err != nil || credential.Bearer != newAccess || credential.Refresh != "concurrent-refresh" || count != 1 {
		t.Fatalf("concurrent rotation was not reused: %v", err)
	}
}

func TestCodexImageGenerationAndJSONEdits(t *testing.T) {
	for _, reference := range []bool{false, true} {
		t.Run(map[bool]string{false: "generation", true: "edit"}[reference], func(t *testing.T) {
			isolateProjectIconCredentials(t, codexImageTestOpenCode(t, "account", time.Now().Add(time.Hour).Unix(), "refresh"))
			codexImageTestFallback(t, `{}`)
			imageData := projectIconTestImage(t, "png")
			imageBase64 := base64.StdEncoding.EncodeToString(imageData)
			credential, _ := codexImageSubscription()
			calls := 0
			mockProjectIconProvider(t, func(req *http.Request) (*http.Response, error) {
				calls++
				endpoint := "generations"
				if reference {
					endpoint = "edits"
				}
				if req.Method != http.MethodPost || req.URL.String() != codexImageBaseURL+endpoint {
					t.Fatalf("wrong endpoint: %s", req.URL)
				}
				if req.Header.Get("Authorization") != "Bearer "+credential.Bearer || req.Header.Get("chatgpt-account-id") != "account" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "application/json" || req.Header.Get("originator") != "glowbom" || len(req.Header.Get("x-codex-image-turn-id")) != 36 {
					t.Fatal("incorrect Codex image headers")
				}
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["model"] != "gpt-image-2" || body["prompt"] != imageAspectPrompt("test prompt", "16:9", reference) || body["quality"] != "low" || body["size"] != "1536x864" || body["background"] != "opaque" || body["n"] != float64(1) {
					t.Fatal("incorrect Codex generation fields")
				}
				if _, ok := body["output_format"]; ok {
					t.Fatal("platform-only field sent to Codex")
				}
				if reference {
					images, ok := body["images"].([]any)
					if !ok || len(images) != 1 || images[0].(map[string]any)["image_url"] != "data:image/png;base64,"+imageBase64 {
						t.Fatal("reference image was not sent in JSON")
					}
				} else if _, ok := body["images"]; ok {
					t.Fatal("generation included unexpected reference")
				}
				return iconProviderResponse(imageData), nil
			})
			input := ""
			if reference {
				input = imageBase64
			}
			dataURI, err := callCodexImageGeneration(context.Background(), "test prompt", input, "16:9")
			if err != nil || dataURI != "data:image/png;base64,"+imageBase64 || calls != 1 {
				t.Fatalf("generation failed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestCodexImageErrorsAreSafeAndNeverReplay(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{401, "reconnect"}, {403, "subscription"}, {429, "limits"}, {500, "request"}, {307, "request"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			isolateProjectIconCredentials(t, `{"openai":{"type":"oauth","access":"secret-access","refresh":"secret-refresh","account_id":"account"}}`)
			codexImageTestFallback(t, `{}`)
			count := 0
			mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
				count++
				response := codexImageTestResponse(tc.status, `{"error":"secret-access secret-refresh personal-data"}`)
				response.Header.Set("Location", "https://api.openai.com/v1/images/generations")
				return response, nil
			})
			_, err := callCodexImageGeneration(context.Background(), "test", "", "1:1")
			var failure *codexImageFailure
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Status != tc.status || count != 1 {
				t.Fatalf("wrong error or replay: count=%d err=%v", count, err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "personal-data") {
				t.Fatal("provider error leaked credentials or personal data")
			}
		})
	}
}

func TestCodexImageRefreshErrorsAreSafe(t *testing.T) {
	isolateProjectIconCredentials(t, codexImageTestOpenCode(t, "account", 1, "secret-refresh"))
	codexImageTestFallback(t, `{}`)
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		return codexImageTestResponse(400, `{"error":"secret-refresh personal-data"}`), nil
	})
	_, err := callCodexImageGeneration(context.Background(), "prompt", "", "")
	var failure *codexImageFailure
	if !errors.As(err, &failure) || failure.Code != "refresh" || strings.Contains(err.Error(), "secret-refresh") || strings.Contains(err.Error(), "personal-data") {
		t.Fatalf("refresh error not redacted: %v", err)
	}
}

func TestCodexImageInvalidResponseAndReference(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":[]}`, `{"data":[{"b64_json":""}]}`, `{"data":[{"b64_json":"not-base64"}]}`, `{"data":[{"b64_json":"aGVsbG8="}]}`} {
		t.Run(body, func(t *testing.T) {
			isolateProjectIconCredentials(t, `{"openai":{"type":"oauth","access":"opaque","account_id":"account"}}`)
			codexImageTestFallback(t, `{}`)
			mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) { return codexImageTestResponse(200, body), nil })
			_, err := callCodexImageGeneration(context.Background(), "prompt", "", "")
			var failure *codexImageFailure
			if !errors.As(err, &failure) || failure.Code != "response" {
				t.Fatalf("invalid response accepted: %v", err)
			}
		})
	}
	isolateProjectIconCredentials(t)
	codexImageTestFallback(t, `{}`)
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid reference triggered a request")
		return nil, nil
	})
	_, err := callCodexImageGeneration(context.Background(), "prompt", "data:text/plain;base64,aGVsbG8=", "")
	var failure *codexImageFailure
	if !errors.As(err, &failure) || failure.Code != "reference" {
		t.Fatalf("invalid reference accepted: %v", err)
	}
}

func TestCodexImageCancellationAndDeadline(t *testing.T) {
	isolateProjectIconCredentials(t, `{"openai":{"type":"oauth","access":"opaque","account_id":"account"}}`)
	codexImageTestFallback(t, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	mockProjectIconProvider(t, func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > codexImageTimeout {
			t.Fatal("image request has no bounded deadline")
		}
		cancel()
		return nil, req.Context().Err()
	})
	defer cancel()
	if _, err := callCodexImageGeneration(ctx, "prompt", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not propagated: %v", err)
	}
}

func TestCodexImageReferenceDetectsActualMIME(t *testing.T) {
	data := projectIconTestImage(t, "jpeg")
	payload := base64.StdEncoding.EncodeToString(data)
	uri, err := codexImageReferenceURI("data:image/png;base64," + payload)
	if err != nil || uri != "data:image/jpeg;base64,"+payload {
		t.Fatalf("reference MIME was not detected: %v", err)
	}
	decoded, _, _ := decodeBase64Payload(uri, "image/png")
	if !bytes.Equal(decoded, data) {
		t.Fatal("reference content was modified")
	}
}
