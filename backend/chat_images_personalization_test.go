package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chatPersonalizationReference(t *testing.T, r *http.Request, source string, enabled bool) (string, string) {
	t.Helper()
	if source == "openai-api" && enabled {
		if r.URL.Path != "/v1/images/edits" || r.Header.Get("Authorization") != "Bearer image-key" {
			t.Fatal("wrong OpenAI reference route or credential")
		}
		if err := r.ParseMultipartForm(projectIconMaxBytes); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		files := r.MultipartForm.File["image[]"]
		if len(files) != 1 || files[0].Header.Get("Content-Type") != "image/png" {
			t.Fatal("reference is not a single normalized PNG")
		}
		file, err := files[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(data), r.FormValue("prompt")
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	prompt, _ := body["prompt"].(string)
	if source == "gemini-api" {
		if r.URL.Host != "generativelanguage.googleapis.com" || r.Header.Get("x-goog-api-key") != "image-key" {
			t.Fatal("wrong Gemini source")
		}
		reference := ""
		for _, content := range body["contents"].([]any) {
			for _, part := range content.(map[string]any)["parts"].([]any) {
				entry := part.(map[string]any)
				if text, ok := entry["text"].(string); ok {
					prompt = text
				}
				if data, ok := entry["inline_data"].(map[string]any); ok {
					if reference != "" || data["mime_type"] != "image/png" {
						t.Fatal("wrong Gemini reference")
					}
					reference, _ = data["data"].(string)
				}
			}
		}
		return reference, prompt
	}
	endpoint := "/v1/images/generations"
	if enabled {
		endpoint = "/v1/images/edits"
	}
	key := "image-key"
	if source == "xai-subscription" {
		key = "subscription-image-key"
	}
	if r.URL.Path != endpoint || r.Header.Get("Authorization") != "Bearer "+key {
		t.Fatal("wrong image route or credential")
	}
	reference := ""
	if values, ok := body["images"].([]any); ok {
		if len(values) != 1 || body["image"] != nil {
			t.Fatal("xAI reference must use one item in the images edit mode")
		}
		value, ok := values[0].(map[string]any)
		if !ok || value["type"] != "image_url" {
			t.Fatal("wrong xAI reference type")
		}
		reference, _ = value["url"].(string)
		if !strings.HasPrefix(reference, "data:image/png;base64,") {
			t.Fatal("wrong xAI reference MIME")
		}
		reference = strings.TrimPrefix(reference, "data:image/png;base64,")
	}
	return reference, prompt
}

func TestChatImagesPersonalizationEverySourceAndExplicitToggle(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	for _, source := range []string{"openai-api", "gemini-api", "xai-api", "xai-subscription"} {
		for _, mode := range []string{"enabled", "disabled", "failed reference"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				isolateProjectIconCredentials(t, `{"xai":{"type":"oauth","access":"subscription-image-key","expires":4102444800000}}`)
				original := `<!doctype html><html><img src="glowbomimages:The person from the reference photo walking by the seaside"></html>`
				root, record := chatImagesFixture(t, original)
				reference, err := normalizeProjectIconReference(base64.StdEncoding.EncodeToString(projectIconTestImage(t, "jpeg")))
				if err != nil {
					t.Fatal(err)
				}
				enabled := mode != "disabled"
				calls := 0
				mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
					calls++
					actual, prompt := chatPersonalizationReference(t, r, source, enabled)
					if enabled && (actual != reference || !strings.Contains(prompt, "Keep the subject recognizable")) {
						t.Fatal("explicit personalization reference was not forwarded")
					}
					if !enabled && (actual != "" || strings.Contains(prompt, "supplied reference")) {
						t.Fatal("disabled personalization used a stale reference")
					}
					if mode == "failed reference" {
						return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"private reference diagnostics"}`))}, nil
					}
					data := projectIconTestImage(t, "png")
					if source == "gemini-api" {
						return chatImageReply(fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":%q}}]}}]}`, base64.StdEncoding.EncodeToString(data))), nil
					}
					return iconProviderResponse(data), nil
				})
				options := chatImageOptions{SourceID: source, APIKey: "image-key", Personalization: enabled, ReferencePath: "attached.jpg", referencePNG: reference}
				final, warnings, err := materializeChatImages(context.Background(), root, record, original, options, func(map[string]any) {})
				if err != nil || calls != 1 {
					t.Fatal("wrong request count", calls, err)
				}
				if mode == "failed reference" {
					if final != original || len(warnings) != 1 || strings.Contains(warnings[0], "private") {
						t.Fatal("failed reference was replaced or leaked provider details", warnings)
					}
				} else if len(warnings) > 0 || strings.Contains(final, "glowbomimages:") {
					t.Fatal("personalized image did not finish", warnings)
				}
				for _, folder := range []string{record, filepath.Join(os.Getenv("GLOWBOM_STUDIO_DIR"), "Assets")} {
					entries, _ := os.ReadDir(folder)
					for _, entry := range entries {
						if !strings.HasSuffix(entry.Name(), ".json") {
							continue
						}
						data, _ := os.ReadFile(filepath.Join(folder, entry.Name()))
						if bytes.Contains(data, []byte(reference)) || bytes.Contains(data, []byte("image-key")) {
							t.Fatal("reference or credential persisted in metadata")
						}
					}
				}
			})
		}
	}
}

func TestChatImagesPersonalizationDetectsIdentityInsteadOfScenery(t *testing.T) {
	for _, prompt := range []string{
		"The person from the reference photo exploring Rio de Janeiro at sunset",
		"A landscape with the same person beside the river",
		"Alex from the reference photo exploring the Brazilian rainforest",
		"Portrait of the traveler on a beach",
		"The pet from the reference image playing on the beach",
		"The referenced product on a wooden table",
		"Put me in a scene overlooking Rio",
	} {
		if !isPersonalizedChatImage(prompt) {
			t.Errorf("missed identity prompt: %s", prompt)
		}
	}
	for _, prompt := range []string{
		"An aerial view of Rio de Janeiro at sunset",
		"A Brazilian beach without people",
		"Brazil cityscape, no people, golden hour",
		"Mangrove forest and a monument beside a cathedral",
		"Brazilian food on a colorful table",
		"A simple camera icon",
		"A red backpack on a white background",
		"People dancing at a Brazilian street festival",
		"A woman selling fruit at a market",
		"A man surfing off the coast of Brazil",
		"A clock face in a historic town square",
	} {
		if isPersonalizedChatImage(prompt) {
			t.Errorf("scenery or unrelated object requested a private reference: %s", prompt)
		}
	}
}

func TestChatImagesPersonalizationPrioritizesScenesWithinImageLimit(t *testing.T) {
	isolateProjectIconCredentials(t)
	prompts := []string{
		"People dancing at a Brazilian street festival", "Brazil beach", "Brazilian food", "Rainforest trail",
		"The person from the reference photo exploring Rio",
		"The person from the reference photo walking on a Brazilian beach",
	}
	original := "<!doctype html><html>"
	for _, prompt := range prompts {
		original += `<img src="glowbomimages:` + prompt + `">`
	}
	original += "</html>"
	root, record := chatImagesFixture(t, original)
	reference := base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))
	calls := 0
	events := []map[string]any{}
	want := []string{prompts[4], prompts[5], prompts[0], prompts[1]}
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > maxChatImages {
			t.Fatal("exceeded the image generation limit")
		}
		personalized := calls <= 2
		actualReference, actualPrompt := chatPersonalizationReference(t, r, "xai-api", personalized)
		if personalized {
			if actualReference != reference || !strings.HasPrefix(actualPrompt, want[calls-1]+" Use the supplied reference") {
				t.Fatal("personalized scene lost its reference")
			}
		} else if actualReference != "" || actualPrompt != imageAspectPrompt(want[calls-1], "16:9", false) {
			t.Fatal("generic scenery received the reference")
		}
		progress, ok := events[len(events)-1]["imagePrompt"].(map[string]any)
		if !ok || progress["prompt"] != want[calls-1] || progress["personalized"] != personalized || progress["index"] != calls || progress["total"] != maxChatImages {
			t.Fatal("incorrect image progress", progress)
		}
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	final, warnings, err := materializeChatImages(context.Background(), root, record, original, chatImageOptions{SourceID: "xai-api", APIKey: "image-key", Personalization: true, ReferencePath: "photo.png", referencePNG: reference}, func(event map[string]any) {
		events = append(events, event)
	})
	if err != nil || calls != maxChatImages || len(warnings) != 0 {
		t.Fatal("image prioritization failed", calls, warnings, err)
	}
	if len(events) == 0 || events[0]["notice"] != "Creating up to four images, with personalized scenes first." || events[0]["warning"] != nil {
		t.Fatal("missing separate personalized image limit notice")
	}
	if !strings.Contains(final, "glowbomimages:Brazilian food") || !strings.Contains(final, "glowbomimages:Rainforest trail") || strings.Contains(final, "glowbomimages:The person") {
		t.Fatal("personalized scenes were skipped instead of scenery")
	}
}

func TestChatImagesPersonalizationWarnsWhenNoIdentityPromptExists(t *testing.T) {
	for _, document := range []string{
		`<!doctype html><html><img src="assets/uploaded-photo.png"></html>`,
		`<!doctype html><html><img src="glowbomimages:Brazilian scenery"></html>`,
	} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/%s", enabled, document), func(t *testing.T) {
				isolateProjectIconCredentials(t)
				root, record := chatImagesFixture(t, document)
				mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
					actualReference, _ := chatPersonalizationReference(t, r, "xai-api", false)
					if actualReference != "" {
						t.Fatal("scenery received a private reference")
					}
					return iconProviderResponse(projectIconTestImage(t, "png")), nil
				})
				reference := base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))
				_, warnings, err := materializeChatImages(context.Background(), root, record, document, chatImageOptions{SourceID: "xai-api", APIKey: "image-key", Personalization: enabled, ReferencePath: "photo.png", referencePNG: reference}, func(map[string]any) {})
				if err != nil {
					t.Fatal(err)
				}
				if enabled && (len(warnings) != 1 || !strings.Contains(warnings[0], "did not include any personalized image prompts")) {
					t.Fatal("missing personalization was silent", warnings)
				}
				if !enabled && len(warnings) > 0 {
					t.Fatal("disabled personalization showed a warning", warnings)
				}
			})
		}
	}
}

func TestChatImagesPersonalizationCacheSeparatesReferences(t *testing.T) {
	isolateProjectIconCredentials(t)
	original := `<!doctype html><html><img src="glowbomimages:portrait"><img src="glowbomimages:A coastal landscape"></html>`
	root, record := chatImagesFixture(t, original)
	first, _ := normalizeProjectIconReference(base64.StdEncoding.EncodeToString(projectIconTestImage(t, "jpeg")))
	var secondPNG bytes.Buffer
	secondImage := image.NewRGBA(image.Rect(0, 0, 2, 2))
	secondImage.Set(0, 0, color.RGBA{R: 190, B: 120, A: 255})
	if err := png.Encode(&secondPNG, secondImage); err != nil {
		t.Fatal(err)
	}
	second := base64.StdEncoding.EncodeToString(secondPNG.Bytes())
	calls := 0
	referenceCalls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/v1/images/edits" {
			referenceCalls++
		}
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	previous := original
	outputs := []string{}
	for i, reference := range []string{first, first, second, ""} {
		if i > 0 {
			if err := saveChatPrototype(root, original, previous, nil, "test/model", nil, &record); err != nil {
				t.Fatal(err)
			}
		}
		options := chatImageOptions{SourceID: "openai-api", APIKey: "image-key", Personalization: reference != "", ReferencePath: "same-photo-path.jpg", referencePNG: reference}
		final, warnings, err := materializeChatImages(context.Background(), root, record, original, options, func(map[string]any) {})
		if err != nil || len(warnings) > 0 {
			t.Fatal(warnings, err)
		}
		previous = final
		outputs = append(outputs, final)
		if !strings.Contains(final, chatImageFilename("openai-api", "A coastal landscape")) {
			t.Fatal("ordinary scenery cache depends on a private reference")
		}
	}
	if calls != 4 || referenceCalls != 2 || outputs[0] != outputs[1] || outputs[0] == outputs[2] || outputs[2] == outputs[3] || outputs[0] == outputs[3] {
		t.Fatal("reference photos or ordinary generation shared a cache entry", calls, outputs)
	}
}

func TestChatImagesPersonalizationInvalidReferenceFailsBeforeModel(t *testing.T) {
	for _, mode := range []string{"missing", "Picsum", "outside uploads", "external symlink", "missing file", "incomplete", "oversized dimensions", "oversized file"} {
		t.Run(mode, func(t *testing.T) {
			original := `<!doctype html><html>Existing</html>`
			root, _ := chatImagesFixture(t, original)
			uploads := t.TempDir()
			path := filepath.Join(uploads, "reference.png")
			data := projectIconTestImage(t, "png")
			if mode == "oversized file" {
				data = make([]byte, 10<<20+1)
			}
			if mode == "incomplete" {
				data = data[:30]
			}
			if mode == "oversized dimensions" {
				var large bytes.Buffer
				if err := png.Encode(&large, image.NewRGBA(image.Rect(0, 0, 1, 4097))); err != nil {
					t.Fatal(err)
				}
				data = large.Bytes()
			}
			if mode == "outside uploads" || mode == "external symlink" {
				outside := filepath.Join(t.TempDir(), "private.png")
				if err := os.WriteFile(outside, data, 0600); err != nil {
					t.Fatal(err)
				}
				if mode == "outside uploads" {
					path = outside
				} else if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			} else if mode != "missing file" {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			options := &chatImageOptions{SourceID: "openai-api", Personalization: true, ReferencePath: path}
			attachments := []string{path}
			switch mode {
			case "missing":
				options.ReferencePath = ""
			case "Picsum":
				options.SourceID = "picsum"
			}
			for _, attached := range []bool{false, true} {
				modelAttachments := attachments
				if !attached {
					modelAttachments = nil
				}
				prepared := false
				service := &chatService{uploads: uploads, prepare: func() error { prepared = true; return errors.New("must not start") }}
				body, _ := json.Marshal(chatRequest{ProjectPath: root, Mode: "prototype", Model: "test/model", Messages: []chatMessage{{Role: "user", Text: "Build"}}, AttachmentPaths: modelAttachments, Images: options})
				response := httptest.NewRecorder()
				service.streamHandler(response, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(body)))
				saved, _ := os.ReadFile(filepath.Join(root, "prototype/index.html"))
				if response.Code != 400 || prepared || string(saved) != original {
					t.Fatal("invalid personalization started generation or changed prototype", attached, response.Code, prepared, response.Body.String())
				}
			}
		})
	}
}

func TestChatImagesPersonalizationCapturesSelectedUploadBeforeGeneration(t *testing.T) {
	isolateProjectIconCredentials(t)
	t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
	rememberCursorModels(nil)
	original := `<!doctype html><html>Existing</html>`
	root, _ := chatImagesFixture(t, original)
	uploads := t.TempDir()
	path := filepath.Join(uploads, "reference.jpg")
	referenceData := projectIconTestImage(t, "jpeg")
	if err := os.WriteFile(path, referenceData, 0600); err != nil {
		t.Fatal(err)
	}
	expected, _ := normalizeProjectIconReference(base64.StdEncoding.EncodeToString(referenceData))
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.openai.com" {
			calls++
			reference, _ := chatPersonalizationReference(t, r, "openai-api", true)
			if reference != expected {
				t.Fatal("reference changed after request validation")
			}
			return iconProviderResponse(projectIconTestImage(t, "png")), nil
		}
		switch r.URL.Path {
		case "/provider":
			return chatImageReply(`{"connected":["test"],"all":[{"id":"test","models":{"model":{"name":"Model","capabilities":{"input":{"image":true}}}}}]}`), nil
		case "/session":
			return chatImageReply(`{"id":"image-session","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`), nil
		case "/experimental/tool/ids":
			return chatImageReply(`[]`), nil
		case "/event":
			return chatImageReply("data: {}\n\n"), nil
		case "/session/image-session/message":
			body, _ := io.ReadAll(r.Body)
			if !bytes.Contains(body, []byte("Image personalization is enabled")) || !bytes.Contains(body, []byte("selected personalization reference")) || bytes.Contains(body, []byte("image-key")) {
				t.Fatal("missing personalization instructions or leaked credential")
			}
			for _, instruction := range []string{"MUST create at least one image placeholder", "requested setting and action, not just scenery", "Reusing the uploaded photo as an avatar", "within the four-image budget", "not a newly generated personalized image"} {
				if !bytes.Contains(body, []byte(instruction)) {
					t.Fatalf("missing contextual image instruction: %s", instruction)
				}
			}
			if err := os.WriteFile(path, []byte("attachment changed"), 0600); err != nil {
				t.Fatal(err)
			}
			response, _ := json.Marshal(map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "text", "text": `<!doctype html><html><img src="glowbomimages:The person from the reference photo exploring the seaside"></html>`}}})
			return chatImageReply(string(response)), nil
		default:
			return chatImageReply(`true`), nil
		}
	})
	service := &chatService{uploads: uploads, directory: "/isolated", serverURL: "http://fixture", client: http.DefaultClient, prepare: func() error { return nil }}
	body, _ := json.Marshal(chatRequest{ProjectPath: root, Mode: "prototype", Model: "test/model", Messages: []chatMessage{{Role: "user", Text: "Build"}}, AttachmentPaths: []string{path}, Images: &chatImageOptions{SourceID: "openai-api", APIKey: "image-key", Personalization: true, ReferencePath: path}})
	response := httptest.NewRecorder()
	service.streamHandler(response, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(body)))
	if response.Code != 200 || calls != 1 || !strings.Contains(response.Body.String(), `"success":true`) {
		t.Fatal(response.Code, calls, response.Body.String())
	}
	savedReference, err := os.ReadFile(filepath.Join(root, "prototype", "assets", "reference.jpg"))
	if err != nil || !bytes.Equal(savedReference, referenceData) {
		t.Fatal("attached original reference was not preserved as an asset")
	}
}

func TestChatImagesIndependentReferenceUsesTextModelWithoutExposingPhoto(t *testing.T) {
	for _, outcome := range []string{"success", "model failure", "canceled during model", "canceled before model", "reference also attached"} {
		t.Run(outcome, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
			rememberCursorModels(nil)
			original := `<!doctype html><html>Existing</html>`
			root, _ := chatImagesFixture(t, original)
			uploads := t.TempDir()
			path := filepath.Join(uploads, "private-reference.jpg")
			data := projectIconTestImage(t, "jpeg")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			expected, err := normalizeProjectIconReference(base64.StdEncoding.EncodeToString(data))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if outcome == "canceled before model" {
				cancel()
			}
			modelCalls, imageCalls, prepareCalls := 0, 0, 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "api.openai.com" {
					imageCalls++
					reference, _ := chatPersonalizationReference(t, r, "openai-api", true)
					if reference != expected {
						t.Fatal("image generator did not receive captured independent reference")
					}
					return iconProviderResponse(projectIconTestImage(t, "png")), nil
				}
				switch r.URL.Path {
				case "/provider":
					return chatImageReply(`{"connected":["test"],"all":[{"id":"test","models":{"text":{"name":"Text","capabilities":{"input":{"image":false}}}}}]}`), nil
				case "/session":
					return chatImageReply(`{"id":"text-session","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`), nil
				case "/experimental/tool/ids":
					return chatImageReply(`[]`), nil
				case "/event":
					return chatImageReply("data: {}\n\n"), nil
				case "/session/text-session/message":
					modelCalls++
					body, _ := io.ReadAll(r.Body)
					var request struct {
						Model struct {
							ProviderID string `json:"providerID"`
							ModelID    string `json:"modelID"`
						} `json:"model"`
						Parts []map[string]any `json:"parts"`
					}
					if json.Unmarshal(body, &request) != nil || request.Model.ProviderID != "test" || request.Model.ModelID != "text" || len(request.Parts) != 1 || request.Parts[0]["type"] != "text" {
						t.Fatal("prototype model changed or received image parts", string(body))
					}
					for _, private := range []string{path, "private-reference.jpg", expected, base64.StdEncoding.EncodeToString(data), "image-key", "data:image/"} {
						if bytes.Contains(body, []byte(private)) {
							t.Fatal("reference bytes, filename, path or image credential reached text model")
						}
					}
					for _, instruction := range []string{"You cannot see that photo or sketch", "Use the user's written description", "MUST create at least one image placeholder", "A person with a green jacket"} {
						if !bytes.Contains(body, []byte(instruction)) {
							t.Fatal("missing accurate reference or description context", instruction)
						}
					}
					if err := os.WriteFile(path, []byte("changed upload after validation"), 0600); err != nil {
						t.Fatal(err)
					}
					if outcome == "model failure" {
						response := chatImageReply(`{"error":{"message":"model unavailable"}}`)
						response.StatusCode = http.StatusServiceUnavailable
						return response, nil
					}
					if outcome == "canceled during model" {
						cancel()
						return nil, context.Canceled
					}
					return chatImageReply(`{"info":{},"parts":[{"type":"text","text":"<!doctype html><html><img src=\"glowbomimages:The person from the reference photo exploring the seaside in a green jacket\"></html>"}]}`), nil
				default:
					return chatImageReply(`true`), nil
				}
			})
			service := &chatService{uploads: uploads, directory: "/isolated", serverURL: "http://fixture", client: http.DefaultClient, prepare: func() error { prepareCalls++; return nil }}
			req := chatRequest{ProjectPath: root, Mode: "prototype", Model: "test/text", Messages: []chatMessage{{Role: "user", Text: "A person with a green jacket. Make a travel journal with a personalized seaside scene."}}, Images: &chatImageOptions{SourceID: "openai-api", APIKey: "image-key", Personalization: true, ReferencePath: path}}
			if outcome == "reference also attached" {
				req.AttachmentPaths = []string{path}
			}
			body, _ := json.Marshal(req)
			response := httptest.NewRecorder()
			service.streamHandler(response, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(body)).WithContext(ctx))
			if outcome == "success" {
				if response.Code != 200 || modelCalls != 1 || imageCalls != 1 || !strings.Contains(response.Body.String(), `"success":true`) {
					t.Fatal("independent reference generation failed", response.Code, modelCalls, imageCalls, response.Body.String())
				}
			} else {
				if imageCalls != 0 || strings.Contains(response.Body.String(), `"success":true`) {
					t.Fatal("failed or canceled model started image generation", imageCalls, response.Body.String())
				}
				saved, _ := os.ReadFile(filepath.Join(root, "prototype/index.html"))
				if string(saved) != original {
					t.Fatal("failed or canceled model modified existing prototype")
				}
				if outcome == "canceled before model" && (modelCalls != 0 || prepareCalls != 0) {
					t.Fatal("already canceled request contacted model")
				}
				if outcome == "reference also attached" && (modelCalls != 0 || response.Code != 400 || !strings.Contains(response.Body.String(), "vision-capable")) {
					t.Fatal("non-vision model accepted image attachment")
				}
			}
			if _, err := os.Stat(filepath.Join(root, "prototype/assets/private-reference.jpg")); !os.IsNotExist(err) {
				t.Fatal("independent reference was persisted as a page asset")
			}
		})
	}
}
