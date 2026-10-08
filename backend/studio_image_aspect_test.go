package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func studioAspectTestImage(t *testing.T, width, height int) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestStudioImageAspectMetadataMatchesGeneratedPixels(t *testing.T) {
	isolateStudioProgress(t)
	t.Setenv(grokSubscriptionMediaFlag, "1")
	for _, source := range []string{"openai-subscription", "xai-subscription"} {
		for _, shape := range []struct {
			aspect        string
			width, height int
		}{{"1:1", 24, 24}, {"16:9", 32, 18}, {"9:16", 18, 32}} {
			for _, withReference := range []bool{false, true} {
				mode := "generation"
				if withReference {
					mode = "edit"
				}
				t.Run(source+"/"+shape.aspect+"/"+mode, func(t *testing.T) {
					isolateProjectIconCredentials(t, `{"openai":{"type":"oauth","access":"subscription-token","accountId":"image-account","expires":4102444800000},"xai":{"type":"oauth","access":"grok-token","expires":4102444800000}}`)
					generated := studioAspectTestImage(t, shape.width, shape.height)
					calls := 0
					mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
						calls++
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						prompt, _ := body["prompt"].(string)
						if !strings.Contains(prompt, shape.aspect) {
							t.Fatal("Canvas instruction omitted selected ratio")
						}
						if source == "xai-subscription" && body["aspect_ratio"] != shape.aspect {
							t.Fatal("Grok request lost selected ratio")
						}
						if source == "openai-subscription" && body["size"] != imageGenerationSize(shape.aspect) {
							t.Fatal("ChatGPT request lost selected ratio")
						}
						return iconProviderResponse(generated), nil
					})
					payload := map[string]string{"prompt": "A small glowing orb", "sourceId": source, "aspectRatio": shape.aspect}
					if withReference {
						payload["referenceImage"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(studioAspectTestImage(t, 16, 16))
					}
					body, _ := json.Marshal(payload)
					recorder := httptest.NewRecorder()
					studioImageGenerateHandler(recorder, studioProgressRequest(http.MethodPost, "/studio/images/generate", string(body)))
					if recorder.Code != http.StatusOK || calls != 1 {
						t.Fatal("Generation failed or replayed", recorder.Code, recorder.Body.String())
					}
					var result struct {
						Image studioImageSummary `json:"image"`
					}
					if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.Image.Dimensions == nil || result.Image.Dimensions.Width != shape.width || result.Image.Dimensions.Height != shape.height {
						t.Fatal("Metadata did not reflect actual output dimensions")
					}
					if result.Image.Prompt != "A small glowing orb" {
						t.Fatal("Internal canvas instructions were saved as the user's prompt")
					}
					stored, err := findStudioAsset(result.Image.ID)
					if err != nil {
						t.Fatal(err)
					}
					pixels, err := base64.StdEncoding.DecodeString(stored.DataBase64)
					if err != nil {
						t.Fatal(err)
					}
					config, _, err := image.DecodeConfig(bytes.NewReader(pixels))
					if err != nil || config.Width != shape.width || config.Height != shape.height {
						t.Fatal("Saved image dimensions changed")
					}
				})
			}
		}
	}
}

func TestStudioImageAspectMismatchIsNotSavedOrReplayed(t *testing.T) {
	isolateStudioProgress(t)
	t.Setenv(grokSubscriptionMediaFlag, "1")
	for _, source := range []string{"openai-subscription", "xai-subscription"} {
		t.Run(source, func(t *testing.T) {
			isolateProjectIconCredentials(t, `{"openai":{"type":"oauth","access":"subscription-token","accountId":"image-account","expires":4102444800000},"xai":{"type":"oauth","access":"grok-token","expires":4102444800000}}`)
			// A 3:2 landscape image still does not fulfill the selected 16:9 ratio.
			generated := studioAspectTestImage(t, 30, 20)
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) { calls++; return iconProviderResponse(generated), nil })
			payload, _ := json.Marshal(map[string]string{"prompt": "An amber orb", "sourceId": source, "aspectRatio": "16:9"})
			recorder := httptest.NewRecorder()
			studioImageGenerateHandler(recorder, studioProgressRequest(http.MethodPost, "/studio/images/generate", string(payload)))
			if recorder.Code != http.StatusBadGateway || calls != 1 || !strings.Contains(recorder.Body.String(), "30×20") || !strings.Contains(recorder.Body.String(), "16:9") {
				t.Fatal("Wrong ratio was accepted or replayed", calls, recorder.Code, recorder.Body.String())
			}
			images, err := listStudioImages()
			if err != nil || len(images) != 0 {
				t.Fatal("An image with the wrong ratio was saved")
			}
		})
	}
}

func TestProjectIconAspectMismatchKeepsPreviousIcon(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	for _, source := range []string{"openai-subscription", "xai-subscription"} {
		t.Run(source, func(t *testing.T) {
			isolateProjectIconCredentials(t, `{"openai":{"type":"oauth","access":"subscription-token","accountId":"image-account","expires":4102444800000},"xai":{"type":"oauth","access":"grok-token","expires":4102444800000}}`)
			dir := t.TempDir()
			previous := studioAspectTestImage(t, 24, 24)
			path := filepath.Join(dir, "icon.png")
			if err := os.WriteFile(path, previous, 0600); err != nil {
				t.Fatal(err)
			}
			wrongShape := studioAspectTestImage(t, 32, 18)
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) { calls++; return iconProviderResponse(wrongShape), nil })
			response := postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "An amber orb", SourceID: source})
			if calls != 1 || !strings.Contains(response.Body.String(), `"success":false`) || !strings.Contains(response.Body.String(), "square (1:1)") {
				t.Fatal("Non-square icon was accepted or replayed")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, previous) {
				t.Fatal("Non-square result replaced previous icon")
			}
		})
	}
}
