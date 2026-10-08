package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	opencode "github.com/sst/opencode-sdk-go"
)

func TestV2PermissionDecisionsUseNativeSchema(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_PASSWORD", "fixture-password")
	for _, response := range []string{"once", "always", "reject"} {
		t.Run(response, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/api/session/ses_fixture/permission/per_fixture/reply" {
					t.Errorf("unexpected permission endpoint: %s %s", r.Method, r.URL.Path)
				}
				if _, password, ok := r.BasicAuth(); !ok || password != "fixture-password" {
					t.Error("permission request is missing authorization")
				}
				if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected query or content type: %s, %s", r.URL.RawQuery, r.Header.Get("Content-Type"))
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				// OpenCode 2.0.21's HTTP schema requires decision, not reply.
				if len(body) != 1 || body["decision"] != response {
					http.Error(w, "Missing key at decision", http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			setOpenCodeProtocol(server.URL, "v2")
			driver := NewOpenCodeDriver(server.URL)
			if err := driver.respondToPermission(context.Background(), "ses_fixture", "per_fixture", response, "/fixture"); err != nil {
				t.Fatalf("permission decision did not succeed: %v", err)
			}
			// Exercise callers using the SDK transport directly as well.
			ok, err := driver.client.Session.Permissions.Respond(context.Background(), "ses_fixture", "per_fixture", opencode.SessionPermissionRespondParams{
				Response: opencode.F(opencode.SessionPermissionRespondParamsResponse(response)),
			})
			if err != nil || ok == nil || !*ok {
				t.Fatalf("SDK did not translate native 204 success: %v, %v", ok, err)
			}
			if calls != 2 {
				t.Fatalf("permission requests = %d, want one per explicit submission", calls)
			}
		})
	}
}

func TestV2PermissionFailureDoesNotRetryOrFallback(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/session/ses_fixture/permission/per_fixture/reply" {
					t.Errorf("unexpected fallback endpoint: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":"permission response rejected"}`)
			}))
			defer server.Close()
			setOpenCodeProtocol(server.URL, "v2")
			driver := NewOpenCodeDriver(server.URL)
			if err := driver.respondToPermission(context.Background(), "ses_fixture", "per_fixture", "once", "/fixture"); err == nil {
				t.Fatal("reported success for a rejected permission response")
			}
			if calls != 1 {
				t.Fatalf("permission requests = %d, want one without retry or fallback", calls)
			}
		})
	}
}

func TestPermissionRejectsUnknownDecisionBeforeSending(t *testing.T) {
	for _, protocol := range []string{"v1", "v2"} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("sent an invalid permission decision")
			}))
			defer server.Close()
			setOpenCodeProtocol(server.URL, protocol)
			if err := NewOpenCodeDriver(server.URL).respondToPermission(context.Background(), "ses_fixture", "per_fixture", "allow", ""); err == nil {
				t.Fatal("accepted an unsupported permission decision")
			}
		})
	}
}

func TestV1PermissionReplyPreservesLegacySchema(t *testing.T) {
	for _, response := range []string{"once", "always", "reject"} {
		t.Run(response, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/permission/per_fixture/reply" || r.URL.Query().Get("directory") != "/fixture" {
					t.Errorf("V1 endpoint changed: %s %s", r.Method, r.URL.String())
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body) != 2 || body["reply"] != response || body["response"] != response {
					t.Errorf("V1 permission payload changed: %#v", body)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, "true")
			}))
			defer server.Close()
			setOpenCodeProtocol(server.URL, "v1")
			if err := NewOpenCodeDriver(server.URL).respondToPermission(context.Background(), "ses_fixture", "per_fixture", response, "/fixture"); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("V1 requests = %d, want one", calls)
			}
		})
	}
}
