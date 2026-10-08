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
	"time"
)

func mockGlowbomImageAccount(t *testing.T, run accountCLIRunner) *accountBridge {
	t.Helper()
	previous := glowbomImageAccount
	bridge := newAccountBridge(run)
	bridge.token = "fixture-token"
	glowbomImageAccount = bridge
	t.Cleanup(func() { glowbomImageAccount = previous })
	return bridge
}

func glowbomSignedIn() []byte {
	return []byte(`{"version":1,"status":"signed_in","uid":"fixture-owner","subscriptionStatus":"premium","idToken":"must-not-leak"}`)
}

func glowbomImagePost(handler http.HandlerFunc, path string, body any, authenticated bool) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	if authenticated {
		r.Header.Set("Authorization", "Bearer fixture-token")
	}
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestGlowbomImageGenerationUsesPrivateCLIReferenceAndAccountLock(t *testing.T) {
	for _, personalized := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "reference"}[personalized], func(t *testing.T) {
			data := projectIconTestImage(t, "png")
			reference := ""
			if personalized {
				reference = base64.StdEncoding.EncodeToString(data)
			}
			calls := 0
			temporary := ""
			bridge := mockGlowbomImageAccount(t, func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				if glowbomImageAccount.begin() {
					t.Fatal("generation did not hold account operation lock")
				}
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("unbounded CLI command")
				}
				if args[0] == "account" {
					if strings.Join(args, " ") != "account --json" || time.Until(deadline) > 25*time.Second || time.Until(deadline) < 24*time.Second {
						t.Fatal("invalid account probe")
					}
					return glowbomSignedIn(), nil
				}
				if args[0] != "generate-image" || time.Until(deadline) < 9*time.Minute || time.Until(deadline) > glowbomImageTimeout {
					t.Fatal("wrong generation command or timeout")
				}
				if args[len(args)-2] != "--" || args[len(args)-1] != "--private prompt" {
					t.Fatal("prompt was parsed as a CLI option")
				}
				values := map[string]string{}
				for i := 1; i < len(args)-2; i += 2 {
					values[args[i]] = args[i+1]
				}
				if values["--format"] != "png" || values["--source"] != "flux" || values["--quality"] != "fast" {
					t.Fatal("wrong Glowbom request options")
				}
				temporary = filepath.Dir(values["--output"])
				info, err := os.Stat(temporary)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatal("reference folder is not private")
				}
				if personalized {
					photo, err := os.ReadFile(values["--ref"])
					info, statErr := os.Stat(values["--ref"])
					if err != nil || statErr != nil || info.Mode().Perm() != 0600 || !bytes.Equal(photo, data) {
						t.Fatal("selected reference omitted or changed")
					}
				} else if values["--ref"] != "" {
					t.Fatal("ordinary generation included a reference")
				}
				return nil, os.WriteFile(values["--output"], data, 0600)
			})
			value, err := callGlowbomImageGeneration(context.Background(), "--private prompt", reference)
			if err != nil || calls != 2 || value != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
				t.Fatal("generation failed", calls, err)
			}
			if _, err := os.Stat(temporary); !os.IsNotExist(err) {
				t.Fatal("reference/output temp folder retained")
			}
			if !bridge.begin() {
				t.Fatal("account lock not released")
			}
			bridge.end()
		})
	}
}

func TestGlowbomImageFailuresNeverFallbackOrRetry(t *testing.T) {
	for _, mode := range []string{"signed out", "bad status", "account unavailable", "busy", "no token", "provider failure", "canceled", "missing output", "symlink output"} {
		t.Run(mode, func(t *testing.T) {
			calls, generations := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bridge := mockGlowbomImageAccount(t, func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if args[0] == "account" {
					if mode == "signed out" {
						return []byte(`{"version":1,"status":"signed_out"}`), nil
					}
					if mode == "bad status" {
						return []byte(`{"idToken":"private diagnostic"}`), nil
					}
					if mode == "account unavailable" {
						return []byte(`{"version":1,"status":"unavailable","code":"account_unavailable"}`), errors.New("exit 1")
					}
					return glowbomSignedIn(), nil
				}
				generations++
				switch mode {
				case "provider failure":
					return nil, errors.New("private provider diagnostic and key")
				case "canceled":
					cancel()
					return nil, context.Canceled
				case "symlink output":
					for i, arg := range args {
						if arg == "--output" {
							outside := filepath.Join(t.TempDir(), "private.png")
							os.WriteFile(outside, projectIconTestImage(t, "png"), 0600)
							return nil, os.Symlink(outside, args[i+1])
						}
					}
				}
				return nil, nil
			})
			if mode == "busy" {
				bridge.begin()
				defer bridge.end()
			}
			if mode == "no token" {
				bridge.token = ""
			}
			_, err := callGlowbomImageGeneration(ctx, "harbor", "")
			if err == nil || strings.Contains(err.Error(), "private") || generations > 1 {
				t.Fatal("unsafe failure", generations, err)
			}
			if generations == 0 && strings.Contains(err.Error(), "allowance may have been used") {
				t.Fatal("preflight incorrectly claimed allowance may have been spent")
			}
			if (mode == "busy" || mode == "no token") && calls != 0 {
				t.Fatal("unauthorized/busy account ran CLI")
			}
		})
	}
}

func TestGlowbomImageOversizedReferenceFailsBeforeAccountOrCharge(t *testing.T) {
	var oversized bytes.Buffer
	if err := png.Encode(&oversized, image.NewRGBA(image.Rect(0, 0, 2049, 2048))); err != nil {
		t.Fatal(err)
	}
	mockGlowbomImageAccount(t, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("invalid reference ran CLI")
		return nil, nil
	})
	for _, reference := range []string{"%%%", base64.StdEncoding.EncodeToString(oversized.Bytes()), strings.Repeat("A", 7_000_000)} {
		if _, err := callGlowbomImageGeneration(context.Background(), "harbor", reference); err == nil {
			t.Fatal("invalid reference accepted")
		}
	}
}

func TestGlowbomImageSourcesAndRoutesRequireAccountAuthentication(t *testing.T) {
	for _, token := range []string{"", "fixture-token"} {
		t.Run(token, func(t *testing.T) {
			bridge := mockGlowbomImageAccount(t, func(context.Context, ...string) ([]byte, error) {
				t.Fatal("unauthorized account command")
				return nil, nil
			})
			bridge.token = token
			root := studioTestProject(t, "Account images")
			icon := glowbomImagePost(openCodeGenerateIconHandler, "/icon", projectIconRequest{Path: root, Prompt: "harbor", SourceID: "glowbom-api"}, false)
			studio := glowbomImagePost(studioImageGenerateHandler, "/studio/images/generate", map[string]string{"prompt": "harbor", "sourceId": "glowbom-api"}, false)
			spacedStudio := glowbomImagePost(studioImageGenerateHandler, "/studio/images/generate", map[string]string{"prompt": "harbor", "sourceId": " glowbom-api "}, false)
			service := &chatService{}
			chat := glowbomImagePost(service.streamHandler, "/chat/stream", chatRequest{Mode: "prototype", ProjectPath: root, Model: "test/model", Messages: []chatMessage{{Role: "user", Text: "harbor"}}, Images: &chatImageOptions{SourceID: "glowbom-api"}}, false)
			if icon.Code != 401 || studio.Code != 401 || spacedStudio.Code != 401 || chat.Code != 401 {
				t.Fatal("account generation accepted missing authentication", icon.Code, studio.Code, chat.Code)
			}
			r := httptest.NewRequest(http.MethodGet, "/sources", nil)
			r.Header.Set("Authorization", "Bearer invalid-token")
			w := httptest.NewRecorder()
			openCodeIconSourcesHandler(w, r)
			if strings.Contains(w.Body.String(), `"authType":"account","available":true`) {
				t.Fatal("unauthorized account probe succeeded")
			}
		})
	}
	mockGlowbomImageAccount(t, func(context.Context, ...string) ([]byte, error) { return glowbomSignedIn(), nil })
	r := httptest.NewRequest(http.MethodGet, "/sources", nil)
	r.Header.Set("Authorization", "Bearer fixture-token")
	w := httptest.NewRecorder()
	openCodeIconSourcesHandler(w, r)
	if !strings.Contains(w.Body.String(), `"authType":"account","available":true`) || strings.Contains(w.Body.String(), "must-not-leak") {
		t.Fatal("account source unavailable or leaked status", w.Body.String())
	}
}

func TestGlowbomImageWorksForIconStudioAndPrototype(t *testing.T) {
	for _, surface := range []string{"icon", "studio", "prototype"} {
		t.Run(surface, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			original := `<!doctype html><html><img src="glowbomimages:The person from the reference photo exploring the harbor"></html>`
			root, record := chatImagesFixture(t, original)
			data := projectIconTestImage(t, "png")
			reference, _ := normalizeProjectIconReference(base64.StdEncoding.EncodeToString(projectIconTestImage(t, "jpeg")))
			generations := 0
			mockGlowbomImageAccount(t, func(ctx context.Context, args ...string) ([]byte, error) {
				if args[0] == "account" {
					return glowbomSignedIn(), nil
				}
				generations++
				deadline, _ := ctx.Deadline()
				if time.Until(deadline) < 9*time.Minute {
					t.Fatal("Glowbom request inherited a short provider timeout")
				}
				output, ref := "", ""
				for i, arg := range args {
					if arg == "--output" {
						output = args[i+1]
					}
					if arg == "--ref" {
						ref = args[i+1]
					}
				}
				photo, _ := os.ReadFile(ref)
				if base64.StdEncoding.EncodeToString(photo) != reference {
					t.Fatal("reference not forwarded")
				}
				return nil, os.WriteFile(output, data, 0600)
			})
			mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) { t.Fatal("fell back to another provider"); return nil, nil })
			switch surface {
			case "icon":
				w := glowbomImagePost(openCodeGenerateIconHandler, "/icon", projectIconRequest{Path: root, Prompt: "harbor", SourceID: "glowbom-api", ReferenceImage: reference}, true)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) {
					t.Fatal(w.Code, w.Body.String())
				}
			case "studio":
				w := glowbomImagePost(studioImageGenerateHandler, "/studio/images/generate", map[string]string{"prompt": "harbor", "sourceId": "glowbom-api", "referenceImage": "data:image/png;base64," + reference, "aspectRatio": ""}, true)
				if w.Code != 200 || !strings.Contains(w.Body.String(), glowbomImageSourceLabel) || !strings.Contains(w.Body.String(), `"dimensions":[2,2]`) {
					t.Fatal(w.Code, w.Body.String())
				}
			case "prototype":
				final, warnings, err := materializeChatImages(context.Background(), root, record, original, chatImageOptions{SourceID: "glowbom-api", Personalization: true, ReferencePath: "attached.png", referencePNG: reference}, func(map[string]any) {})
				if err != nil || len(warnings) > 0 || strings.Contains(final, "glowbomimages:") {
					t.Fatal(warnings, err)
				}
			}
			if generations != 1 {
				t.Fatal("wrong generation count", generations)
			}
			images, _, err := allStudioProjectAssets()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, asset := range images {
				found = found || asset.SourceService == glowbomImageSourceLabel
			}
			if !found {
				t.Fatal("generated image missing from Studio")
			}
		})
	}
}

func TestStudioImageStatusStillSupportsSourcesWhenXAICredentialsInvalid(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("GLOWBOM_OPENCODE_DATA_HOME", t.TempDir())
	paths := xAIAuthFileCandidates()
	if len(paths) == 0 {
		t.Fatal("missing isolated auth path")
	}
	if err := os.MkdirAll(filepath.Dir(paths[0]), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	studioImagesStatusHandler(w, httptest.NewRequest(http.MethodGet, "/studio/images/status", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sourceSelectionSupported":true`) || !strings.Contains(w.Body.String(), `"connected":false`) || !strings.Contains(w.Body.String(), `"error":`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestStudioExplicitImageSourcesKeepReferenceAndSelection(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	t.Setenv(grokSubscriptionMediaFlag, "1")
	for _, source := range []string{"openai-api", "gemini-api", "xai-api", "xai-subscription"} {
		t.Run(source, func(t *testing.T) {
			isolateProjectIconCredentials(t, `{"xai":{"type":"oauth","access":"subscription-image-key","expires":4102444800000}}`)
			t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
			data := projectIconTestImage(t, "png")
			reference := base64.StdEncoding.EncodeToString(data)
			generated := studioAspectTestImage(t, 32, 18)
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				actual, _ := chatPersonalizationReference(t, r, source, true)
				if actual != reference {
					t.Fatal("Studio source ignored the reference")
				}
				if source == "gemini-api" {
					body, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"inlineData": map[string]string{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(generated)}}}}}}})
					return chatImageReply(string(body)), nil
				}
				return iconProviderResponse(generated), nil
			})
			w := glowbomImagePost(studioImageGenerateHandler, "/studio/images/generate", map[string]string{"prompt": "harbor", "sourceId": source, "apiKey": "image-key", "referenceImage": "data:image/png;base64," + reference, "aspectRatio": "16:9"}, true)
			if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), `"dimensions":[32,18]`) {
				t.Fatal("Studio did not use exactly the selected source", w.Code, calls, w.Body.String())
			}
		})
	}
}
