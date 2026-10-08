package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type postPassVideoTestKeyStore struct {
	key   string
	err   error
	reads int
}

func (s *postPassVideoTestKeyStore) Get(string) (string, error) {
	s.reads++
	return s.key, s.err
}
func (s *postPassVideoTestKeyStore) Set(string, string) error { return s.err }
func (s *postPassVideoTestKeyStore) Delete(string) error      { return s.err }

func videoSettingsFixture(t *testing.T) (string, *OpenCodeMediaApproval) {
	t.Helper()
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	project := mediaEditProject(t, `<img src="glowbomimage:video settings frame"><video src="glowbyvideo:video settings motion|from:video settings frame"></video>`)
	if err := os.WriteFile(filepath.Join(project, "glowbom.json"), []byte(`{"name":"Video settings fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: project, ImageSource: "openai-api"})
	if err != nil || plan == nil || len(plan.Items) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	return project, plan
}

func mockPostPassVideoSettings(t *testing.T, generate func(context.Context, string, studioVideoOptions, string, []byte, string) ([]byte, error)) {
	t.Helper()
	previousImage, previousVideo := generatePostPassSelectedImage, generatePostPassVideo
	t.Cleanup(func() {
		generatePostPassSelectedImage, generatePostPassVideo = previousImage, previousVideo
	})
	generatePostPassSelectedImage = func(context.Context, string, string, string, string, string) (string, string, error) {
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, "")), "OpenAI", nil
	}
	generatePostPassVideo = generate
}

func assertVideoSettingsSecretsAbsent(t *testing.T, directories []string, secrets []string) {
	t.Helper()
	for _, directory := range directories {
		err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, secret := range secrets {
				if strings.Contains(string(data), secret) {
					t.Fatalf("provider credential was saved in %s", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestMediaApprovalVideoSettingsSurviveReviewAndGeneration(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "video-fixture-token")
	for _, test := range []struct {
		name    string
		options studioVideoOptions
		key     string
	}{
		{"latest Grok", studioVideoOptions{SourceID: "xai-api", ModelID: "grok-imagine-video-1.5", Resolution: "1080p", DurationSeconds: 9, AspectRatio: "9:16"}, "central-xai-fixture-key"},
		{"Veo Fast", studioVideoOptions{SourceID: "veo-api", ModelID: "veo-3.1-fast-generate-preview", Resolution: "720p", DurationSeconds: 6, AspectRatio: "9:16"}, "central-google-fixture-key"},
		{"Veo Standard", studioVideoOptions{SourceID: "veo-api", ModelID: "veo-3.1-generate-preview", Resolution: "1080p", DurationSeconds: 8, AspectRatio: "16:9"}, "central-google-fixture-key"},
		{"Grok subscription", studioVideoOptions{SourceID: "xai-subscription", ModelID: "grok-imagine-video", Resolution: "720p", DurationSeconds: 7, AspectRatio: "1:1"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, plan := videoSettingsFixture(t)
			items := append([]OpenCodeMediaApprovalItem{}, plan.Items...)
			items[1].SourceID, items[1].ModelID = test.options.SourceID, test.options.ModelID
			items[1].Resolution, items[1].DurationSeconds, items[1].AspectRatio = test.options.Resolution, float64(test.options.DurationSeconds), test.options.AspectRatio
			items[1].Prompt = "reviewed motion"
			approvalID, response := registerOpenCodeMediaApproval(project, plan)
			t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
			keys := map[string]string{"openai-api": "central-image-fixture-key", "xai-api": "image-xai-fixture-key", "gemini-api": "image-google-fixture-key"}
			videoKeys := map[string]string{"xai-api": "central-xai-fixture-key", "gemini-api": "central-google-fixture-key"}
			body, err := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: approvalID, Response: "generate", ProjectPath: project, Items: items, ImageAPIKeys: keys, VideoAPIKeys: videoKeys})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer video-fixture-token")
			openCodeMediaApprovalRespondHandler(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("review failed: %d %s", recorder.Code, recorder.Body.String())
			}
			selection, err := waitForOpenCodeMediaApproval(context.Background(), approvalID, response)
			if err != nil || len(selection.Items) != 2 {
				t.Fatalf("selection=%+v err=%v", selection, err)
			}
			selected := selection.Items[1]
			if selected.SourceID != test.options.SourceID || selected.ModelID != test.options.ModelID || selected.Resolution != test.options.Resolution || selected.DurationSeconds != float64(test.options.DurationSeconds) || selected.AspectRatio != test.options.AspectRatio {
				t.Fatalf("review changed selected video settings: %+v", selected)
			}
			calls := 0
			mockPostPassVideoSettings(t, func(_ context.Context, prompt string, options studioVideoOptions, key string, frame []byte, mime string) ([]byte, error) {
				calls++
				if prompt != "reviewed motion" || options != test.options || key != test.key || len(frame) == 0 || mime != "image/png" {
					t.Fatalf("generation lost review settings: options=%+v keyMatched=%t frameBytes=%d mime=%s", options, key == test.key, len(frame), mime)
				}
				return []byte("fixture mp4"), nil
			})
			result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: selection.Items, ImageAPIKeys: selection.ImageAPIKeys, VideoAPIKeys: selection.VideoAPIKeys, VeoGeminiKey: "legacy-google-fixture-key", XaiKey: "legacy-xai-fixture-key", videoSubscriptionAuthorized: selection.videoSubscriptionAuthorized})
			if err != nil || calls != 1 || len(result.GeneratedAssets) != 2 {
				t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
			}
			video := result.GeneratedAssets[1]
			if video.StudioAssetID == "" {
				t.Fatal("generated project video was not saved to Studio")
			}
			saved, err := findStudioAsset(video.StudioAssetID)
			if err != nil || saved.SourceID != test.options.SourceID || saved.Model != test.options.ModelID || saved.Resolution != test.options.Resolution || saved.RequestedDurationSeconds == nil || *saved.RequestedDurationSeconds != float64(test.options.DurationSeconds) {
				t.Fatalf("saved video settings=%+v err=%v", saved, err)
			}
			if saved.Duration != nil {
				t.Fatal("requested video length was presented as measured duration")
			}
			assertVideoSettingsSecretsAbsent(t, []string{project, os.Getenv("GLOWBOM_STUDIO_DIR")}, []string{"central-image-fixture-key", "central-xai-fixture-key", "central-google-fixture-key", "image-xai-fixture-key", "image-google-fixture-key", "legacy-google-fixture-key", "legacy-xai-fixture-key"})
		})
	}
}

func TestMediaApprovalLegacyVideoDefaultsToVeoLiteFourSeconds(t *testing.T) {
	project, plan := videoSettingsFixture(t)
	video := plan.Items[1]
	if video.SourceID != "veo-api" || video.ModelID != "veo-3.1-lite-generate-preview" || video.DurationSeconds != 4 || video.Resolution != "720p" {
		t.Fatalf("legacy video has unexpected defaults: %+v", video)
	}
	calls := 0
	mockPostPassVideoSettings(t, func(_ context.Context, _ string, options studioVideoOptions, key string, _ []byte, _ string) ([]byte, error) {
		calls++
		if options.SourceID != "veo-api" || options.ModelID != "veo-3.1-lite-generate-preview" || options.DurationSeconds != 4 || options.Resolution != "720p" || key != "central-google-fixture-key" {
			t.Fatalf("legacy generation changed defaults: %+v", options)
		}
		return []byte("fixture mp4"), nil
	})
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, ImageAPIKeys: map[string]string{"openai-api": "central-image-fixture-key", "gemini-api": "central-google-fixture-key"}, ImageSource: "openai-api"})
	if err != nil || calls != 1 || len(result.GeneratedAssets) != 2 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestInvalidReviewedVideoSettingsNeverReachGeneration(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*OpenCodeMediaApprovalItem)
	}{
		{"unknown source", func(item *OpenCodeMediaApprovalItem) { item.SourceID = "unknown-api" }},
		{"unknown model", func(item *OpenCodeMediaApprovalItem) { item.ModelID = "unknown-model" }},
		{"negative duration", func(item *OpenCodeMediaApprovalItem) { item.DurationSeconds = -1 }},
		{"fractional duration", func(item *OpenCodeMediaApprovalItem) { item.DurationSeconds = 4.5 }},
		{"unsupported Veo duration", func(item *OpenCodeMediaApprovalItem) { item.DurationSeconds = 5 }},
		{"long Grok duration", func(item *OpenCodeMediaApprovalItem) {
			item.SourceID, item.ModelID, item.Resolution, item.DurationSeconds = "xai-api", "grok-imagine-video-1.5", "720p", 16
		}},
		{"unknown resolution", func(item *OpenCodeMediaApprovalItem) { item.Resolution = "4k" }},
		{"short Veo 1080p", func(item *OpenCodeMediaApprovalItem) { item.Resolution = "1080p" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, plan := videoSettingsFixture(t)
			items := append([]OpenCodeMediaApprovalItem{}, plan.Items...)
			test.edit(&items[1])
			approvalID, response := registerOpenCodeMediaApproval(project, plan)
			t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
			body, err := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: approvalID, Response: "generate", ProjectPath: project, Items: items})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			openCodeMediaApprovalRespondHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body))))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("invalid review accepted: %d %s", recorder.Code, recorder.Body.String())
			}
			select {
			case <-response:
				t.Fatal("invalid review consumed its pending approval")
			default:
			}
			calls := 0
			mockPostPassVideoSettings(t, func(context.Context, string, studioVideoOptions, string, []byte, string) ([]byte, error) {
				calls++
				return nil, errors.New("invalid settings reached generation")
			})
			generatePostPassSelectedImage = func(context.Context, string, string, string, string, string) (string, string, error) {
				calls++
				return "", "", errors.New("invalid settings started image generation")
			}
			if _, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: items}); err == nil || calls != 0 {
				t.Fatalf("invalid video reached generation: calls=%d err=%v", calls, err)
			}
		})
	}
	for _, duration := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e20} {
		item := OpenCodeMediaApprovalItem{ID: "added-video", MediaType: "video", Prompt: "motion", SourceID: "veo-api", FromKey: "frame", DurationSeconds: duration, UsagePrompt: "Use on welcome"}
		if _, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: t.TempDir(), Items: []OpenCodeMediaApprovalItem{item}}); err == nil {
			t.Errorf("invalid untrusted video duration was accepted: %v", duration)
		}
	}
}

func TestMediaPostPassSavedVideoKeyRequiresInternalAuthentication(t *testing.T) {
	for _, test := range []struct {
		name      string
		request   OpenCodeMediaPostPassRequest
		source    string
		store     *postPassVideoTestKeyStore
		wantKey   string
		wantError string
		reads     int
	}{
		{"unauthorized", OpenCodeMediaPostPassRequest{VideoUseSavedKey: true}, "xai-api", &postPassVideoTestKeyStore{key: "saved-fixture-key"}, "", "Local authentication", 0},
		{"authorized", OpenCodeMediaPostPassRequest{VideoUseSavedKey: true, videoSavedKeyAuthorized: true}, "xai-api", &postPassVideoTestKeyStore{key: " saved-fixture-key "}, "saved-fixture-key", "", 1},
		{"unavailable", OpenCodeMediaPostPassRequest{VideoUseSavedKey: true, videoSavedKeyAuthorized: true}, "xai-api", &postPassVideoTestKeyStore{err: errors.New("private vault and fixture-key")}, "", "Unlock your system credential store", 1},
		{"central override", OpenCodeMediaPostPassRequest{ImageAPIKeys: map[string]string{"xai-api": "central-fixture-key"}, XaiKey: "legacy-fixture-key", VideoUseSavedKey: true, videoSavedKeyAuthorized: true}, "xai-api", &postPassVideoTestKeyStore{key: "saved-fixture-key"}, "central-fixture-key", "", 0},
		{"unauthorized subscription", OpenCodeMediaPostPassRequest{ImageAPIKeys: map[string]string{"xai-api": "central-fixture-key"}, XaiKey: "legacy-fixture-key"}, "xai-subscription", &postPassVideoTestKeyStore{key: "saved-fixture-key"}, "", "Local authentication", 0},
		{"subscription ignores API keys", OpenCodeMediaPostPassRequest{ImageAPIKeys: map[string]string{"xai-api": "central-fixture-key"}, VideoAPIKeys: map[string]string{"xai-api": "video-fixture-key"}, XaiKey: "legacy-fixture-key", VideoUseSavedKey: true, videoSavedKeyAuthorized: true, videoSubscriptionAuthorized: true}, "xai-subscription", &postPassVideoTestKeyStore{key: "saved-fixture-key"}, "", "", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := studioVideoKeys
			studioVideoKeys = test.store
			t.Cleanup(func() { studioVideoKeys = previous })
			key, err := resolvePostPassVideoKey(context.Background(), test.request, test.source)
			if key != test.wantKey || test.store.reads != test.reads || (test.wantError == "" && err != nil) || (test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError))) {
				t.Fatalf("keyMatched=%t reads=%d err=%v", key == test.wantKey, test.store.reads, err)
			}
			if err != nil && (strings.Contains(err.Error(), "private vault") || strings.Contains(err.Error(), "fixture-key")) {
				t.Fatal("video credential error exposed private data")
			}
		})
	}
}

func TestMediaPostPassVideoKeyMapOverridesCapturedCredentials(t *testing.T) {
	t.Setenv("XAI_API_KEY", "environment-xai-fixture-key")
	t.Setenv("GEMINI_API_KEY", "environment-google-fixture-key")
	for _, source := range []string{"xai-api", "veo-api"} {
		t.Run(source, func(t *testing.T) {
			mapSource, environmentKey := "xai-api", "environment-xai-fixture-key"
			if source == "veo-api" {
				mapSource, environmentKey = "gemini-api", "environment-google-fixture-key"
			}
			for _, test := range []struct {
				name  string
				keys  map[string]string
				saved bool
				want  string
				reads int
			}{
				{"explicit video key", map[string]string{mapSource: "current-video-fixture-key"}, true, "current-video-fixture-key", 0},
				{"empty map uses saved key", map[string]string{}, true, "saved-video-fixture-key", 1},
				{"empty map uses environment", map[string]string{}, false, environmentKey, 0},
				{"legacy map uses central image key", nil, false, "captured-image-fixture-key", 0},
			} {
				t.Run(test.name, func(t *testing.T) {
					store := &postPassVideoTestKeyStore{key: "saved-video-fixture-key"}
					previous := studioVideoKeys
					studioVideoKeys = store
					t.Cleanup(func() { studioVideoKeys = previous })
					request := OpenCodeMediaPostPassRequest{
						VideoAPIKeys: test.keys, ImageAPIKeys: map[string]string{mapSource: "captured-image-fixture-key"},
						VeoGeminiKey: "captured-legacy-fixture-key", XaiKey: "captured-legacy-fixture-key",
						VideoUseSavedKey: test.saved, videoSavedKeyAuthorized: test.saved,
					}
					key, err := resolvePostPassVideoKey(context.Background(), request, source)
					if err != nil || key != test.want || store.reads != test.reads {
						t.Fatalf("video key precedence failed: keyMatched=%t reads=%d err=%v", key == test.want, store.reads, err)
					}
				})
			}
		})
	}
}

func TestMediaPostPassSavedVideoFlagCannotForgeAuthentication(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "video-fixture-token")
	store := &postPassVideoTestKeyStore{key: "saved-fixture-key"}
	previous := studioVideoKeys
	studioVideoKeys = store
	t.Cleanup(func() { studioVideoKeys = previous })
	body, err := json.Marshal(map[string]any{"projectPath": t.TempDir(), "videoUseSavedKey": true, "videoSavedKeyAuthorized": true})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	openCodeMediaPostPassHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/media/post-pass", strings.NewReader(string(body))))
	if recorder.Code != http.StatusUnauthorized || store.reads != 0 {
		t.Fatalf("forged authentication reached saved keys: status=%d reads=%d", recorder.Code, store.reads)
	}
}

func TestMediaApprovalSavedVideoFlagRequiresAuthenticatedResponse(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "video-fixture-token")
	project, plan := videoSettingsFixture(t)
	approvalID, response := registerOpenCodeMediaApproval(project, plan)
	t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
	body, err := json.Marshal(map[string]any{"approvalID": approvalID, "response": "generate", "projectPath": project, "items": plan.Items, "videoUseSavedKey": true, "videoSavedKeyAuthorized": true})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	openCodeMediaApprovalRespondHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body))))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated approval accepted: %d %s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-response:
		t.Fatal("unauthenticated response consumed pending approval")
	default:
	}
	request := httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer video-fixture-token")
	recorder = httptest.NewRecorder()
	openCodeMediaApprovalRespondHandler(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated approval failed: %d %s", recorder.Code, recorder.Body.String())
	}
	selection, err := waitForOpenCodeMediaApproval(context.Background(), approvalID, response)
	if err != nil || !selection.VideoUseSavedKey || !selection.videoSavedKeyAuthorized {
		t.Fatalf("authenticated key preference was lost: err=%v", err)
	}
	encoded, err := json.Marshal(selection)
	if err != nil || strings.Contains(string(encoded), "videoSavedKeyAuthorized") || strings.Contains(string(encoded), "video-fixture-token") {
		t.Fatal("private authorization state was exposed in JSON")
	}
}

func TestMediaPostPassSubscriptionVideoCannotForgeAuthentication(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "video-fixture-token")
	project, plan := videoSettingsFixture(t)
	item := plan.Items[1]
	item.SourceID, item.ModelID, item.Resolution, item.DurationSeconds = "xai-subscription", "grok-imagine-video", "480p", 5
	items := []OpenCodeMediaApprovalItem{item}
	body, err := json.Marshal(map[string]any{"projectPath": project, "items": items, "videoSubscriptionAuthorized": true})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	mockPostPassVideoSettings(t, func(context.Context, string, studioVideoOptions, string, []byte, string) ([]byte, error) {
		calls++
		return nil, errors.New("unauthorized subscription reached generation")
	})
	recorder := httptest.NewRecorder()
	openCodeMediaPostPassHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/media/post-pass", strings.NewReader(string(body))))
	if recorder.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("forged subscription authorization reached generation: status=%d calls=%d", recorder.Code, calls)
	}
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: items})
	if err != nil || calls != 0 || len(result.GeneratedAssets) != 0 || !strings.Contains(strings.Join(result.Warnings, " "), "Local authentication") {
		t.Fatalf("unauthorized internal subscription reached generation: calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestMediaApprovalSubscriptionVideoRequiresAuthenticatedResponse(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "video-fixture-token")
	project, plan := videoSettingsFixture(t)
	items := append([]OpenCodeMediaApprovalItem{}, plan.Items...)
	items[1].SourceID, items[1].ModelID, items[1].Resolution, items[1].DurationSeconds = "xai-subscription", "grok-imagine-video", "480p", 5
	approvalID, response := registerOpenCodeMediaApproval(project, plan)
	t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
	body, err := json.Marshal(map[string]any{"approvalID": approvalID, "response": "generate", "projectPath": project, "items": items, "videoSubscriptionAuthorized": true})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	openCodeMediaApprovalRespondHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body))))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated subscription approval accepted: %d %s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-response:
		t.Fatal("unauthenticated subscription consumed pending approval")
	default:
	}
	request := httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer video-fixture-token")
	recorder = httptest.NewRecorder()
	openCodeMediaApprovalRespondHandler(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated subscription approval failed: %d %s", recorder.Code, recorder.Body.String())
	}
	selection, err := waitForOpenCodeMediaApproval(context.Background(), approvalID, response)
	if err != nil || !selection.videoSubscriptionAuthorized {
		t.Fatalf("authenticated subscription permission was lost: err=%v", err)
	}
	encoded, err := json.Marshal(selection)
	if err != nil || strings.Contains(string(encoded), "videoSubscriptionAuthorized") || strings.Contains(string(encoded), "video-fixture-token") {
		t.Fatal("private subscription authorization was exposed in JSON")
	}
}

func TestPostPassVideoLengthsKeepSeparateRequestsAndFiles(t *testing.T) {
	for _, reviewed := range []bool{false, true} {
		name := "automatic"
		if reviewed {
			name = "approved"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			first := "glowbyvideo:same motion|from:separate video frame|source:xai-api|model:grok-imagine-video-1.5|duration:3|resolution:480p"
			second := "glowbyvideo:same motion|from:separate video frame|source:xai-api|model:grok-imagine-video-1.5|duration:7|resolution:480p"
			project := mediaEditProject(t, `<img src="glowbomimage:separate video frame"><video src="`+first+`"></video><video src="`+second+`"></video>`)
			request := OpenCodeMediaPostPassRequest{ProjectPath: project, ImageSource: "openai-api", ImageAPIKeys: map[string]string{"openai-api": "image-fixture-key"}, VideoAPIKeys: map[string]string{"xai-api": "video-fixture-key"}}
			if reviewed {
				plan, err := buildOpenCodeMediaApproval(request)
				if err != nil || plan == nil || len(plan.Items) != 3 {
					t.Fatalf("plan=%+v err=%v", plan, err)
				}
				request.Items = plan.Items
			}
			var durations []int
			mockPostPassVideoSettings(t, func(_ context.Context, prompt string, options studioVideoOptions, key string, frame []byte, _ string) ([]byte, error) {
				if prompt != "same motion" || options.SourceID != "xai-api" || options.ModelID != "grok-imagine-video-1.5" || options.Resolution != "480p" || key != "video-fixture-key" || len(frame) == 0 {
					t.Fatalf("clip generation changed settings: %+v", options)
				}
				durations = append(durations, options.DurationSeconds)
				return []byte(fmt.Sprintf("fixture mp4 length=%d", options.DurationSeconds)), nil
			})
			result, err := runOpenCodeMediaPostPass(context.Background(), request)
			if err != nil || len(durations) != 2 || durations[0] != 3 || durations[1] != 7 || len(result.GeneratedAssets) != 3 {
				t.Fatalf("durations=%v result=%+v err=%v", durations, result, err)
			}
			firstAsset, secondAsset := result.GeneratedAssets[1], result.GeneratedAssets[2]
			if firstAsset.Filename == secondAsset.Filename {
				t.Fatal("different video lengths overwrote the same project file")
			}
			for index, asset := range []OpenCodeMediaAsset{firstAsset, secondAsset} {
				data, err := os.ReadFile(filepath.Join(project, asset.RelativePath))
				if err != nil || string(data) != fmt.Sprintf("fixture mp4 length=%d", durations[index]) {
					t.Fatalf("clip output was overwritten: asset=%+v err=%v", asset, err)
				}
			}
		})
	}
}

func TestPostPassStudioAssetsRespectDirectoryOverride(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", root)
	directory := filepath.Join(root, "Assets")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := studioAssetRecord{ID: "isolated-video-fixture", MediaType: "video", Prompt: "isolated video", SourceService: "xAI", DataBase64: base64.StdEncoding.EncodeToString([]byte("fixture mp4"))}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "isolated-video.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	assets, err := loadStudioAssets()
	if err != nil || len(assets) != 1 || assets[0] != fixture {
		t.Fatalf("Studio directory override was ignored: assetCount=%d err=%v", len(assets), err)
	}
}
