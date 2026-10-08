package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexImageNativeTransportDoesNotReadLegacyCredentialsOrFallback(t *testing.T) {
	t.Setenv("GLOWBOM_CHATGPT_IMAGE_TRANSPORT", "codex-app-server")
	t.Setenv("GLOWBOM_CODEX_BIN", filepath.Join(t.TempDir(), "missing-codex"))
	resetProbe := func() {
		codexAppServerImageProbeCache.Lock()
		codexAppServerImageProbeCache.until = time.Time{}
		codexAppServerImageProbeCache.Unlock()
	}
	resetProbe()
	t.Cleanup(resetProbe)
	original := codexImageCodexAuthFilePath
	codexImageCodexAuthFilePath = func() string {
		t.Fatal("native transport tried to read legacy credentials")
		return ""
	}
	t.Cleanup(func() { codexImageCodexAuthFilePath = original })
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("native transport fell back to direct HTTP")
		return nil, nil
	})
	if available, code := codexImageSourceAvailability(context.Background()); available || code != "codex_cli" {
		t.Fatalf("missing CLI readiness = %v, %q", available, code)
	}
	if _, _, err := resolveProjectIconSource(projectIconRequest{SourceID: "openai-subscription", APIKey: "must-not-fallback"}); err == nil {
		t.Fatal("missing CLI source was allowed")
	}
	_, err := callCodexImageGeneration(context.Background(), "test image", "", "1:1")
	var failure *codexAppServerImageFailure
	if !errors.As(err, &failure) || failure.Code != "cli" {
		t.Fatalf("missing CLI generation error = %v", err)
	}
}

func TestCodexImageUnknownTransportCannotUseDefaultConnection(t *testing.T) {
	t.Setenv("GLOWBOM_CHATGPT_IMAGE_TRANSPORT", "codex-app-sever")
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("unknown transport attempted a provider request")
		return nil, nil
	})
	if available, code := codexImageSourceAvailability(context.Background()); available || code != "codex_transport" {
		t.Fatalf("unknown transport readiness = %v, %q", available, code)
	}
	if _, _, err := resolveProjectIconSource(projectIconRequest{SourceID: "openai-subscription"}); err == nil {
		t.Fatal("unknown transport source was allowed")
	}
	if _, err := callCodexImageGeneration(context.Background(), "test image", "", "1:1"); err == nil {
		t.Fatal("unknown transport was accepted")
	}
}
