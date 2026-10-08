package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

func imageAspectTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestImageGenerationSizeMatchesSelectedRatio(t *testing.T) {
	for aspect, size := range map[string]string{
		"1:1": "1024x1024", "16:9": "1536x864", "9:16": "864x1536", "": "auto", "unsupported": "auto",
	} {
		if got := imageGenerationSize(aspect); got != size {
			t.Errorf("aspect %q: size %s, want %s", aspect, got, size)
		}
	}
}

func TestImageAspectPromptReframesReferenceCanvas(t *testing.T) {
	for _, aspect := range []string{"1:1", "16:9", "9:16"} {
		for _, hasReference := range []bool{false, true} {
			got := imageAspectPrompt("Keep the singer and guitar.", aspect, hasReference)
			if !strings.HasPrefix(got, "Keep the singer and guitar.\n\n") || !strings.Contains(got, aspect+" width-to-height aspect ratio") {
				t.Fatalf("missing original request or final canvas: %q", got)
			}
			if strings.Contains(got, "Expand or reframe") != hasReference || strings.Contains(got, "without stretching or distortion") != hasReference {
				t.Fatalf("wrong reference instructions: %q", got)
			}
		}
	}
	for _, aspect := range []string{"", "unsupported"} {
		if got := imageAspectPrompt("Original prompt.", aspect, true); got != "Original prompt." {
			t.Fatalf("unspecified aspect changed prompt: %q", got)
		}
	}
}

func TestGeneratedImageAspectChecksActualCanvas(t *testing.T) {
	for _, test := range []struct {
		name          string
		width, height int
		aspect        string
		wantFailure   bool
	}{
		{"square", 100, 100, "1:1", false},
		{"tolerance boundary", 98, 100, "1:1", false},
		{"landscape", 160, 90, "16:9", false},
		{"portrait", 90, 160, "9:16", false},
		{"rounded landscape", 180, 100, "16:9", false},
		{"rounded portrait", 100, 180, "9:16", false},
		{"wrong square", 100, 100, "16:9", true},
		{"wrong orientation", 160, 90, "9:16", true},
		{"old landscape mapping", 150, 100, "16:9", true},
		{"old portrait mapping", 100, 150, "9:16", true},
		{"outside tolerance", 185, 100, "16:9", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateGeneratedImageAspect(imageAspectTestPNG(t, test.width, test.height), test.aspect)
			if !test.wantFailure {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var failure *imageAspectFailure
			if !errors.As(err, &failure) || failure.Width != test.width || failure.Height != test.height || failure.Aspect != test.aspect {
				t.Fatalf("wrong mismatch failure: %v", err)
			}
			if !strings.Contains(err.Error(), test.aspect) {
				t.Fatalf("expected shape missing: %v", err)
			}
		})
	}
}

func TestGeneratedImageAspectSupportsJPEGAndWebP(t *testing.T) {
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 90, 160)), nil); err != nil {
		t.Fatal(err)
	}
	if err := validateGeneratedImageAspect(jpegBytes.Bytes(), "9:16"); err != nil {
		t.Fatal(err)
	}
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGeneratedImageAspect(webp, "1:1"); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedImageAspectRejectsUnverifiableDimensionsSafely(t *testing.T) {
	private := []byte("invalid provider response with a private credential")
	var failure *imageAspectFailure
	err := validateGeneratedImageAspect(private, "16:9")
	if !errors.As(err, &failure) || failure.Width != 0 || failure.Height != 0 || strings.Contains(err.Error(), "credential") {
		t.Fatalf("invalid image error exposed provider data: %v", err)
	}
	if err := validateGeneratedImageAspect(private, ""); err != nil {
		t.Fatalf("unspecified aspect should skip validation: %v", err)
	}
}

func TestOpenAIImageRequestsUseSelectedCanvas(t *testing.T) {
	for _, aspect := range []string{"1:1", "16:9", "9:16", ""} {
		for _, reference := range []bool{false, true} {
			name := aspect + "/generate"
			if reference {
				name = aspect + "/edit"
			}
			t.Run(name, func(t *testing.T) {
				generated := imageAspectTestPNG(t, 16, 16)
				calls := 0
				mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer image-key" {
						t.Fatal("wrong OpenAI route or credential")
					}
					var prompt, size string
					if reference {
						if r.URL.Path != "/v1/images/edits" {
							t.Fatal("reference used wrong endpoint")
						}
						if err := r.ParseMultipartForm(projectIconMaxBytes); err != nil {
							t.Fatal(err)
						}
						defer r.MultipartForm.RemoveAll()
						prompt, size = r.FormValue("prompt"), r.FormValue("size")
					} else {
						if r.URL.Path != "/v1/images/generations" {
							t.Fatal("generation used wrong endpoint")
						}
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						prompt, _ = body["prompt"].(string)
						size, _ = body["size"].(string)
					}
					wantSize := imageGenerationSize(aspect)
					if aspect == "" {
						wantSize = "1024x1024"
					}
					if size != wantSize || prompt != imageAspectPrompt("Keep the singer and guitar.", aspect, reference) {
						t.Fatalf("wrong canvas request: size=%s prompt=%q", size, prompt)
					}
					return iconProviderResponse(generated), nil
				})
				var value string
				var err error
				if reference {
					value, err = callOpenAIImageGenerationWithReference("Keep the singer and guitar.", base64.StdEncoding.EncodeToString(generated), aspect, "png", "image-key")
				} else {
					value, err = callOpenAIImageGeneration("Keep the singer and guitar.", aspect, "png", "image-key")
				}
				if err != nil || calls != 1 || value != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(generated) {
					t.Fatalf("wrong result or request count: calls=%d err=%v", calls, err)
				}
			})
		}
	}
}
