package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"time"
)

type studioAudioTransport func(*http.Request) (*http.Response, error)

func (f studioAudioTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockStudioAudioProvider(t *testing.T, transport studioAudioTransport) {
	t.Helper()
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultClient = previous })
}

func studioAudioProviderResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("ID3saved audio"))}
}

func TestElevenLabsMusicAlwaysSendsExplicitLengthAndInstrumental(t *testing.T) {
	for _, seconds := range []float64{0, 3, 8.5, 30, 600} {
		t.Run(strings.ReplaceAll(time.Duration(seconds*float64(time.Second)).String(), ".", "_"), func(t *testing.T) {
			calls := 0
			mockStudioAudioProvider(t, func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Path != "/v1/music" || request.Header.Get("xi-api-key") != "test-key" || request.URL.Query().Get("output_format") != defaultElevenOutputFormat {
					t.Fatalf("unexpected request: %s", request.URL)
				}
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				want := seconds
				if want == 0 {
					want = defaultElevenMusicDuration
				}
				if body["music_length_ms"] != want*1000 || body["force_instrumental"] != true || body["model_id"] != defaultElevenMusicModel {
					t.Fatalf("music payload = %#v", body)
				}
				if _, wrong := body["is_instrumental"]; wrong {
					t.Fatal("obsolete instrumental field was sent")
				}
				return studioAudioProviderResponse(), nil
			})
			data, mimeType, err := callElevenLabsMusic(ElevenLabsAudioRequest{Prompt: "Quiet piano", DurationSeconds: seconds, ForceInstrumental: true}, "test-key")
			if err != nil || mimeType != "audio/mpeg" || string(data) != "ID3saved audio" || calls != 1 {
				t.Fatalf("music result = %q, %s, %v, calls=%d", data, mimeType, err, calls)
			}
		})
	}
}

func TestElevenLabsInvalidAudioOptionsDoNotCallProvider(t *testing.T) {
	called := false
	mockStudioAudioProvider(t, func(*http.Request) (*http.Response, error) { called = true; return studioAudioProviderResponse(), nil })
	badInfluence := 1.1
	for _, request := range []ElevenLabsAudioRequest{
		{AudioType: "music", Prompt: "Tune", DurationSeconds: 2.99},
		{AudioType: "music", Prompt: "Tune", DurationSeconds: 601},
		{AudioType: "music", Prompt: "Tune", DurationSeconds: -1},
		{AudioType: "music", Prompt: "Tune", DurationSeconds: math.NaN()},
		{AudioType: "sound", Prompt: "Rain", DurationSeconds: 0.49},
		{AudioType: "sound", Prompt: "Rain", DurationSeconds: 31},
		{AudioType: "sound", Prompt: "Rain", PromptInfluence: &badInfluence},
		{AudioType: "sound", Prompt: "Rain", SoundModel: "eleven_text_to_sound_v1", Loop: true},
		{AudioType: "voice", Prompt: "  "},
	} {
		var err error
		switch request.AudioType {
		case "voice":
			_, _, err = callElevenLabsVoice(request, "test-key")
		case "sound":
			_, _, err = callElevenLabsSound(request, "test-key")
		case "music":
			_, _, err = callElevenLabsMusic(request, "test-key")
		}
		if err == nil {
			t.Fatalf("invalid request was accepted: %#v", request)
		}
	}
	if called {
		t.Fatal("invalid options reached a paid provider endpoint")
	}
}

func TestElevenLabsCustomModelsForwardWithoutFallback(t *testing.T) {
	for _, audioType := range []string{"voice", "music", "sound"} {
		t.Run(audioType, func(t *testing.T) {
			calls := 0
			mockStudioAudioProvider(t, func(request *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if json.NewDecoder(request.Body).Decode(&body) != nil || body["model_id"] != "custom_model-2026.10" {
					t.Fatal("custom model was replaced")
				}
				return studioRecoveryResponse(400, "application/json", `{"detail":{"status":"model_not_found","message":"private key"}}`), nil
			})
			req := ElevenLabsAudioRequest{Prompt: "An audio cue", VoiceModel: "custom_model-2026.10", MusicModel: "custom_model-2026.10", SoundModel: "custom_model-2026.10"}
			var err error
			switch audioType {
			case "voice":
				_, _, err = callElevenLabsVoice(req, "fixture-key")
			case "music":
				_, _, err = callElevenLabsMusic(req, "fixture-key")
			case "sound":
				_, _, err = callElevenLabsSound(req, "fixture-key")
			}
			if err == nil || calls != 1 || strings.Contains(err.Error(), "private key") {
				t.Fatalf("model fallback or unsafe error: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestElevenLabsV4UsesDialogueAndTurboRejectsBeforeProvider(t *testing.T) {
	calls := 0
	mockStudioAudioProvider(t, func(request *http.Request) (*http.Response, error) {
		calls++
		var body struct {
			ModelID string `json:"model_id"`
			Inputs  []struct {
				Text    string `json:"text"`
				VoiceID string `json:"voice_id"`
			} `json:"inputs"`
		}
		if request.URL.Path != "/v1/text-to-dialogue" || json.NewDecoder(request.Body).Decode(&body) != nil || body.ModelID != "eleven_v4" || len(body.Inputs) != 1 || body.Inputs[0].VoiceID != "customVoice" || body.Inputs[0].Text != "Hello" {
			t.Fatal("incorrect v4 dialogue adapter")
		}
		return studioAudioProviderResponse(), nil
	})
	if _, _, err := callElevenLabsVoice(ElevenLabsAudioRequest{Prompt: "Hello", VoiceModel: "eleven_v4", VoiceID: "customVoice"}, "fixture-key"); err != nil {
		t.Fatal(err)
	}
	for _, req := range []ElevenLabsAudioRequest{{Prompt: "Hello", VoiceModel: "eleven_v4_turbo"}, {Prompt: strings.Repeat("a", 2001), VoiceModel: "eleven_v4"}, {Prompt: "Hello", VoiceModel: "bad/model"}, {Prompt: "Hello", MusicModel: "bad model"}} {
		if _, _, err := callElevenLabsVoice(req, "fixture-key"); err == nil {
			t.Fatalf("unsupported or unsafe model accepted: %#v", req)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid request reached provider: %d", calls)
	}
}

func TestElevenLabsSoundSendsReviewedControls(t *testing.T) {
	influence := 0.6
	mockStudioAudioProvider(t, func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["duration_seconds"] != 8.5 || body["prompt_influence"] != 0.6 || body["loop"] != true || body["model_id"] != defaultElevenSoundModel {
			t.Fatalf("sound payload = %#v", body)
		}
		return studioAudioProviderResponse(), nil
	})
	if _, _, err := callElevenLabsSound(ElevenLabsAudioRequest{Prompt: "Rain", DurationSeconds: 8.5, PromptInfluence: &influence, Loop: true}, "test-key"); err != nil {
		t.Fatal(err)
	}
}

func TestElevenLabsProviderErrorDoesNotExposeSecrets(t *testing.T) {
	mockStudioAudioProvider(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"detail":{"message":"private prompt test-key","status":"bad_key"}}`))}, nil
	})
	_, _, err := callElevenLabsMusic(ElevenLabsAudioRequest{Prompt: "private prompt"}, "test-key")
	if err == nil || strings.Contains(err.Error(), "test-key") || strings.Contains(err.Error(), "private prompt") {
		t.Fatalf("unsafe provider error: %v", err)
	}
}

type studioAudioZeroReader struct{}

func (studioAudioZeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestElevenLabsRejectsOversizedEmptyAndNonAudioResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		mime string
		body io.Reader
	}{
		{"too large", "audio/mpeg", io.LimitReader(studioAudioZeroReader{}, elevenLabsAudioMaxBytes+1)},
		{"empty", "audio/mpeg", strings.NewReader("")},
		{"json success", "application/json", strings.NewReader(`{"message":"not audio"}`)},
		{"html success", "text/html", strings.NewReader("<html>Not audio</html>")},
	} {
		t.Run(test.name, func(t *testing.T) {
			mockStudioAudioProvider(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {test.mime}}, Body: io.NopCloser(test.body)}, nil
			})
			if _, _, err := callElevenLabsMusic(ElevenLabsAudioRequest{Prompt: "Piano"}, "test-key"); err == nil {
				t.Fatal("unsupported provider response was accepted")
			}
		})
	}
}

func TestElevenLabsRequestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mockStudioAudioProvider(t, func(request *http.Request) (*http.Response, error) {
		if request.Context().Err() == nil {
			t.Fatal("provider did not receive cancelled request context")
		}
		return nil, request.Context().Err()
	})
	_, _, err := callElevenLabsMusic(ElevenLabsAudioRequest{Prompt: "Piano"}, "test-key", ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestStudioAudioGenerationSavesNativeRecordWithoutKey(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	mockStudioAudioProvider(t, func(*http.Request) (*http.Response, error) { return studioAudioProviderResponse(), nil })
	response := httptest.NewRecorder()
	studioAudioGenerateHandler(response, httptest.NewRequest(http.MethodPost, "/studio/audio/generate", strings.NewReader(`{"prompt":"Piano","audioType":"music","forceInstrumental":true,"elevenLabsKey":"test-secret"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("generation: %d %s", response.Code, response.Body)
	}
	var payload struct {
		Asset studioAudioSummary `json:"asset"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Asset.RequestedDurationSeconds == nil || *payload.Asset.RequestedDurationSeconds != 30 || payload.Asset.Duration != nil || payload.Asset.Dimensions != nil || payload.Asset.AudioType != "music" || payload.Asset.MediaType != "audio" {
		t.Fatalf("summary = %#v", payload.Asset)
	}
	_, asset, encoded, err := readStudioAudio(payload.Asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if asset.AssetType != "generated" || asset.SourceType != "generated" || normalizedStudioUUID(asset.ID) != asset.ID || asset.MimeType != "audio/mpeg" || asset.Model != defaultElevenMusicModel {
		t.Fatalf("native record = %#v", asset)
	}
	if _, err := time.Parse(time.RFC3339, asset.Timestamp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "test-secret") || strings.Contains(string(encoded), "elevenLabsKey") {
		t.Fatal("key leaked into Studio record")
	}
}

func TestStudioAudioRejectsInvalidLengthOrUnplayableFormatBeforeProvider(t *testing.T) {
	called := false
	mockStudioAudioProvider(t, func(*http.Request) (*http.Response, error) { called = true; return studioAudioProviderResponse(), nil })
	for _, body := range []string{
		`{"prompt":"Piano","audioType":"music","durationSeconds":2,"elevenLabsKey":"test-key"}`,
		`{"prompt":"Piano","audioType":"music","outputFormat":"pcm_44100","elevenLabsKey":"test-key"}`,
	} {
		response := httptest.NewRecorder()
		studioAudioGenerateHandler(response, httptest.NewRequest(http.MethodPost, "/studio/audio/generate", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid request: %d %s", response.Code, response.Body)
		}
	}
	if called {
		t.Fatal("invalid request reached a paid provider endpoint")
	}
}

func studioTestAudioAsset(t *testing.T, audioType string, projectID string) studioImageRecord {
	t.Helper()
	req, err := normalizeElevenLabsAudioRequest(ElevenLabsAudioRequest{Prompt: "Audio " + audioType, AudioType: audioType})
	if err != nil {
		t.Fatal(err)
	}
	asset, err := saveStudioAudioAsset(req, []byte("ID3saved audio"), "audio/mpeg", projectID)
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestStudioAudioListsNativeAssetsAndFiltersProjects(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	projectID := "AAAAAAAA-BBBB-CCCC-DDDD-111111111111"
	asset := studioTestAudioAsset(t, "music", projectID)
	other := studioTestAudioAsset(t, "voice", "")
	path, fields, _, err := studioAssetRaw(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	delete(fields, "audioType")
	delete(fields, "mimeType")
	fields["sourceService"] = json.RawMessage(`"ElevenLabs (Sound FX)"`)
	fields["usedInProjects"], _ = json.Marshal([]string{projectID})
	encoded, _ := json.Marshal(fields)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	studioTestAudioAsset(t, "sound", "")
	studioTestAsset(t)
	items, more, err := listStudioAudioPage(strings.ToLower(projectID), 0, 1)
	if err != nil || len(items) != 1 || !more {
		t.Fatalf("audio page = %#v %v %v", items, more, err)
	}
	items, more, err = listStudioAudioPage(projectID, 0, 24)
	if err != nil || len(items) != 2 || more {
		t.Fatalf("filtered catalog = %#v %v %v", items, more, err)
	}
	seen := map[string]string{}
	for _, item := range items {
		seen[item.ID] = item.AudioType
	}
	if seen[asset.ID] != "music" || seen[other.ID] != "sound" {
		t.Fatalf("native audio types = %#v", seen)
	}
	count, err := studioProjectAssetCount(projectID)
	if err != nil || count != 2 {
		t.Fatalf("project audio count = %d %v", count, err)
	}
}

func TestStudioAudioContentRangeAndNativeWAV(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	asset := studioTestAudioAsset(t, "voice", "")
	request := httptest.NewRequest(http.MethodGet, "/studio/audio/content?id="+asset.ID, nil)
	request.Header.Set("Range", "bytes=0-2")
	response := httptest.NewRecorder()
	studioAudioContentHandler(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "ID3" || response.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("range = %d %s %s", response.Code, response.Body, response.Header())
	}
	path, fields, _, err := studioAssetRaw(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	delete(fields, "mimeType")
	fields["dataBase64"], _ = json.Marshal(base64.StdEncoding.EncodeToString([]byte("RIFFabcdWAVEwav data")))
	encoded, _ := json.Marshal(fields)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	studioAudioContentHandler(response, httptest.NewRequest(http.MethodHead, "/studio/audio/content?id="+asset.ID, nil))
	if response.Code != http.StatusOK || response.Body.Len() != 0 || response.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("native WAV = %d %s %s", response.Code, response.Body, response.Header())
	}
}

func TestStudioAudioUsePreservesNativeMetadataAndProjectCopy(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	root := studioTestProject(t, "Audio project")
	asset := studioTestAudioAsset(t, "music", "")
	path, fields, _, err := studioAssetRaw(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	fields["unknown"] = json.RawMessage(`{"precise":9007199254740993}`)
	encoded, _ := json.Marshal(fields)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	used, project, relative, err := useStudioAudio(asset.ID, root)
	if err != nil {
		t.Fatal(err)
	}
	if project.AssetCount != 1 || len(used.UsedInProjects) != 1 || used.UsedInProjects[0] != project.ID || !strings.HasSuffix(relative, ".mp3") {
		t.Fatalf("audio usage = %#v %#v %s", used, project, relative)
	}
	copy, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil || !bytes.Equal(copy, []byte("ID3saved audio")) {
		t.Fatalf("project copy = %q %v", copy, err)
	}
	_, saved, _, err := studioAssetRaw(asset.ID)
	if err != nil || string(saved["unknown"]) != string(fields["unknown"]) {
		t.Fatalf("native metadata was rewritten: %#v %v", saved, err)
	}
	if _, _, _, err := useStudioAudio(asset.ID, root); err != nil {
		t.Fatalf("repeat use: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(relative)), []byte("new owner audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := useStudioAudio(asset.ID, root); err == nil {
		t.Fatal("changed project audio was overwritten or relinked")
	}
	copy, _ = os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if string(copy) != "new owner audio" {
		t.Fatal("owner's newer project audio was changed")
	}
}

func TestStudioAudioDeleteKeepsProjectCopy(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	root := studioTestProject(t, "Audio project")
	asset := studioTestAudioAsset(t, "voice", "")
	_, _, relative, err := useStudioAudio(asset.ID, root)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	studioAudioHandler(response, httptest.NewRequest(http.MethodDelete, "/studio/audio?id="+asset.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", response.Code, response.Body)
	}
	if _, _, _, err := readStudioAudio(asset.ID); !os.IsNotExist(err) {
		t.Fatalf("deleted asset = %v", err)
	}
	if copy, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative))); err != nil || string(copy) != "ID3saved audio" {
		t.Fatalf("project audio was removed: %q %v", copy, err)
	}
}

func TestStudioAudioRejectsSymlinkRecordAndProjectDestination(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	root := studioTestProject(t, "Audio project")
	asset := studioTestAudioAsset(t, "voice", "")
	path, _, encoded, err := studioAssetRaw(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	studioAudioContentHandler(response, httptest.NewRequest(http.MethodGet, "/studio/audio/content?id="+asset.ID, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("symlink record was read: %d", response.Code)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := chatWriteDirectory(root, "prototype/assets")
	if err != nil {
		t.Fatal(err)
	}
	outsideAudio := filepath.Join(t.TempDir(), "owner.mp3")
	if err := os.WriteFile(outsideAudio, []byte("ID3saved audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideAudio, filepath.Join(dir, "studio-"+strings.ToLower(asset.ID)+".mp3")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := useStudioAudio(asset.ID, root); err == nil {
		t.Fatal("symlink destination was accepted")
	}
}
