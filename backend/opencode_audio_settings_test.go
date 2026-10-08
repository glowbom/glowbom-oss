package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaApprovalMusicDurationSurvivesPlanningAndResponse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	token := "glowbyaudio:soft piano|type:music|instrumental:true"
	project := mediaEditProject(t, `<audio src="`+token+`"></audio>`)
	plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: project})
	if err != nil || plan == nil || len(plan.Items) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if plan.Items[0].DurationSeconds != 30 || !plan.Items[0].ForceInstrumental {
		t.Fatalf("music settings were not made explicit: %+v", plan.Items[0])
	}
	approvalID, response := registerOpenCodeMediaApproval(project, plan)
	defer removeOpenCodeMediaApproval(approvalID)
	edited := plan.Items[0]
	edited.DurationSeconds = 12.5
	body, err := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: approvalID, Response: "generate", ProjectPath: project, Items: []OpenCodeMediaApprovalItem{edited}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	openCodeMediaApprovalRespondHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approval failed: %d %s", recorder.Code, recorder.Body.String())
	}
	selection, err := waitForOpenCodeMediaApproval(context.Background(), approvalID, response)
	if err != nil || len(selection.Items) != 1 || selection.Items[0].DurationSeconds != 12.5 || !selection.Items[0].ForceInstrumental {
		t.Fatalf("selection=%+v err=%v", selection, err)
	}
}

func TestPostPassAudioSettingsReachElevenLabsRequest(t *testing.T) {
	for _, test := range []struct {
		name     string
		payload  string
		path     string
		expected map[string]any
	}{
		{"default music", "soft piano|type:music|instrumental:true", "/v1/music", map[string]any{"prompt": "soft piano", "music_length_ms": float64(30000), "force_instrumental": true}},
		{"edited music", "soft piano|type:music|duration:12.5|instrumental:true", "/v1/music", map[string]any{"music_length_ms": float64(12500), "force_instrumental": true}},
		{"sound", "rain|type:sound|model:eleven_text_to_sound_v2|duration:7.5|influence:0.8|loop:true", "/v1/sound-generation", map[string]any{"text": "rain", "duration_seconds": float64(7.5), "prompt_influence": float64(.8), "loop": true, "model_id": "eleven_text_to_sound_v2"}},
		{"voice", "Welcome home|type:voice|voice:chosen-narrator|model:eleven_multilingual_v2", "/v1/text-to-speech/chosen-narrator", map[string]any{"text": "Welcome home", "model_id": "eleven_multilingual_v2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			mockProjectIconProvider(t, func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Path != test.path || request.Header.Get("xi-api-key") != "fixture-key" {
					t.Fatalf("wrong provider route or credential: %s", request.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				for key, expected := range test.expected {
					if body[key] != expected {
						t.Errorf("%s=%v, want %v", key, body[key], expected)
					}
				}
				if _, exists := body["is_instrumental"]; exists {
					t.Fatal("obsolete instrumental field was sent")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("ID3fixture-audio"))}, nil
			})
			placeholder := parseGlowbyAudioPayload("glowbyaudio:"+test.payload, test.payload)
			_, _, _, err := generateElevenLabsAudioForPostPass(placeholder, "fixture-key", "fallback-narrator", "eleven_multilingual_v2")
			if err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestApprovedMusicDurationDefaultsBeforeGeneration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	token := "glowbyaudio:soft piano|type:music"
	project := mediaEditProject(t, `<audio src="`+token+`"></audio>`)
	if err := os.WriteFile(filepath.Join(project, "glowbom.json"), []byte(`{"name":"Music settings fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	item := OpenCodeMediaApprovalItem{ID: mediaApprovalItemID("audio", token), Placeholder: token, MediaType: "audio", Prompt: "soft piano", SourceID: "elevenlabs-api", AudioType: "music", ForceInstrumental: true}
	previous := generatePostPassAudio
	t.Cleanup(func() { generatePostPassAudio = previous })
	calls := 0
	generatePostPassAudio = func(placeholder postPassAudioPlaceholder, apiKey, voiceID, model string) ([]byte, string, string, error) {
		calls++
		if placeholder.durationSeconds != 30 || !placeholder.forceInstrumental {
			t.Fatalf("lost explicit music defaults: %+v", placeholder)
		}
		return []byte("fixture audio"), "audio/mpeg", "ElevenLabs (Music)", nil
	}
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{item}})
	if err != nil || calls != 1 || len(result.GeneratedAssets) != 1 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
	}
	if item.DurationSeconds != 0 {
		t.Fatal("normalizing the run mutated the original approval item")
	}
	if result.GeneratedAssets[0].StudioAssetID == "" {
		t.Fatal("generated project audio was not saved to Studio")
	}
	saved, err := findStudioAsset(result.GeneratedAssets[0].StudioAssetID)
	if err != nil || saved.MediaType != "audio" || saved.AudioType != "music" || saved.RequestedDurationSeconds == nil || *saved.RequestedDurationSeconds != 30 || !saved.ForceInstrumental || saved.SourceProjectID == "" || len(saved.UsedInProjects) != 1 || saved.Dimensions != nil {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}

func TestApprovedMusicEditsBypassSamePromptStudioAudio(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	token := "glowbyaudio:soft piano|type:music|duration:300"
	project := mediaEditProject(t, `<audio src="`+token+`"></audio>`)
	config, _ := os.UserConfigDir()
	legacyDir := filepath.Join(config, "Glowbom", "Studio", "Assets")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	cached, _ := json.Marshal(studioAssetRecord{ID: "old-music", MediaType: "audio", Prompt: "soft piano", SourceService: "ElevenLabs", DataBase64: "b2xkIG11c2lj"})
	if err := os.WriteFile(filepath.Join(legacyDir, "old.json"), cached, 0600); err != nil {
		t.Fatal(err)
	}
	item := OpenCodeMediaApprovalItem{ID: mediaApprovalItemID("audio", token), Placeholder: token, MediaType: "audio", Prompt: "soft piano", SourceID: "elevenlabs-api", AudioType: "music", ModelID: "music_v1", DurationSeconds: 12.5, ForceInstrumental: true}
	previous := generatePostPassAudio
	t.Cleanup(func() { generatePostPassAudio = previous })
	calls := 0
	generatePostPassAudio = func(placeholder postPassAudioPlaceholder, apiKey, voiceID, model string) ([]byte, string, string, error) {
		calls++
		if placeholder.durationSeconds != 12.5 || placeholder.modelID != "music_v1" || !placeholder.forceInstrumental {
			t.Fatalf("lost reviewed music edits: %+v", placeholder)
		}
		return []byte("new short music"), "audio/mpeg", "ElevenLabs (Music)", nil
	}
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{item}})
	if err != nil || calls != 1 || len(result.GeneratedAssets) != 1 || len(result.ReusedStudioAssets) != 0 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestAutomaticAudioWithExplicitSettingsSkipsPromptOnlyReuse(t *testing.T) {
	cached := []studioAssetRecord{{ID: "legacy-audio", MediaType: "audio", Prompt: "rain", SourceService: "ElevenLabs", DataBase64: "bGVnYWN5"}}
	for _, payload := range []string{"rain|type:voice", "rain|type:sound", "rain|duration:2", "rain|model:eleven_v3", "rain|voice:narrator", "rain|loop:false", "rain|instrumental:false"} {
		placeholder := parseGlowbyAudioPayload("glowbyaudio:"+payload, payload)
		if _, reusable := findReusablePostPassAudio(cached, placeholder); reusable {
			t.Errorf("explicit settings reused prompt-only audio: %s", payload)
		}
	}
	legacy := parseGlowbyAudioPayload("glowbyaudio:rain", "rain")
	if _, reusable := findReusablePostPassAudio(cached, legacy); !reusable {
		t.Fatal("legacy audio with no explicit settings could not be reused")
	}
}

func TestAutomaticMusicWithDifferentLengthsKeepsSeparateFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	project := mediaEditProject(t, `<audio src="glowbyaudio:piano|type:music|duration:10"></audio><audio src="glowbyaudio:piano|type:music|duration:20"></audio>`)
	previous := generatePostPassAudio
	t.Cleanup(func() { generatePostPassAudio = previous })
	calls := 0
	generatePostPassAudio = func(placeholder postPassAudioPlaceholder, apiKey, voiceID, model string) ([]byte, string, string, error) {
		calls++
		return []byte(placeholder.token), "audio/mpeg", "ElevenLabs (Music)", nil
	}
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project})
	if err != nil || calls != 2 || len(result.GeneratedAssets) != 2 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
	}
	if result.GeneratedAssets[0].Filename == result.GeneratedAssets[1].Filename {
		t.Fatal("different music lengths overwrote the same project asset")
	}
	for _, asset := range result.GeneratedAssets {
		data, err := os.ReadFile(filepath.Join(project, asset.RelativePath))
		if err != nil || string(data) != asset.Placeholder {
			t.Fatalf("asset=%+v data=%q err=%v", asset, data, err)
		}
	}
}

func TestInvalidAudioSettingsNeverReachProvider(t *testing.T) {
	calls := 0
	mockProjectIconProvider(t, func(request *http.Request) (*http.Response, error) {
		calls++
		t.Fatal("invalid audio settings reached provider")
		return nil, nil
	})
	for _, payload := range []string{
		"piano|type:music|duration:601", "piano|type:music|duration:2", "piano|type:music|duration:-1", "piano|type:music|duration:NaN", "piano|type:music|duration:Inf", "piano|type:music|duration:invalid",
		"rain|type:sound|duration:31", "rain|type:sound|duration:0.4", "rain|type:sound|influence:1.1", "rain|type:sound|influence:NaN", "rain|type:sound|influence:invalid",
	} {
		placeholder := parseGlowbyAudioPayload("glowbyaudio:"+payload, payload)
		if _, _, _, err := generateElevenLabsAudioForPostPass(placeholder, "fixture-key", "", ""); err == nil {
			t.Errorf("invalid payload accepted: %s", payload)
		}
	}
	if calls != 0 {
		t.Fatalf("unexpected provider calls: %d", calls)
	}
	for _, duration := range []float64{-1, 2, 601, math.NaN(), math.Inf(1)} {
		item := OpenCodeMediaApprovalItem{ID: "added-music", MediaType: "audio", Prompt: "piano", SourceID: "elevenlabs-api", AudioType: "music", DurationSeconds: duration, UsagePrompt: "Play on welcome"}
		if _, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: t.TempDir(), Items: []OpenCodeMediaApprovalItem{item}, ElevenLabsKey: "fixture-key"}); err == nil {
			t.Errorf("invalid approval duration accepted: %v", duration)
		}
	}
}

func TestMediaPostPassSavedAudioKeyRequiresAuthentication(t *testing.T) {
	for _, test := range []struct {
		name      string
		request   OpenCodeMediaPostPassRequest
		store     *memoryVoiceKey
		wantKey   string
		wantError string
	}{
		{"unauthorized", OpenCodeMediaPostPassRequest{ElevenLabsUseSavedKey: true}, &memoryVoiceKey{key: "saved-fixture-key"}, "", "Local authentication"},
		{"authorized", OpenCodeMediaPostPassRequest{ElevenLabsUseSavedKey: true, elevenLabsSavedKeyAuthorized: true}, &memoryVoiceKey{key: " saved-fixture-key "}, "saved-fixture-key", ""},
		{"missing", OpenCodeMediaPostPassRequest{ElevenLabsUseSavedKey: true, elevenLabsSavedKeyAuthorized: true}, &memoryVoiceKey{}, "", "Add your ElevenLabs key"},
		{"unavailable", OpenCodeMediaPostPassRequest{ElevenLabsUseSavedKey: true, elevenLabsSavedKeyAuthorized: true}, &memoryVoiceKey{err: errors.New("private vault path and secret")}, "", "Unlock your system credential store"},
		{"session key", OpenCodeMediaPostPassRequest{ElevenLabsKey: " explicit-fixture-key ", ElevenLabsUseSavedKey: true}, &memoryVoiceKey{key: "saved-fixture-key"}, "explicit-fixture-key", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			key, err := resolvePostPassAudioKey(test.request, test.store)
			if key != test.wantKey || (test.wantError == "" && err != nil) || (test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError))) {
				t.Fatalf("key matched=%t err=%v", key == test.wantKey, err)
			}
			if err != nil && (strings.Contains(err.Error(), "private vault") || strings.Contains(err.Error(), "fixture-key")) {
				t.Fatal("credential failure exposed private data")
			}
		})
	}
}

func TestMediaPostPassSavedAudioFlagCannotOverrideAuthentication(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	body := `{"projectPath":"` + t.TempDir() + `","elevenLabsUseSavedKey":true,"elevenLabsSavedKeyAuthorized":true}`
	recorder := httptest.NewRecorder()
	openCodeMediaPostPassHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/media/post-pass", strings.NewReader(body)))
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Local authentication required") {
		t.Fatalf("untrusted authentication override was accepted: %d %s", recorder.Code, recorder.Body.String())
	}
}
