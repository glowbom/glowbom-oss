package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Desktop serves packaged assets and the existing API on one loopback origin.
// The token is supplied by the native window, never by public HTML or config.
func desktopHandler(api http.Handler) (http.Handler, error) {
	if os.Getenv("GLOWBOM_DESKTOP") != "1" {
		return withGlowbomSecurity(api), nil
	}
	root := os.Getenv("GLOWBOM_WEB_DIR")
	if !filepath.IsAbs(root) || glowbomServerToken() == "" || backendBindHost() != "127.0.0.1" {
		return nil, errors.New("invalid desktop startup configuration")
	}
	if info, err := os.Stat(filepath.Join(root, "index.html")); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("packaged interface is missing")
	}
	secureAPI := withGlowbomSecurity(api)
	files := http.FileServer(http.Dir(root))
	expectedHost := "127.0.0.1:" + os.Getenv("GLOWBOM_PORT")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != expectedHost {
			http.Error(w, "Host not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.StripPrefix("/api", secureAPI).ServeHTTP(w, r)
			return
		}
		// Preserve direct backend routes for local companions and OAuth callbacks.
		// The packaged web interface uses /api; both paths share authentication.
		if routes, ok := api.(*http.ServeMux); ok {
			_, pattern := routes.Handler(r)
			if pattern != "" && pattern != "/" && pattern != "/favicon.png" && pattern != "/logo-svg.svg" {
				secureAPI.ServeHTTP(w, r)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", 405)
			return
		}
		// The workspace is a single-page app. No directory listings or hidden files.
		clean := filepath.Clean("/" + r.URL.Path)
		for _, part := range strings.Split(clean, "/") {
			if strings.HasPrefix(part, ".") {
				http.NotFound(w, r)
				return
			}
		}
		path := filepath.Join(root, clean)
		info, err := os.Stat(path)
		if clean == "/" {
			path = filepath.Join(root, "index.html")
		} else if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		if clean == "/" || clean == "/index.html" {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' https: ipc: http://ipc.localhost; style-src 'self' 'unsafe-inline' https:; font-src 'self' data: https:; img-src 'self' data: blob: https:; media-src 'self' blob: data: https:; connect-src 'self' ipc: http://ipc.localhost https:; frame-src 'self' blob: https: http://127.0.0.1:*; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
			http.ServeFile(w, r, path)
			return
		}
		files.ServeHTTP(w, r)
	}), nil
}
