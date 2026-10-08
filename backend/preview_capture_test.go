package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewCaptureOrigin(t *testing.T) {
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "")
	t.Setenv("GLOWBY_ALLOWED_ORIGINS", "")
	for _, origin := range []string{"http://127.0.0.1:4572", "http://localhost:5173", "http://[::1]:8080"} {
		if !validPreviewCaptureOrigin(origin) {
			t.Errorf("rejected local app origin: %s", origin)
		}
	}
	for _, origin := range []string{"", "null", "https://example.com", "http://127.0.0.1.evil.test:80", "http://127.0.0.1:4572/path", "http://127.0.0.1:4572?test=1", "http://user@localhost:5173", "http://localhost", "http://localhost:99999"} {
		if validPreviewCaptureOrigin(origin) {
			t.Errorf("accepted invalid app origin: %s", origin)
		}
	}
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://localhost:4572")
	if validPreviewCaptureOrigin("http://localhost:5173") || !validPreviewCaptureOrigin("http://localhost:4572") {
		t.Fatal("capture did not respect the configured origin allowlist")
	}
}

func TestPreviewCaptureIsAuthenticatedAndDoesNotChangeProject(t *testing.T) {
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://localhost:4572")
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	html := "<!doctype html><html><body>Personal prototype<img src='me.png'></body></html>"
	previewFixture(t, project, "index.html", html)
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const token = "123456789012capture-token"
	handler := staticPreviewHandler(root, token, 5001)
	request := func(path string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://localhost:5001"+path, nil)
		if auth {
			r.AddCookie(&http.Cookie{Name: "glowbom_preview_" + token[:12], Value: token})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := "/__glowbom_preview__/capture.js?parent=" + url.QueryEscape("http://localhost:4572")
	for _, asset := range []string{path, "/__glowbom_preview__/html2canvas-pro-2.4.5.js"} {
		if got := request(asset, false).Code; got != http.StatusUnauthorized {
			t.Fatalf("unauthenticated capture asset status: %d", got)
		}
		response := request(asset, true)
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "javascript") {
			t.Fatalf("capture asset failed: %d", response.Code)
		}
	}
	response := request(path, true)
	if !strings.Contains(response.Body.String(), `const parentOrigin = "http://localhost:4572"`) || strings.Contains(response.Body.String(), "__GLOWBOM_PARENT_ORIGIN__") {
		t.Fatal("bridge does not bind to the exact parent origin")
	}
	if request("/__glowbom_preview__/capture.js?parent=https://untrusted.test", true).Code != http.StatusForbidden {
		t.Fatal("bridge accepted a remote parent")
	}
	if strings.Contains(request("/", true).Body.String(), "capture.js") {
		t.Fatal("standard preview unexpectedly loaded capture tools")
	}
	response = request("/?glowbom_capture_origin="+url.QueryEscape("http://localhost:4572"), true)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "/__glowbom_preview__/capture.js?parent=") {
		t.Fatal("area editing did not inject the bridge")
	}
	if strings.Contains(request("/?glowbom_capture_origin=https://untrusted.test", true).Body.String(), "capture.js") {
		t.Fatal("area editing injected an untrusted parent")
	}
	saved, _ := os.ReadFile(filepath.Join(project, "index.html"))
	if string(saved) != html {
		t.Fatal("capture modified the generated source")
	}
}
