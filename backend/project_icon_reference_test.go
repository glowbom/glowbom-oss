package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectIconReferenceReachesEverySelectedSource(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	for _, source := range []string{"openai-api", "gemini-api", "xai-api", "xai-subscription"} {
		for _, mode := range []string{"reference", "ordinary", "reference failure"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				isolateProjectIconCredentials(t, `{"xai":{"type":"oauth","access":"subscription-reference-key","expires":4102444800000}}`)
				referenceData := projectIconTestImage(t, "jpeg")
				expectedReference, err := normalizeProjectIcon(referenceData)
				if err != nil {
					t.Fatal(err)
				}
				generated := projectIconTestImage(t, "png")
				hasReference := mode != "ordinary"
				fail := mode == "reference failure"
				calls := 0
				mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
					calls++
					var reference, prompt string
					switch source {
					case "openai-api":
						if r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer reference-api-key" {
							t.Fatal("wrong OpenAI source")
						}
						if hasReference {
							if r.URL.Path != "/v1/images/edits" {
								t.Fatal("OpenAI ignored reference")
							}
							if err := r.ParseMultipartForm(projectIconMaxBytes); err != nil {
								t.Fatal(err)
							}
							defer r.MultipartForm.RemoveAll()
							files := r.MultipartForm.File["image[]"]
							if len(files) != 1 || files[0].Header.Get("Content-Type") != "image/png" {
								t.Fatal("OpenAI reference image missing or wrong format")
							}
							file, err := files[0].Open()
							if err != nil {
								t.Fatal(err)
							}
							data, err := io.ReadAll(file)
							file.Close()
							if err != nil {
								t.Fatal(err)
							}
							reference = base64.StdEncoding.EncodeToString(data)
							prompt = r.FormValue("prompt")
							if r.FormValue("model") != openAIImageModelID || r.FormValue("size") != "1024x1024" {
								t.Fatal("wrong icon settings")
							}
						} else {
							if r.URL.Path != "/v1/images/generations" {
								t.Fatal("ordinary request used reference endpoint")
							}
							var body map[string]any
							if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
								t.Fatal(err)
							}
							prompt, _ = body["prompt"].(string)
							if _, ok := body["image"]; ok {
								t.Fatal("unexpected ordinary reference")
							}
						}
					case "gemini-api":
						if r.URL.Host != "generativelanguage.googleapis.com" || !strings.HasSuffix(r.URL.Path, nanoBanana2ModelID+":generateContent") || r.Header.Get("x-goog-api-key") != "reference-api-key" {
							t.Fatal("wrong Gemini source")
						}
						var body struct {
							Contents []struct {
								Parts []struct {
									Text       string `json:"text"`
									InlineData struct {
										MIME string `json:"mime_type"`
										Data string `json:"data"`
									} `json:"inline_data"`
								} `json:"parts"`
							} `json:"contents"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						for _, content := range body.Contents {
							for _, part := range content.Parts {
								if part.Text != "" {
									prompt = part.Text
								}
								if part.InlineData.Data != "" {
									if reference != "" || part.InlineData.MIME != "image/png" {
										t.Fatal("Gemini reference repeated or wrong format")
									}
									reference = part.InlineData.Data
								}
							}
						}
					default:
						key := "reference-api-key"
						if source == "xai-subscription" {
							key = "subscription-reference-key"
						}
						endpoint := "/v1/images/generations"
						if hasReference {
							endpoint = "/v1/images/edits"
						}
						if r.URL.Host != "api.x.ai" || r.URL.Path != endpoint || r.Header.Get("Authorization") != "Bearer "+key {
							t.Fatal("wrong xAI reference route or credential")
						}
						var body struct {
							Prompt string `json:"prompt"`
							Images []struct {
								Type string `json:"type"`
								URL  string `json:"url"`
							} `json:"images"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						prompt = body.Prompt
						if hasReference {
							if len(body.Images) != 1 || body.Images[0].Type != "image_url" || !strings.HasPrefix(body.Images[0].URL, "data:image/png;base64,") {
								t.Fatal("xAI reference has wrong MIME")
							}
							reference = strings.TrimPrefix(body.Images[0].URL, "data:image/png;base64,")
						} else if len(body.Images) != 0 {
							t.Fatal("ordinary generation unexpectedly included image references")
						}
					}
					if hasReference {
						actual, err := base64.StdEncoding.DecodeString(reference)
						if err != nil || !bytes.Equal(actual, expectedReference) {
							t.Fatal("reference photo changed or was omitted")
						}
						if !strings.Contains(prompt, "Keep the subject recognizable") {
							t.Fatal("reference instructions missing")
						}
					} else if reference != "" || strings.Contains(prompt, "supplied photo") {
						t.Fatal("ordinary generation unexpectedly referenced a photo")
					}
					if fail {
						return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"reference request rejected"}`)), Header: http.Header{}}, nil
					}
					if source == "gemini-api" {
						payload, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"inlineData": map[string]string{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(generated)}}}}}}})
						return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload)), Header: http.Header{}}, nil
					}
					return iconProviderResponse(generated), nil
				})
				dir := t.TempDir()
				previous := projectIconTestImage(t, "jpeg")
				if err := os.WriteFile(filepath.Join(dir, "icon.png"), previous, 0600); err != nil {
					t.Fatal(err)
				}
				request := projectIconRequest{Path: dir, Prompt: "A portrait app", SourceID: source, APIKey: "reference-api-key"}
				if hasReference {
					request.ReferenceImage = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(referenceData)
					if source == "gemini-api" {
						request.ReferenceImage = base64.StdEncoding.EncodeToString(referenceData)
					}
				}
				response := postProjectIcon(t, request)
				if response.Code != 200 || calls != 1 {
					t.Fatal("reference did not use exactly one selected provider request", response.Code, calls, response.Body.String())
				}
				data, err := os.ReadFile(filepath.Join(dir, "icon.png"))
				if err != nil {
					t.Fatal(err)
				}
				if fail {
					if !strings.Contains(response.Body.String(), `"success":false`) || !bytes.Equal(previous, data) {
						t.Fatal("failed reference request changed icon", response.Body.String())
					}
				} else if !strings.Contains(response.Body.String(), `"success":true`) || !bytes.Equal(generated, data) {
					t.Fatal("generated icon not saved", response.Body.String())
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 || entries[0].Name() != "icon.png" {
					t.Fatal("reference or temporary files left in project")
				}
			})
		}
	}
}

func TestProjectIconInvalidReferencesFailBeforeProviderAndPreserveIcon(t *testing.T) {
	valid := projectIconTestImage(t, "png")
	encoded := base64.StdEncoding.EncodeToString(valid)
	var tall bytes.Buffer
	if err := png.Encode(&tall, image.NewRGBA(image.Rect(0, 0, 1, 4097))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, value string }{
		{"external URL", "https://example.invalid/private-photo.png"},
		{"malformed base64", "data:image/png;base64,%%%"},
		{"unsupported MIME", "data:text/plain;base64," + encoded},
		{"missing base64 tag", "data:image/png," + encoded},
		{"incomplete image", base64.StdEncoding.EncodeToString(valid[:30])},
		{"oversized dimensions", base64.StdEncoding.EncodeToString(tall.Bytes())},
		{"too many bytes", strings.Repeat("A", base64.StdEncoding.EncodedLen(projectIconMaxBytes)+4)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected provider call")
			})
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "icon.png"), valid, 0600); err != nil {
				t.Fatal(err)
			}
			response := postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "A portrait app", SourceID: "openai-api", APIKey: "key", ReferenceImage: tc.value})
			after, err := os.ReadFile(filepath.Join(dir, "icon.png"))
			if response.Code != 400 || calls != 0 || err != nil || !bytes.Equal(valid, after) {
				t.Fatal("invalid reference called provider or changed icon", response.Code, calls, err)
			}
			if !strings.Contains(response.Body.String(), "reference photo") {
				t.Fatal("missing reference-specific error", response.Body.String())
			}
		})
	}
}
