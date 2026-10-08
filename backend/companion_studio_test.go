package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCompanionStudioGlowbomAccountUsesAutomaticShape(t *testing.T) {
	for _, shape := range []string{"", "1:1", "16:9", "9:16"} {
		t.Run("shape="+shape, func(t *testing.T) {
			isolateCompanionStudio(t)
			t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
			chatImagesFixture(t, "<!doctype html><html></html>")
			data := projectIconTestImage(t, "png")
			reference := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "jpeg"))
			calls := 0
			mockGlowbomImageAccount(t, func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "account" {
					return glowbomSignedIn(), nil
				}
				calls++
				var referencePath string
				for i, arg := range args {
					if arg == "--ref" {
						referencePath = args[i+1]
					}
				}
				prepared, err := os.ReadFile(referencePath)
				if err != nil || validateGlowbomImageReference(base64.StdEncoding.EncodeToString(prepared)) != nil {
					t.Error("JPEG reference was not prepared for the account generator")
				}
				for i, arg := range args {
					if arg == "--output" {
						return nil, os.WriteFile(args[i+1], data, 0600)
					}
				}
				return nil, nil
			})
			s := testCompanion(t, http.HandlerFunc(studioImageGenerateHandler))
			body, _ := json.Marshal(map[string]string{"prompt": "Synthetic harbor", "sourceId": "glowbom-api", "aspectRatio": shape, "referenceImage": reference})
			response := httptest.NewRecorder()
			s.ServeHTTP(response, companionRequest(s, http.MethodPost, "/images", string(body)))
			if response.Code != http.StatusAccepted {
				t.Fatalf("generation rejected: %d", response.Code)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				jobs := s.jobList("image")
				if len(jobs) == 1 && jobs[0]["status"] != "running" {
					if jobs[0]["status"] != "completed" || calls != 1 || jobs[0]["image"] == nil {
						t.Fatalf("account generation failed: %v", jobs[0]["status"])
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("account generation did not finish")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestCompanionStudioPreservesSafeAccountFailureWithoutLeakingProviderOutput(t *testing.T) {
	for _, diagnostic := range []string{
		glowbomImageError(&accountCommandFailure{code: "allowance_required"}).Error(),
		glowbomImageError(&accountCommandFailure{code: "invalid_image_request"}).Error(),
		"private provider output: secret-key https://example.test/?token=private",
	} {
		isolateCompanionStudio(t)
		s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, diagnostic, http.StatusBadGateway)
		}))
		response := httptest.NewRecorder()
		s.ServeHTTP(response, companionRequest(s, http.MethodPost, "/images", `{"prompt":"Synthetic harbor","sourceId":"glowbom-api","aspectRatio":""}`))
		if response.Code != http.StatusAccepted {
			t.Fatal("request rejected before error forwarding")
		}
		deadline := time.Now().Add(time.Second)
		for {
			jobs := s.jobList("image")
			if len(jobs) == 1 && jobs[0]["status"] == "failed" {
				encoded, _ := json.Marshal(jobs)
				if strings.Contains(string(encoded), "secret-key") || strings.Contains(string(encoded), "token=private") {
					t.Fatal("private provider output reached the phone")
				}
				if !strings.HasPrefix(diagnostic, "private") && jobs[0]["error"] != diagnostic {
					t.Fatal("safe account diagnosis was lost")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("failed generation did not finish")
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func isolateCompanionStudio(t *testing.T) {
	t.Helper()
	isolateProjectIconCredentials(t)
	previous := xAIMediaAuthFileCandidates
	xAIMediaAuthFileCandidates = func() []string { return nil }
	t.Cleanup(func() { xAIMediaAuthFileCandidates = previous })
}

func TestCompanionStudioReferenceRejectsUnreadableAndRemoteInputs(t *testing.T) {
	inline := "data:image/png;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))
	if err := validateCompanionStudioReference("", inline); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"https://example.test/image.png", "file:///private/image.jpg", "data:image/svg+xml;base64,PHN2Zz4=", "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png")), "data:image/png;base64,aGVsbG8=", strings.Repeat("x", 3<<20)} {
		if validateCompanionStudioReference("", value) == nil {
			t.Fatal("accepted an invalid or oversized reference")
		}
	}
	if validateCompanionStudioReference("not-an-id", "") == nil || validateCompanionStudioReference(randomUUIDString(), inline) == nil {
		t.Fatal("accepted an invalid or ambiguous Studio reference")
	}
}

func TestCompanionStudioImagesAndVideosForwardReferencesAndKeepJobsSeparate(t *testing.T) {
	isolateCompanionStudio(t)
	t.Setenv("GLOWBOM_SERVER_TOKEN", "synthetic-studio-token")
	inline := "data:image/png;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))
	for _, kind := range []string{"image", "video"} {
		t.Run(kind, func(t *testing.T) {
			calls := make(chan map[string]any, 1)
			s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/studio/"+kind+"s/generate" || r.Host != "127.0.0.1" || r.RemoteAddr != "127.0.0.1:0" || r.Header.Get("Authorization") != "Bearer synthetic-studio-token" {
					t.Error("unsafe internal generation route")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				calls <- body
				writeJSON(w, map[string]any{kind: map[string]any{"id": randomUUIDString(), "timestamp": "2026-10-05T00:00:00Z", "prompt": "Synthetic result", "sourceService": "Fixture"}})
			}))
			body := map[string]any{"prompt": "Use this reference", "aspectRatio": "16:9", "sourceId": "openai-api", "referenceImage": inline}
			if kind == "video" {
				body["sourceId"] = "veo-api"
				body["durationSeconds"] = 4
			}
			encoded, _ := json.Marshal(body)
			response := httptest.NewRecorder()
			s.ServeHTTP(response, companionRequest(s, http.MethodPost, "/"+kind+"s", string(encoded)))
			if response.Code != http.StatusAccepted {
				t.Fatalf("generation status=%d: %s", response.Code, response.Body.String())
			}
			select {
			case forwarded := <-calls:
				if forwarded["referenceImage"] != inline || forwarded["prompt"] != body["prompt"] {
					t.Fatal("reference or prompt changed")
				}
				if kind == "video" && (forwarded["useSavedKey"] != true || forwarded["resolution"] != "720p" || forwarded["modelId"] != "veo-3.1-lite-generate-preview") {
					t.Fatal("video did not use validated defaults and the saved Desktop key")
				}
			case <-time.After(time.Second):
				t.Fatal("generation did not reach internal Studio handler")
			}
			deadline := time.Now().Add(time.Second)
			for {
				jobs := s.jobList(kind)
				if len(jobs) == 1 && jobs[0]["status"] == "completed" {
					if jobs[0][kind] == nil || len(s.jobList(map[string]string{"image": "video", "video": "image"}[kind])) != 0 {
						t.Fatal("result or job kinds mixed")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("generation did not save its result")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestCompanionStudioRejectsInvalidGenerationBeforeDispatch(t *testing.T) {
	isolateCompanionStudio(t)
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request reached Studio") }))
	for _, body := range []string{`{"prompt":"clip"}`, `{"prompt":"clip","sourceId":"unknown"}`, `{"prompt":"clip","sourceId":"veo-api","aspectRatio":"1:1"}`, `{"prompt":"clip","sourceId":"veo-api","referenceImage":"https://example.test/photo.jpg"}`, `{"prompt":"clip","sourceId":"veo-api","apiKey":"must not cross pairing"}`} {
		response := httptest.NewRecorder()
		s.ServeHTTP(response, companionRequest(s, http.MethodPost, "/videos", body))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid video request status=%d", response.Code)
		}
	}
	if len(s.jobs) != 0 {
		t.Fatal("invalid generation created a job")
	}
	for _, path := range []string{"/images", "/videos", "/videos/sources", "/videos/generations"} {
		r := companionRequest(s, http.MethodPost, path, `{}`)
		r.Header.Del("Authorization")
		response := httptest.NewRecorder()
		s.ServeHTTP(response, r)
		if response.Code != http.StatusUnauthorized {
			t.Fatal("Studio route bypassed pairing")
		}
	}
}

func TestCompanionStudioVideoSourcesExposeAvailabilityWithoutKeys(t *testing.T) {
	isolateCompanionStudio(t)
	previous := studioVideoKeys
	studioVideoKeys = fixtureStudioVideoKeys{"veo-api": "synthetic-provider-secret"}
	t.Cleanup(func() { studioVideoKeys = previous })
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("source listing dispatched generation") }))
	response := httptest.NewRecorder()
	s.ServeHTTP(response, companionRequest(s, http.MethodGet, "/videos/sources", ""))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "synthetic-provider-secret") {
		t.Fatal("source listing failed or exposed its key")
	}
	var catalogue studioVideoCatalogue
	if json.Unmarshal(response.Body.Bytes(), &catalogue) != nil {
		t.Fatal("invalid source catalogue")
	}
	for _, source := range catalogue.Sources {
		if source.ID == "veo-api" && !source.Connected {
			t.Fatal("saved Desktop key was not recognized")
		}
	}
}
