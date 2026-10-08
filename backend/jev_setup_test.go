package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallJevToolCopiesBundledToolOnce(t *testing.T) {
	directory := t.TempDir()
	if err := installJevTool(directory, func() ([]byte, error) { return []byte("trusted tool"), nil }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "tools", "jev.ts"))
	if err != nil || string(data) != "trusted tool" {
		t.Fatalf("tool not installed completely: %q, %v", data, err)
	}
	if err := installJevTool(directory, func() ([]byte, error) { t.Fatal("existing tool replaced"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(directory, "tools"))
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected temporary files: %v, %v", files, err)
	}
}

func TestInstallJevToolPreservesExistingVariants(t *testing.T) {
	for _, path := range []string{"tools/jev.ts", "tools/jev.js", "tool/jev.ts", "tool/jev.js"} {
		t.Run(path, func(t *testing.T) {
			directory := t.TempDir()
			destination := filepath.Join(directory, path)
			if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, []byte("user customization"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := installJevTool(directory, func() ([]byte, error) { t.Fatal("existing tool replaced"); return nil, nil }); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(destination)
			if err != nil || string(data) != "user customization" {
				t.Fatal("custom tool changed")
			}
		})
	}
}

func TestInstallJevToolDoesNotPublishFailedSource(t *testing.T) {
	directory := t.TempDir()
	if err := installJevTool(directory, func() ([]byte, error) { return nil, errors.New("source missing") }); err == nil {
		t.Fatal("missing source accepted")
	}
	if exists, err := existingJevTool(directory); exists || err != nil {
		t.Fatalf("failed setup left a tool: %t, %v", exists, err)
	}
}

func TestEnsureJevInstalledRespectsConfigHome(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", directory)
	if err := ensureJevInstalled(); err != nil {
		t.Fatal(err)
	}
	if exists, err := existingJevTool(filepath.Join(directory, "opencode")); !exists || err != nil {
		t.Fatalf("configured tool directory unused: %t, %v", exists, err)
	}
}

type jevSetupTransport func(*http.Request) (*http.Response, error)

func (f jevSetupTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPrepareJevToolRefreshesOnlyIdleProject(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		loaded       bool
		installError bool
		unavailable  bool
		want         bool
		wantDispose  bool
	}{
		{name: "already loaded", loaded: true, want: true},
		{name: "no active sessions", status: `{}`, want: true, wantDispose: true},
		{name: "idle session", status: `{"one":{"type":"idle"}}`, want: true, wantDispose: true},
		{name: "busy session", status: `{"one":{"type":"busy"}}`},
		{name: "retrying session", status: `{"one":{"type":"retry"}}`},
		{name: "unknown session", status: `{"one":{}}`},
		{name: "null status", status: `null`},
		{name: "invalid status", status: `invalid`},
		{name: "failed install", installError: true},
		{name: "tool still missing after reload", status: `{}`, unavailable: true, wantDispose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			installed, disposed := false, false
			client := &http.Client{Transport: jevSetupTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("directory") != directory {
					t.Fatal("request not scoped to project")
				}
				body := ""
				switch r.Method + " " + r.URL.Path {
				case "GET /experimental/tool/ids":
					body = `["read"]`
					if tc.loaded || (disposed && !tc.unavailable) {
						body = `["read","jev"]`
					}
				case "GET /session/status":
					body = tc.status
				case "POST /instance/dispose":
					disposed = true
					body = `true`
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			s := &chatService{directory: directory, serverURL: "http://opencode.test", client: client}
			got := prepareJevToolWithInstaller(context.Background(), s, func() error {
				installed = true
				if tc.installError {
					return errors.New("cannot install")
				}
				return nil
			})
			if got != tc.want || disposed != tc.wantDispose {
				t.Fatalf("prepared=%t, disposed=%t; want %t, %t", got, disposed, tc.want, tc.wantDispose)
			}
			if tc.loaded && installed {
				t.Fatal("loaded tool reinstalled")
			}
		})
	}
}

func TestPrepareJevToolDoesNotInstallWhenDiscoveryFails(t *testing.T) {
	s := &chatService{directory: t.TempDir(), serverURL: "http://opencode.test", client: &http.Client{
		Transport: jevSetupTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Path != "/experimental/tool/ids" {
				t.Fatal("mutated unavailable OpenCode instance")
			}
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
		}),
	}}
	if prepareJevToolWithInstaller(context.Background(), s, func() error {
		t.Fatal("installed tool before a successful discovery")
		return nil
	}) {
		t.Fatal("unavailable OpenCode reported ready")
	}
}
