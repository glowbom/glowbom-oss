package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolateStudioProgress(t *testing.T) {
	t.Helper()
	original := studioGenerations
	studioGenerations = newStudioProgressStore()
	t.Cleanup(func() { studioGenerations = original })
	t.Setenv("GLOWBOM_SERVER_TOKEN", "studio-progress-test")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://localhost:4572")
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
}

func studioProgressRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Glowbom-Token", "studio-progress-test")
	r.Header.Set("Origin", "http://localhost:4572")
	return r
}

func TestStudioProgressUnknownThenActualStages(t *testing.T) {
	isolateStudioProgress(t)
	id := "fixture-image-generation"
	r := studioProgressRequest(http.MethodGet, "/studio/generation/status?id="+id, "")
	w := httptest.NewRecorder()
	studioGenerationStatusHandler(w, r)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"found":false}` {
		t.Fatalf("unknown generation must remain pollable: %s", w.Body.String())
	}
	progress, ok := beginStudioProgress(w, r, id, "image")
	if !ok {
		t.Fatal("could not register generation")
	}
	if state := studioGenerations.read(id); state.Stage != "preparing" || state.Kind != "image" || !state.Found {
		t.Fatal("missing initial stage")
	}
	for _, stage := range []string{"generating", "saving", "complete"} {
		progress.stage(stage)
		w = httptest.NewRecorder()
		studioGenerationStatusHandler(w, r)
		var state studioProgressState
		if json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Stage != stage {
			t.Fatalf("stage %s was not reported", stage)
		}
	}
	progress.finish()
	if studioGenerations.read(id).Stage != "complete" {
		t.Fatal("deferred cleanup overwrote completed result")
	}
}

func TestStudioProgressRejectsUnauthorizedQueries(t *testing.T) {
	for _, name := range []string{"token", "origin", "method", "identifier"} {
		t.Run(name, func(t *testing.T) {
			isolateStudioProgress(t)
			r := studioProgressRequest(http.MethodGet, "/studio/generation/status?id=fixture-image-generation", "")
			want := 401
			switch name {
			case "token":
				r.Header.Del("X-Glowbom-Token")
			case "origin":
				r.Header.Set("Origin", "https://untrusted.example")
				want = 403
			case "method":
				r.Method = http.MethodPost
				want = 405
			case "identifier":
				r.URL.RawQuery = "id=bad"
				want = 400
			}
			w := httptest.NewRecorder()
			studioGenerationStatusHandler(w, r)
			if w.Code != want {
				t.Fatalf("got %d, want %d", w.Code, want)
			}
		})
	}
}

func TestStudioProgressIsBoundedAndRejectsDuplicateSubmission(t *testing.T) {
	store := newStudioProgressStore()
	if store.begin("same-generation", "video") != 200 || store.begin("same-generation", "video") != 409 {
		t.Fatal("duplicate request was not blocked")
	}
	store.update("same-generation", "complete")
	if store.begin("same-generation", "video") != 409 {
		t.Fatal("completed request could be charged again")
	}
	store.entries["same-generation"] = studioProgressEntry{updated: time.Now().Add(-2 * time.Hour)}
	if store.read("same-generation").Found || len(store.entries) != 0 {
		t.Fatal("expired progress was retained")
	}
	for index := range 512 {
		store.entries[string(rune(index))] = studioProgressEntry{updated: time.Now()}
	}
	if store.begin("another-generation", "image") != 429 {
		t.Fatal("progress store grew without a bound")
	}
}

func TestStudioProgressLegacyAndUnauthenticatedDevelopmentRemainOptional(t *testing.T) {
	isolateStudioProgress(t)
	t.Setenv("GLOWBOM_SERVER_TOKEN", "")
	for _, id := range []string{"", "fixture-image-generation"} {
		w := httptest.NewRecorder()
		progress, ok := beginStudioProgress(w, httptest.NewRequest(http.MethodPost, "/studio/images/generate", nil), id, "image")
		if !ok {
			t.Fatal("optional progress changed legacy generation")
		}
		progress.stage("generating")
		progress.stage("complete")
		progress.finish()
	}
	if len(studioGenerations.entries) != 0 {
		t.Fatal("unauthenticated development progress was stored")
	}
}

func TestStudioProgressImageTracksOneProviderRequestAndPreservesJSON(t *testing.T) {
	isolateStudioProgress(t)
	isolateProjectIconCredentials(t)
	id := "fixture-image-generation"
	image := projectIconTestImage(t, "png")
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.openai.com" || studioGenerations.read(id).Stage != "generating" {
			t.Fatal("generation phase did not match provider call")
		}
		return iconProviderResponse(image), nil
	})
	body := `{"generationId":"` + id + `","prompt":"Test image","sourceId":"openai-api","apiKey":"fixture-key"}`
	w := httptest.NewRecorder()
	studioImageGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
	var result map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result["image"] == nil || calls != 1 || studioGenerations.read(id).Stage != "complete" {
		t.Fatalf("image response changed: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	studioImageGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/images/generate", body))
	if w.Code != 409 || calls != 1 {
		t.Fatal("duplicate tracking ID triggered another charged call")
	}
}

func TestStudioProgressFailedReferenceDoesNotStartGeneration(t *testing.T) {
	isolateStudioProgress(t)
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	id := "fixture-video-generation"
	w := httptest.NewRecorder()
	studioVideoGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/videos/generate", `{"generationId":"`+id+`","prompt":"Test video","referenceId":"missing"}`))
	if w.Code != 400 || studioGenerations.read(id).Stage != "failed" {
		t.Fatal("failed request did not finish progress")
	}
}

func TestStudioProgressVideoReflectsTextGenerationAndDownloadPhases(t *testing.T) {
	isolateStudioProgress(t)
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	if err := os.MkdirAll(filepath.Join(dataHome, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataHome, "opencode", "auth.json"), []byte(`{"xai":{"type":"api","key":"fixture-xai-key"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	id := "fixture-video-generation"
	image := projectIconTestImage(t, "jpeg")
	stages := []string{}
	starts := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		stages = append(stages, studioGenerations.read(id).Stage)
		body, contentType := "", "application/json"
		switch r.URL.Path {
		case "/v1/images/generations":
			return iconProviderResponse(image), nil
		case "/v1/videos/generations":
			starts++
			body = `{"request_id":"fixture-operation"}`
		case "/v1/videos/fixture-operation":
			body = `{"status":"completed","video":{"url":"https://api.x.ai/fixture.mp4"}}`
		case "/fixture.mp4":
			body, contentType = "fixture video bytes", "video/mp4"
		default:
			t.Fatalf("unexpected provider request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	w := httptest.NewRecorder()
	studioVideoGenerateHandler(w, studioProgressRequest(http.MethodPost, "/studio/videos/generate", `{"generationId":"`+id+`","prompt":"Test video","durationSeconds":5}`))
	var result map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result["video"] == nil || result["firstFrame"] != nil {
		t.Fatalf("video response changed: %d %s", w.Code, w.Body.String())
	}
	if strings.Join(stages, ",") != "generating,generating,downloading" || starts != 1 || studioGenerations.read(id).Stage != "complete" {
		t.Fatalf("unexpected phases or starts: %v %d", stages, starts)
	}
}
