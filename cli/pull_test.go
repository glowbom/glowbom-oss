package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func pullHTTPTestZIP(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, file := range []struct{ name, data string }{
		{"glowbom.json", `{"syncFormatVersion":"1.0","promptPath":"prompt.txt","exports":[{"path":"exports/index.html","platform":"web"}]}`},
		{"prompt.txt", "Make a garden planner"},
		{"exports/index.html", "<!doctype html><title>Garden</title>"},
	} {
		entry, err := writer.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func pullHTTPTestClient(server *httptest.Server, output io.Writer, store accountStore) *accountClient {
	return &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: store, out: output}
}

func assertPullFailureClean(t *testing.T, dir string, err error, terminal string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected pull to fail")
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed pull left local files: readErr=%v entries=%d", readErr, len(entries))
	}
	if strings.Contains(err.Error()+terminal, "private-") || strings.Contains(terminal, "Saved project:") {
		t.Fatalf("unsafe failure output: err=%v output=%q", err, terminal)
	}
}

func TestPullOptionsAcceptOnlyOutputAndHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"--output", "garden"}, {"--output=garden"}} {
		opts, err := parsePullOptions(args)
		if err != nil {
			t.Fatalf("rejected valid arguments %q: %v", args, err)
		}
		if (len(args) == 0 && opts.Output != "") || (len(args) != 0 && opts.Output != "garden") {
			t.Fatalf("wrong output for %q: %q", args, opts.Output)
		}
	}
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		if _, err := parsePullOptions(args); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("help did not return flag.ErrHelp: %v", err)
		}
	}
	for _, args := range [][]string{
		{"garden"}, {"--unknown"}, {"--output"}, {"--output="}, {"--output", ""},
		{"--output", "garden", "--output", "other"}, {"--output=garden", "--output=other"},
		{"--output", "garden", "extra"},
	} {
		if _, err := parsePullOptions(args); err == nil {
			t.Fatalf("accepted invalid arguments %q", args)
		}
	}
}

func TestPullCommandHelpAndInvalidOptionsDoNotNeedLogin(t *testing.T) {
	t.Setenv("GLOWBOM_ACCOUNT_API_URL", "invalid-api-address")
	if run([]string{"pull", "--help"}) != 0 {
		t.Fatal("pull help failed")
	}
	for _, args := range [][]string{{"pull", "--unknown"}, {"pull", "folder"}, {"pull", "--output"}} {
		if run(args) != 2 {
			t.Fatalf("invalid arguments did not produce usage failure: %q", args)
		}
	}
}

func TestPullCommandUsesConfiguredLoginAndDownloadsProject(t *testing.T) {
	archive := pullHTTPTestZIP(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/project" || r.Header.Get("Authorization") != "Bearer private-id-token" {
			t.Error("CLI did not use the configured API and saved credentials")
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(archive)
	}))
	defer server.Close()
	t.Setenv("GLOWBOM_AUTH_LOCAL", "1")
	t.Setenv("GLOWBOM_ACCOUNT_API_URL", server.URL)
	t.Setenv("GLOWBOM_LOGIN_URL", server.URL+"/draw/")
	t.Setenv("GLOWBOM_CREDENTIAL_STORE", "file")
	t.Setenv("GLOWBOM_CONFIG_DIR", t.TempDir())
	store, err := newCredentialStore(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testCredentials()); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "garden")
	if exitCode := run([]string{"pull", "--output", output}); exitCode != 0 {
		t.Fatalf("pull CLI failed with exit code %d", exitCode)
	}
	data, err := os.ReadFile(filepath.Join(output, "exports", "index.html"))
	if err != nil || string(data) != "<!doctype html><title>Garden</title>" || calls != 1 {
		t.Fatalf("CLI project download failed: readErr=%v calls=%d", err, calls)
	}
}

func TestPullDownloadsAuthorizedProjectWithoutCookies(t *testing.T) {
	archive := pullHTTPTestZIP(t)
	for _, contentType := range []string{"application/zip", "application/octet-stream"} {
		t.Run(contentType, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/project" || r.URL.RawQuery != "" {
					t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer private-id-token" || r.Header.Get("Cookie") != "" {
					t.Error("missing bearer token or forwarded browser cookies")
				}
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Content-Disposition", `attachment; filename="private-untrusted-name.zip"`)
				w.Write(archive)
			}))
			defer server.Close()
			var terminal bytes.Buffer
			client := pullHTTPTestClient(server, &terminal, &memoryAccountStore{value: testCredentials()})
			client.http.Timeout = 40 * time.Second
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			serverURL, _ := url.Parse(server.URL)
			jar.SetCookies(serverURL, []*http.Cookie{{Name: "session", Value: "private-cookie"}})
			client.http.Jar = jar
			output := filepath.Join(t.TempDir(), "garden")
			if err := client.pullProject(context.Background(), pullOptions{Output: output}); err != nil {
				t.Fatal(err)
			}
			for file, want := range map[string]string{"prompt.txt": "Make a garden planner", "exports/index.html": "<!doctype html><title>Garden</title>"} {
				data, err := os.ReadFile(filepath.Join(output, file))
				if err != nil || string(data) != want {
					t.Fatalf("incorrect saved file %s: %v", file, err)
				}
			}
			canonical, err := filepath.EvalSymlinks(output)
			if err != nil {
				t.Fatal(err)
			}
			text := terminal.String()
			if calls != 1 || !strings.Contains(text, "Saved project: "+canonical) || !strings.Contains(text, "3") || strings.Contains(text, "private-") {
				t.Fatalf("incorrect success report: calls=%d output=%q", calls, text)
			}
			if client.http.Timeout != 40*time.Second || client.http.Jar != jar {
				t.Fatal("pull changed the shared account HTTP client")
			}
		})
	}
}

func TestPullRefreshesExpiredOrRejectedCredentialsOnce(t *testing.T) {
	archive := pullHTTPTestZIP(t)
	for _, expired := range []bool{true, false} {
		t.Run(map[bool]string{true: "expired", false: "rejected"}[expired], func(t *testing.T) {
			reads, refreshes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/cliAuth" {
					refreshes++
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Method != http.MethodPost || body["action"] != "refresh" || body["refreshToken"] != "private-refresh-token" {
						t.Error("incorrect refresh request")
					}
					updated := testCredentials()
					updated.IDToken = "fresh-token"
					updated.RefreshToken = "rotated-refresh-token"
					json.NewEncoder(w).Encode(updated)
					return
				}
				reads++
				if r.Header.Get("Authorization") != "Bearer fresh-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/zip")
				w.Write(archive)
			}))
			defer server.Close()
			creds := testCredentials()
			if expired {
				creds.ExpiresAt = time.Now().Add(-time.Minute).Unix()
			}
			store := &memoryAccountStore{value: creds}
			var terminal bytes.Buffer
			client := pullHTTPTestClient(server, &terminal, store)
			if err := client.pullProject(context.Background(), pullOptions{Output: filepath.Join(t.TempDir(), "garden")}); err != nil {
				t.Fatal(err)
			}
			wantReads := 2
			if expired {
				wantReads = 1
			}
			if reads != wantReads || refreshes != 1 || store.saves != 1 || store.value.RefreshToken != "rotated-refresh-token" {
				t.Fatalf("incorrect refresh behavior: reads=%d refreshes=%d saves=%d", reads, refreshes, store.saves)
			}
		})
	}
}

func TestPullRejectsRepeatedUnauthorizedAndDifferentAccount(t *testing.T) {
	for _, mode := range []string{"unauthorized-again", "expired-then-unauthorized", "different-account"} {
		t.Run(mode, func(t *testing.T) {
			reads, refreshes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/cliAuth" {
					refreshes++
					updated := testCredentials()
					if mode == "different-account" {
						updated.UID = "different-owner"
					}
					json.NewEncoder(w).Encode(updated)
					return
				}
				reads++
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			creds := testCredentials()
			if mode == "expired-then-unauthorized" {
				creds.ExpiresAt = time.Now().Add(-time.Minute).Unix()
			}
			store := &memoryAccountStore{value: creds}
			var terminal bytes.Buffer
			client := pullHTTPTestClient(server, &terminal, store)
			dir := t.TempDir()
			err := client.pullProject(context.Background(), pullOptions{Output: filepath.Join(dir, "garden")})
			assertPullFailureClean(t, dir, err, terminal.String())
			wantReads := 1
			if mode == "unauthorized-again" {
				wantReads = 2
			}
			if reads != wantReads || refreshes != 1 {
				t.Fatalf("retried authentication too often: reads=%d refreshes=%d", reads, refreshes)
			}
			if mode == "different-account" && store.saves != 0 {
				t.Fatal("saved credentials for a different account")
			}
		})
	}
}

func TestPullExplainsServerFailuresWithoutLeakingTheirBodies(t *testing.T) {
	for _, failure := range []struct {
		name, code, message string
		status              int
	}{
		{"no-project", "project_not_found", "save", 404},
		{"incomplete", "project_incomplete", "wait", 409},
		{"too-large", "project_too_large", "9", 413},
		{"invalid", "invalid_project", "save", 422},
		{"rate-limit", "", "wait", 429},
		{"server-error", "private-untrusted-code", "", 500},
		{"gateway-timeout", "", "", 504},
		{"missing-endpoint", "", "", 404},
		{"unexpected-success", "", "", 204},
	} {
		t.Run(failure.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(failure.status)
				json.NewEncoder(w).Encode(map[string]string{"code": failure.code, "message": "private-id-token https://host.invalid/?private-signed-query"})
			}))
			defer server.Close()
			var terminal bytes.Buffer
			client := pullHTTPTestClient(server, &terminal, &memoryAccountStore{value: testCredentials()})
			dir := t.TempDir()
			err := client.pullProject(context.Background(), pullOptions{Output: filepath.Join(dir, "garden")})
			assertPullFailureClean(t, dir, err, terminal.String())
			if calls != 1 || !strings.Contains(strings.ToLower(err.Error()), failure.message) {
				t.Fatalf("incorrect server error handling: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestPullDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
	defer other.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", other.URL+"/?private-signed-query")
				w.WriteHeader(status)
			}))
			defer server.Close()
			var terminal bytes.Buffer
			client := pullHTTPTestClient(server, &terminal, &memoryAccountStore{value: testCredentials()})
			dir := t.TempDir()
			err := client.pullProject(context.Background(), pullOptions{Output: filepath.Join(dir, "garden")})
			assertPullFailureClean(t, dir, err, terminal.String())
			if calls != 1 || followed.Load() != 0 {
				t.Fatal("followed the API redirect")
			}
		})
	}
}

func TestPullRejectsUnreadableOrOversizedDownloads(t *testing.T) {
	archive := pullHTTPTestZIP(t)
	for _, response := range []struct {
		name, contentType, length string
		body                      []byte
	}{
		{"not-zip", "application/zip", "", []byte("private-invalid-zip")},
		{"truncated-zip", "application/zip", "", archive[:len(archive)/2]},
		{"truncated-body", "application/zip", "9999", archive},
		{"wrong-content-type", "text/html", "", archive},
		{"missing-content-type", "", "", archive},
		{"oversized-declared", "application/zip", "10000001", archive},
		{"oversized-stream", "application/zip", "", bytes.Repeat([]byte("x"), 10000001)},
	} {
		t.Run(response.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header()["Content-Type"] = []string{response.contentType}
				if response.length != "" {
					w.Header().Set("Content-Length", response.length)
				}
				w.Write(response.body)
			}))
			defer server.Close()
			var terminal bytes.Buffer
			client := pullHTTPTestClient(server, &terminal, &memoryAccountStore{value: testCredentials()})
			dir := t.TempDir()
			err := client.pullProject(context.Background(), pullOptions{Output: filepath.Join(dir, "garden")})
			assertPullFailureClean(t, dir, err, terminal.String())
			if calls != 1 {
				t.Fatalf("retried invalid response %d times", calls)
			}
		})
	}
}

func TestPullValidatesDestinationAndLoginBeforeRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("preserve local work"), 0600); err != nil {
		t.Fatal(err)
	}
	emptyDir := filepath.Join(dir, "empty")
	if err := os.Mkdir(emptyDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{dir, existing, emptyDir, filepath.Join(dir, "missing", "garden")} {
		var terminal bytes.Buffer
		client := pullHTTPTestClient(server, &terminal, &memoryAccountStore{value: testCredentials()})
		if err := client.pullProject(context.Background(), pullOptions{Output: output}); err == nil {
			t.Fatalf("accepted invalid destination %s", output)
		}
	}
	var terminal bytes.Buffer
	client := pullHTTPTestClient(server, &terminal, &memoryAccountStore{})
	if err := client.pullProject(context.Background(), pullOptions{Output: filepath.Join(dir, "garden")}); err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("missing sign-in was accepted: %v", err)
	}
	data, err := os.ReadFile(existing)
	entries, readErr := os.ReadDir(dir)
	if calls.Load() != 0 || err != nil || string(data) != "preserve local work" || readErr != nil || len(entries) != 2 {
		t.Fatalf("invalid input changed local files or called API: calls=%d readErr=%v entries=%d", calls.Load(), readErr, len(entries))
	}
}

func TestPullCancellationLeavesNoFiles(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before-request", false: "during-download"}[before], func(t *testing.T) {
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/zip")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				cancel()
				<-r.Context().Done()
			}))
			defer server.Close()
			if before {
				cancel()
			}
			var terminal bytes.Buffer
			client := pullHTTPTestClient(server, &terminal, &memoryAccountStore{value: testCredentials()})
			dir := t.TempDir()
			err := client.pullProject(ctx, pullOptions{Output: filepath.Join(dir, "garden")})
			assertPullFailureClean(t, dir, err, terminal.String())
			wantCalls := int32(1)
			if before {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("incorrect cancellation handling: calls=%d", calls.Load())
			}
		})
	}
}
