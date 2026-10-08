package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestStudioVideoOptionsValidateBeforeProvider(t *testing.T) {
	bad := []studioVideoOptions{
		{SourceID: "xai-api", ModelID: "grok-imagine-video-1.5", DurationSeconds: 16},
		{SourceID: "xai-api", ModelID: "grok-imagine-video", Resolution: "1080p"},
		{SourceID: "veo-api", DurationSeconds: 5},
		{SourceID: "veo-api", DurationSeconds: 4, Resolution: "1080p"},
		{SourceID: "veo-api", AspectRatio: "1:1"},
		{SourceID: "xai-subscription", ModelID: "unsupported-grok-video-model"},
		{SourceID: "xai-api", ModelID: "unpriced-custom-model"},
	}
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid video settings contacted provider")
		return nil, nil
	})
	for _, options := range bad {
		if _, err := generateStudioSelectedVideo(context.Background(), options, "fixture-key", "A tiny clip", nil); err == nil {
			t.Fatalf("accepted invalid options: %#v", options)
		}
	}
	for _, source := range []string{"xai-api", "veo-api", "xai-subscription"} {
		options, err := normalizeStudioVideoOptions(studioVideoOptions{SourceID: source})
		if err != nil || options.DurationSeconds == 0 || options.ModelID == "" || options.Resolution == "" {
			t.Fatalf("missing explicit defaults: %#v %v", options, err)
		}
	}
}

func TestStudioVideoGrokLatestAndOlderPayloadNoFallback(t *testing.T) {
	for _, model := range []string{"grok-imagine-video-1.5", "grok-imagine-video"} {
		t.Run(model, func(t *testing.T) {
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Fatal("invalid body")
				}
				if body["model"] != model || body["duration"] != float64(5) || body["resolution"] != "480p" || body["aspect_ratio"] != "16:9" || body["image"] != nil {
					t.Fatalf("incorrect explicit text-only video: %#v", body)
				}
				return studioRecoveryResponse(422, "application/json", `{"error":"fixture-key private provider body"}`), nil
			})
			_, err := generateStudioSelectedVideo(context.Background(), studioVideoOptions{SourceID: "xai-api", ModelID: model}, "fixture-key", "A tiny clip", nil)
			if err == nil || calls != 1 || strings.Contains(err.Error(), "fixture-key") {
				t.Fatalf("start retried or leaked provider secret: %d %v", calls, err)
			}
		})
	}
}

func TestStudioVideoVeoEveryModelSendsLengthAndResolution(t *testing.T) {
	for _, model := range []string{"veo-3.1-lite-generate-preview", "veo-3.1-fast-generate-preview", "veo-3.1-generate-preview"} {
		t.Run(model, func(t *testing.T) {
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/v1beta/models/"+model+":predictLongRunning" || r.Header.Get("x-goog-api-key") != "fixture-google-key" {
					t.Fatalf("unexpected Google endpoint: %s", r.URL.Path)
				}
				var body struct {
					Parameters struct {
						DurationSeconds int    `json:"durationSeconds"`
						Resolution      string `json:"resolution"`
						AspectRatio     string `json:"aspectRatio"`
					} `json:"parameters"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Parameters.DurationSeconds != 8 || body.Parameters.Resolution != "1080p" || body.Parameters.AspectRatio != "9:16" {
					t.Fatalf("Google length/resolution omitted: %#v", body)
				}
				return studioRecoveryResponse(200, "application/json", `{"name":"models/`+model+`/operations/fixture-operation"}`), nil
			})
			response, err := generateStudioSelectedVideo(context.Background(), studioVideoOptions{SourceID: "veo-api", ModelID: model, DurationSeconds: 8, Resolution: "1080p", AspectRatio: "9:16"}, "fixture-google-key", "A tiny clip", nil)
			if err != nil || response.OperationID == "" || calls != 1 {
				t.Fatalf("Google generation: %#v %v calls%d", response, err, calls)
			}
		})
	}
}

func TestStudioVideoSubscriptionExplicitWithoutImageFlag(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "0")
	isolateGrokMediaCredentials(t, grokMediaTestSubscription)
	found := false
	for _, source := range studioVideoCapabilities().Sources {
		if source.ID == "xai-subscription" {
			found = source.Connected && !source.Experimental
		}
	}
	if !found {
		t.Fatal("explicit subscription video hidden without image flag")
	}
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fixture-grok-access" {
			t.Fatal("subscription did not use selected credential")
		}
		return studioRecoveryResponse(200, "application/json", `{"request_id":"fixture-sub-video"}`), nil
	})
	if _, err := generateStudioSelectedVideo(context.Background(), studioVideoOptions{SourceID: "xai-subscription"}, "", "An orb moves", nil); err != nil || calls != 1 {
		t.Fatalf("public explicit subscription video: %v calls%d", err, calls)
	}
}

func TestStudioVideoMetadataAndSessionKeyStayOutOfRecovery(t *testing.T) {
	isolateStudioRecovery(t)
	id := "fixture-video-exact-settings"
	posts := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost:
			posts++
			return studioRecoveryResponse(200, "application/json", `{"request_id":"`+studioRecoveryOperation+`"}`), nil
		case strings.Contains(r.URL.Path, "/v1/videos/"):
			return studioRecoveryResponse(200, "application/json", `{"status":"done","video":{"url":"`+studioRecoveryURL+`","duration":5}}`), nil
		case r.URL.String() == studioRecoveryURL:
			return nil, errors.New("download temporarily unavailable")
		}
		return nil, errors.New("unexpected request")
	})
	body := studioRecoveryJSON(t, map[string]any{"generationId": id, "prompt": "A tiny clip", "sourceId": "xai-api", "modelId": "grok-imagine-video-1.5", "durationSeconds": 5, "resolution": "1080p", "apiKey": "private-session-override"})
	w := httptest.NewRecorder()
	studioVideoGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/videos/generate", body))
	state := studioGenerations.read(id)
	if posts != 1 || state.SourceID != "xai-api" || state.ModelID != "grok-imagine-video-1.5" || state.Resolution != "1080p" || state.DurationSeconds != 5 || state.ActualDurationSeconds != 5 || !state.CanResume {
		t.Fatalf("lost frozen video selection: %#v", state)
	}
	path, _, err := studioGenerationRecordsDirectory(false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path + "/" + id + ".json")
	if err != nil || strings.Contains(string(data), "private-session-override") || strings.Contains(w.Body.String(), "private-session-override") {
		t.Fatal("session API key entered checkpoint or response")
	}
}

type fixtureStudioVideoKeys map[string]string

func (k fixtureStudioVideoKeys) Get(source string) (string, error) { return k[source], nil }
func (k fixtureStudioVideoKeys) Set(source, key string) error      { k[source] = key; return nil }
func (k fixtureStudioVideoKeys) Delete(source string) error        { delete(k, source); return nil }
func TestStudioVideoKeyRoutesRequireAuthenticationAndDoNotReturnKey(t *testing.T) {
	previous := studioVideoKeys
	studioVideoKeys = fixtureStudioVideoKeys{"veo-api": "private-fixture-video-key"}
	t.Cleanup(func() { studioVideoKeys = previous })
	isolateStudioProgress(t)
	w := httptest.NewRecorder()
	studioVideoKeyHandler(w, httptest.NewRequest(http.MethodGet, "/studio/videos/key?sourceId=veo-api", nil))
	if w.Code != 401 {
		t.Fatal("saved key route accepted unauthenticated request")
	}
	w = httptest.NewRecorder()
	studioVideoKeyHandler(w, studioProgressRequest(http.MethodGet, "/studio/videos/key?sourceId=veo-api", ""))
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-fixture-video-key") {
		t.Fatalf("key status exposed key or failed: %d %s", w.Code, w.Body)
	}
	var status map[string]bool
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || !status["saved"] || !status["configured"] {
		t.Fatal("securely stored video key was not identified")
	}
	delete(studioVideoKeys.(fixtureStudioVideoKeys), "veo-api")
	t.Setenv("GEMINI_API_KEY", "fixture-env-only-video-key")
	w = httptest.NewRecorder()
	studioVideoKeyHandler(w, studioProgressRequest(http.MethodGet, "/studio/videos/key?sourceId=veo-api", ""))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status["saved"] || !status["configured"] || strings.Contains(w.Body.String(), "fixture-env-only-video-key") {
		t.Fatal("environment key was misreported as securely saved")
	}
}

func TestStudioVideoSubscriptionLatestPreservesSelectedModelWithoutFallback(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "0")
	isolateGrokMediaCredentials(t, grokMediaTestSubscription)
	found := false
	for _, source := range studioVideoCapabilities().Sources {
		if source.ID != "xai-subscription" {
			continue
		}
		if len(source.Models) != 2 || source.Models[0].ID != "grok-imagine-video" || source.Experimental || !source.Connected {
			t.Fatal("subscription defaults or availability changed")
		}
		for _, model := range source.Models {
			if len(model.PricesPerSecondUSD) != 0 || model.InputImageUSD != 0 || model.ImageInputPriceUSD != 0 {
				t.Fatal("subscription inherited API prices")
			}
			if model.ID == "grok-imagine-video-1.5" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("latest subscription model missing")
	}
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.URL.Path != "/v1/videos/generations" || r.Header.Get("Authorization") != "Bearer fixture-grok-access" || body["model"] != "grok-imagine-video-1.5" || body["resolution"] != "1080p" || body["duration"] != float64(3) || body["aspect_ratio"] != "9:16" {
			t.Fatalf("selected subscription settings changed: %#v", body)
		}
		return studioRecoveryResponse(422, "application/json", `{"error":"private account detail"}`), nil
	})
	_, err := generateStudioSelectedVideo(context.Background(), studioVideoOptions{SourceID: "xai-subscription", ModelID: "grok-imagine-video-1.5", DurationSeconds: 3, Resolution: "1080p", AspectRatio: "9:16"}, "", "A tiny video", nil)
	if err == nil || calls != 1 || strings.Contains(err.Error(), "private account detail") {
		t.Fatalf("subscription failed unsafely or fell back: calls=%d err=%v", calls, err)
	}
}
