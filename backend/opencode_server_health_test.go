package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
)

func TestLiveV1ServerHealthAndCatalog(t *testing.T) {
	serverURL := strings.TrimSpace(os.Getenv("GLOWBOM_TEST_OPENCODE_V1_URL"))
	if serverURL == "" {
		t.Skip("set GLOWBOM_TEST_OPENCODE_V1_URL to check an existing OpenCode V1 server")
	}
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	setOpenCodeProtocol(serverURL, "")
	t.Cleanup(func() { openCodeProtocols.Delete(strings.TrimRight(serverURL, "/")) })
	if running, err := probeOpenCodeServer(serverURL); !running || err != nil {
		t.Fatalf("live V1 probe = %t, %v", running, err)
	}
	if protocol := openCodeProtocol(serverURL); protocol != "v1" {
		t.Fatalf("live protocol = %q; want v1", protocol)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	driver := NewOpenCodeDriver(serverURL)
	providers, err := driver.client.App.Providers(ctx, opencode.AppProvidersParams{Directory: opencode.F(directory)})
	if err != nil {
		t.Fatalf("live V1 provider catalog: %v", err)
	}
	if providers == nil || len(providers.Providers) == 0 {
		t.Fatal("live V1 provider catalog is empty")
	}
}

func TestOpenCodeServerProbeModernV1PreservesProviderRoutes(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_PASSWORD", "fixture-password")
	paths := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		if _, password, ok := r.BasicAuth(); !ok || password != "fixture-password" {
			t.Error("connection credentials missing")
		}
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><html>OpenCode</html>")
		case "/global/health":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"healthy":true,"version":"1.18.32"}`)
		case "/provider", "/config/providers":
			if r.URL.Query().Get("directory") != "/fixture" || r.URL.Query().Has("location[directory]") {
				t.Errorf("V1 directory query changed: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/provider" {
				io.WriteString(w, `{"all":[{"id":"fixture","name":"Fixture","models":{}}],"connected":["fixture"],"default":{}}`)
			} else {
				io.WriteString(w, `{"providers":[{"id":"fixture","name":"Fixture","models":{}}],"default":{}}`)
			}
		default:
			t.Errorf("unexpected V1 request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "")
	t.Cleanup(func() { openCodeProtocols.Delete(server.URL) })
	if running, err := probeOpenCodeServer(server.URL); !running || err != nil {
		t.Fatalf("probe = %t, %v; want healthy V1", running, err)
	}
	if protocol := openCodeProtocol(server.URL); protocol != "v1" {
		t.Fatalf("protocol = %q; want v1", protocol)
	}
	service := &chatService{directory: "/fixture", serverURL: server.URL, client: openCodeHTTPClient(&http.Client{})}
	var catalog struct {
		Connected []string `json:"connected"`
	}
	if err := service.json(context.Background(), http.MethodGet, "/provider", nil, &catalog); err != nil {
		t.Fatalf("chat provider request: %v", err)
	}
	if len(catalog.Connected) != 1 || catalog.Connected[0] != "fixture" {
		t.Fatalf("chat provider response changed: %#v", catalog)
	}
	driver := NewOpenCodeDriver(server.URL)
	providers, err := driver.client.App.Providers(context.Background(), opencode.AppProvidersParams{Directory: opencode.F("/fixture")})
	if err != nil || providers == nil || len(providers.Providers) != 1 || providers.Providers[0].ID != "fixture" {
		t.Fatalf("SDK provider response = %#v, %v", providers, err)
	}
	for _, expected := range []string{"/health", "/global/health", "/provider", "/config/providers"} {
		if path := <-paths; path != expected {
			t.Fatalf("request = %s; want %s", path, expected)
		}
	}
}

func TestOpenCodeServerProbeGlobalHealthAuthStopsDiscovery(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/health":
					http.NotFound(w, r)
				case "/global/health":
					http.Error(w, "private diagnostic", status)
				default:
					t.Errorf("continued discovery after auth rejection: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			setOpenCodeProtocol(server.URL, "")
			t.Cleanup(func() { openCodeProtocols.Delete(server.URL) })
			if running, err := probeOpenCodeServer(server.URL); running || !errors.Is(err, errChatServerAuth) {
				t.Fatalf("probe = %t, %v; want authentication error", running, err)
			}
		})
	}
}

func TestOpenCodeServerProbeGlobalHTMLStillDetectsV2(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/global/health":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><html>OpenCode</html>")
		case "/api/info":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"version":"2.0.21"}`)
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "")
	t.Cleanup(func() { openCodeProtocols.Delete(server.URL) })
	if running, err := probeOpenCodeServer(server.URL); !running || err != nil {
		t.Fatalf("probe = %t, %v; want healthy V2", running, err)
	}
	if protocol := openCodeProtocol(server.URL); protocol != "v2" {
		t.Fatalf("protocol = %q; want v2", protocol)
	}
}

func TestOpenCodeServerProbeRedetectsV1AfterV2Replacement(t *testing.T) {
	var modernV1 atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if modernV1.Load() {
			if r.URL.Path == "/global/health" {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"healthy":true,"version":"1.18.32"}`)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><html>OpenCode</html>")
			return
		}
		if r.URL.Path == "/api/info" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"version":"2.0.21"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "")
	t.Cleanup(func() { openCodeProtocols.Delete(server.URL) })
	if running, err := probeOpenCodeServer(server.URL); !running || err != nil || openCodeProtocol(server.URL) != "v2" {
		t.Fatalf("initial V2 probe = %t, %v; protocol %q", running, err, openCodeProtocol(server.URL))
	}
	modernV1.Store(true)
	if running, err := probeOpenCodeServer(server.URL); !running || err != nil || openCodeProtocol(server.URL) != "v1" {
		t.Fatalf("replacement V1 probe = %t, %v; protocol %q", running, err, openCodeProtocol(server.URL))
	}
}

func TestOpenCodeServerProbeUsesConnectionCredentials(t *testing.T) {
	for _, username := range []string{"", "glowbom-test"} {
		t.Run("username="+username, func(t *testing.T) {
			t.Setenv("OPENCODE_SERVER_USERNAME", username)
			t.Setenv("OPENCODE_SERVER_PASSWORD", "fixture-password")
			expected := username
			if expected == "" {
				expected = "opencode"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if r.URL.Path != "/health" || !ok || user != expected || password != "fixture-password" {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			if running, err := probeOpenCodeServer(server.URL); !running || err != nil {
				t.Fatalf("probe = %t, %v; want healthy", running, err)
			}
		})
	}
}

func TestOpenCodeServerProbeAbsent(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	if running, err := probeOpenCodeServer(server.URL); running || err != nil {
		t.Fatalf("probe = %t, %v; want absent without error", running, err)
	}
}

func TestOpenCodeServerProbeDoesNotTreatBrokenListenerAsAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	}))
	defer server.Close()
	if running, err := probeOpenCodeServer(server.URL); running || err == nil {
		t.Fatalf("probe = %t, %v; want a connection error for an occupied port", running, err)
	}
}

func TestOpenCodeServerProbeRejectsHTTPFailuresWithoutStarting(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("OPENCODE_SERVER_PASSWORD", "fixture-password")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "private diagnostic fixture-password", status)
			}))
			defer server.Close()
			address, _ := url.Parse(server.URL)
			t.Setenv("OPENCODE_SERVER_HOSTNAME", address.Hostname())
			t.Setenv("GLOWBOM_AGENT_PORT", address.Port())
			// A startup attempt would fail with a missing CLI instead of the probe error.
			t.Setenv("PATH", t.TempDir())
			err := ensureOpenCodeServerReady(t.TempDir(), "", "", "", "", "", "", "", "", "opencode-config", "", 0)
			if err == nil || strings.Contains(err.Error(), "CLI") || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "fixture-password") {
				t.Fatalf("expected a safe connection error before startup, got %v", err)
			}
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				if !errors.Is(err, errChatServerAuth) || chatFailureCode(err) != "opencode_server_auth" {
					t.Fatalf("expected local server authentication error, got %v", err)
				}
			}
			if waitErr := waitForOpenCodeServerReady(server.URL, time.Second); waitErr == nil || waitErr.Error() != err.Error() {
				t.Fatalf("readiness wait should preserve connection error, got %v", waitErr)
			}
		})
	}
}

func TestOpenCodeHealthHandlerDoesNotStartOverExistingServer(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" {
					t.Errorf("unexpected request to %s", r.URL.Path)
				}
				http.Error(w, "private diagnostic", status)
			}))
			defer server.Close()
			previous := openCodeDriver
			openCodeDriver = &OpenCodeDriver{serverURL: server.URL}
			t.Cleanup(func() { openCodeDriver = previous })
			recorder := httptest.NewRecorder()
			openCodeHealthHandler(recorder, httptest.NewRequest(http.MethodGet, "/opencode/health", nil))
			var response struct {
				Healthy bool   `json:"healthy"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if status == http.StatusOK {
				if recorder.Code != http.StatusOK || !response.Healthy {
					t.Fatalf("healthy server reported unavailable: %s", recorder.Body)
				}
			} else if recorder.Code != http.StatusServiceUnavailable || response.Healthy || response.Error != errChatServerAuth.Error() {
				t.Fatalf("expected safe authentication error without auto-start: %s", recorder.Body)
			}
		})
	}
}

func TestOpenCodeHealthHandlerDoesNotStartExternalServer(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	t.Setenv("PATH", t.TempDir())
	previous := openCodeDriver
	openCodeDriver = &OpenCodeDriver{serverURL: server.URL}
	t.Cleanup(func() { openCodeDriver = previous })
	recorder := httptest.NewRecorder()
	openCodeHealthHandler(recorder, httptest.NewRequest(http.MethodGet, "/opencode/health", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "configured OpenCode server is not running") {
		t.Fatalf("expected external server guidance without local startup: %s", recorder.Body)
	}
}
