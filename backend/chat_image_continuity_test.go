package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

func chatContinuityPixels(t *testing.T, shade color.RGBA) []byte {
	t.Helper()
	pixels := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			pixels.SetRGBA(x, y, shade)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, pixels); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func chatContinuityFixture(t *testing.T, previous, next, filename string) (string, string, *prototypeImageReferenceSnapshot, []byte) {
	t.Helper()
	isolateProjectIconCredentials(t)
	root, record := chatImagesFixture(t, previous)
	data := chatContinuityPixels(t, color.RGBA{R: 180, A: 255})
	if err := os.MkdirAll(filepath.Join(root, "prototype", "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prototype", "assets", filename), data, 0644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := capturePrototypeImageReferences(root, previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveChatPrototype(root, next, previous, nil, "test/model", nil, &record); err != nil {
		t.Fatal(err)
	}
	return root, record, snapshot, data
}

func TestChatImagesUseFrozenPreviousReferenceForTheSameSlot(t *testing.T) {
	previous := `<!doctype html><html><img id="portrait" alt="Owner portrait" src="assets/portrait.png"></html>`
	next := `<!doctype html><html><img id="portrait" alt="Owner portrait" src="glowbomimages:Portrait in a green jacket"><img id="scenery" src="glowbomimages:A coastal landscape"></html>`
	root, record, snapshot, pixels := chatContinuityFixture(t, previous, next, "portrait.png")
	changed := chatContinuityPixels(t, color.RGBA{B: 180, A: 255})
	if err := os.WriteFile(filepath.Join(root, "prototype", "assets", "portrait.png"), changed, 0644); err != nil {
		t.Fatal(err)
	}
	want, err := normalizeProjectIconReference(base64.StdEncoding.EncodeToString(pixels))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		reference, prompt := chatPersonalizationReference(t, r, "xai-api", calls == 1)
		if calls == 1 && (reference != want || !strings.Contains(prompt, "Keep the subject recognizable")) {
			t.Fatal("the regenerated portrait lost its frozen previous image")
		}
		if calls == 2 && reference != "" {
			t.Fatal("an unrelated new landscape received the portrait reference")
		}
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	final, warnings, err := materializeChatImages(context.Background(), root, record, next, chatImageOptions{SourceID: "xai-api", APIKey: "image-key", previousImages: snapshot}, func(map[string]any) {})
	if err != nil || calls != 2 || len(warnings) != 0 || strings.Contains(final, "glowbomimages:") {
		t.Fatal("continuity generation failed", calls, warnings, err, final)
	}
}

func TestChatImagesExplicitPersonalizationOverridesThePreviousImage(t *testing.T) {
	previous := `<!doctype html><html><img id="portrait" src="assets/portrait.png"></html>`
	next := `<!doctype html><html><img id="portrait" src="glowbomimages:Portrait in a green jacket"></html>`
	root, record, snapshot, _ := chatContinuityFixture(t, previous, next, "portrait.png")
	explicit := base64.StdEncoding.EncodeToString(chatContinuityPixels(t, color.RGBA{G: 180, A: 255}))
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		reference, _ := chatPersonalizationReference(t, r, "xai-api", true)
		if reference != explicit {
			t.Fatal("the previous portrait replaced the explicitly selected photo")
		}
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	options := chatImageOptions{SourceID: "xai-api", APIKey: "image-key", Personalization: true, ReferencePath: "selected-photo.png", referencePNG: explicit, previousImages: snapshot}
	_, warnings, err := materializeChatImages(context.Background(), root, record, next, options, func(map[string]any) {})
	if err != nil || calls != 1 || len(warnings) != 0 {
		t.Fatal(calls, warnings, err)
	}
}

func TestChatImagesFailedRegenerationKeepsCapturedPreviousPixels(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "changed file"}[changed], func(t *testing.T) {
			previous := `<!doctype html><html><img id="portrait" src="assets/portrait%22old.png"></html>`
			next := `<!doctype html><html><img id="portrait" src="glowbomimages:Portrait in a green jacket"></html>`
			root, record, snapshot, pixels := chatContinuityFixture(t, previous, next, `portrait"old.png`)
			newerPixels := chatContinuityPixels(t, color.RGBA{B: 180, A: 255})
			if changed {
				if err := os.WriteFile(filepath.Join(root, "prototype", "assets", `portrait"old.png`), newerPixels, 0644); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				chatPersonalizationReference(t, r, "xai-api", true)
				return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"private provider diagnostic"}`))}, nil
			})
			options := chatImageOptions{SourceID: "xai-api", APIKey: "image-key", previousImages: snapshot}
			final, warnings, err := materializeChatImages(context.Background(), root, record, next, options, func(map[string]any) {})
			if err != nil || calls != 1 || len(warnings) != 1 || !strings.Contains(warnings[0], "previous image was kept") || strings.Contains(final, "glowbomimages:") || strings.Contains(warnings[0], "private") {
				t.Fatal("failed regeneration did not preserve its image safely", calls, warnings, err, final)
			}
			if !changed && !strings.Contains(final, `src="assets/portrait%22old.png"`) {
				t.Fatal("unchanged previous asset URL was not safely reused", final)
			}
			if changed {
				current, _ := os.ReadFile(filepath.Join(root, "prototype", "assets", `portrait"old.png`))
				if !bytes.Equal(current, newerPixels) {
					t.Fatal("fallback overwrote the changed project image")
				}
				match, _, err := matchPreviousPrototypeImageReference(snapshot, next, "glowbomimages:Portrait in a green jacket", "xai-api")
				if err != nil {
					t.Fatal(err)
				}
				filename := chatImageFilename("previous-project-image", "Portrait in a green jacket", match.ReferenceImage)
				copied, err := readChatImage(root, filename)
				if err != nil || !bytes.Equal(copied, pixels) || !strings.Contains(final, filename) {
					t.Fatal("fallback did not save the frozen original pixels", err, final)
				}
			}
		})
	}
}

func TestChatImagesPicsumKeepsExistingImageWithoutRequestingASample(t *testing.T) {
	previous := `<!doctype html><html><img id="portrait" src="assets/portrait.png"></html>`
	next := `<!doctype html><html><img id="portrait" src="glowbomimages:Portrait in a green jacket"></html>`
	root, record, snapshot, _ := chatContinuityFixture(t, previous, next, "portrait.png")
	mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("sample photos cannot regenerate an existing image from its reference")
		return nil, nil
	})
	final, warnings, err := materializeChatImages(context.Background(), root, record, next, chatImageOptions{SourceID: "picsum", previousImages: snapshot}, func(map[string]any) {})
	if err != nil || len(warnings) != 1 || !strings.Contains(final, `src="assets/portrait.png"`) {
		t.Fatal("sample mode did not retain the portrait", warnings, err, final)
	}
}

func TestChatImagesFreshSubjectMarkerDisablesThePreviousReference(t *testing.T) {
	previous := `<!doctype html><html><img id="portrait" src="assets/portrait.png"></html>`
	next := `<!doctype html><html><img id="portrait" data-glowbom-reference="none" src="glowbomimages:Portrait of a different person"></html>`
	root, record, snapshot, _ := chatContinuityFixture(t, previous, next, "portrait.png")
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		reference, prompt := chatPersonalizationReference(t, r, "xai-api", false)
		if reference != "" || strings.Contains(prompt, "supplied reference") {
			t.Fatal("the explicit fresh-subject marker was ignored")
		}
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	_, warnings, err := materializeChatImages(context.Background(), root, record, next, chatImageOptions{SourceID: "xai-api", APIKey: "image-key", previousImages: snapshot}, func(map[string]any) {})
	if err != nil || calls != 1 || len(warnings) != 0 {
		t.Fatal(calls, warnings, err)
	}
	if !strings.Contains(chatImageInstructions(chatImageOptions{SourceID: "xai-api"}), `data-glowbom-reference="none"`) {
		t.Fatal("the model was not told how to honor a fresh-subject request")
	}
}

func TestChatImagesGroupedPlaceholdersDoNotReferenceAFreshSlot(t *testing.T) {
	for _, fresh := range []string{
		`<img id="fresh" data-glowbom-reference="none" src="glowbomimages:Portrait">`,
		`<img id="new-slot" src="glowbomimages:%50ortrait">`,
	} {
		t.Run(fresh, func(t *testing.T) {
			previous := `<!doctype html><html><img id="portrait" src="assets/portrait.png"></html>`
			next := `<!doctype html><html><img id="portrait" src="glowbomimage:Portrait">` + fresh + `</html>`
			root, record, snapshot, _ := chatContinuityFixture(t, previous, next, "portrait.png")
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				reference, _ := chatPersonalizationReference(t, r, "xai-api", false)
				if reference != "" {
					t.Fatal("grouped alias bypassed the fresh or unmatched image slot")
				}
				return iconProviderResponse(projectIconTestImage(t, "png")), nil
			})
			final, warnings, err := materializeChatImages(context.Background(), root, record, next, chatImageOptions{SourceID: "xai-api", APIKey: "image-key", previousImages: snapshot}, func(map[string]any) {})
			if err != nil || calls != 1 || len(warnings) != 0 || strings.Contains(final, "glowbomimage") {
				t.Fatal("grouped replacement failed", calls, warnings, err, final)
			}
		})
	}
}

func TestChatPrototypeCapturesPreviousImageBeforeTheModelRuns(t *testing.T) {
	previous := `<!doctype html><html><img id="portrait" src="assets/portrait.png"></html>`
	next := `<!doctype html><html><img id="portrait" src="glowbomimages:Portrait in a green jacket"></html>`
	root, _, _, pixels := chatContinuityFixture(t, previous, previous, "portrait.png")
	t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
	rememberCursorModels(nil)
	want, _ := normalizeProjectIconReference(base64.StdEncoding.EncodeToString(pixels))
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.x.ai" {
			calls++
			reference, _ := chatPersonalizationReference(t, r, "xai-api", true)
			if reference != want {
				t.Fatal("reference pixels were read after model setup")
			}
			return iconProviderResponse(projectIconTestImage(t, "png")), nil
		}
		switch r.URL.Path {
		case "/provider":
			return chatImageReply(`{"connected":["test"],"all":[{"id":"test","models":{"model":{"name":"Model","capabilities":{"input":{"image":false}}}}}]}`), nil
		case "/session":
			return chatImageReply(`{"id":"continuity-session","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`), nil
		case "/experimental/tool/ids":
			return chatImageReply(`[]`), nil
		case "/event":
			return chatImageReply("data: {}\n\n"), nil
		case "/session/continuity-session/message":
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte(want)) || bytes.Contains(body, []byte("image-key")) || !bytes.Contains(body, []byte(`data-glowbom-reference=\"none\"`)) {
				t.Fatal("reference pixels leaked or continuity guidance was missing")
			}
			response, _ := json.Marshal(map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "text", "text": next}}})
			return chatImageReply(string(response)), nil
		default:
			return chatImageReply(`true`), nil
		}
	})
	service := &chatService{directory: "/isolated", serverURL: "http://fixture", client: http.DefaultClient, prepare: func() error {
		return os.WriteFile(filepath.Join(root, "prototype", "assets", "portrait.png"), chatContinuityPixels(t, color.RGBA{B: 180, A: 255}), 0644)
	}}
	body, _ := json.Marshal(chatRequest{ProjectPath: root, Mode: "prototype", Model: "test/model", Messages: []chatMessage{{Role: "user", Text: "Change the portrait jacket to green"}}, Images: &chatImageOptions{SourceID: "xai-api", APIKey: "image-key"}})
	response := httptest.NewRecorder()
	service.streamHandler(response, httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body)))
	if response.Code != http.StatusOK || calls != 1 || !strings.Contains(response.Body.String(), `"success":true`) {
		t.Fatal("prototype continuity did not complete", response.Code, calls, response.Body.String())
	}
}
