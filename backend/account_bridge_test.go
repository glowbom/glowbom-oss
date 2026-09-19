// Created by Codex under Jacob's direction, Glowbom Labs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func accountTestRequest(b http.Handler, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer fixture-token")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	return w
}

func TestAccountBridgeRequiresAuthenticationEvenWithoutGlobalToken(t *testing.T) {
	for _, token := range []string{"", "fixture-token"} {
		t.Setenv("GLOWBOM_SERVER_TOKEN", token)
		t.Setenv("GLOWBY_SERVER_TOKEN", "")
		b := newAccountBridge(func(context.Context, ...string) ([]byte, error) { t.Fatal("unauthorized command ran"); return nil, nil })
		for _, path := range []string{"/account/status", "/account/login", "/account/logout", "/account/login/cancel"} {
			w := httptest.NewRecorder()
			b.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
			if w.Code != 401 {
				t.Fatalf("%s accepted unauthenticated request", path)
			}
		}
	}
}

func TestAccountBridgeValidatesAndSanitizesCLIResponses(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	for _, tc := range []struct {
		body, status, code string
		err                error
	}{
		{`{"version":1,"status":"signed_in","uid":"owner","subscriptionStatus":"premium","idToken":"secret"}`, "signed_in", "", nil},
		{`{"version":1,"status":"signed_out","uid":"discard","email":"discard"}`, "signed_out", "", nil},
		{`{"version":1,"status":"unavailable","code":"account_unavailable"}`, "unavailable", "account_unavailable", errors.New("exit 1")},
		{`{"version":1,"status":"signed_in","uid":"owner","subscriptionStatus":"premium"}`, "unavailable", "account_unavailable", errors.New("exit 1")},
		{`{"version":2,"status":"signed_in"}`, "unavailable", "cli_update_required", nil},
		{`{"version":1,"status":"signed_in","uid":"owner","subscriptionStatus":"mystery"}`, "unavailable", "cli_update_required", nil},
		{`{"version":1,"status":"unavailable","code":"secret"}`, "unavailable", "cli_update_required", nil},
		{"Account: old CLI", "unavailable", "cli_update_required", nil},
		{"", "unavailable", "cli_unavailable", errAccountCLIMissing},
	} {
		b := newAccountBridge(func(_ context.Context, args ...string) ([]byte, error) {
			if strings.Join(args, " ") != "account --json" {
				t.Errorf("wrong command: %v", args)
			}
			return []byte(tc.body), tc.err
		})
		w := accountTestRequest(b, "GET", "/account/status")
		var got localAccountStatus
		if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Status != tc.status || got.Code != tc.code {
			t.Fatalf("unexpected response: %s", w.Body)
		}
		if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "discard") {
			t.Fatal("untrusted fields leaked")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("account response may be cached")
		}
	}
}

func TestAccountLoginIsAsyncDeduplicatedCancelableAndSerializesLogout(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	var calls atomic.Int32
	started := make(chan struct{})
	b := newAccountBridge(func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "login" {
			calls.Add(1)
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if args[0] != "logout" {
			t.Errorf("unexpected command: %v", args)
		}
		return nil, nil
	})
	if w := accountTestRequest(b, "POST", "/account/login"); w.Code != 202 {
		t.Fatal(w.Body)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("login did not start")
	}
	if w := accountTestRequest(b, "POST", "/account/login"); w.Code != 202 {
		t.Fatal(w.Body)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate login")
	}
	for _, request := range [][2]string{{"POST", "/account/logout"}, {"GET", "/account/status"}} {
		if w := accountTestRequest(b, request[0], request[1]); w.Code != 409 {
			t.Fatal("credential race allowed")
		}
	}
	accountTestRequest(b, "POST", "/account/login/cancel")
	select {
	case <-b.loginDone:
	case <-time.After(time.Second):
		t.Fatal("login did not cancel")
	}
	if w := accountTestRequest(b, "GET", "/account/login"); !strings.Contains(w.Body.String(), `"canceled"`) {
		t.Fatal(w.Body)
	}
	if w := accountTestRequest(b, "POST", "/account/logout"); w.Code != 200 || !strings.Contains(w.Body.String(), `"signed_out"`) {
		t.Fatal(w.Body)
	}
}

func TestAccountBridgeMethodsOriginsAndFailedLogin(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	var calls atomic.Int32
	b := newAccountBridge(func(context.Context, ...string) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("private stderr")
	})
	for _, request := range [][2]string{{"POST", "/account/status"}, {"GET", "/account/logout"}, {"GET", "/account/login/cancel"}, {"DELETE", "/account/login"}} {
		if w := accountTestRequest(b, request[0], request[1]); w.Code != 405 {
			t.Fatal("unsafe method accepted")
		}
	}
	r := httptest.NewRequest("POST", "/account/login", nil)
	r.Header.Set("Authorization", "Bearer fixture-token")
	r.Header.Set("Origin", "https://untrusted.example")
	w := httptest.NewRecorder()
	withGlowbomSecurity(b).ServeHTTP(w, r)
	if w.Code != 403 || calls.Load() != 0 {
		t.Fatal("untrusted origin ran a command")
	}
	accountTestRequest(b, "POST", "/account/login")
	select {
	case <-b.loginDone:
	case <-time.After(time.Second):
		t.Fatal("login did not finish")
	}
	if w := accountTestRequest(b, "GET", "/account/login"); !strings.Contains(w.Body.String(), `"failed"`) || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Body)
	}
	if w := accountTestRequest(b, "POST", "/account/logout"); w.Code != 503 {
		t.Fatal("failed logout reported success")
	}
}
