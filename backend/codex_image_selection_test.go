package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

func codexImageIdentityTestAuth(t *testing.T, codex bool, account, subject string, issued, expires int64) (string, string) {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account},
		"sub":                         subject, "iat": issued, "exp": expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	key := "openai"
	fields := map[string]any{"type": "oauth", "access": token, "refresh": "test-refresh", "accountId": account}
	if codex {
		key = "tokens"
		fields = map[string]any{"access_token": token, "refresh_token": "test-refresh", "account_id": account}
	}
	encoded, err := json.Marshal(map[string]any{key: fields})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded), token
}

func TestCodexImageSelectsNewerSignInOnlyForSameIdentity(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		name, account, subject       string
		firstIssued, issued, expires int64
		wantCodex                    bool
	}{
		{"newer same user and workspace", "workspace", "user", now - 200, now - 100, now + 3600, true},
		{"older token", "workspace", "user", now - 200, now - 300, now + 3600, false},
		{"same issuance", "workspace", "user", now - 200, now - 200, now + 3600, false},
		{"different workspace", "other-workspace", "user", now - 200, now - 100, now + 3600, false},
		{"different user in same workspace", "workspace", "other-user", now - 200, now - 100, now + 3600, false},
		{"unknown user", "workspace", "", now - 200, now - 100, now + 3600, false},
		{"unknown original issuance", "workspace", "user", 0, now - 100, now + 3600, false},
		{"unknown candidate issuance", "workspace", "user", now - 200, 0, now + 3600, false},
		{"expired candidate", "workspace", "user", now - 200, now - 100, now - 1, false},
		{"future issuance", "workspace", "user", now - 200, now + 3600, now + 7200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, firstToken := codexImageIdentityTestAuth(t, false, "workspace", "user", tc.firstIssued, now+3600)
			second, secondToken := codexImageIdentityTestAuth(t, true, tc.account, tc.subject, tc.issued, tc.expires)
			isolateProjectIconCredentials(t, first)
			path := codexImageTestFallback(t, second)
			mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
				t.Fatal("credential selection must not contact a provider")
				return nil, nil
			})
			selected, ok := codexImageSubscription()
			want := firstToken
			if tc.wantCodex {
				want = secondToken
			}
			if !ok || selected.Bearer != want || selected.AccountID != "workspace" || selected.Subject != "user" {
				t.Fatal("selection did not preserve the newest eligible sign-in and selected identity")
			}
			for authPath, before := range map[string]string{projectIconAuthFileCandidates()[0]: first, path: second} {
				after, err := os.ReadFile(authPath)
				if err != nil || string(after) != before {
					t.Fatal("selection changed stored credentials")
				}
			}
		})
	}
}

func TestCodexImageNewerSignInUsedBeforeSingleGenerationRequest(t *testing.T) {
	now := time.Now().Unix()
	first, _ := codexImageIdentityTestAuth(t, false, "workspace", "user", now-200, now+3600)
	second, token := codexImageIdentityTestAuth(t, true, "workspace", "user", now-100, now+3600)
	isolateProjectIconCredentials(t, first)
	codexImageTestFallback(t, second)
	data := projectIconTestImage(t, "png")
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "chatgpt.com" || r.URL.Path != "/backend-api/codex/images/edits" ||
			r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-Id") != "workspace" {
			t.Fatal("generation did not use the newer sign-in for the selected account")
		}
		return iconProviderResponse(data), nil
	})
	_, err := callCodexImageGeneration(context.Background(), "A garden", base64.StdEncoding.EncodeToString(data), "1:1")
	if err != nil || calls != 1 {
		t.Fatalf("expected one successful request, calls=%d err=%v", calls, err)
	}
}
