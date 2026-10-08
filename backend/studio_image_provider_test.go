package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/zalando/go-keyring"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStudioImageOptionsRejectUnsupportedBeforeProvider(t *testing.T) {
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid settings contacted provider")
		return nil, nil
	})
	for _, options := range []studioImageOptions{
		{SourceID: "openai-api", ModelID: "unknown"},
		{SourceID: "gemini-api", ModelID: "gemini-2.5-flash-image"},
		{SourceID: "gemini-api", ModelID: "gemini-3.1-flash-lite-image", Resolution: "4K"},
		{SourceID: "xai-api", ModelID: "grok-imagine-image", Quality: "medium"},
		{SourceID: "xai-api", Resolution: "8k"},
		{SourceID: "openai-api", ModelID: "gpt-image-1", Quality: "max"},
		{SourceID: "openai-subscription", ModelID: "gpt-image-2.5-flare"},
		{SourceID: "glowbom-api", Resolution: "1K"},
		{SourceID: "glowbom-api", AspectRatio: "1:1"},
	} {
		if _, _, err := generateStudioSelectedImageWithOptions(context.Background(), options, "fixture-key", "An icon", ""); err == nil {
			t.Fatalf("accepted unsupported settings: %#v", options)
		}
	}
}

func TestStudioImageGrokSelectedModelResolutionAndQualityNoFallback(t *testing.T) {
	for _, model := range []string{"grok-imagine-image-2.0", "grok-imagine-image"} {
		t.Run(model, func(t *testing.T) {
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Fatal("invalid body")
				}
				if r.URL.Path != "/v1/images/generations" || body["model"] != model || body["resolution"] != "2k" || body["aspect_ratio"] != "1:1" {
					t.Fatalf("wrong settings: %#v", body)
				}
				if model == "grok-imagine-image-2.0" && body["quality"] != "low" {
					t.Fatal("quality omitted")
				}
				if model == "grok-imagine-image" && body["quality"] != nil {
					t.Fatal("unsupported quality field")
				}
				return studioRecoveryResponse(422, "application/json", `{"error":"fixture-key private prompt"}`), nil
			})
			_, _, err := generateStudioSelectedImageWithOptions(context.Background(), studioImageOptions{SourceID: "xai-api", ModelID: model, Resolution: "2k"}, "fixture-key", "An icon", "")
			if err == nil || calls != 1 || strings.Contains(err.Error(), "fixture-key") {
				t.Fatalf("fallback or secret in error: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestStudioImageGoogleSelectedModelsAndSizes(t *testing.T) {
	for _, model := range []string{"gemini-3.1-flash-lite-image", "gemini-3.1-flash-image", "gemini-3-pro-image"} {
		t.Run(model, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1beta/models/"+model+":generateContent" || r.Header.Get("x-goog-api-key") != "fixture-key" {
					t.Fatal("wrong model or key")
				}
				var body struct {
					GenerationConfig struct {
						ImageConfig struct{ AspectRatio, ImageSize string }
					}
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.GenerationConfig.ImageConfig.ImageSize != "1K" || body.GenerationConfig.ImageConfig.AspectRatio != "1:1" {
					t.Fatal("missing explicit size or shape")
				}
				return studioRecoveryResponse(200, "application/json", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"`+encoded+`"}}]}}]}`), nil
			})
			if _, _, err := generateStudioSelectedImageWithOptions(context.Background(), studioImageOptions{SourceID: "gemini-api", ModelID: model}, "fixture-key", "An icon", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStudioImageOpenAISelectedOptionsGenerationAndEdit(t *testing.T) {
	for _, reference := range []string{"", base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))} {
		t.Run(map[bool]string{true: "edit", false: "generate"}[reference != ""], func(t *testing.T) {
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				if reference == "" {
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "gpt-image-2.5-sunburst" || body["quality"] != "xhigh" || body["size"] != "1024x1024" {
						t.Fatalf("wrong settings: %#v", body)
					}
				} else {
					if r.URL.Path != "/v1/images/edits" || r.ParseMultipartForm(1<<20) != nil || r.FormValue("model") != "gpt-image-2.5-sunburst" || r.FormValue("quality") != "xhigh" || r.FormValue("size") != "1024x1024" {
						t.Fatal("wrong edit settings")
					}
				}
				return iconProviderResponse(projectIconTestImage(t, "png")), nil
			})
			if _, _, err := generateStudioSelectedImageWithOptions(context.Background(), studioImageOptions{SourceID: "openai-api", ModelID: "gpt-image-2.5-sunburst", Quality: "xhigh"}, "fixture-key", "An icon", reference); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStudioSharedProviderKeyGoogleVideoAliasAndAuthorization(t *testing.T) {
	isolateStudioProgress(t)
	isolateProjectIconCredentials(t)
	previous := studioVideoKeys
	studioVideoKeys = fixtureStudioVideoKeys{}
	t.Cleanup(func() { studioVideoKeys = previous })
	w := httptest.NewRecorder()
	studioProviderKeyHandler(w, studioProgressRequest(http.MethodPost, "/studio/providers/key", `{"sourceId":"gemini-api","key":"private-shared-google"}`))
	if w.Code != 200 {
		t.Fatalf("save failed: %d", w.Code)
	}
	for _, source := range []string{"gemini-api", "veo-api"} {
		key, err := resolveStudioProviderKey(context.Background(), source, "", true)
		if err != nil || key != "private-shared-google" {
			t.Fatalf("alias failed: %s %v", source, err)
		}
	}
	w = httptest.NewRecorder()
	studioVideoKeyHandler(w, studioProgressRequest(http.MethodGet, "/studio/videos/key?sourceId=veo-api", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"saved":true`) || strings.Contains(w.Body.String(), "private-shared-google") {
		t.Fatal("legacy status mismatch")
	}
	w = httptest.NewRecorder()
	studioProviderKeyHandler(w, httptest.NewRequest(http.MethodGet, "/studio/providers/key?sourceId=gemini-api", nil))
	if w.Code != 401 {
		t.Fatal("saved key status allowed without token")
	}
	w = httptest.NewRecorder()
	studioProviderKeyHandler(w, studioProgressRequest(http.MethodDelete, "/studio/providers/key?sourceId=gemini-api", ""))
	if w.Code != 200 || studioVideoKeys.(fixtureStudioVideoKeys)["veo-api"] != "" {
		t.Fatal("alias delete failed")
	}
}

func TestStudioProviderSystemStoreMigratesLegacyVideoKeys(t *testing.T) {
	previousGet, previousSet, previousDelete := studioProviderKeyringGet, studioProviderKeyringSet, studioProviderKeyringDelete
	t.Cleanup(func() {
		studioProviderKeyringGet, studioProviderKeyringSet, studioProviderKeyringDelete = previousGet, previousSet, previousDelete
	})
	entries := map[string]string{"Glowbom Video/Google Gemini": "legacy-fixture-key"}
	studioProviderKeyringGet = func(service, account string) (string, error) {
		key, ok := entries[service+"/"+account]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return key, nil
	}
	studioProviderKeyringSet = func(service, account, key string) error { entries[service+"/"+account] = key; return nil }
	studioProviderKeyringDelete = func(service, account string) error { delete(entries, service+"/"+account); return nil }
	store := systemStudioVideoKeyStore{}
	if key, err := store.Get("gemini-api"); err != nil || key != "legacy-fixture-key" || entries["Glowbom Providers/Google Gemini"] != key {
		t.Fatal("legacy key was not migrated to shared storage")
	}
	if err := store.Set("gemini-api", "replacement-fixture-key"); err != nil {
		t.Fatal(err)
	}
	if key, err := store.Get("veo-api"); err != nil || key != "replacement-fixture-key" {
		t.Fatal("image/video keys diverged")
	}
	if err := store.Delete("veo-api"); err != nil {
		t.Fatal(err)
	}
	if key, err := store.Get("gemini-api"); err != nil || key != "" {
		t.Fatal("deletion restored a stale legacy key")
	}
	if err := store.Set("openai-api", "openai-fixture-key"); err != nil {
		t.Fatal(err)
	}
	if entries["Glowbom Providers/OpenAI"] != "openai-fixture-key" {
		t.Fatal("OpenAI key was not shared")
	}
}

func TestStudioImageSavedKeyAndSelectedMetadataSurviveRecovery(t *testing.T) {
	isolateProjectIconCredentials(t)
	isolateStudioRecovery(t)
	previous := studioVideoKeys
	studioVideoKeys = fixtureStudioVideoKeys{"xai-api": "private-saved-image-key"}
	t.Cleanup(func() { studioVideoKeys = previous })
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer private-saved-image-key" {
			t.Fatal("saved shared key not used")
		}
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	id := "fixture-image-selected-options"
	body := `{"generationId":"` + id + `","prompt":"An icon","sourceId":"xai-api","modelId":"grok-imagine-image-2.0","resolution":"2k","quality":"medium","useSavedKey":true}`
	w := httptest.NewRecorder()
	studioImageGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
	if w.Code != 200 {
		t.Fatalf("generation failed: %d %s", w.Code, w.Body.String())
	}
	record, err := findStudioAsset(studioGenerationAssetID(id))
	if err != nil || record.SourceID != "xai-api" || record.Model != "grok-imagine-image-2.0" || record.Resolution != "2k" || record.Quality != "medium" {
		t.Fatalf("selected settings not saved: %+v %v", record, err)
	}
	studioGenerations = newStudioProgressStore()
	state := studioGenerations.read(id)
	if state.ModelID != record.Model || state.Resolution != "2k" || state.Quality != "medium" || state.Asset == nil || state.Asset.ModelID != record.Model {
		t.Fatalf("recovery lost settings: %+v", state)
	}
	rootDir, _ := studioAssetsDirectory()
	_ = filepath.WalkDir(rootDir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(data), "private-saved-image-key") {
				t.Fatal("key entered asset or checkpoint")
			}
		}
		return err
	})
}

func TestStudioImageUseSavedKeyCannotBypassAuthentication(t *testing.T) {
	isolateStudioProgress(t)
	isolateProjectIconCredentials(t)
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("unauthenticated request contacted provider")
		return nil, nil
	})
	w := httptest.NewRecorder()
	studioImageGenerateHandler(w, httptest.NewRequest(http.MethodPost, "/studio/images/generate", strings.NewReader(`{"prompt":"An icon","sourceId":"xai-api","useSavedKey":true}`)))
	if w.Code != 401 {
		t.Fatalf("expected authentication, got %d", w.Code)
	}
}

func TestStudioOpenAIImageCropDoesNotGenerateAgain(t *testing.T) {
	value, err := cropStudioOpenAIImage(base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png")), "2:1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := decodeBase64Payload(value, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(strings.NewReader(string(raw)))
	if err != nil || config.Width != 2 || config.Height != 1 {
		t.Fatalf("wrong local crop: %v %v", config, err)
	}
}

func TestStudioImageCatalogActiveModelsAndHonestPrices(t *testing.T) {
	if options, err := normalizeStudioImageOptions(studioImageOptions{SourceID: "glowbom-api"}); err != nil || options.AspectRatio != "" || options.Resolution != "" || options.Quality != "" {
		t.Fatalf("account image transport invented options: %+v %v", options, err)
	}
	for _, source := range studioImageModels() {
		for _, model := range source.Models {
			if strings.Contains(model.ID, "2.5-flash") || strings.Contains(model.ID, "-preview") || strings.Contains(model.ID, "-quality") {
				t.Fatalf("retiring preset: %s", model.ID)
			}
			if source.ID == "openai-api" && strings.HasPrefix(model.ID, "gpt-image-2.5-") {
				if model.PricesByQualityUSD["medium"]["1024x1024"] != .01317 || model.PricesByQualityUSD["medium"]["1536x1024"] != .01029 || !strings.Contains(model.PricingNotice, "inputs cost extra") {
					t.Fatal("incorrect output-only estimate")
				}
			}
			if source.ID == "xai-subscription" && (model.PricesUSD != nil || model.PricesByQualityUSD != nil) {
				t.Fatal("subscription inherits API prices")
			}
		}
	}
}

func TestStudioImageCapabilitiesGlowbomConnectionRequiresAuthenticatedAccountProbe(t *testing.T) {
	isolateProjectIconCredentials(t)
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://localhost:4572")
	calls := 0
	mockGlowbomImageAccount(t, func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		if strings.Join(args, " ") != "account --json" {
			t.Fatal("capabilities attempted generation")
		}
		return glowbomSignedIn(), nil
	})
	for _, test := range []struct {
		name          string
		authenticated bool
		origin        string
		connected     bool
	}{{"unauthenticated", false, "http://localhost:4572", false}, {"foreign origin", true, "https://untrusted.example", false}, {"connected account", true, "http://localhost:4572", true}} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/studio/images/capabilities", nil)
			r.Header.Set("Origin", test.origin)
			if test.authenticated {
				r.Header.Set("Authorization", "Bearer fixture-token")
			}
			w := httptest.NewRecorder()
			studioImageCapabilitiesHandler(w, r)
			var result studioImageCatalogue
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatal("capability request failed")
			}
			found := false
			for _, source := range result.Sources {
				if source.ID == "glowbom-api" {
					found = true
					if source.Connected != test.connected {
						t.Fatalf("incorrect account availability: %+v", source)
					}
				}
			}
			if !found || strings.Contains(w.Body.String(), "must-not-leak") {
				t.Fatal("missing account source or leaked account data")
			}
		})
	}
	if calls != 1 {
		t.Fatalf("account was probed without token/origin authorization: %d", calls)
	}
}

func TestStudioGoogleFourKGeneratedImageCanBeSavedAndUsed(t *testing.T) {
	isolateProjectIconCredentials(t)
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewGray(image.Rect(0, 0, 5504, 3072))); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(imageBytes.Bytes())
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		return studioRecoveryResponse(200, "application/json", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"`+encoded+`"}}]}}]}`), nil
	})
	w := httptest.NewRecorder()
	studioImageGenerateHandler(w, httptest.NewRequest(http.MethodPost, "/studio/images/generate", strings.NewReader(`{"prompt":"Wide image","sourceId":"gemini-api","modelId":"gemini-3.1-flash-image","resolution":"4K","aspectRatio":"16:9","apiKey":"fixture-key"}`)))
	var result struct {
		Image studioImageSummary `json:"image"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Image.Dimensions == nil || result.Image.Dimensions.Width != 5504 || result.Image.Dimensions.Height != 3072 {
		t.Fatalf("advertised4K output rejected: %d %s", w.Code, w.Body.String())
	}
	root := studioTestProject(t, "FourK")
	if _, _, relative, err := useStudioImage(result.Image.ID, root); err != nil || relative == "" {
		t.Fatalf("generated4K could not be used in project: %v", err)
	}
}
