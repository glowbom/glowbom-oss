package main

import (
	"net/http/httptest"
	"testing"
)

func TestImageSourceAcceptsCurrentAndLegacyNames(t *testing.T) {
	for _, source := range []string{
		"Glowbom Images",
		"Glowbom Images (Nano Banana 2)",
		"Glowby Images",
		"Glowby Images (gpt-image-2)",
	} {
		if !isGlowbomImagesSource(source) {
			t.Errorf("isGlowbomImagesSource(%q) = false, want true", source)
		}
	}
}

func TestServerTokenAcceptsCurrentAndLegacyHeaders(t *testing.T) {
	for _, header := range []string{"X-Glowbom-Token", "X-Glowby-Token"} {
		request := httptest.NewRequest("GET", "http://127.0.0.1:4569/opencode/health", nil)
		request.Header.Set(header, "secret")
		if !hasValidGlowbomServerToken(request, "secret") {
			t.Errorf("hasValidGlowbomServerToken rejected %s", header)
		}
	}
}

func TestGlowbomEnvironmentTakesPriority(t *testing.T) {
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.2")
	t.Setenv("GLOWBY_BIND_HOST", "127.0.0.3")
	if got := backendBindHost(); got != "127.0.0.2" {
		t.Fatalf("backendBindHost = %q, want 127.0.0.2", got)
	}

	t.Setenv("GLOWBOM_SERVER_TOKEN", "new-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "legacy-token")
	if got := glowbomServerToken(); got != "new-token" {
		t.Fatalf("glowbomServerToken = %q, want new-token", got)
	}
}
