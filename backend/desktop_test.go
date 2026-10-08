package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopPackagedInterface(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "index.html"), []byte("workspace"), 0600)
	os.WriteFile(filepath.Join(root, ".env"), []byte("not public"), 0600)
	t.Setenv("GLOWBOM_DESKTOP", "1")
	t.Setenv("GLOWBOM_WEB_DIR", root)
	t.Setenv("GLOWBOM_PORT", "4587")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://127.0.0.1:4587")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "test-secret")
	handler, err := desktopHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/test" {
			t.Errorf("API path = %s", r.URL.Path)
		}
		w.WriteHeader(204)
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		path, token, origin, host string
		code                      int
	}{
		{"/", "", "", "", 200},
		{"/.env", "", "", "", 404},
		{"/missing", "", "", "", 404},
		{"/api/test", "", "", "", 401},
		{"/api/test", "test-secret", "http://evil.example", "", 403},
		{"/api/test", "test-secret", "http://127.0.0.1:4587", "", 204},
		{"/", "", "", "evil.example", 403},
	} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:4587"+tt.path, nil)
		if tt.host != "" {
			r.Host = tt.host
		}
		if tt.token != "" {
			r.Header.Set("Authorization", "Bearer "+tt.token)
		}
		if tt.origin != "" {
			r.Header.Set("Origin", tt.origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tt.code {
			t.Errorf("%s got %d, want %d", tt.path, w.Code, tt.code)
		}
		if tt.path == "/" && tt.code == 200 && w.Header().Get("Content-Security-Policy") == "" {
			t.Error("missing CSP")
		}
	}
}

func TestDesktopRejectsMissingAssetsAndUnauthenticatedStartup(t *testing.T) {
	t.Setenv("GLOWBOM_DESKTOP", "1")
	t.Setenv("GLOWBOM_WEB_DIR", t.TempDir())
	t.Setenv("GLOWBOM_SERVER_TOKEN", "")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	if _, err := desktopHandler(http.NotFoundHandler()); err == nil {
		t.Fatal("accepted unsafe configuration")
	}
}

func TestDesktopCompanionRoutesKeepAuthentication(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "index.html"), []byte("workspace"), 0600)
	t.Setenv("GLOWBOM_DESKTOP", "1")
	t.Setenv("GLOWBOM_WEB_DIR", root)
	t.Setenv("GLOWBOM_PORT", "4587")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture")
	mux := http.NewServeMux()
	mux.HandleFunc("/buzz/session", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h, err := desktopHandler(mux)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/buzz/session", "/api/buzz/session"} {
		for _, authenticated := range []bool{false, true} {
			r := httptest.NewRequest("GET", "http://127.0.0.1:4587"+path, nil)
			want := 401
			if authenticated {
				r.Header.Set("Authorization", "Bearer fixture")
				want = 204
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != want {
				t.Errorf("%s authenticated=%v got %d want %d", path, authenticated, w.Code, want)
			}
		}
	}
}
