package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type memoryAccountStore struct {
	value accountCredentials
	saves int
}

func (s *memoryAccountStore) Load() (accountCredentials, error) { return s.value, nil }
func (s *memoryAccountStore) Save(v accountCredentials) error {
	s.value = v
	s.saves++
	return nil
}
func (s *memoryAccountStore) Delete() error { s.value = accountCredentials{}; return nil }

func testCredentials() accountCredentials {
	return accountCredentials{UID: "owner", Email: "owner@example.test", IDToken: "private-id-token",
		RefreshToken: "private-refresh-token", ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

func TestAccountURLsRequireHTTPSAndSeparateLocalMode(t *testing.T) {
	t.Setenv("GLOWBOM_ACCOUNT_API_URL", "")
	t.Setenv("GLOWBOM_LOGIN_URL", "")
	t.Setenv("GLOWBOM_AUTH_LOCAL", "")
	if _, err := loadAccountConfig(); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"http://api.example", "https://user:pass@api.example", "https://api.example?token=x", "https://api.example/#fragment"} {
		t.Setenv("GLOWBOM_ACCOUNT_API_URL", address)
		if _, err := loadAccountConfig(); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	t.Setenv("GLOWBOM_AUTH_LOCAL", "1")
	t.Setenv("GLOWBOM_ACCOUNT_API_URL", "http://127.0.0.1:15001/demo-glowbom-cli/us-central1")
	if _, err := loadAccountConfig(); err == nil {
		t.Fatal("local mode accepted a production login URL")
	}
	t.Setenv("GLOWBOM_LOGIN_URL", "http://localhost:15200/")
	if _, err := loadAccountConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_ACCOUNT_API_URL", "http://127.0.0.1.evil.example/")
	if _, err := loadAccountConfig(); err == nil {
		t.Fatal("accepted a non-loopback emulator")
	}
}

func TestLoginBindsProofAndSavesBeforeBrowserCompletion(t *testing.T) {
	store := &memoryAccountStore{}
	var output bytes.Buffer
	var challenge, opened string
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cliAuth" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body["action"] {
		case "start":
			challenge = body["codeChallenge"]
			json.NewEncoder(w).Encode(map[string]any{"sessionId": strings.Repeat("s", 32), "deviceCode": strings.Repeat("d", 43), "userCode": "ABCD-EFGH", "expiresIn": 30, "interval": 1})
		case "exchange":
			proof := sha256.Sum256([]byte(body["codeVerifier"]))
			if base64.RawURLEncoding.EncodeToString(proof[:]) != challenge || body["deviceCode"] != strings.Repeat("d", 43) {
				t.Error("incorrect terminal proof")
			}
			polls++
			if polls == 1 {
				if store.saves != 0 {
					t.Error("credentials stored before approval")
				}
				json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
			} else {
				json.NewEncoder(w).Encode(struct {
					Status string `json:"status"`
					accountCredentials
				}{"authorized", testCredentials()})
			}
		case "complete":
			if store.saves != 1 || r.Header.Get("Authorization") != "Bearer private-id-token" {
				t.Error("completion preceded credential storage")
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "complete"})
		default:
			t.Errorf("unexpected action %s", body["action"])
		}
	}))
	defer server.Close()
	client := &accountClient{config: accountConfig{APIURL: server.URL, LoginURL: "https://glowbom.com/draw/"},
		http: server.Client(), store: store, out: &output, open: func(_ context.Context, u string) error { opened = u; return nil }}
	if err := client.login(context.Background(), false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opened, "flow=cli") || strings.Contains(opened, "deviceCode") || strings.Contains(opened, "codeVerifier") || strings.Contains(opened, "ABCD") {
		t.Fatalf("bad browser link: %s", opened)
	}
	if strings.Contains(output.String(), "private-") || strings.Contains(output.String(), strings.Repeat("d", 43)) {
		t.Fatal("terminal output leaked credentials")
	}
	if !strings.Contains(output.String(), "Connected to Glowbom") {
		t.Fatal("missing completion")
	}
}

func TestAccountRefreshesOnceAndStoresRotatedCredentials(t *testing.T) {
	store := &memoryAccountStore{value: testCredentials()}
	var output bytes.Buffer
	refreshes, reads := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cliAuth" {
			refreshes++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["action"] != "refresh" || body["refreshToken"] != "private-refresh-token" {
				t.Error("wrong refresh payload")
			}
			updated := testCredentials()
			updated.IDToken = "refreshed-id"
			updated.RefreshToken = "rotated-refresh"
			json.NewEncoder(w).Encode(updated)
			return
		}
		reads++
		if reads == 1 {
			w.WriteHeader(401)
			w.Write([]byte(`{"code":"unauthorized"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-id" {
			t.Error("did not use refreshed token")
		}
		w.Write([]byte(`{"subscriptionStatus":"premium","remainingUsd":12.5,"allowanceUsd":20}`))
	}))
	defer server.Close()
	client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: store, out: &output}
	if err := client.account(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 || reads != 2 || store.value.RefreshToken != "rotated-refresh" {
		t.Fatal("incorrect refresh behavior")
	}
	if !strings.Contains(output.String(), "$12.5000") || strings.Contains(output.String(), "token") {
		t.Fatal("unexpected account output")
	}
}

func TestAccountDoesNotAcceptDifferentUserAfterRefresh(t *testing.T) {
	store := &memoryAccountStore{value: testCredentials()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := testCredentials()
		value.UID = "someone-else"
		json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: store}
	if _, err := client.refresh(context.Background(), store.value); err == nil {
		t.Fatal("accepted a different account")
	}
	if store.saves != 0 {
		t.Fatal("saved unexpected credentials")
	}
}

func TestCredentialFilesArePrivateAndIsolatedByAPI(t *testing.T) {
	t.Setenv("GLOWBOM_CREDENTIAL_STORE", "file")
	t.Setenv("GLOWBOM_CONFIG_DIR", t.TempDir())
	a, err := newCredentialStore("https://api.glowbom.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newCredentialStore("http://127.0.0.1:15001/demo/us-central1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Save(testCredentials()); err != nil {
		t.Fatal(err)
	}
	path := a.(*credentialStore).file
	file, _ := os.Stat(path)
	dir, _ := os.Stat(filepath.Dir(path))
	if file.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		t.Fatal("credentials were not private")
	}
	if _, err := b.Load(); err == nil {
		t.Fatal("local configuration loaded production credentials")
	}
	if value, err := a.Load(); err != nil || value.UID != "owner" {
		t.Fatal("could not load saved account")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Load(); err == nil {
		t.Fatal("accepted readable credential file")
	}
	if err := a.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("logout did not remove credentials")
	}
}

func TestAPIErrorsDoNotExposeServerBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"secret-token","code":"login_unavailable"}`))
	}))
	defer server.Close()
	client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client()}
	err := client.request(context.Background(), "/cliAuth", "", map[string]string{"action": "start"}, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal("unsafe error")
	}
}
