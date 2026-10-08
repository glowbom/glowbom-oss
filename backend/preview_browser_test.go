package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPreviewBrowserCommandsUseArgumentsAndLocalURLs(t *testing.T) {
	address := "http://127.0.0.1:41234/?glowbom_preview=test-token"
	for _, tc := range []struct {
		platform string
		name     string
		args     []string
	}{
		{"darwin", "open", []string{address}},
		{"windows", "rundll32.exe", []string{"url.dll,FileProtocolHandler", address}},
		{"linux", "xdg-open", []string{address}},
	} {
		name, args, err := previewBrowserCommand(tc.platform, address)
		if err != nil || name != tc.name || !reflect.DeepEqual(args, tc.args) {
			t.Fatalf("%s: %s %#v %v", tc.platform, name, args, err)
		}
	}
	for _, address := range []string{"https://example.com/", "file:///tmp/test", "javascript:alert(1)", "http://localhost.evil:1234/", "http://secret@localhost:1234/", "http://localhost/", "http://localhost:0/", "http://127.0.0.1:65536/", "-a Safari"} {
		if _, _, err := previewBrowserCommand("darwin", address); err == nil {
			t.Fatalf("accepted unsafe preview address %q", address)
		}
	}
	if _, _, err := previewBrowserCommand("unsupported", address); err == nil {
		t.Fatal("unsupported platform accepted")
	}
}

func TestPreviewBrowserOpensOnlyManagedRunningSession(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	previewFixture(t, project, "prototype/index.html", "<h1>Preview</h1>")
	manager := newProjectPreviewManager()
	var opened []string
	manager.openBrowser = func(_ context.Context, address string) error { opened = append(opened, address); return nil }
	request := previewRequest{Path: project, Target: "prototype", Action: "browser"}
	if _, status := previewCall(t, manager, request); status != http.StatusConflict {
		t.Fatalf("missing session status %d", status)
	}
	address := "http://127.0.0.1:41234/?glowbom_preview=test-token"
	session := &previewSession{view: previewTarget{Target: "prototype", Status: "running", URL: address}, project: project}
	manager.sessions[project+"/prototype"] = session
	// Extra URL fields never choose what the operating system opens.
	body, _ := json.Marshal(map[string]string{"path": project, "target": "prototype", "action": "browser", "url": "https://example.com/untrusted"})
	recorder := httptest.NewRecorder()
	manager.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/preview", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK || !reflect.DeepEqual(opened, []string{address}) {
		t.Fatalf("managed preview not opened: status %d, calls %#v", recorder.Code, opened)
	}
	for _, view := range []previewTarget{
		{Status: "starting", URL: address},
		{Status: "failed", URL: address},
		{Status: "running", URL: "https://example.com/"},
	} {
		session.view = view
		if _, status := previewCall(t, manager, request); status != http.StatusConflict {
			t.Fatalf("invalid session status %d", status)
		}
	}
	if len(opened) != 1 {
		t.Fatalf("invalid sessions opened: %#v", opened)
	}
	request.Target = "../prototype"
	if _, status := previewCall(t, manager, request); status != http.StatusBadRequest {
		t.Fatalf("unknown target status %d", status)
	}
}

func TestPreviewBrowserFailureDoesNotExposeCommandOutput(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	previewFixture(t, project, "prototype/index.html", "<h1>Preview</h1>")
	manager := newProjectPreviewManager()
	manager.sessions[project+"/prototype"] = &previewSession{view: previewTarget{Status: "running", URL: "http://127.0.0.1:41234/?glowbom_preview=private-token"}, project: project}
	manager.openBrowser = func(_ context.Context, address string) error { return errors.New(address) }
	body, _ := json.Marshal(previewRequest{Path: project, Target: "prototype", Action: "browser"})
	recorder := httptest.NewRecorder()
	manager.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/preview", bytes.NewReader(body)))
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "default browser") || strings.Contains(recorder.Body.String(), "private-token") {
		t.Fatalf("unsafe failure response: %d %s", recorder.Code, recorder.Body.String())
	}
}
