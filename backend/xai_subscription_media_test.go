package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const grokMediaTestSubscription = `{"xai":{"type":"oauth","access":"fixture-grok-access","refresh":"fixture-grok-refresh","expires":4102444800000}}`

func isolateGrokMediaCredentials(t *testing.T, credentials ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	isolateProjectIconCredentials(t, credentials...)
	original := xAIMediaAuthFileCandidates
	xAIMediaAuthFileCandidates = projectIconAuthFileCandidates
	t.Cleanup(func() { xAIMediaAuthFileCandidates = original })
}

func TestGrokSubscriptionMediaEnabledByDefaultWithExplicitOptOut(t *testing.T) {
	for _, value := range []string{"", "0", " 0 ", "1"} {
		t.Run("flag="+value, func(t *testing.T) {
			t.Setenv(grokSubscriptionMediaFlag, value)
			if value == "" {
				if err := os.Unsetenv(grokSubscriptionMediaFlag); err != nil {
					t.Fatal(err)
				}
			}
			if got := grokSubscriptionMediaEnabled(); got != (strings.TrimSpace(value) != "0") {
				t.Fatalf("flag %q enabled=%t", value, got)
			}
		})
	}
}

func TestGrokSubscriptionMediaDefaultRequiresConnectedOpenCodeAccount(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "")
	isolateGrokMediaCredentials(t)
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("unconnected subscription contacted a provider")
		return nil, nil
	})
	for _, source := range studioImageCapabilities(context.Background()).Sources {
		if source.ID == "xai-subscription" && source.Connected {
			t.Fatal("default-enabled source claimed a connected account")
		}
	}
	if _, _, err := resolveProjectIconSource(projectIconRequest{SourceID: "xai-subscription", APIKey: "must-not-fallback"}); err == nil || !strings.Contains(err.Error(), "OpenCode") {
		t.Fatalf("unconnected icon source: %v", err)
	}
	if _, _, err := generateStudioSelectedImage(context.Background(), "xai-subscription", "must-not-fallback", "An orb", "", "1:1"); err == nil || !strings.Contains(err.Error(), "OpenCode") {
		t.Fatalf("unconnected image source: %v", err)
	}
	if _, err := generateStudioSelectedVideo(context.Background(), studioVideoOptions{SourceID: "xai-subscription"}, "must-not-fallback", "An orb moves", nil); err == nil || !strings.Contains(err.Error(), "connected Grok account") {
		t.Fatalf("subscription video accepted API key fallback: %v", err)
	}
	if _, err := generateStudioSelectedVideo(context.Background(), studioVideoOptions{SourceID: "xai-subscription"}, "", "An orb moves", nil); err == nil || !strings.Contains(err.Error(), "OpenCode") {
		t.Fatalf("unconnected video source: %v", err)
	}
}

func TestGrokSubscriptionMediaDefaultGeneratesWithConnectedAccount(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "")
	isolateGrokMediaCredentials(t, grokMediaTestSubscription)
	data := projectIconTestImage(t, "png")
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.x.ai" || r.Header.Get("Authorization") != "Bearer fixture-grok-access" {
			t.Fatal("default-enabled image used the wrong source or credential")
		}
		return iconProviderResponse(data), nil
	})
	if _, _, err := generateStudioSelectedImage(context.Background(), "xai-subscription", "must-not-fallback", "An orb", "", "1:1"); err != nil || calls != 1 {
		t.Fatalf("default subscription image: %v calls=%d", err, calls)
	}
}

func TestGrokSubscriptionMediaDisabledBlocksImageAndLegacyVideoRoutes(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "0")
	isolateGrokMediaCredentials(t, grokMediaTestSubscription)
	isolateStudioProgress(t)
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("disabled subscription contacted a provider")
		return nil, nil
	})
	ctx := context.Background()
	for _, generate := range []struct {
		name string
		call func() error
	}{
		{"icon selection", func() error {
			_, _, err := resolveProjectIconSource(projectIconRequest{SourceID: "xai-subscription", APIKey: "must-not-fallback"})
			return err
		}},
		{"icon", func() error {
			_, _, err := generateProjectIcon(ctx, "xai-subscription", "must-not-fallback", "An orb", "")
			return err
		}},
		{"studio", func() error {
			_, _, err := generateStudioSelectedImage(ctx, "xai-subscription", "must-not-fallback", "An orb", "", "1:1")
			return err
		}},
		{"chat and prototype", func() error {
			_, _, err := generateChatImage(ctx, "xai-subscription", "must-not-fallback", "An orb", "")
			return err
		}},
		{"prototype validation", func() error { return validateChatImageOptions(chatImageOptions{SourceID: "xai-subscription"}) }},
		{"held credential refresh", func() error {
			_, err := refreshXAICredential(xAICredential{Kind: "subscription", Refresh: "fixture-refresh"}, xAIOAuthTokenURL, http.DefaultClient, ctx)
			return err
		}},
		{"implicit media bearer", func() error {
			_, err := withXAIBearerContext(ctx, func(string) (string, error) {
				t.Fatal("disabled subscription was used")
				return "", nil
			})
			return err
		}},
	} {
		t.Run(generate.name, func(t *testing.T) {
			if err := generate.call(); !errors.Is(err, errGrokSubscriptionMediaDisabled) {
				t.Fatalf("disabled route returned %v", err)
			}
		})
	}
	for _, route := range []struct {
		name string
		call http.HandlerFunc
		body string
	}{
		{"explicit Studio image", studioImageGenerateHandler, `{"prompt":"An orb","sourceId":"xai-subscription"}`},
		{"legacy Studio image", studioImageGenerateHandler, `{"prompt":"An orb"}`},
		{"Studio video", studioVideoGenerateHandler, `{"prompt":"An orb moves"}`},
	} {
		t.Run(route.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.call(w, studioProgressRequest(http.MethodPost, "/fixture/generate", route.body))
			if w.Code < 400 || !strings.Contains(w.Body.String(), "subscription images and videos are disabled") {
				t.Fatalf("disabled HTTP route: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestGrokSubscriptionMediaDisabledDoesNotDecodeOAuthTokens(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "0")
	isolateGrokMediaCredentials(t, `{"xai":{"type":"oauth","access":123,"refresh":456}}`)
	paths := projectIconAuthFileCandidates()
	credential, connected, err := readXAIStoredCredential(paths[0])
	if err != nil || connected || credential.Bearer != "" || credential.Refresh != "" {
		t.Fatalf("disabled OAuth credential was decoded: connected=%t err=%v", connected, err)
	}
	if _, connected := projectIconSubscription(); connected {
		t.Fatal("disabled subscription discovered")
	}
}

func TestGrokSubscriptionMediaCatalogAndStatusFollowFlag(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "default"}[enabled], func(t *testing.T) {
			t.Setenv(grokSubscriptionMediaFlag, map[bool]string{false: "0", true: ""}[enabled])
			isolateGrokMediaCredentials(t, grokMediaTestSubscription)
			w := httptest.NewRecorder()
			openCodeIconSourcesHandler(w, httptest.NewRequest(http.MethodGet, "/opencode/project/icon/sources", nil))
			var catalog struct {
				Sources     []projectIconSource `json:"sources"`
				Recommended string              `json:"recommendedSource"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, source := range catalog.Sources {
				if source.ID == "xai-subscription" {
					found = true
					if !source.Available {
						t.Fatal("connected source unavailable")
					}
				}
			}
			if found != enabled || (catalog.Recommended == "xai-subscription") != enabled {
				t.Fatalf("catalog does not follow flag: %s", w.Body.String())
			}
			w = httptest.NewRecorder()
			studioImagesStatusHandler(w, httptest.NewRequest(http.MethodGet, "/studio/images/status", nil))
			var status struct {
				Connected  bool   `json:"connected"`
				Credential string `json:"credential"`
				Enabled    bool   `json:"grokSubscriptionMediaEnabled"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Connected != enabled || status.Enabled != enabled || (status.Credential == "subscription") != enabled {
				t.Fatalf("status does not follow flag: %s", w.Body.String())
			}
			for _, secret := range []string{"fixture-grok-access", "fixture-grok-refresh"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("status exposed credentials")
				}
			}
		})
	}
}

func TestGrokSubscriptionMediaDisabledStillAllowsAPIKeys(t *testing.T) {
	for _, fromEnv := range []bool{false, true} {
		t.Run(map[bool]string{false: "stored", true: "environment"}[fromEnv], func(t *testing.T) {
			t.Setenv(grokSubscriptionMediaFlag, "0")
			isolateGrokMediaCredentials(t, grokMediaTestSubscription, `{"xai":{"type":"api","key":"fixture-xai-key"}}`)
			if fromEnv {
				t.Setenv("XAI_API_KEY", "fixture-xai-env-key")
			}
			credential, err := resolveXAICredential(false)
			if err != nil || credential.Kind != "api-key" {
				t.Fatalf("API key unavailable: %v", err)
			}
			data := projectIconTestImage(t, "png")
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "api.x.ai" || r.Header.Get("Authorization") != "Bearer "+credential.Bearer {
					t.Fatal("incorrect API credential")
				}
				return iconProviderResponse(data), nil
			})
			_, _, err = generateStudioSelectedImage(context.Background(), "xai-api", credential.Bearer, "An orb", "", "1:1")
			if err != nil || calls != 1 {
				t.Fatalf("API image failed: %v", err)
			}
			_, err = withXAIBearer(func(bearer string) (string, error) {
				if bearer != credential.Bearer {
					t.Fatal("implicit video/image selected OAuth")
				}
				return "fixture-video", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			studioImagesStatusHandler(w, httptest.NewRequest(http.MethodGet, "/studio/images/status", nil))
			if !strings.Contains(w.Body.String(), `"connected":true`) || !strings.Contains(w.Body.String(), `"credential":"api-key"`) {
				t.Fatal("API key status unavailable")
			}
		})
	}
}

func TestGrokSubscriptionMediaDefaultPreservesStoredCredentialPreference(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "default"}[enabled], func(t *testing.T) {
			t.Setenv(grokSubscriptionMediaFlag, map[bool]string{false: "0", true: ""}[enabled])
			isolateGrokMediaCredentials(t, grokMediaTestSubscription)
			t.Setenv("XAI_API_KEY", "fixture-environment-key")
			credential, err := resolveXAICredential(false)
			if err != nil {
				t.Fatal(err)
			}
			if enabled {
				if credential.Kind != "subscription" || credential.Bearer != "fixture-grok-access" {
					t.Fatal("subscription media silently selected API billing")
				}
			} else if credential.Kind != "api-key" || credential.Bearer != "fixture-environment-key" {
				t.Fatal("disabled subscription media did not use available API key")
			}
		})
	}
}

func TestGrokSubscriptionMediaDisabledKeepsSavedAssets(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "0")
	isolateGrokMediaCredentials(t, grokMediaTestSubscription)
	image := "data:image/png;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))
	saved, err := saveStudioImage("Saved orb", image)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	studioImageContentHandler(w, httptest.NewRequest(http.MethodGet, "/studio/images/content?id="+saved.ID, nil))
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatal("saved image unavailable")
	}
	video, err := saveStudioAsset(studioSaveOptions{Prompt: "Saved clip", DataURI: "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte(studioRecoveryMP4)), MediaType: "video", Source: xAIVideoSourceLabel})
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	studioVideoContentHandler(w, httptest.NewRequest(http.MethodGet, "/studio/videos/content?id="+video.ID, nil))
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatal("saved video unavailable")
	}
}
