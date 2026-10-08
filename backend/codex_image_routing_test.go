package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexImageRoutingAuth = `{"openai":{"type":"oauth","access":"subscription-token","refresh":"refresh-token","accountId":"image-account","expires":4102444800000}}`

func TestChatGPTImageSourceAvailableWithoutPlatformKey(t *testing.T) {
	isolateProjectIconCredentials(t, codexImageRoutingAuth)
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("source discovery made a network request")
		return nil, nil
	})
	recorder := httptest.NewRecorder()
	openCodeIconSourcesHandler(recorder, httptest.NewRequest(http.MethodGet, "/opencode/project/icon/sources", nil))
	var result struct {
		Sources     []projectIconSource `json:"sources"`
		Recommended string              `json:"recommendedSource"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Recommended != "openai-subscription" {
		t.Fatal("connected ChatGPT was not recommended")
	}
	for _, source := range result.Sources {
		if source.ID == "openai-subscription" && (!source.Available || source.AuthType != "subscription") {
			t.Fatal("ChatGPT subscription unavailable")
		}
		if source.ID == "openai-api" && source.Available {
			t.Fatal("OAuth login counted as an API key")
		}
	}
	for _, secret := range []string{"subscription-token", "refresh-token", "image-account"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatal("source discovery exposed credentials")
		}
	}
}

func TestChatGPTImageSourceDoesNotAcceptAPIKeyAsSubscription(t *testing.T) {
	isolateProjectIconCredentials(t, `{"openai":{"type":"api","key":"platform-key"}}`)
	if _, _, err := resolveProjectIconSource(projectIconRequest{SourceID: "openai-subscription", APIKey: "another-platform-key"}); err == nil {
		t.Fatal("subscription selection accepted an API key")
	}
}

func TestChatGPTImageRoutingPreservesReferencesAndAspects(t *testing.T) {
	for _, entry := range []struct {
		name     string
		size     string
		generate func(context.Context, string, string) (string, string, error)
	}{
		{"icon", "1024x1024", func(ctx context.Context, prompt, reference string) (string, string, error) {
			return generateProjectIcon(ctx, "openai-subscription", "ignored-api-key", prompt, reference)
		}},
		{"studio", "864x1536", func(ctx context.Context, prompt, reference string) (string, string, error) {
			return generateStudioSelectedImage(ctx, "openai-subscription", "ignored-api-key", prompt, reference, "9:16")
		}},
		{"prototype", "1536x864", func(ctx context.Context, prompt, reference string) (string, string, error) {
			return generateChatImage(ctx, "openai-subscription", "ignored-api-key", prompt, reference)
		}},
	} {
		for _, withReference := range []bool{false, true} {
			name := "ordinary"
			if withReference {
				name = "reference"
			}
			t.Run(entry.name+"/"+name, func(t *testing.T) {
				isolateProjectIconCredentials(t, codexImageRoutingAuth)
				data := projectIconTestImage(t, "png")
				reference := ""
				if withReference {
					reference = base64.StdEncoding.EncodeToString(data)
				}
				calls := 0
				mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
					calls++
					endpoint := "/backend-api/codex/images/generations"
					if withReference {
						endpoint = "/backend-api/codex/images/edits"
					}
					if r.URL.Host != "chatgpt.com" || r.URL.Path != endpoint || r.Header.Get("Authorization") != "Bearer subscription-token" || r.Header.Get("ChatGPT-Account-Id") != "image-account" {
						t.Fatal("wrong subscription image route or credentials")
					}
					var body struct {
						Size   string `json:"size"`
						Images []struct {
							URL string `json:"image_url"`
						} `json:"images"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Size != entry.size {
						t.Fatal("selected aspect ratio lost")
					}
					if withReference {
						if len(body.Images) != 1 || body.Images[0].URL != "data:image/png;base64,"+reference {
							t.Fatal("reference image omitted or changed")
						}
					} else if len(body.Images) != 0 {
						t.Fatal("unexpected reference")
					}
					return iconProviderResponse(data), nil
				})
				value, label, err := entry.generate(context.Background(), "A small glowing orb", reference)
				if err != nil || calls != 1 || label != codexImageSourceLabel || value != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
					t.Fatal("subscription generation failed", err)
				}
			})
		}
	}
}

func TestChatGPTIconAndStudioSaveGeneratedImages(t *testing.T) {
	for _, target := range []string{"icon", "studio"} {
		t.Run(target, func(t *testing.T) {
			isolateProjectIconCredentials(t, codexImageRoutingAuth)
			data := projectIconTestImage(t, "png")
			calls := 0
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "chatgpt.com" {
					t.Fatal("contacted another provider")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("image request has no timeout")
				}
				return iconProviderResponse(data), nil
			})
			var recorder *httptest.ResponseRecorder
			if target == "icon" {
				dir := t.TempDir()
				recorder = postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "A small glowing orb", SourceID: "openai-subscription"})
				if saved, err := os.ReadFile(filepath.Join(dir, "icon.png")); err != nil || len(saved) == 0 {
					t.Fatal("icon not saved")
				}
			} else {
				isolateStudioProgress(t)
				body := `{"prompt":"A small glowing orb","sourceId":"openai-subscription","aspectRatio":"1:1"}`
				recorder = httptest.NewRecorder()
				studioImageGenerateHandler(recorder, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
			}
			if recorder.Code != 200 || calls != 1 {
				t.Fatal("generation failed", recorder.Code, recorder.Body.String())
			}
			images, err := listStudioImages()
			if err != nil || len(images) != 1 || images[0].SourceService != codexImageSourceLabel {
				t.Fatal("generated image not saved to Studio", err)
			}
		})
	}
}

func TestChatGPTImageFailureDoesNotReplayOrLeakProviderBody(t *testing.T) {
	isolateProjectIconCredentials(t, codexImageRoutingAuth)
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"private-provider-detail"}`)), Header: http.Header{}}, nil
	})
	dir := t.TempDir()
	previous := projectIconTestImage(t, "png")
	if err := os.WriteFile(filepath.Join(dir, "icon.png"), previous, 0600); err != nil {
		t.Fatal(err)
	}
	recorder := postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "A glowing orb", SourceID: "openai-subscription", APIKey: "must-ignore"})
	if calls != 1 || !strings.Contains(recorder.Body.String(), `"success":false`) || strings.Contains(recorder.Body.String(), "private-provider-detail") {
		t.Fatal("unsafe failure or repeated request")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "icon.png"))
	if !bytes.Equal(previous, after) {
		t.Fatal("failed generation replaced previous icon")
	}
}
