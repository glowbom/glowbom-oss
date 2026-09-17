package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExportCommandOptionsAndHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"--output", "new project"}, {"--output=new project"}} {
		opts, err := parseExportOptions(args)
		if err != nil || (len(args) > 0 && opts.Output != "new project") {
			t.Fatalf("options %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"unexpected"}, {"--output"}, {"--output="}, {"--output=a", "--output=b"}, {"--force"}} {
		if _, err := parseExportOptions(args); err == nil || strings.Contains(err.Error(), "pull only") {
			t.Fatalf("invalid options %v returned %v", args, err)
		}
	}
	t.Setenv("GLOWBOM_ACCOUNT_API_URL", "invalid and should not be used")
	t.Setenv("GLOWBOM_CREDENTIAL_STORE", "invalid and should not be used")
	if code := run([]string{"export", "--help"}); code != 0 {
		t.Fatalf("export help returned %d", code)
	}
	if !strings.Contains(usage, "glowbom export") {
		t.Fatal("export missing from main help")
	}
}

func TestExportDownloadsAndAssemblesWithoutSendingCredentialsToStarter(t *testing.T) {
	saved := archiveTestZIP(t, exportTestSavedEntries())
	starter := archiveTestZIP(t, exportTestStarterEntries())
	var projectRequests, starterRequests atomic.Int32
	httpClient := templateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		switch r.URL.Path {
		case "/project":
			projectRequests.Add(1)
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer private-id-token" {
				t.Error("saved generation request was not authenticated")
			}
			_, _ = w.Write(saved)
		case "/starter.zip":
			starterRequests.Add(1)
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("public starter received account credentials")
			}
			_, _ = w.Write(starter)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	transport := httpClient.Transport
	httpClient.Transport = templateRoundTripper(func(r *http.Request) (*http.Response, error) {
		if (r.URL.Path == "/project" && r.URL.Host != "api.glowbom.com") ||
			(r.URL.Path == "/starter.zip" && r.URL.String() != templateURL) {
			t.Errorf("unexpected download destination: %s", r.URL.Redacted())
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("download sent ambient cookies")
		}
		return transport.RoundTrip(r)
	})
	jar, _ := cookiejar.New(nil)
	for _, address := range []string{templateURL, "https://api.glowbom.com"} {
		u, _ := url.Parse(address)
		jar.SetCookies(u, []*http.Cookie{{Name: "private", Value: "must-not-send"}})
	}
	httpClient.Jar = jar
	httpClient.Timeout = 40 * time.Second
	var terminal bytes.Buffer
	client := &accountClient{config: accountConfig{APIURL: "https://api.glowbom.com"}, http: httpClient,
		store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
	parent := t.TempDir()
	destination := filepath.Join(parent, "complete project")
	if err := client.exportProject(context.Background(), pullOptions{Output: destination}); err != nil {
		t.Fatal(err)
	}
	if projectRequests.Load() != 1 || starterRequests.Load() != 1 ||
		httpClient.Jar != jar || httpClient.Timeout != 40*time.Second || httpClient.CheckRedirect != nil {
		t.Fatal("wrong request count or shared HTTP client was changed")
	}
	for name, want := range map[string]string{
		"prototype/index.html":                                         "Processed preview",
		"apple/Custom/AiExtensions.swift":                              "static let enabled: Bool = true",
		"android/app/src/main/java/com/glowbom/custom/AiExtensions.kt": "val enabled: Boolean = true",
		"web/src/app/components/AiExtensions.tsx":                      "export const enabled: boolean = true",
		"exports/AiExtensions.swift":                                   "static let enabled: Bool = false",
		"inputs/prompt.txt":                                            "Build my garden journal.",
	} {
		data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil || !strings.Contains(string(data), want) {
			t.Fatalf("incorrect output %s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || !strings.Contains(terminal.String(), "Exported project:") || strings.Contains(terminal.String(), "private-") {
		t.Fatalf("unexpected output or leftovers: %v, %q", err, terminal.String())
	}
}

func TestExportRefreshesCredentialsAndEnforcesAccountIdentity(t *testing.T) {
	saved := archiveTestZIP(t, exportTestSavedEntries())
	starter := archiveTestZIP(t, exportTestStarterEntries())
	for _, mode := range []string{"expired", "rejected", "different-account", "rejected-again"} {
		t.Run(mode, func(t *testing.T) {
			var reads, refreshes, templates atomic.Int32
			httpClient := templateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/cliAuth":
					refreshes.Add(1)
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["action"] != "refresh" || body["refreshToken"] != "private-refresh-token" {
						t.Error("incorrect refresh request")
					}
					updated := testCredentials()
					updated.IDToken = "fresh-token"
					updated.RefreshToken = "rotated-refresh-token"
					if mode == "different-account" {
						updated.UID = "someone-else"
					}
					_ = json.NewEncoder(w).Encode(updated)
				case "/project":
					reads.Add(1)
					if mode == "rejected-again" || r.Header.Get("Authorization") != "Bearer fresh-token" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					w.Header().Set("Content-Type", "application/zip")
					_, _ = w.Write(saved)
				case "/starter.zip":
					templates.Add(1)
					if r.Header.Get("Authorization") != "" {
						t.Error("starter received refreshed token")
					}
					w.Header().Set("Content-Type", "application/zip")
					_, _ = w.Write(starter)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			credentials := testCredentials()
			if mode == "expired" {
				credentials.ExpiresAt = time.Now().Add(-time.Minute).Unix()
			}
			store := &memoryAccountStore{value: credentials}
			var terminal bytes.Buffer
			client := &accountClient{config: accountConfig{APIURL: "https://api.glowbom.com"}, http: httpClient, store: store, out: &terminal}
			parent := t.TempDir()
			err := client.exportProject(context.Background(), pullOptions{Output: filepath.Join(parent, "project")})
			wantReads := int32(2)
			if mode == "expired" || mode == "different-account" {
				wantReads = 1
			}
			if reads.Load() != wantReads || refreshes.Load() != 1 {
				t.Fatalf("incorrect auth retries: reads=%d refreshes=%d", reads.Load(), refreshes.Load())
			}
			if mode == "different-account" || mode == "rejected-again" {
				assertExportFailureClean(t, parent, err, terminal.String())
				if templates.Load() != 0 || (mode == "different-account" && store.saves != 0) {
					t.Fatal("continued export or saved a different account")
				}
			} else if err != nil || templates.Load() != 1 || store.saves != 1 || store.value.RefreshToken != "rotated-refresh-token" {
				t.Fatalf("incorrect successful refresh: %v", err)
			}
		})
	}
}

func TestExportCleansUpFailedDownloadsAndCancellation(t *testing.T) {
	saved := archiveTestZIP(t, exportTestSavedEntries())
	starter := archiveTestZIP(t, exportTestStarterEntries())
	incompleteStarter := archiveTestZIP(t, templateTestEntries())
	for _, mode := range []string{"invalid-saved", "template-missing", "invalid-template", "cancel-template", "assembly-failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var templates atomic.Int32
			httpClient := templateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/zip")
				if r.URL.Path == "/project" {
					if mode == "invalid-saved" {
						_, _ = w.Write([]byte("private-invalid-zip"))
					} else {
						_, _ = w.Write(saved)
					}
					return
				}
				templates.Add(1)
				switch mode {
				case "template-missing":
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte("private-server-error"))
				case "invalid-template":
					_, _ = w.Write([]byte("private-invalid-zip"))
				case "cancel-template":
					w.(http.Flusher).Flush()
					cancel()
					<-r.Context().Done()
				case "assembly-failure":
					_, _ = w.Write(incompleteStarter)
				default:
					_, _ = w.Write(starter)
				}
			})
			var terminal bytes.Buffer
			client := &accountClient{config: accountConfig{APIURL: "https://api.glowbom.com"}, http: httpClient,
				store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
			parent := t.TempDir()
			err := client.exportProject(ctx, pullOptions{Output: filepath.Join(parent, "project")})
			assertExportFailureClean(t, parent, err, terminal.String())
			if mode == "invalid-saved" && templates.Load() != 0 {
				t.Fatal("requested starter after saved bundle validation failed")
			}
		})
	}
}

func TestExportPreflightAndConcurrentDestinationPreserveExistingFiles(t *testing.T) {
	saved := archiveTestZIP(t, exportTestSavedEntries())
	starter := archiveTestZIP(t, exportTestStarterEntries())
	for _, mode := range []string{"signed-out", "existing-directory", "existing-file", "canceled", "concurrent-destination"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			destination := filepath.Join(parent, "project")
			var calls atomic.Int32
			httpClient := templateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/zip")
				if r.URL.Path == "/project" {
					_, _ = w.Write(saved)
					return
				}
				if mode == "concurrent-destination" {
					if err := os.WriteFile(destination, []byte("keep me"), 0600); err != nil {
						t.Error(err)
					}
				}
				_, _ = w.Write(starter)
			})
			store := &memoryAccountStore{value: testCredentials()}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "signed-out":
				store.value = accountCredentials{}
			case "existing-directory":
				if err := os.Mkdir(destination, 0700); err != nil {
					t.Fatal(err)
				}
			case "existing-file":
				if err := os.WriteFile(destination, []byte("keep me"), 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			var terminal bytes.Buffer
			client := &accountClient{config: accountConfig{APIURL: "https://api.glowbom.com"}, http: httpClient, store: store, out: &terminal}
			err := client.exportProject(ctx, pullOptions{Output: destination})
			if err == nil || strings.Contains(terminal.String(), "Exported project:") {
				t.Fatal("export unexpectedly succeeded")
			}
			if mode != "concurrent-destination" && calls.Load() != 0 {
				t.Fatal("network request preceded preflight")
			}
			if mode == "existing-file" || mode == "concurrent-destination" {
				data, err := os.ReadFile(destination)
				if err != nil || string(data) != "keep me" {
					t.Fatal("existing destination was changed")
				}
			} else if mode == "existing-directory" {
				files, err := os.ReadDir(destination)
				if err != nil || len(files) != 0 {
					t.Fatal("existing directory was changed")
				}
			} else {
				assertExportFailureClean(t, parent, err, terminal.String())
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) > 1 {
				t.Fatal("temporary output was not cleaned up")
			}
		})
	}
}

func assertExportFailureClean(t *testing.T, parent string, err error, terminal string) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(terminal, "private-") || strings.Contains(terminal, "Exported project:") {
		t.Fatalf("missing or unsafe export failure: %v %q", err, terminal)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("export left incomplete output: %v, %d entries", readErr, len(entries))
	}
}

func TestExportDefaultCreatesUniqueCompleteProjectDirectories(t *testing.T) {
	saved := archiveTestZIP(t, exportTestSavedEntries())
	starter := archiveTestZIP(t, exportTestStarterEntries())
	httpClient := templateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		if r.URL.Path == "/project" {
			_, _ = w.Write(saved)
		} else {
			_, _ = w.Write(starter)
		}
	})
	parent := t.TempDir()
	t.Chdir(parent)
	var terminal bytes.Buffer
	client := &accountClient{config: accountConfig{APIURL: "https://api.glowbom.com"}, http: httpClient,
		store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
	for range 2 {
		if err := client.exportProject(context.Background(), pullOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 2 {
		t.Fatalf("wrong output directory count: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "glowbom-export-") {
			t.Fatalf("unexpected output %q", entry.Name())
		}
		if _, err := os.Stat(filepath.Join(parent, entry.Name(), "prototype", "index.html")); err != nil {
			t.Fatalf("default directory has no prototype: %v", err)
		}
	}
}
