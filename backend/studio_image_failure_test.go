package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStudioImageFailureClassificationPreservesSafeMessages(t *testing.T) {
	const fallback = "The selected image source could not finish."
	for _, tc := range []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{"reconnect", &codexImageFailure{Code: "reconnect"}, http.StatusPreconditionFailed, "Reconnect ChatGPT in OpenCode or Codex to generate images."},
		{"refresh", &codexImageFailure{Code: "refresh"}, http.StatusPreconditionFailed, "ChatGPT could not refresh its connection. Reconnect in OpenCode or Codex."},
		{"save", &codexImageFailure{Code: "save"}, http.StatusPreconditionFailed, "The refreshed ChatGPT connection could not be saved. Reconnect in OpenCode or Codex."},
		{"limits", &codexImageFailure{Code: "limits"}, http.StatusTooManyRequests, "ChatGPT image generation reached its usage limit. Try again later or choose another image source."},
		{"subscription", &codexImageFailure{Code: "subscription"}, http.StatusBadGateway, "ChatGPT could not authorize image generation. Check its subscription connection or choose another image source."},
		{"app server", &codexAppServerImageFailure{Code: "login"}, http.StatusBadGateway, "Sign in with ChatGPT in Codex to try image generation."},
		{"Glowbom", glowbomImageError(&accountCommandFailure{code: "allowance_required"}), http.StatusBadGateway, "Glowbom image generation was denied. Check your account allowance."},
		{"disabled", errGrokSubscriptionMediaDisabled, http.StatusPreconditionFailed, errGrokSubscriptionMediaDisabled.Error()},
		{"xAI", errors.New("Connect xAI in OpenCode first"), http.StatusPreconditionFailed, "Connect xAI in OpenCode first"},
		{"untrusted xAI diagnostic", errors.New("Reconnect xAI: private-provider-token"), http.StatusPreconditionFailed, "Reconnect xAI in OpenCode."},
		{"unknown", errors.New("private-provider-token and personal data"), http.StatusBadGateway, fallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeStudioProviderError(w, tc.err, fallback)
			if w.Code != tc.status || strings.TrimSpace(w.Body.String()) != tc.want {
				t.Fatalf("status=%d message=%q, want %d %q", w.Code, w.Body.String(), tc.status, tc.want)
			}
		})
	}
	wrapped := fmt.Errorf("private-provider-token: %w", &codexImageFailure{Code: "limits"})
	message, status := studioProviderError(wrapped, fallback)
	if status != http.StatusTooManyRequests || strings.Contains(message, "private-provider-token") {
		t.Fatal("wrapped provider diagnostics escaped the safe classifier")
	}
}

func assertStudioImageFailureRecovery(t *testing.T, id, prompt, referenceID, want string) {
	t.Helper()
	for _, reloaded := range []bool{false, true} {
		if reloaded {
			studioGenerations = newStudioProgressStore()
		}
		state, body := readStudioRecoveryStatus(t, id)
		if !state.Found || state.Stage != "failed" || state.Error != want || state.Prompt != prompt || state.ReferenceID != referenceID {
			t.Fatalf("saved failure changed after reload=%v: %+v", reloaded, state)
		}
		if strings.Contains(body, "private-provider-token") || state.CanResume {
			t.Fatal("failed image status exposed private diagnostics or offered an unsafe retry")
		}
	}
}

func TestStudioImageFailureChatGPTEditSurvivesStatusAndRestart(t *testing.T) {
	for _, tc := range []struct {
		providerStatus int
		wantStatus     int
		code           string
	}{
		{http.StatusUnauthorized, http.StatusPreconditionFailed, "reconnect"},
		{http.StatusForbidden, http.StatusBadGateway, "subscription"},
		{http.StatusTooManyRequests, http.StatusTooManyRequests, "limits"},
		{http.StatusInternalServerError, http.StatusBadGateway, "request"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			isolateStudioProgress(t)
			isolateProjectIconCredentials(t, `{"openai":{"type":"oauth","access":"fixture-access","account_id":"fixture-account"}}`)
			t.Setenv("GLOWBOM_CHATGPT_IMAGE_TRANSPORT", "")
			id, prompt := "fixture-chatgpt-image-failure", "Make the reference blue"
			referenceID := studioRecoveryReference(t)
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				var body map[string]json.RawMessage
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/edits") || json.NewDecoder(r.Body).Decode(&body) != nil || body["images"] == nil {
					t.Fatal("image editing lost its reference or called another provider route")
				}
				return codexImageTestResponse(tc.providerStatus, `{"error":"private-provider-token and personal data"}`), nil
			})
			w := httptest.NewRecorder()
			body := studioRecoveryJSON(t, map[string]string{"generationId": id, "prompt": prompt, "sourceId": "openai-subscription", "modelId": "gpt-image-2", "referenceId": referenceID})
			studioImageGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
			want := (&codexImageFailure{Code: tc.code}).Error()
			if w.Code != tc.wantStatus || strings.TrimSpace(w.Body.String()) != want || calls != 1 {
				t.Fatalf("unexpected response or provider replay: status=%d body=%q calls=%d", w.Code, w.Body.String(), calls)
			}
			assertStudioImageFailureRecovery(t, id, prompt, referenceID, want)
		})
	}
}

func TestStudioImageFailureGlowbomSurvivesStatusAndRestart(t *testing.T) {
	isolateStudioProgress(t)
	isolateProjectIconCredentials(t)
	calls := 0
	bridge := mockGlowbomImageAccount(t, func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "account" {
			return glowbomSignedIn(), nil
		}
		calls++
		return nil, &accountCommandFailure{code: "allowance_required"}
	})
	bridge.token = "studio-progress-test"
	id, prompt := "fixture-glowbom-image-failure", "A blue bird"
	w := httptest.NewRecorder()
	body := studioRecoveryJSON(t, map[string]string{"generationId": id, "prompt": prompt, "sourceId": "glowbom-api"})
	studioImageGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
	want := "Glowbom image generation was denied. Check your account allowance."
	if w.Code != http.StatusBadGateway || strings.TrimSpace(w.Body.String()) != want || calls != 1 {
		t.Fatalf("unexpected response or provider replay: status=%d body=%q calls=%d", w.Code, w.Body.String(), calls)
	}
	assertStudioImageFailureRecovery(t, id, prompt, "", want)
}

func TestStudioImageFailureCodexAppServerSurvivesStatusAndRestart(t *testing.T) {
	isolateStudioProgress(t)
	isolateProjectIconCredentials(t)
	mockCodexAppServerImages(t, "terminal-error")
	t.Setenv("GLOWBOM_CHATGPT_IMAGE_TRANSPORT", "codex-app-server")
	id, prompt := "fixture-codex-image-failure", "A blue bird"
	w := httptest.NewRecorder()
	body := studioRecoveryJSON(t, map[string]string{"generationId": id, "prompt": prompt, "sourceId": "openai-subscription"})
	studioImageGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
	want := (&codexAppServerImageFailure{Code: "request"}).Error()
	if w.Code != http.StatusBadGateway || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("unexpected Codex response: status=%d body=%q", w.Code, w.Body.String())
	}
	assertStudioImageFailureRecovery(t, id, prompt, "", want)
}

func TestStudioImageFailureKeepsExplicitCancellationStopped(t *testing.T) {
	isolateStudioProgress(t)
	isolateProjectIconCredentials(t)
	id, prompt := "fixture-stopped-image-failure", "A blue bird"
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		if _, err := studioGenerations.stop(id); err != nil {
			t.Fatal(err)
		}
		return nil, errors.New("private-provider-token and personal data")
	})
	w := httptest.NewRecorder()
	body := studioRecoveryJSON(t, map[string]string{"generationId": id, "prompt": prompt, "sourceId": "openai-api", "apiKey": "fixture-api-key"})
	studioImageGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
	state, _ := readStudioRecoveryStatus(t, id)
	if state.Stage != "stopped" || state.Error != "Stopped locally. The provider may still be working." {
		t.Fatalf("provider failure replaced explicit cancellation: %+v", state)
	}
}

func TestStudioImageFailureUnknownProviderDiagnosticsStayPrivate(t *testing.T) {
	isolateStudioProgress(t)
	isolateProjectIconCredentials(t)
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private-provider-token and personal data")
	})
	id, prompt := "fixture-unknown-image-failure", "A blue bird"
	w := httptest.NewRecorder()
	body := studioRecoveryJSON(t, map[string]string{"generationId": id, "prompt": prompt, "sourceId": "openai-api", "apiKey": "fixture-api-key"})
	studioImageGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
	want := "The selected source could not generate this image. Check its connection or API key, then try again."
	if w.Code != http.StatusBadGateway || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("unsafe failure response: status=%d body=%q", w.Code, w.Body.String())
	}
	assertStudioImageFailureRecovery(t, id, prompt, "", want)
}
