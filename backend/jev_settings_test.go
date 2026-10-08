package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJevSettingsProbeAndSetup(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "token")
	for _, tc := range []struct {
		name, method           string
		online, installFails   bool
		wantCode, wantInstalls int
	}{
		{"available without installed tool", "GET", true, false, 200, 0},
		{"offline", "GET", false, false, 200, 0},
		{"enable installs", "POST", true, false, 200, 1},
		{"offline never installs", "POST", false, false, 503, 0},
		{"setup failure", "POST", true, true, 500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			handler := jevSettingsHandler(func(context.Context) bool { return tc.online }, func() error {
				calls++
				if tc.installFails {
					return errors.New("private local path")
				}
				return nil
			})
			req := httptest.NewRequest(tc.method, "/settings/build/jev", nil)
			req.Header.Set("Authorization", "Bearer token")
			w := httptest.NewRecorder()
			handler(w, req)
			if w.Code != tc.wantCode || calls != tc.wantInstalls {
				t.Fatalf("status=%d installs=%d body=%s", w.Code, calls, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private local path") {
				t.Fatal("setup error leaked")
			}
			if w.Code == 200 {
				var status jevAvailability
				if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Available != tc.online {
					t.Fatalf("incorrect endpoint status: %s", w.Body.String())
				}
			}
		})
	}
}
func TestJevProbeHandlesFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{200, `{"answers":{"decision":{"type":"choice","choice":"yes"}}}`, true},
		{200, `{"error":"unknown model"}`, false},
		{200, `{"answers":{"decision":{"type":"choice","choice":"no"}}}`, false},
		{200, `not json`, false},
		{200, strings.Repeat(" ", 65537), false},
		{503, `{"error":"down"}`, false},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.Header.Get("Authorization") != "" {
				t.Error("unexpected method or forwarded credentials")
			}
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		got := probeJev(context.Background(), server.Client(), server.URL)
		server.Close()
		if got != tc.want {
			t.Fatal("incorrect probe outcome")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if probeJev(ctx, http.DefaultClient, "http://127.0.0.1:1") {
		t.Fatal("canceled probe succeeded")
	}
}
func TestJevStatusRequiresAuthentication(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	w := httptest.NewRecorder()
	for _, method := range []string{"GET", "POST"} {
		w = httptest.NewRecorder()
		jevSettingsHandler(func(context.Context) bool { t.Fatal("unauthenticated probe"); return true }, func() error { t.Fatal("unauthenticated install"); return nil })(w, httptest.NewRequest(method, "/settings/build/jev", nil))
		if w.Code != 401 {
			t.Fatal("unauthenticated request allowed")
		}
	}
	if w.Code != 401 {
		t.Fatal("unauthenticated probe allowed")
	}
}
func TestJevPromptPreference(t *testing.T) {
	for _, tc := range []struct{ enabled, available bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		result := jevBuildPrompt("Build iOS", tc.enabled, tc.available)
		if !strings.HasPrefix(result, "Build iOS") {
			t.Fatal("original instructions lost")
		}
		if strings.Contains(result, jevDecisionInstruction) != (tc.enabled && tc.available) {
			t.Fatal("Jev instruction not scoped to availability and preference")
		}
	}
}

func TestJevPromptRequestsInitialDecision(t *testing.T) {
	result := jevBuildPrompt("Fix the iOS navigation", true, true)
	for _, required := range []string{
		"After inspecting the relevant project context and before editing files",
		"call the installed jev tool once",
		"at least two key=description choices",
		"Do not send whole files, credentials, or unrelated project content",
		"then continue the build",
		"Make further Jev calls only when another narrow decision would benefit",
		"If the tool fails or is unavailable, continue the build yourself",
		"without installing tools or repeatedly retrying",
	} {
		if !strings.Contains(result, required) {
			t.Errorf("enabled Jev prompt is missing %q", required)
		}
	}
}
