package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentAudioSavedKeyRequiresAuthenticationBeforeRun(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(`{"projectPath":"unused","elevenLabsUseSavedKey":true}`))
	openCodeRefineHandler(w, r)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Local authentication required") {
		t.Fatalf("saved key access must be rejected before project work: %d", w.Code)
	}
}
