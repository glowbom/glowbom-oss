package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func imageSettingsFixture(t *testing.T, content string) (string, *OpenCodeMediaApproval) {
	t.Helper()
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	project := mediaEditProject(t, content)
	if err := os.WriteFile(filepath.Join(project, "glowbom.json"), []byte(`{"name":"Image settings fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{ProjectPath: project, ImageSource: "openai-api"})
	if err != nil || plan == nil || len(plan.Items) == 0 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	return project, plan
}

func mockPostPassImageOptions(t *testing.T, generate func(context.Context, studioImageOptions, string, string, string) (string, string, error)) {
	t.Helper()
	previous := generatePostPassSelectedImageWithOptions
	generatePostPassSelectedImageWithOptions = generate
	t.Cleanup(func() { generatePostPassSelectedImageWithOptions = previous })
}

func setImageApprovalOptions(item *OpenCodeMediaApprovalItem, options studioImageOptions) {
	item.SourceID, item.ModelID, item.AspectRatio = options.SourceID, options.ModelID, options.AspectRatio
	item.Resolution, item.Quality = options.Resolution, options.Quality
}

func TestMediaApprovalImageSettingsSurviveReviewAndGeneration(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "image-fixture-token")
	t.Setenv("GLOWBOM_ENABLE_GROK_SUBSCRIPTION_MEDIA", "1")
	for _, options := range []studioImageOptions{
		{SourceID: "xai-api", ModelID: "grok-imagine-image-2.0", AspectRatio: "16:9", Resolution: "2k", Quality: "medium"},
		{SourceID: "gemini-api", ModelID: "gemini-3.1-flash-image", AspectRatio: "9:16", Resolution: "4K"},
		{SourceID: "gemini-api", ModelID: "gemini-3-pro-image", AspectRatio: "1:1", Resolution: "2K"},
		{SourceID: "openai-api", ModelID: "gpt-image-2.5-flare", AspectRatio: "1:1", Resolution: "1024x1024", Quality: "xhigh"},
		{SourceID: "openai-subscription", ModelID: "gpt-image-2", AspectRatio: "1:1"},
		{SourceID: "xai-subscription", ModelID: "grok-imagine-image-2.0", AspectRatio: "1:1", Resolution: "2k", Quality: "medium"},
	} {
		t.Run(options.SourceID+"/"+options.ModelID, func(t *testing.T) {
			project, plan := imageSettingsFixture(t, `<img src="glowbomimage:image settings">`)
			item := plan.Items[0]
			setImageApprovalOptions(&item, options)
			item.Prompt = "reviewed image prompt"
			reference := "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, ""))
			item.ReferenceImages = []string{reference}
			approvalID, responses := registerOpenCodeMediaApproval(project, plan)
			t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
			keys := map[string]string{"openai-api": "review-openai-fixture-key", "gemini-api": "review-google-fixture-key", "xai-api": "review-xai-fixture-key"}
			body, err := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: approvalID, ProjectPath: project, Response: "generate", Items: []OpenCodeMediaApprovalItem{item}, ImageAPIKeys: keys})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer image-fixture-token")
			recorder := httptest.NewRecorder()
			openCodeMediaApprovalRespondHandler(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("review failed: %d %s", recorder.Code, recorder.Body.String())
			}
			decision, err := waitForOpenCodeMediaApproval(context.Background(), approvalID, responses)
			if err != nil || len(decision.Items) != 1 {
				t.Fatalf("decision=%+v err=%v", decision, err)
			}
			selected := decision.Items[0]
			if selected.SourceID != options.SourceID || selected.ModelID != options.ModelID || selected.AspectRatio != options.AspectRatio || selected.Resolution != options.Resolution || selected.Quality != options.Quality {
				t.Fatalf("review changed image settings: %+v", selected)
			}
			calls := 0
			mockPostPassImageOptions(t, func(_ context.Context, actual studioImageOptions, key, prompt, ref string) (string, string, error) {
				calls++
				if actual != options || prompt != item.Prompt || ref == "" || (strings.HasSuffix(options.SourceID, "subscription") && key != "") || (!strings.HasSuffix(options.SourceID, "subscription") && key != keys[options.SourceID]) {
					t.Fatalf("generation changed settings: %+v keyMatched=%t prompt=%s referencePresent=%t", actual, key == keys[options.SourceID], prompt, ref != "")
				}
				return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, options.AspectRatio)), "fixture provider", nil
			})
			result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: decision.Items, ImageAPIKeys: decision.ImageAPIKeys, imageSubscriptionAuthorized: decision.imageSubscriptionAuthorized, OpenAIKey: "captured-openai-fixture-key", GeminiKey: "captured-google-fixture-key", XaiKey: "captured-xai-fixture-key"})
			if err != nil || calls != 1 || result == nil || len(result.GeneratedAssets) != 1 {
				t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
			}
			saved, err := findStudioAsset(result.GeneratedAssets[0].StudioAssetID)
			if err != nil || saved.SourceID != options.SourceID || saved.Model != options.ModelID || saved.Resolution != options.Resolution || saved.Quality != options.Quality {
				t.Fatalf("saved image settings=%+v err=%v", saved, err)
			}
			assertVideoSettingsSecretsAbsent(t, []string{project, os.Getenv("GLOWBOM_STUDIO_DIR")}, []string{"review-openai-fixture-key", "review-google-fixture-key", "review-xai-fixture-key", "captured-openai-fixture-key", "captured-google-fixture-key", "captured-xai-fixture-key", "image-fixture-token"})
		})
	}
}

func TestInvalidReviewedImageSettingsNeverReachGeneration(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*OpenCodeMediaApprovalItem)
	}{
		{"unknown source", func(item *OpenCodeMediaApprovalItem) { item.SourceID = "unknown-api" }},
		{"unknown model", func(item *OpenCodeMediaApprovalItem) { item.ModelID = "unknown-model" }},
		{"wrong provider model", func(item *OpenCodeMediaApprovalItem) { item.ModelID = "gemini-3.1-flash-image" }},
		{"unsupported resolution", func(item *OpenCodeMediaApprovalItem) { item.Resolution = "4k" }},
		{"unsupported quality", func(item *OpenCodeMediaApprovalItem) { item.Quality = "max" }},
		{"unsupported aspect", func(item *OpenCodeMediaApprovalItem) { item.AspectRatio = "5:1" }},
		{"image duration", func(item *OpenCodeMediaApprovalItem) { item.DurationSeconds = 5 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, plan := imageSettingsFixture(t, `<img src="glowbomimage:invalid image settings">`)
			item := plan.Items[0]
			setImageApprovalOptions(&item, studioImageOptions{SourceID: "xai-api", ModelID: "grok-imagine-image-2.0", AspectRatio: "1:1", Resolution: "1k", Quality: "low"})
			test.edit(&item)
			calls := 0
			mockPostPassImageOptions(t, func(context.Context, studioImageOptions, string, string, string) (string, string, error) {
				calls++
				return "", "", errors.New("invalid settings reached generation")
			})
			approvalID, responses := registerOpenCodeMediaApproval(project, plan)
			t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
			body, _ := json.Marshal(openCodeMediaApprovalResponse{ApprovalID: approvalID, ProjectPath: project, Response: "generate", Items: []OpenCodeMediaApprovalItem{item}})
			recorder := httptest.NewRecorder()
			openCodeMediaApprovalRespondHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/media/approval/respond", strings.NewReader(string(body))))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("invalid review accepted: %d %s", recorder.Code, recorder.Body.String())
			}
			select {
			case <-responses:
				t.Fatal("invalid settings consumed the pending approval")
			default:
			}
			result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{item}, ImageAPIKeys: map[string]string{"xai-api": "unused-fixture-key"}})
			if err == nil || calls != 0 || result != nil {
				t.Fatalf("invalid settings reached generation: calls=%d result=%+v err=%v", calls, result, err)
			}
		})
	}
}

func TestPostPassImageSharedKeyPrecedenceAndAuthorization(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "environment-google-fixture-key")
	for _, test := range []struct {
		name    string
		request OpenCodeMediaPostPassRequest
		store   *postPassVideoTestKeyStore
		key     string
		reads   int
		error   string
	}{
		{"legacy captured", OpenCodeMediaPostPassRequest{GeminiKey: "captured-google-fixture-key"}, &postPassVideoTestKeyStore{key: "saved-google-fixture-key"}, "captured-google-fixture-key", 0, ""},
		{"empty map current environment", OpenCodeMediaPostPassRequest{ImageAPIKeys: map[string]string{}, GeminiKey: "captured-google-fixture-key"}, &postPassVideoTestKeyStore{key: "saved-google-fixture-key"}, "environment-google-fixture-key", 0, ""},
		{"saved over captured", OpenCodeMediaPostPassRequest{ImageAPIKeys: map[string]string{}, ImageUseSavedKey: true, imageSavedKeyAuthorized: true, GeminiKey: "captured-google-fixture-key"}, &postPassVideoTestKeyStore{key: " saved-google-fixture-key "}, "saved-google-fixture-key", 1, ""},
		{"explicit over saved", OpenCodeMediaPostPassRequest{ImageAPIKeys: map[string]string{"gemini-api": " explicit-google-fixture-key "}, ImageUseSavedKey: true, imageSavedKeyAuthorized: true, GeminiKey: "captured-google-fixture-key"}, &postPassVideoTestKeyStore{key: "saved-google-fixture-key"}, "explicit-google-fixture-key", 0, ""},
		{"missing saved environment fallback", OpenCodeMediaPostPassRequest{ImageAPIKeys: map[string]string{}, ImageUseSavedKey: true, imageSavedKeyAuthorized: true}, &postPassVideoTestKeyStore{}, "environment-google-fixture-key", 1, ""},
		{"unauthorized saved", OpenCodeMediaPostPassRequest{ImageAPIKeys: map[string]string{}, ImageUseSavedKey: true}, &postPassVideoTestKeyStore{key: "saved-google-fixture-key"}, "", 0, "Local authentication"},
		{"unavailable saved safe error", OpenCodeMediaPostPassRequest{ImageUseSavedKey: true, imageSavedKeyAuthorized: true}, &postPassVideoTestKeyStore{err: errors.New("private vault path and fixture-secret")}, "", 1, "Unlock your system credential store"},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := studioVideoKeys
			studioVideoKeys = test.store
			t.Cleanup(func() { studioVideoKeys = previous })
			key, err := resolvePostPassImageKey(context.Background(), test.request, "gemini-api")
			if key != test.key || test.store.reads != test.reads || (test.error == "" && err != nil) || (test.error != "" && (err == nil || !strings.Contains(err.Error(), test.error))) {
				t.Fatalf("keyMatched=%t reads=%d err=%v", key == test.key, test.store.reads, err)
			}
			if err != nil && (strings.Contains(err.Error(), "private vault") || strings.Contains(err.Error(), "fixture-secret")) {
				t.Fatal("credential error exposed private data")
			}
		})
	}
}

func TestPostPassImageSubscriptionNeverUsesAPIKey(t *testing.T) {
	t.Setenv("GLOWBOM_ENABLE_GROK_SUBSCRIPTION_MEDIA", "1")
	store := &postPassVideoTestKeyStore{key: "saved-api-fixture-key"}
	previous := studioVideoKeys
	studioVideoKeys = store
	t.Cleanup(func() { studioVideoKeys = previous })
	for _, source := range []string{"openai-subscription", "xai-subscription"} {
		t.Run(source, func(t *testing.T) {
			options, err := normalizeStudioImageOptions(studioImageOptions{SourceID: source})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			mockPostPassImageOptions(t, func(_ context.Context, actual studioImageOptions, key, _, _ string) (string, string, error) {
				calls++
				if actual != options || key != "" {
					t.Fatalf("subscription used API credentials: %+v keyPresent=%t", actual, key != "")
				}
				return "fixture result", "fixture subscription", nil
			})
			req := OpenCodeMediaPostPassRequest{ImageSource: source, imageOptions: &options, ImageAPIKeys: map[string]string{"openai-api": "explicit-api-fixture-key", "xai-api": "explicit-api-fixture-key"}, OpenAIKey: "captured-api-fixture-key", XaiKey: "captured-api-fixture-key"}
			if _, _, err := generateImageForPostPass(req, "subscription image", ""); err == nil || calls != 0 || store.reads != 0 {
				t.Fatalf("unauthorized subscription reached generation: calls=%d reads=%d err=%v", calls, store.reads, err)
			}
			req.imageSubscriptionAuthorized = true
			if _, _, err := generateImageForPostPass(req, "subscription image", ""); err != nil || calls != 1 || store.reads != 0 {
				t.Fatalf("subscription resolution failed: calls=%d reads=%d err=%v", calls, store.reads, err)
			}
		})
	}
}

func TestImagePrivateAuthorizationCannotBeForgedOverHTTP(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "image-fixture-token")
	t.Setenv("GLOWBOM_ENABLE_GROK_SUBSCRIPTION_MEDIA", "1")
	store := &postPassVideoTestKeyStore{key: "saved-api-fixture-key"}
	previous := studioVideoKeys
	studioVideoKeys = store
	t.Cleanup(func() { studioVideoKeys = previous })
	for _, source := range []string{"openai-api", "openai-subscription", "xai-subscription"} {
		t.Run(source, func(t *testing.T) {
			project, plan := imageSettingsFixture(t, `<img src="glowbomimage:authenticated image">`)
			options, err := normalizeStudioImageOptions(studioImageOptions{SourceID: source})
			if err != nil {
				t.Fatal(err)
			}
			item := plan.Items[0]
			setImageApprovalOptions(&item, options)
			approvalID, responses := registerOpenCodeMediaApproval(project, plan)
			t.Cleanup(func() { removeOpenCodeMediaApproval(approvalID) })
			saved := source == "openai-api"
			payload := map[string]any{"approvalID": approvalID, "projectPath": project, "response": "generate", "items": []OpenCodeMediaApprovalItem{item}, "imageUseSavedKey": saved, "imageSavedKeyAuthorized": true, "imageSubscriptionAuthorized": true}
			for _, handler := range []http.HandlerFunc{openCodeMediaApprovalRespondHandler, openCodeMediaPostPassHandler} {
				body, _ := json.Marshal(payload)
				recorder := httptest.NewRecorder()
				handler(recorder, httptest.NewRequest(http.MethodPost, "/media", strings.NewReader(string(body))))
				if recorder.Code != http.StatusUnauthorized || store.reads != 0 {
					t.Fatalf("forged authorization accepted: status=%d reads=%d", recorder.Code, store.reads)
				}
			}
			select {
			case <-responses:
				t.Fatal("unauthenticated response consumed approval")
			default:
			}
			body, _ := json.Marshal(payload)
			request := httptest.NewRequest(http.MethodPost, "/media", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer image-fixture-token")
			recorder := httptest.NewRecorder()
			openCodeMediaApprovalRespondHandler(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("authenticated approval failed: %d %s", recorder.Code, recorder.Body.String())
			}
			decision, err := waitForOpenCodeMediaApproval(context.Background(), approvalID, responses)
			if err != nil || decision.ImageUseSavedKey != saved || decision.imageSavedKeyAuthorized != saved || decision.imageSubscriptionAuthorized != !saved {
				t.Fatalf("verified private authorization was lost: saved=%t err=%v", saved, err)
			}
			encoded, _ := json.Marshal(decision)
			if strings.Contains(string(encoded), "Authorized") || strings.Contains(string(encoded), "image-fixture-token") {
				t.Fatal("private authorization was exposed in JSON")
			}
			result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: []OpenCodeMediaApprovalItem{item}, ImageUseSavedKey: saved})
			if err == nil || result != nil || store.reads != 0 {
				t.Fatalf("unverified internal request accepted: result=%+v reads=%d err=%v", result, store.reads, err)
			}
		})
	}
}

func TestRefineImageCredentialsRequireAuthenticatedIntake(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "image-fixture-token")
	for _, payload := range []map[string]any{
		{"imageUseSavedKey": true, "imageSavedKeyAuthorized": true},
		{"imageSource": "openai-subscription", "imageSubscriptionAuthorized": true},
		{"imageSource": "xai-subscription", "imageSubscriptionAuthorized": true},
	} {
		body, _ := json.Marshal(payload)
		recorder := httptest.NewRecorder()
		openCodeRefineHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(body))))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("unverified credentials accepted at intake: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestReviewedImageSettingsUseDistinctFilesAndRequests(t *testing.T) {
	project, plan := imageSettingsFixture(t, `<img src="glowbomimage:first image"><img src="glowbomimage:second image">`)
	options := []studioImageOptions{
		{SourceID: "xai-api", ModelID: "grok-imagine-image-2.0", AspectRatio: "1:1", Resolution: "1k", Quality: "low"},
		{SourceID: "xai-api", ModelID: "grok-imagine-image-2.0", AspectRatio: "1:1", Resolution: "2k", Quality: "medium"},
	}
	for i := range plan.Items {
		setImageApprovalOptions(&plan.Items[i], options[i])
		plan.Items[i].Prompt = "same reviewed image prompt"
	}
	calls := 0
	mockPostPassImageOptions(t, func(_ context.Context, actual studioImageOptions, _, prompt, _ string) (string, string, error) {
		if calls >= len(options) || actual != options[calls] || prompt != "same reviewed image prompt" {
			t.Fatalf("image settings changed: %+v prompt=%s", actual, prompt)
		}
		calls++
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, "")), "fixture provider", nil
	})
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: plan.Items, ImageAPIKeys: map[string]string{"xai-api": "explicit-fixture-key"}})
	if err != nil || result == nil || calls != 2 || len(result.GeneratedAssets) != 2 || result.GeneratedAssets[0].Filename == result.GeneratedAssets[1].Filename || result.GeneratedAssets[0].StudioAssetID == result.GeneratedAssets[1].StudioAssetID {
		t.Fatalf("different reviewed settings shared an image: calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestUnreviewedImageSettingsBypassPromptCacheAndUseDistinctFiles(t *testing.T) {
	project, _ := imageSettingsFixture(t, `<img src="glowbomimage:same image prompt">`)
	imageURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, ""))
	studioAssets := []studioAssetRecord{{ID: "wrong-model-fixture", MediaType: "image", Prompt: "same image prompt", SourceService: "SpaceXAI", DataBase64: imageURI}}
	options := []studioImageOptions{
		{SourceID: "xai-api", ModelID: "grok-imagine-image", AspectRatio: "1:1", Resolution: "1k"},
		{SourceID: "xai-api", ModelID: "grok-imagine-image-2.0", AspectRatio: "1:1", Resolution: "2k", Quality: "medium"},
	}
	calls := 0
	mockPostPassImageOptions(t, func(_ context.Context, actual studioImageOptions, _, _, _ string) (string, string, error) {
		if calls >= len(options) || actual != options[calls] {
			t.Fatalf("image settings changed: %+v", actual)
		}
		calls++
		return imageURI, "SpaceXAI", nil
	})
	response := &OpenCodeMediaPostPassResponse{}
	for i := range options {
		req := OpenCodeMediaPostPassRequest{ImageSource: "xai-api", imageOptions: &options[i], ImageAPIKeys: map[string]string{"xai-api": "explicit-fixture-key"}}
		if _, err := materializeImagePlaceholder(req, project, "same image prompt", "glowbomimage:same image prompt", "", studioAssets, response); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || len(response.ReusedStudioAssets) != 0 || len(response.GeneratedAssets) != 2 || response.GeneratedAssets[0].Filename == response.GeneratedAssets[1].Filename {
		t.Fatalf("explicit settings reused another image: calls=%d result=%+v", calls, response)
	}
}

func TestLegacyImageApprovalKeepsLegacyGenerator(t *testing.T) {
	project, plan := imageSettingsFixture(t, `<img src="glowbomimage:legacy image">`)
	if mediaApprovalHasImageOptions(plan.Items[0]) {
		t.Fatal("legacy review unexpectedly selected a new image model")
	}
	previous := generatePostPassSelectedImage
	t.Cleanup(func() { generatePostPassSelectedImage = previous })
	calls := 0
	generatePostPassSelectedImage = func(_ context.Context, source, key, prompt, reference, aspect string) (string, string, error) {
		calls++
		if source != "openai-api" || key != "legacy-fixture-key" || prompt != "legacy image" || reference != "" || aspect != "" {
			t.Fatalf("legacy request changed: source=%s keyMatched=%t prompt=%s aspect=%s", source, key == "legacy-fixture-key", prompt, aspect)
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEditImage(t, "")), "OpenAI", nil
	}
	mockPostPassImageOptions(t, func(context.Context, studioImageOptions, string, string, string) (string, string, error) {
		t.Fatal("legacy image reached the model-specific generator")
		return "", "", nil
	})
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: plan.Items, OpenAIKey: "legacy-fixture-key"})
	if err != nil || result == nil || calls != 1 || len(result.GeneratedAssets) != 1 {
		t.Fatalf("legacy request failed: calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestReviewedGoogleFourKImageCanBeSavedToProjectAndStudio(t *testing.T) {
	project, plan := imageSettingsFixture(t, `<img src="glowbomimage:large image">`)
	options := studioImageOptions{SourceID: "gemini-api", ModelID: "gemini-3.1-flash-image", AspectRatio: "16:9", Resolution: "4K"}
	setImageApprovalOptions(&plan.Items[0], options)
	var payload bytes.Buffer
	if err := png.Encode(&payload, image.NewNRGBA(image.Rect(0, 0, 5504, 3072))); err != nil {
		t.Fatal(err)
	}
	calls := 0
	mockPostPassImageOptions(t, func(_ context.Context, actual studioImageOptions, _, _, _ string) (string, string, error) {
		calls++
		if actual != options {
			t.Fatalf("4K settings changed: %+v", actual)
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload.Bytes()), "Google", nil
	})
	result, err := runOpenCodeMediaPostPass(context.Background(), OpenCodeMediaPostPassRequest{ProjectPath: project, Items: plan.Items, ImageAPIKeys: map[string]string{"gemini-api": "four-k-fixture-key"}})
	if err != nil || result == nil || calls != 1 || len(result.GeneratedAssets) != 1 {
		t.Fatalf("4K output was rejected after generation: calls=%d result=%+v err=%v", calls, result, err)
	}
	saved, err := findStudioAsset(result.GeneratedAssets[0].StudioAssetID)
	if err != nil || saved.Resolution != "4K" || saved.Dimensions == nil || saved.Dimensions.Width != 5504 || saved.Dimensions.Height != 3072 {
		t.Fatalf("4K output was not saved to Studio: %+v err=%v", saved, err)
	}
	data, err := os.ReadFile(filepath.Join(project, result.GeneratedAssets[0].RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 5504 || config.Height != 3072 {
		t.Fatalf("project output changed size: %+v err=%v", config, err)
	}
}
