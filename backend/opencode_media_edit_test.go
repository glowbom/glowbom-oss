package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mediaEditProject(t *testing.T, content string) string {
	t.Helper()
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "prototype"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "prototype", "index.html"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return project
}

func mediaEditImage(t *testing.T, aspect string) []byte {
	t.Helper()
	width, height := 10, 10
	switch aspect {
	case "16:9":
		width, height = 16, 9
	case "9:16":
		width, height = 9, 16
	}
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func imageApprovalItem(token, prompt string) OpenCodeMediaApprovalItem {
	return OpenCodeMediaApprovalItem{ID: mediaApprovalItemID("image", token), Placeholder: token, MediaType: "image", Prompt: prompt, Provider: "OpenAI", SourceID: "openai-api"}
}

func TestMediaApprovalCarriesStableIdentityAndParameters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := mediaEditProject(t, `<img src="glowbomimage:forest"><video src="glowbyvideo:flyover|from:forest|aspect:9:16"></video><audio src="glowbyaudio:rain|type:sound|model:sfx|duration:5|influence:0.4|loop:true"></audio>`)
	reference := filepath.Join(project, "reference.png")
	if err := os.WriteFile(reference, projectIconTestImage(t, "png"), 0600); err != nil {
		t.Fatal(err)
	}
	req := OpenCodeMediaPostPassRequest{ProjectPath: project, ImageSource: "openai-subscription", ReferenceImagePath: reference}
	plan, err := buildOpenCodeMediaApproval(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 3 {
		t.Fatalf("items = %+v", plan.Items)
	}
	image, video, audio := plan.Items[0], plan.Items[1], plan.Items[2]
	if image.ID != mediaApprovalItemID("image", "glowbomimage:forest") || image.Placeholder != "glowbomimage:forest" || image.SourceID != "openai-subscription" || len(image.ReferenceImages) != 1 {
		t.Fatalf("image identity/reference = %+v", image)
	}
	if video.FromKey != "forest" || video.AspectRatio != "9:16" || video.SourceID != "veo-api" {
		t.Fatalf("video options = %+v", video)
	}
	if audio.AudioType != "sound" || audio.ModelID != "sfx" || audio.DurationSeconds != 5 || audio.PromptInfluence == nil || *audio.PromptInfluence != .4 || !audio.Loop {
		t.Fatalf("audio options = %+v", audio)
	}
	second, err := buildOpenCodeMediaApproval(req)
	if err != nil || second.Items[0].ID != image.ID {
		t.Fatal("approval IDs changed when rebuilding the same plan")
	}
}

func TestMediaApprovalInvalidEditsRemainPending(t *testing.T) {
	original := imageApprovalItem("glowbomimage:original", "original")
	cases := []struct {
		name string
		edit func([]OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem
	}{
		{"unknown ID", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].ID = "unknown"
			return items
		}},
		{"changed type", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].MediaType = "audio"
			return items
		}},
		{"changed placeholder", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].Placeholder = "glowbomimage:other"
			return items
		}},
		{"duplicate", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem { return append(items, items[0]) }},
		{"unsupported source", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].SourceID = "random"
			return items
		}},
		{"invalid reference", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].ReferenceImages = []string{"data:image/png;base64,bad"}
			return items
		}},
		{"too many references", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].ReferenceImages = []string{"a", "b"}
			return items
		}},
		{"wrong params", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].DurationSeconds = 5
			return items
		}},
		{"unsupported aspect", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].AspectRatio = "2:7"
			return items
		}},
		{"added without usage", func(items []OpenCodeMediaApprovalItem) []OpenCodeMediaApprovalItem {
			items[0].ID = "added-1"
			items[0].Placeholder = ""
			return items
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			project := mediaEditProject(t, `<img src="glowbomimage:original">`)
			id, responses := registerOpenCodeMediaApproval(project, &OpenCodeMediaApproval{Items: []OpenCodeMediaApprovalItem{original}})
			defer removeOpenCodeMediaApproval(id)
			respond := func(items []OpenCodeMediaApprovalItem) int {
				payload, _ := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: id, Response: "generate", ProjectPath: project, Items: items})
				w := httptest.NewRecorder()
				openCodeMediaApprovalRespondHandler(w, httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", bytes.NewReader(payload)))
				return w.Code
			}
			if status := respond(test.edit([]OpenCodeMediaApprovalItem{original})); status != http.StatusBadRequest {
				t.Fatalf("invalid response status = %d", status)
			}
			select {
			case <-responses:
				t.Fatal("invalid edit consumed approval")
			default:
			}
			if status := respond([]OpenCodeMediaApprovalItem{original}); status != http.StatusOK {
				t.Fatalf("corrected response status = %d", status)
			}
			decision, err := waitForOpenCodeMediaApproval(context.Background(), id, responses)
			if err != nil || decision.Response != "generate" || len(decision.Items) != 1 {
				t.Fatalf("decision = %+v, error = %v", decision, err)
			}
		})
	}
}

func TestMediaApprovalRejectsStalePlaceholderWithoutConsuming(t *testing.T) {
	project := mediaEditProject(t, `<img src="glowbomimage:original">`)
	original := imageApprovalItem("glowbomimage:original", "original")
	id, _ := registerOpenCodeMediaApproval(project, &OpenCodeMediaApproval{Items: []OpenCodeMediaApprovalItem{original}})
	defer removeOpenCodeMediaApproval(id)
	if err := os.WriteFile(filepath.Join(project, "prototype", "index.html"), []byte(`<img src="glowbomimage:changed">`), 0644); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: id, Response: "generate", Items: []OpenCodeMediaApprovalItem{original}})
	w := httptest.NewRecorder()
	openCodeMediaApprovalRespondHandler(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("stale response status = %d", w.Code)
	}
	openCodeMediaApprovalState.mu.Lock()
	pending := openCodeMediaApprovalState.pending[id]
	openCodeMediaApprovalState.mu.Unlock()
	if pending == nil || pending.answered {
		t.Fatal("stale approval was consumed")
	}
}

func TestApprovedMediaEditsReplaceOriginalAndPreserveExclusions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := mediaEditProject(t, `<img src="glowbomimage:old prompt"><img src="glowbomimage:excluded"><img src="glowbomimage:omitted">`)
	edited := imageApprovalItem("glowbomimage:old prompt", "new prompt")
	edited.SourceID, edited.AspectRatio = "gemini-api", "9:16"
	excluded := imageApprovalItem("glowbomimage:excluded", "excluded")
	excluded.Excluded = true
	added := imageApprovalItem("", "extra artwork")
	added.ID, added.UsagePrompt = "added-extra", "Place this in the welcome screen."
	oldGenerate := generatePostPassSelectedImage
	defer func() { generatePostPassSelectedImage = oldGenerate }()
	calls := []string{}
	generatePostPassSelectedImage = func(ctx context.Context, source, key, prompt, reference, aspect string) (string, string, error) {
		calls = append(calls, source+"|"+prompt+"|"+aspect)
		if reference != "" {
			t.Fatal("cleared inherited reference was still used")
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, aspect)), source, nil
	}
	resp, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{
		ProjectPath: project, Items: []OpenCodeMediaApprovalItem{edited, excluded, added}, ImageAPIKeys: map[string]string{"gemini-api": "test-key", "openai-api": "test-key"}, ReferenceImagePath: "missing.png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "gemini-api|new prompt|9:16" || len(resp.GeneratedAssets) != 2 {
		t.Fatalf("calls=%v, result=%+v", calls, resp)
	}
	html, _ := os.ReadFile(filepath.Join(project, "prototype", "index.html"))
	if strings.Contains(string(html), edited.Placeholder) || !strings.Contains(string(html), excluded.Placeholder) || !strings.Contains(string(html), "glowbomimage:omitted") {
		t.Fatalf("replacements=%s", html)
	}
	data, _ := os.ReadFile(filepath.Join(project, "prototype", "assets.json"))
	var manifest prototypeAssetsManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range manifest.Assets {
		if item.Prompt == added.Prompt {
			found = item.UsagePrompt == added.UsagePrompt
		}
	}
	if !found {
		t.Fatalf("added asset instructions missing: %s", data)
	}
	if !strings.Contains(buildAssetPlacementPrompt(project), "usagePrompt") {
		t.Fatal("placement prompt does not read approved usage instructions")
	}
}

func TestSelectedImageFailureNeverReplaysOrFallsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := mediaEditProject(t, `<img src="glowbomimage:original">`)
	item := imageApprovalItem("glowbomimage:original", "original")
	item.SourceID = "xai-api"
	item.ReferenceImages = []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))}
	oldGenerate := generatePostPassSelectedImage
	defer func() { generatePostPassSelectedImage = oldGenerate }()
	calls := 0
	generatePostPassSelectedImage = func(ctx context.Context, source, key, prompt, reference, aspect string) (string, string, error) {
		calls++
		if source != "xai-api" || reference == "" {
			t.Fatalf("unexpected request source=%s reference=%q", source, reference)
		}
		return "", "", fmt.Errorf("provider rejected request")
	}
	resp, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{item}, ImageAPIKeys: map[string]string{"xai-api": "test-key", "openai-api": "unused"}, ScanTargets: []string{"prototype/index.html", "prototype/index.html"}})
	if err != nil || calls != 1 || len(resp.GeneratedAssets) != 0 || len(resp.Warnings) == 0 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, resp, err)
	}
}

func TestInvalidImageReferenceNeverCallsProvider(t *testing.T) {
	oldGenerate := generatePostPassSelectedImage
	defer func() { generatePostPassSelectedImage = oldGenerate }()
	generatePostPassSelectedImage = func(context.Context, string, string, string, string, string) (string, string, error) {
		t.Fatal("invalid reference reached provider")
		return "", "", nil
	}
	_, _, err := generateImageForPostPass(OpenCodeMediaPostPassRequest{ImageSource: "openai-api", ImageAPIKeys: map[string]string{"openai-api": "test-key"}}, "asset", "bad-base64")
	if err == nil {
		t.Fatal("invalid reference was accepted")
	}
}

func TestApprovedAssetsWithSamePromptRemainDistinct(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := mediaEditProject(t, `<img src="glowbomimage:first"><img src="glowbomimage:second">`)
	first, second := imageApprovalItem("glowbomimage:first", "same prompt"), imageApprovalItem("glowbomimage:second", "same prompt")
	first.AspectRatio, second.AspectRatio = "1:1", "16:9"
	oldGenerate := generatePostPassSelectedImage
	defer func() { generatePostPassSelectedImage = oldGenerate }()
	generatePostPassSelectedImage = func(ctx context.Context, source, key, prompt, reference, aspect string) (string, string, error) {
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, aspect)), "OpenAI", nil
	}
	resp, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{first, second}, ImageAPIKeys: map[string]string{"openai-api": "test-key"}})
	if err != nil || len(resp.GeneratedAssets) != 2 || resp.GeneratedAssets[0].Filename == resp.GeneratedAssets[1].Filename {
		t.Fatalf("result=%+v err=%v", resp, err)
	}
}

func TestApprovedAudioUsesEditedParametersInsteadOfPromptOnlyReuse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := mediaEditProject(t, `<audio src="glowbyaudio:rain|type:sound|duration:1"></audio>`)
	config, _ := os.UserConfigDir()
	studio := filepath.Join(config, "Glowbom", "Studio", "Assets")
	if err := os.MkdirAll(studio, 0755); err != nil {
		t.Fatal(err)
	}
	cached, _ := json.Marshal(studioAssetRecord{ID: "old-audio", MediaType: "audio", Prompt: "rain", SourceService: "ElevenLabs", DataBase64: base64.StdEncoding.EncodeToString([]byte("old sound"))})
	if err := os.WriteFile(filepath.Join(studio, "old.json"), cached, 0600); err != nil {
		t.Fatal(err)
	}
	influence := .8
	item := OpenCodeMediaApprovalItem{ID: mediaApprovalItemID("audio", "glowbyaudio:rain|type:sound|duration:1"), Placeholder: "glowbyaudio:rain|type:sound|duration:1", MediaType: "audio", Prompt: "rain", Provider: "ElevenLabs", SourceID: "elevenlabs-api", AudioType: "sound", ModelID: "eleven_text_to_sound_v2", DurationSeconds: 8, PromptInfluence: &influence, Loop: true}
	oldAudio := generatePostPassAudio
	defer func() { generatePostPassAudio = oldAudio }()
	calls := 0
	generatePostPassAudio = func(placeholder postPassAudioPlaceholder, apiKey, defaultVoiceID, defaultVoiceModel string) ([]byte, string, string, error) {
		calls++
		if placeholder.durationSeconds != 8 || placeholder.modelID != "eleven_text_to_sound_v2" || placeholder.promptInfluence == nil || *placeholder.promptInfluence != .8 || !placeholder.loop {
			t.Fatalf("lost audio edits: %+v", placeholder)
		}
		return []byte("new sound"), "audio/mpeg", "ElevenLabs (Sound FX)", nil
	}
	resp, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{item}})
	if err != nil || calls != 1 || len(resp.GeneratedAssets) != 1 || len(resp.ReusedStudioAssets) != 0 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, resp, err)
	}
}

func TestApprovedVideoKeepsOriginalImageReferenceAndUsesCentralKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := mediaEditProject(t, `<img src="glowbomimage:old frame"><video src="glowbyvideo:move|from:old frame"></video>`)
	frame := imageApprovalItem("glowbomimage:old frame", "edited frame")
	video := OpenCodeMediaApprovalItem{ID: mediaApprovalItemID("video", "glowbyvideo:move|from:old frame"), Placeholder: "glowbyvideo:move|from:old frame", MediaType: "video", Prompt: "new motion", Provider: "Veo", SourceID: "veo-api", FromKey: "old frame", AspectRatio: "9:16"}
	oldImage, oldVideo := generatePostPassSelectedImage, generatePostPassVideo
	defer func() { generatePostPassSelectedImage, generatePostPassVideo = oldImage, oldVideo }()
	generatePostPassSelectedImage = func(context.Context, string, string, string, string, string) (string, string, error) {
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, "")), "OpenAI", nil
	}
	calls := 0
	generatePostPassVideo = func(ctx context.Context, prompt string, options studioVideoOptions, key string, frame []byte, mime string) ([]byte, error) {
		calls++
		if prompt != "new motion" || options.AspectRatio != "9:16" || options.DurationSeconds != 4 || key != "current-central-key" || len(frame) == 0 {
			t.Fatalf("lost video edits: prompt=%s options=%+v bytes=%d", prompt, options, len(frame))
		}
		return []byte("test video"), nil
	}
	resp, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{frame, video}, ImageAPIKeys: map[string]string{"openai-api": "test-key", "gemini-api": "current-central-key"}, VeoGeminiKey: "older-key"})
	if err != nil || calls != 1 || len(resp.GeneratedAssets) != 2 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, resp, err)
	}
}

func TestMediaApprovalVideoCannotUseExcludedOriginalFrame(t *testing.T) {
	frame := imageApprovalItem("glowbomimage:old frame", "old frame")
	edited := frame
	edited.Prompt, edited.Excluded = "edited frame", true
	video := OpenCodeMediaApprovalItem{ID: "added-video", MediaType: "video", Prompt: "motion", SourceID: "veo-api", FromKey: "old frame", UsagePrompt: "Use as the welcome background."}
	if err := validateMediaApprovalVideoReferences(map[string]OpenCodeMediaApprovalItem{frame.ID: frame}, []OpenCodeMediaApprovalItem{edited, video}); err == nil {
		t.Fatal("excluded original frame was allowed")
	}
	edited.Excluded = false
	if err := validateMediaApprovalVideoReferences(map[string]OpenCodeMediaApprovalItem{frame.ID: frame}, []OpenCodeMediaApprovalItem{edited, video}); err != nil {
		t.Fatalf("edited included frame should keep original key: %v", err)
	}
}

func TestAssetPlacementUsesConfiguredDefaultModel(t *testing.T) {
	params := assetPlacementPromptParams("/tmp/project", "", "")
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, exists := decoded["model"]; exists {
		t.Fatalf("configured default should omit model: %s", data)
	}
	if !strings.Contains(string(data), "usagePrompt") {
		t.Fatal("placement prompt lost approved added asset instructions")
	}
	params = assetPlacementPromptParams("/tmp/project", "selected-model", "selected-provider")
	data, err = json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "selected-model") || !strings.Contains(string(data), "selected-provider") {
		t.Fatalf("selected model was lost: %s", data)
	}
}
