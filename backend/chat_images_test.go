package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func chatImagesFixture(t *testing.T, document string) (string, string) {
	t.Helper()
	root, err := createSketchProject(t.TempDir(), "Images")
	if err != nil {
		t.Fatal(err)
	}
	record := ""
	if err := saveChatPrototype(root, document, "", nil, "test/model", nil, &record); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	return root, record
}
func chatImageReply(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestChatImagesEmitPromptBeforeProviderRequest(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			isolateProjectIconCredentials(t)
			original := `<!doctype html><html><img src="glowbomimages:A%20harbor%20%26%20boats"><img src='glowbomimages: A &quot;quiet&quot;   portrait '></html>`
			root, record := chatImagesFixture(t, original)
			data := projectIconTestImage(t, "png")
			reference := base64.StdEncoding.EncodeToString(data)
			prompts := []string{"A harbor & boats", `A "quiet" portrait`}
			events := []map[string]any{}
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > len(prompts) || len(events) == 0 {
					t.Fatal("provider called before image prompt progress")
				}
				encoded, err := json.Marshal(events[len(events)-1])
				if err != nil {
					t.Fatal(err)
				}
				var progress struct {
					Status      string `json:"status"`
					ImagePrompt struct {
						Index  int    `json:"index"`
						Total  int    `json:"total"`
						Prompt string `json:"prompt"`
					} `json:"imagePrompt"`
				}
				if err := json.Unmarshal(encoded, &progress); err != nil {
					t.Fatal(err)
				}
				if progress.Status != fmt.Sprintf("Creating image %d of 2", calls) || progress.ImagePrompt.Index != calls || progress.ImagePrompt.Total != 2 || progress.ImagePrompt.Prompt != prompts[calls-1] {
					t.Fatalf("wrong image prompt progress: %s", encoded)
				}
				actualReference, actualPrompt := chatPersonalizationReference(t, r, "xai-api", calls == 2)
				if calls == 2 && (actualReference != reference || !strings.HasPrefix(actualPrompt, prompts[calls-1]+" Use the supplied reference")) {
					t.Fatal("provider did not receive the personalized prompt")
				}
				if calls == 1 && (actualReference != "" || actualPrompt != imageAspectPrompt(prompts[0], "16:9", false)) {
					t.Fatal("generic scenery unexpectedly received personalization")
				}
				if failed {
					return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"private-provider-diagnostic"}`))}, nil
				}
				return iconProviderResponse(data), nil
			})
			options := chatImageOptions{SourceID: "xai-api", APIKey: "image-key", Personalization: true, ReferencePath: "/private/reference-photo.png", referencePNG: reference}
			final, warnings, err := materializeChatImages(context.Background(), root, record, original, options, func(event map[string]any) {
				events = append(events, event)
			})
			if err != nil || calls != 2 {
				t.Fatal("image pass failed", calls, err)
			}
			if failed && (final != original || len(warnings) != 2) {
				t.Fatal("failure behavior changed", warnings)
			}
			if !failed && (len(warnings) != 0 || strings.Contains(final, "glowbomimages:")) {
				t.Fatal("images were not materialized", warnings)
			}
			encoded, err := json.Marshal(events)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{options.APIKey, options.ReferencePath, reference, "private-provider-diagnostic", "Use the supplied reference", "data:image/"} {
				if strings.Contains(string(encoded), private) {
					t.Fatal("image progress exposed private generation details")
				}
			}
		})
	}
}

func TestChatImagesMaterializeSelectedSourceDeduplicateAndReuse(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	for _, source := range []string{"picsum", "openai-api", "gemini-api", "xai-api", "xai-subscription"} {
		t.Run(source, func(t *testing.T) {
			isolateProjectIconCredentials(t, `{"xai":{"type":"oauth","access":"subscription-image-key","expires":4102444800000}}`)
			original := `<!doctype html><html><img src="glowbomimages:A harbor"><div style="background:url('glowbyimage:A%20harbor')"></div></html>`
			root, record := chatImagesFixture(t, original)
			data := projectIconTestImage(t, "png")
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				switch source {
				case "picsum":
					if r.URL.Host != "picsum.photos" || !strings.HasPrefix(r.URL.Path, "/seed/") || r.Header.Get("Authorization") != "" {
						t.Fatal("wrong sample source")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
				case "openai-api":
					if r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer transient-image-key" {
						t.Fatal("wrong OpenAI source")
					}
				case "gemini-api":
					if r.URL.Host != "generativelanguage.googleapis.com" || r.Header.Get("x-goog-api-key") != "transient-image-key" {
						t.Fatal("wrong Gemini source")
					}
					return chatImageReply(fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":%q}}]}}]}`, base64.StdEncoding.EncodeToString(data))), nil
				default:
					key := "transient-image-key"
					if source == "xai-subscription" {
						key = "subscription-image-key"
					}
					if r.URL.Host != "api.x.ai" || r.Header.Get("Authorization") != "Bearer "+key {
						t.Fatal("wrong xAI source")
					}
				}
				return iconProviderResponse(data), nil
			})
			events := []map[string]any{}
			emit := func(event map[string]any) { events = append(events, event) }
			final, warnings, err := materializeChatImages(context.Background(), root, record, original, chatImageOptions{SourceID: source, APIKey: "transient-image-key"}, emit)
			if err != nil || len(warnings) != 0 || calls != 1 || strings.Contains(final, "glowbomimages:") || strings.Count(final, "assets/glowbom-image-") != 2 {
				t.Fatal("image pass failed", calls, warnings, err, final)
			}
			saved, _ := os.ReadFile(filepath.Join(root, "prototype/index.html"))
			history, _ := os.ReadFile(filepath.Join(record, "result.html"))
			if string(saved) != final || string(history) != final {
				t.Fatal("saved HTML/history mismatch")
			}
			assets, _ := os.ReadDir(filepath.Join(root, "prototype/assets"))
			if len(assets) != 1 {
				t.Fatal("image not deduplicated")
			}
			studio, _ := os.ReadDir(filepath.Join(os.Getenv("GLOWBOM_STUDIO_DIR"), "Assets"))
			if len(studio) != 1 {
				t.Fatal("image missing in Studio", studio)
			}
			raw, _ := os.ReadFile(filepath.Join(os.Getenv("GLOWBOM_STUDIO_DIR"), "Assets", studio[0].Name()))
			var entry studioImageRecord
			if json.Unmarshal(raw, &entry) != nil || entry.Prompt != "A harbor" || entry.Dimensions.Width != 2 {
				t.Fatal("bad Studio metadata")
			}
			if len(events) < 2 || events[len(events)-1]["text"] != final {
				t.Fatal("missing updated HTML progress")
			}
			request, _ := os.ReadFile(filepath.Join(record, "request.json"))
			if strings.Contains(string(request), "transient-image-key") {
				t.Fatal("key persisted")
			}
			if err := saveChatPrototype(root, original, final, nil, "test/model", nil, &record); err != nil {
				t.Fatal(err)
			}
			_, warnings, err = materializeChatImages(context.Background(), root, record, original, chatImageOptions{SourceID: source, APIKey: "transient-image-key"}, emit)
			if err != nil || len(warnings) != 0 || calls != 1 {
				t.Fatal("existing project image charged again", calls, warnings, err)
			}
			studio, _ = os.ReadDir(filepath.Join(os.Getenv("GLOWBOM_STUDIO_DIR"), "Assets"))
			if len(studio) != 1 {
				t.Fatal("cached image duplicated in Studio")
			}
		})
	}
}

func TestChatImagesLimitsFailuresAndCancellation(t *testing.T) {
	for _, mode := range []string{"limit", "failure", "cancel", "changed", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			original := `<!doctype html><html>`
			for i := 0; i < 6; i++ {
				original += fmt.Sprintf(`<img src="glowbomimages:image %d">`, i)
			}
			original += `</html>`
			root, record := chatImagesFixture(t, original)
			outside := t.TempDir()
			if mode == "symlink" {
				os.Remove(filepath.Join(root, "prototype/assets"))
				if err := os.Symlink(outside, filepath.Join(root, "prototype/assets")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if mode == "failure" {
					return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"private-key secret diagnostics"}`)), Header: http.Header{}}, nil
				}
				if mode == "cancel" {
					cancel()
					return nil, context.Canceled
				}
				if mode == "changed" {
					os.WriteFile(filepath.Join(root, "prototype/index.html"), []byte("newer prototype"), 0600)
				}
				return iconProviderResponse(projectIconTestImage(t, "png")), nil
			})
			notices := []string{}
			final, warnings, err := materializeChatImages(ctx, root, record, original, chatImageOptions{SourceID: "openai-api", APIKey: "private-key"}, func(event map[string]any) {
				if notice, ok := event["notice"].(string); ok {
					if _, warning := event["warning"]; warning {
						t.Fatal("routine image limit emitted as a warning")
					}
					notices = append(notices, notice)
				}
			})
			if len(notices) != 1 || notices[0] != "Creating up to four images for this prototype." {
				t.Fatal("missing separate image limit notice", notices)
			}
			if strings.Contains(strings.Join(warnings, " "), "private-key") || strings.Contains(strings.Join(warnings, " "), "secret") {
				t.Fatal("provider details leaked")
			}
			switch mode {
			case "limit":
				if err != nil || calls != 4 || len(warnings) != 0 || strings.Count(final, "glowbomimages:") != 2 {
					t.Fatal(calls, warnings, err)
				}
			case "failure":
				if err != nil || calls != 4 || final != original || len(warnings) != 4 {
					t.Fatal(calls, warnings, err)
				}
			case "cancel":
				if err == nil || calls != 1 || !strings.Contains(err.Error(), "Prototype saved") {
					t.Fatal(calls, err)
				}
			case "changed":
				saved, _ := os.ReadFile(filepath.Join(root, "prototype/index.html"))
				if err == nil || calls != 1 || string(saved) != "newer prototype" {
					t.Fatal("overwrote newer prototype", calls, err)
				}
			case "symlink":
				entries, _ := os.ReadDir(outside)
				if calls != 0 || len(entries) != 0 {
					t.Fatal("escaped image output directory")
				}
			}
		})
	}
}

func TestChatImagesStreamStartsOnlyAfterValidHTMLSaved(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			isolateProjectIconCredentials(t)
			t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
			rememberCursorModels(nil)
			original := `<!doctype html><html>Existing</html>`
			root, _ := chatImagesFixture(t, original)
			imageCalls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "api.openai.com" {
					imageCalls++
					saved, _ := os.ReadFile(filepath.Join(root, "prototype/index.html"))
					if !strings.Contains(string(saved), "glowbomimages:") {
						t.Fatal("images started before HTML saved")
					}
					return iconProviderResponse(projectIconTestImage(t, "png")), nil
				}
				switch r.URL.Path {
				case "/provider":
					return chatImageReply(`{"connected":["test"],"all":[{"id":"test","models":{"model":{"name":"Model"}}}]}`), nil
				case "/session":
					return chatImageReply(`{"id":"image-session","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`), nil
				case "/experimental/tool/ids":
					return chatImageReply(`[]`), nil
				case "/event":
					return chatImageReply("data: {}\n\n"), nil
				case "/session/image-session/message":
					body, _ := io.ReadAll(r.Body)
					if strings.Contains(string(body), "private-image-key") || !strings.Contains(string(body), "glowbomimages:") {
						t.Fatal("image prompt setup leaked key or omitted placeholders")
					}
					html := `<!doctype html><html><img src="glowbomimages:harbor"></html>`
					if invalid {
						html = "incomplete"
					}
					response, _ := json.Marshal(map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "text", "text": html}}})
					return chatImageReply(string(response)), nil
				default:
					return chatImageReply(`true`), nil
				}
			})
			service := &chatService{directory: "/isolated", serverURL: "http://fixture", client: http.DefaultClient, prepare: func() error { return nil }}
			body, _ := json.Marshal(chatRequest{ProjectPath: root, Mode: "prototype", Model: "test/model", Messages: []chatMessage{{Role: "user", Text: "Build"}}, Images: &chatImageOptions{SourceID: "openai-api", APIKey: "private-image-key"}})
			recorder := httptest.NewRecorder()
			service.streamHandler(recorder, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(body)))
			output := recorder.Body.String()
			if invalid {
				if imageCalls != 0 || strings.Contains(output, `"previewReady":true`) || !strings.Contains(output, `"success":false`) {
					t.Fatal(imageCalls, output)
				}
			} else if imageCalls != 1 || !strings.Contains(output, `"previewReady":true`) || !strings.Contains(output, `"success":true`) || !strings.Contains(output, "assets/glowbom-image-") {
				t.Fatal(imageCalls, output)
			}
		})
	}
}

func TestChatImageOptionsRejectImplicitOrUnknownProviders(t *testing.T) {
	for _, source := range []string{"", "auto", "unknown"} {
		if validateChatImageOptions(chatImageOptions{SourceID: source}) == nil {
			t.Fatal("accepted implicit source")
		}
	}
	request := chatRequest{Mode: "chat", Model: "test/model", Messages: []chatMessage{{Role: "user", Text: "hello"}}, Images: &chatImageOptions{SourceID: "picsum"}}
	if validateChatRequest(request) == nil {
		t.Fatal("chat can trigger image generation")
	}
}

func TestChatImagesConcurrentPassDoesNotDuplicateProviderCharge(t *testing.T) {
	isolateProjectIconCredentials(t)
	original := `<!doctype html><html><img src="glowbomimages:harbor"></html>`
	root, record := chatImagesFixture(t, original)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	results := make(chan error, 2)
	run := func() {
		_, _, err := materializeChatImages(context.Background(), root, record, original, chatImageOptions{SourceID: "openai-api", APIKey: "fixture"}, func(map[string]any) {})
		results <- err
	}
	go run()
	<-started
	go run()
	close(release)
	first, second := <-results, <-results
	if calls.Load() != 1 || (first == nil) == (second == nil) {
		t.Fatal("overlapping passes charged twice or overwrote a revision", calls.Load(), first, second)
	}
}

func TestChatImagesSimilarPromptsKeepSeparatePlaceholders(t *testing.T) {
	isolateProjectIconCredentials(t)
	original := `<!doctype html><html><img src="glowbomimages:harbor"><img src="glowbomimages:harbor at night"></html>`
	root, record := chatImagesFixture(t, original)
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	final, warnings, err := materializeChatImages(context.Background(), root, record, original, chatImageOptions{SourceID: "openai-api", APIKey: "fixture"}, func(map[string]any) {})
	if err != nil || len(warnings) != 0 || calls != 2 || !strings.Contains(final, `src="assets/`+chatImageFilename("openai-api", "harbor")+`"`) || !strings.Contains(final, `src="assets/`+chatImageFilename("openai-api", "harbor at night")+`"`) {
		t.Fatal("short image prompt corrupted a longer placeholder", calls, warnings, err, final)
	}
}

func TestChatImagesQuotedDescriptionsKeepPunctuationAndFollowingCode(t *testing.T) {
	isolateProjectIconCredentials(t)
	original := "<!doctype html><html><img src=\"glowbomimages:harbor (sunset)\"><style>.hero {background:url('glowbyimage:harbor (sunset)')}</style><script>const src = `glowbomimages:forest`; const after = ')';</script></html>"
	root, record := chatImagesFixture(t, original)
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	final, warnings, err := materializeChatImages(context.Background(), root, record, original, chatImageOptions{SourceID: "openai-api", APIKey: "fixture"}, func(map[string]any) {})
	if err != nil || len(warnings) != 0 || calls != 2 || strings.Count(final, chatImageFilename("openai-api", "harbor (sunset)")) != 2 || !strings.Contains(final, "`; const after = ')';") || strings.Contains(final, "glowbomimages:") || strings.Contains(final, "glowbyimage:") {
		t.Fatal("quoted placeholder corrupted surrounding code", calls, warnings, err, final)
	}
}

func TestChatImagesQueuedWorkChecksLatestPrototypeBeforeProvider(t *testing.T) {
	isolateProjectIconCredentials(t)
	original := `<!doctype html><html><img src="glowbomimages:harbor"></html>`
	root, record := chatImagesFixture(t, original)
	chatImageSlots <- struct{}{}
	chatImageSlots <- struct{}{}
	var calls atomic.Int32
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return iconProviderResponse(projectIconTestImage(t, "png")), nil
	})
	preparing := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := materializeChatImages(context.Background(), root, record, original, chatImageOptions{SourceID: "openai-api", APIKey: "fixture"}, func(event map[string]any) {
			if _, ok := event["status"]; ok {
				close(preparing)
			}
		})
		done <- err
	}()
	<-preparing
	if err := os.WriteFile(filepath.Join(root, "prototype/index.html"), []byte("newer prototype"), 0600); err != nil {
		t.Error(err)
	}
	<-chatImageSlots
	err := <-done
	<-chatImageSlots
	if err == nil || calls.Load() != 0 {
		t.Fatal("queued stale request reached image provider", calls.Load(), err)
	}
}

func TestChatImagesKeepAssetsChangedWhilePreparing(t *testing.T) {
	for _, mode := range []string{"cached changed", "cached unchanged", "new appeared"} {
		t.Run(mode, func(t *testing.T) {
			original := `<!doctype html><html>Before</html>`
			root, record := chatImagesFixture(t, original)
			path := filepath.Join(root, "prototype/assets/image.png")
			jpeg := projectIconTestImage(t, "jpeg")
			data, err := normalizeProjectIcon(jpeg)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cached changed" {
				jpeg = projectIconTestImage(t, "png")
			}
			if err := os.WriteFile(path, jpeg, 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			err = commitChatImage(context.Background(), root, record, original, `<!doctype html><html>After</html>`, "image.png", data, mode == "new appeared")
			after, _ := os.Stat(path)
			saved, _ := os.ReadFile(path)
			if !bytes.Equal(saved, jpeg) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("existing asset was rewritten")
			}
			if mode == "cached unchanged" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("changed asset was not detected")
			}
		})
	}
}
