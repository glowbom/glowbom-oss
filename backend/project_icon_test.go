package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type projectIconTestTransport func(*http.Request) (*http.Response, error)

func (fn projectIconTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func isolateProjectIconCredentials(t *testing.T, credentials ...string) {
	t.Helper()
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	for _, name := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY"} {
		t.Setenv(name, "")
	}
	var paths []string
	for _, contents := range credentials {
		path := filepath.Join(t.TempDir(), "auth.json")
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	original := projectIconAuthFileCandidates
	projectIconAuthFileCandidates = func() []string { return paths }
	t.Cleanup(func() { projectIconAuthFileCandidates = original })
	originalCodexPath := codexImageCodexAuthFilePath
	missingCodexPath := filepath.Join(t.TempDir(), "missing-auth.json")
	codexImageCodexAuthFilePath = func() string { return missingCodexPath }
	t.Cleanup(func() { codexImageCodexAuthFilePath = originalCodexPath })
}

func mockProjectIconProvider(t *testing.T, fn projectIconTestTransport) {
	t.Helper()
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: fn}
	t.Cleanup(func() { http.DefaultClient = original })
}

func projectIconTestImage(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&out, img, nil)
	} else {
		err = png.Encode(&out, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func iconProviderResponse(data []byte) *http.Response {
	body, _ := json.Marshal(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(data)}}})
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}
}

func postProjectIcon(t *testing.T, req projectIconRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	openCodeGenerateIconHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/project/icon/generate", bytes.NewReader(body)))
	return recorder
}

func TestProjectIconSourcesPreferSubscriptionWithoutExposingCredentials(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	isolateProjectIconCredentials(t,
		`{"xai":{"type":"api","key":"secret-xai-api"},"openai":{"type":"oauth","access":"secret-codex-oauth"}}`,
		`{"xai":{"type":"oauth","access":"secret-xai-oauth","expires":4102444800000},"google":{"type":"api","key":"secret-google-api"}}`,
	)
	t.Setenv("OPENAI_API_KEY", "secret-openai-env")
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("source listing made a network request")
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
	if recorder.Code != 200 || result.Recommended != "xai-subscription" || len(result.Sources) != 6 {
		t.Fatalf("unexpected catalog: %s", recorder.Body.String())
	}
	for _, source := range result.Sources {
		if source.ID == "glowbom-api" || source.ID == "openai-subscription" {
			continue
		}
		if !source.Available {
			t.Fatalf("source unavailable: %+v", source)
		}
	}
	if strings.Contains(recorder.Body.String(), "secret-") {
		t.Fatal("source listing exposed a credential")
	}
}

func TestProjectIconSourcesRejectUnusableSubscriptionAndChatOAuth(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	isolateProjectIconCredentials(t, `{"xai":{"type":"oauth","access":"expired","expires":1},"openai":{"type":"oauth","access":"chat-only"}}`)
	for _, source := range projectIconSources() {
		if source.Available {
			t.Fatalf("unexpected available source %+v", source)
		}
	}
	if _, _, err := resolveProjectIconSource(projectIconRequest{SourceID: "xai-subscription", APIKey: "must-not-fallback"}); err == nil {
		t.Fatal("expired subscription accepted")
	}
	if _, _, err := resolveProjectIconSource(projectIconRequest{SourceID: "unknown", APIKey: "must-not-fallback"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestProjectIconGenerationUsesSelectedAPIAndSavesRealPNG(t *testing.T) {
	isolateProjectIconCredentials(t)
	dir := t.TempDir()
	jpegData := projectIconTestImage(t, "jpeg")
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer transient-key" {
			t.Fatal("wrong image provider or credential")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != openAIImageModelID || body["size"] != "1024x1024" || body["output_format"] != "png" {
			t.Fatalf("unexpected payload %v", body)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("generation has no timeout")
		}
		return iconProviderResponse(jpegData), nil
	})
	recorder := postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "A music app", SourceID: "openai-api", APIKey: "transient-key"})
	var result struct {
		Success bool   `json:"success"`
		Image   string `json:"image"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("generation failed: %s", recorder.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("icon is not PNG: %v", err)
	}
	if !strings.HasPrefix(result.Image, "data:image/png;base64,") {
		t.Fatal("response icon is not PNG")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || files[0].Name() != "icon.png" {
		t.Fatal("generation persisted an unexpected file")
	}
}

func TestProjectIconSubscriptionDoesNotFallbackToAPI(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	isolateProjectIconCredentials(t, `{"xai":{"type":"api","key":"api-key"}}`, `{"xai":{"type":"oauth","access":"subscription-key","expires":4102444800000}}`)
	data := projectIconTestImage(t, "png")
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.x.ai" || r.Header.Get("Authorization") != "Bearer subscription-key" {
			t.Fatal("subscription selection used another credential")
		}
		return iconProviderResponse(data), nil
	})
	recorder := postProjectIcon(t, projectIconRequest{Path: t.TempDir(), Prompt: "A music app", SourceID: "xai-subscription", APIKey: "api-must-be-ignored"})
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("subscription generation failed: %s", recorder.Body.String())
	}
}

func TestProjectIconPreservesExistingOnProviderFailureOrInvalidImage(t *testing.T) {
	for _, kind := range []string{"provider-failure", "invalid-image"} {
		t.Run(kind, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			dir := t.TempDir()
			previous := projectIconTestImage(t, "png")
			if err := os.WriteFile(filepath.Join(dir, "icon.png"), previous, 0644); err != nil {
				t.Fatal(err)
			}
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				if kind == "provider-failure" {
					return nil, errors.New("do not expose private-key or provider payload")
				}
				return iconProviderResponse([]byte("not-an-image")), nil
			})
			recorder := postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "An app", SourceID: "openai-api", APIKey: "private-key"})
			if strings.Contains(recorder.Body.String(), "private-key") || !strings.Contains(recorder.Body.String(), `"success":false`) {
				t.Fatalf("unsafe failure: %s", recorder.Body.String())
			}
			after, _ := os.ReadFile(filepath.Join(dir, "icon.png"))
			if !bytes.Equal(previous, after) {
				t.Fatal("failed generation changed existing icon")
			}
		})
	}
}

func TestProjectIconCancellationReachesProviderAndKeepsExisting(t *testing.T) {
	isolateProjectIconCredentials(t)
	dir := t.TempDir()
	previous := projectIconTestImage(t, "png")
	if err := os.WriteFile(filepath.Join(dir, "icon.png"), previous, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		cancel()
		if r.Context().Err() != context.Canceled {
			t.Fatal("provider request did not receive cancellation")
		}
		return iconProviderResponse(projectIconTestImage(t, "jpeg")), nil
	})
	body, _ := json.Marshal(projectIconRequest{Path: dir, Prompt: "An app", SourceID: "openai-api", APIKey: "key"})
	recorder := httptest.NewRecorder()
	openCodeGenerateIconHandler(recorder, httptest.NewRequest(http.MethodPost, "/opencode/project/icon/generate", bytes.NewReader(body)).WithContext(ctx))
	after, _ := os.ReadFile(filepath.Join(dir, "icon.png"))
	if !bytes.Equal(previous, after) || !strings.Contains(recorder.Body.String(), `"success":false`) {
		t.Fatal("cancelled generation overwrote previous icon")
	}
}

func TestProjectIconRejectsSymlinksAndKeepsNewerIcon(t *testing.T) {
	isolateProjectIconCredentials(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "icon.png")); err != nil {
		t.Fatal(err)
	}
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("unsafe path reached provider")
		return nil, nil
	})
	recorder := httptest.NewRecorder()
	openCodeProjectIconHandler(recorder, httptest.NewRequest(http.MethodGet, "/opencode/project/icon?path="+url.QueryEscape(dir), nil))
	if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "private") {
		t.Fatal("icon read followed symlink")
	}
	recorder = postProjectIcon(t, projectIconRequest{Path: dir, Prompt: "An app", SourceID: "openai-api", APIKey: "key"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal("icon write accepted symlink")
	}
	if err := os.Remove(filepath.Join(dir, "icon.png")); err != nil {
		t.Fatal(err)
	}
	root, err := openProjectIconRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	newer := projectIconTestImage(t, "png")
	if err := os.WriteFile(filepath.Join(dir, "icon.png"), newer, 0644); err != nil {
		t.Fatal(err)
	}
	if err := saveProjectIcon(root, nil, projectIconTestImage(t, "jpeg")); err == nil {
		t.Fatal("overwrote a newer icon")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "icon.png"))
	if !bytes.Equal(newer, after) {
		t.Fatal("newer icon changed")
	}
}

func TestProjectIconMissingIsSuccessfulAndMethodsAreRestricted(t *testing.T) {
	recorder := httptest.NewRecorder()
	openCodeProjectIconHandler(recorder, httptest.NewRequest(http.MethodGet, "/opencode/project/icon?path="+url.QueryEscape(t.TempDir()), nil))
	if !strings.Contains(recorder.Body.String(), `"exists":false`) || !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("unexpected missing result %s", recorder.Body.String())
	}
	for _, handler := range []http.HandlerFunc{openCodeProjectIconHandler, openCodeIconSourcesHandler} {
		recorder = httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatal("read endpoint accepted POST")
		}
	}
}

func TestProjectIconGeminiUsesConfiguredKeyAndImageResponse(t *testing.T) {
	isolateProjectIconCredentials(t, `{"google":{"type":"api","key":"stored-google-key"}}`)
	data := projectIconTestImage(t, "png")
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "generativelanguage.googleapis.com" || r.URL.RawQuery != "" || r.Header.Get("x-goog-api-key") != "stored-google-key" {
			t.Fatal("Gemini did not use the stored key safely")
		}
		payload, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"inlineData": map[string]string{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(data)}}}}}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload)), Header: http.Header{}}, nil
	})
	recorder := postProjectIcon(t, projectIconRequest{Path: t.TempDir(), Prompt: "An app", SourceID: "gemini-api"})
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("Gemini generation failed: %s", recorder.Body.String())
	}
}

func TestProjectIconSubscriptionRefreshesSelectedOAuthAndRetries(t *testing.T) {
	t.Setenv(grokSubscriptionMediaFlag, "1")
	isolateProjectIconCredentials(t, `{"xai":{"type":"api","key":"api-key"}}`, `{"xai":{"type":"oauth","access":"old-subscription","refresh":"refresh-token","expires":4102444800000}}`)
	data := projectIconTestImage(t, "png")
	generations, refreshes := 0, 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "auth.x.ai" {
			refreshes++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("refresh_token") != "refresh-token" {
				t.Fatal("used wrong refresh token")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new-subscription","refresh_token":"new-refresh","expires_in":3600}`)), Header: http.Header{}}, nil
		}
		generations++
		if generations == 1 {
			if r.Header.Get("Authorization") != "Bearer old-subscription" {
				t.Fatal("used wrong initial token")
			}
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"expired"}`)), Header: http.Header{}}, nil
		}
		if r.Header.Get("Authorization") != "Bearer new-subscription" {
			t.Fatal("retry did not use refreshed subscription")
		}
		return iconProviderResponse(data), nil
	})
	recorder := postProjectIcon(t, projectIconRequest{Path: t.TempDir(), Prompt: "An app", SourceID: "xai-subscription"})
	if !strings.Contains(recorder.Body.String(), `"success":true`) || generations != 2 || refreshes != 1 {
		t.Fatalf("subscription retry failed: %s (%d generations, %d refreshes)", recorder.Body.String(), generations, refreshes)
	}
}

func TestProjectIconDownloadDoesNotSendProviderTokenToOtherHosts(t *testing.T) {
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("download leaked provider credential")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(projectIconTestImage(t, "png"))), Header: http.Header{"Content-Type": []string{"image/png"}}}, nil
	})
	if _, err := downloadImageURLAsDataURI("https://assets.example.com/image.png", "private-key"); err != nil {
		t.Fatal(err)
	}
}

func TestProjectIconGrokReferenceHasMatchingPNGMIMEType(t *testing.T) {
	isolateProjectIconCredentials(t)
	jpegData := projectIconTestImage(t, "jpeg")
	generated := projectIconTestImage(t, "png")
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.x.ai" || r.URL.Path != "/v1/images/edits" {
			t.Fatal("reference generation used the wrong endpoint")
		}
		var body struct {
			Images []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"images"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Images) != 1 || body.Images[0].Type != "image_url" || !strings.HasPrefix(body.Images[0].URL, "data:image/png;base64,") {
			t.Fatal("normalized PNG reference has the wrong MIME type")
		}
		data, _, err := decodeBase64Payload(body.Images[0].URL, "image/png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Fatalf("reference is not actual PNG data: %v", err)
		}
		return iconProviderResponse(generated), nil
	})
	recorder := postProjectIcon(t, projectIconRequest{
		Path: t.TempDir(), Prompt: "A music app", SourceID: "xai-api", APIKey: "key",
		ReferenceImage: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegData),
	})
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("Grok reference generation failed: %s", recorder.Body.String())
	}
}
