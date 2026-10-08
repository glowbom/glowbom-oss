package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// html2canvas-pro 2.4.5 is distributed under the adjacent MIT license.
// These assets are embedded so the public OSS backend stays self-contained.
//
//go:embed preview_assets/html2canvas-pro-2.4.5.min.js
var previewHTML2Canvas []byte

//go:embed preview_assets/capture.js
var previewCaptureScript string

func validPreviewCaptureOrigin(raw string) bool {
	origin, err := url.Parse(raw)
	if err != nil || origin.Scheme != "http" || !isLoopbackHost(origin.Hostname()) || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	port, err := strconv.Atoi(origin.Port())
	if err != nil || port < 1 || port > 65535 || origin.String() != raw {
		return false
	}
	if allowed := glowbomAllowedOrigins(); len(allowed) > 0 {
		_, ok := allowed[raw]
		return ok
	}
	return true
}

// Called only after the preview's host and access-cookie checks.
func servePreviewCaptureAsset(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/__glowbom_preview__/html2canvas-pro-2.4.5.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write(previewHTML2Canvas)
		}
		return true
	case "/__glowbom_preview__/capture.js":
		origin := r.URL.Query().Get("parent")
		if !validPreviewCaptureOrigin(origin) {
			http.Error(w, "Capture origin not allowed", http.StatusForbidden)
			return true
		}
		encoded, _ := json.Marshal(origin)
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(strings.Replace(previewCaptureScript, "__GLOWBOM_PARENT_ORIGIN__", string(encoded), 1)))
		}
		return true
	}
	return false
}

func previewCaptureTag(r *http.Request) string {
	origin := r.URL.Query().Get("glowbom_capture_origin")
	if !validPreviewCaptureOrigin(origin) {
		return ""
	}
	return `<script src="/__glowbom_preview__/capture.js?parent=` + url.QueryEscape(origin) + `"></script>`
}
