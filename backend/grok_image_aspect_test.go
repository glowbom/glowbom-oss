package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
)

func grokAspectTestImage(t *testing.T, width, height int) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestGrokImageGenerationRequestedAspectRatios(t *testing.T) {
	for _, shape := range []struct {
		name, ratio   string
		width, height int
	}{
		{"square", "1:1", 64, 64},
		{"landscape", "16:9", 128, 72},
		{"portrait", "9:16", 72, 128},
	} {
		for _, format := range []string{"base64", "url"} {
			t.Run(shape.name+"/"+format, func(t *testing.T) {
				generated := grokAspectTestImage(t, shape.width, shape.height)
				calls := 0
				mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method == http.MethodGet {
						if r.URL.String() != "https://images.example.test/result.png" || r.Header.Get("Authorization") != "" {
							t.Fatal("unexpected download URL or provider credential disclosure")
						}
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(generated)), Header: http.Header{"Content-Type": []string{"image/png"}}}, nil
					}
					if r.URL.String() != xAIImageGenerationURL || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer mock-grok-key" {
						t.Fatal("wrong generation endpoint, method, or provider credential")
					}
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if request["aspect_ratio"] != shape.ratio || request["resolution"] != "1k" || request["model"] != xAIImageModel || request["n"] != float64(1) {
						t.Fatalf("wrong requested image shape or model: %#v", request)
					}
					if request["prompt"] != imageAspectPrompt("A Georgian landscape with a small amber orb.", shape.ratio, false) || request["image"] != nil || request["images"] != nil {
						t.Fatal("ordinary generation changed prompt or added a reference")
					}
					if format == "base64" {
						return iconProviderResponse(generated), nil
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[{"url":"https://images.example.test/result.png"}]}`)), Header: http.Header{}}, nil
				})
				dataURI, err := callGrokImageGeneration("A Georgian landscape with a small amber orb.", "mock-grok-key", shape.ratio, context.Background())
				if err != nil {
					t.Fatal(err)
				}
				_, payload, ok := strings.Cut(dataURI, ",")
				if !ok {
					t.Fatal("image response is not a data URI")
				}
				data, err := base64.StdEncoding.DecodeString(payload)
				if err != nil {
					t.Fatal(err)
				}
				config, _, err := image.DecodeConfig(bytes.NewReader(data))
				if err != nil || config.Width != shape.width || config.Height != shape.height || !bytes.Equal(data, generated) {
					t.Fatalf("returned pixels changed: %dx%d, %v", config.Width, config.Height, err)
				}
				wantCalls := 1
				if format == "url" {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Fatalf("provider retried generation: %d requests", calls)
				}
			})
		}
	}
}

func TestGrokImageEditOverridesReferenceAspectRatio(t *testing.T) {
	for _, shape := range []struct {
		name, ratio   string
		width, height int
	}{
		{"square", "1:1", 64, 64},
		{"landscape", "16:9", 128, 72},
		{"portrait", "9:16", 72, 128},
	} {
		t.Run(shape.name, func(t *testing.T) {
			reference := grokAspectTestImage(t, 96, 64)
			referenceURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(reference)
			generated := grokAspectTestImage(t, shape.width, shape.height)
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != xAIImageEditURL || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer mock-grok-key" {
					t.Fatal("wrong edit endpoint, method, or provider credential")
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request["aspect_ratio"] != shape.ratio || request["resolution"] != "1k" || request["model"] != xAIImageModel || request["n"] != float64(1) {
					t.Fatalf("wrong requested image shape or model: %#v", request)
				}
				if request["prompt"] != imageAspectPrompt("Keep the singer and guitar.", shape.ratio, true) || request["image"] != nil {
					t.Fatal("explicit ratio edit used the input-shape mode or lost edit instructions")
				}
				images, ok := request["images"].([]any)
				if !ok || len(images) != 1 {
					t.Fatal("explicit ratio edit must send exactly one reference in images")
				}
				entry, ok := images[0].(map[string]any)
				if !ok || entry["type"] != "image_url" || entry["url"] != referenceURI {
					t.Fatal("reference image was cropped, stretched, or changed")
				}
				return iconProviderResponse(generated), nil
			})
			dataURI, err := callGrokImageGenerationWithReference("Keep the singer and guitar.", referenceURI, "mock-grok-key", shape.ratio, context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, payload, ok := strings.Cut(dataURI, ",")
			if !ok {
				t.Fatal("image response is not a data URI")
			}
			data, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				t.Fatal(err)
			}
			config, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || config.Width != shape.width || config.Height != shape.height || !bytes.Equal(data, generated) {
				t.Fatalf("returned pixels changed: %dx%d, %v", config.Width, config.Height, err)
			}
			if calls != 1 {
				t.Fatalf("edit repeated a provider request: %d requests", calls)
			}
		})
	}
}

func TestGrokImageEditDefaultPreservesReferenceAspectRatio(t *testing.T) {
	for _, aspect := range []string{"", "auto"} {
		t.Run("aspect="+aspect, func(t *testing.T) {
			reference := grokAspectTestImage(t, 96, 64)
			referenceURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(reference)
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request["aspect_ratio"] != "auto" || request["images"] != nil || request["prompt"] != "Keep the singer and guitar." {
					t.Fatal("default edit changed the requested input shape or prompt")
				}
				entry, ok := request["image"].(map[string]any)
				if !ok || entry["type"] != "image_url" || entry["url"] != referenceURI {
					t.Fatal("default edit changed the reference")
				}
				return iconProviderResponse(reference), nil
			})
			_, err := callGrokImageGenerationWithReference("Keep the singer and guitar.", referenceURI, "mock-grok-key", aspect, context.Background())
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
