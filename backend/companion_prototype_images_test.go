package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func companionImageProgressJob(root string) *companionDesktopPrototypeJob {
	return &companionDesktopPrototypeJob{
		status: "running", started: time.Now(), projectPath: root,
		project: &companionProject{ID: companionProjectID(root), Name: "Images", path: root},
		request: companionDesktopPrototypeRequest{Images: &companionDesktopPrototypeImages{SourceID: "picsum"}},
	}
}

func TestCompanionPrototypeImagesAppearBeforeNextImageCompletes(t *testing.T) {
	isolateProjectIconCredentials(t)
	document := `<!doctype html><html><img src="glowbomimages:Harbor"><img src="glowbomimages:Mountains"></html>`
	root, record := chatImagesFixture(t, document)
	job := companionImageProgressJob(root)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx = context.WithValue(ctx, companionPrototypeImageContextKey{}, job)
	data := projectIconTestImage(t, "png")
	secondStarted, release := make(chan struct{}), make(chan struct{})
	calls := 0
	mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			close(secondStarted)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
	})
	done := make(chan error, 1)
	go func() {
		_, warnings, err := materializeChatImages(ctx, root, record, document, chatImageOptions{SourceID: "picsum"}, func(map[string]any) {})
		if err == nil && len(warnings) != 0 {
			err = errors.New("image materialization reported warnings")
		}
		done <- err
	}()
	select {
	case <-secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("second image never started")
	}
	images, ok := job.snapshot(time.Now())["generatedImages"].([]companionPrototypeImage)
	if !ok || len(images) != 1 || images[0].Prompt != "Harbor" || images[0].Timestamp == "" || images[0].SourceService != "Lorem Picsum" {
		t.Fatal("first saved image was not exposed while the next was running", images)
	}
	session := testCompanion(t, http.HandlerFunc(studioImageContentHandler))
	response := httptest.NewRecorder()
	session.ServeHTTP(response, companionRequest(session, http.MethodGet, "/images/"+images[0].ID+"/content", ""))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" || !bytes.Equal(response.Body.Bytes(), data) {
		t.Fatal("progress image could not load through the existing paired image route", response.Code)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	images = job.snapshot(time.Now())["generatedImages"].([]companionPrototypeImage)
	if len(images) != 2 || images[0].ID == images[1].ID || images[1].Prompt != "Mountains" {
		t.Fatal("saved progress images were missing or duplicated", images)
	}
	job.mu.Lock()
	job.status = "completed"
	job.mu.Unlock()
	if len(job.snapshot(time.Now())["generatedImages"].([]companionPrototypeImage)) != 2 {
		t.Fatal("completion discarded generated images")
	}
}

func TestCompanionPrototypeImagesDoNotPublishFailedSaves(t *testing.T) {
	for _, failure := range []string{"provider", "studio"} {
		t.Run(failure, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			document := `<!doctype html><html><img src="glowbomimages:Harbor"></html>`
			root, record := chatImagesFixture(t, document)
			job := companionImageProgressJob(root)
			ctx := context.WithValue(context.Background(), companionPrototypeImageContextKey{}, job)
			data := projectIconTestImage(t, "png")
			mockProjectIconProvider(t, func(*http.Request) (*http.Response, error) {
				if failure == "provider" {
					return nil, errors.New("synthetic provider failure")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
			})
			if failure == "studio" {
				if err := os.WriteFile(filepath.Join(os.Getenv("GLOWBOM_STUDIO_DIR"), "Assets"), []byte("unavailable Studio directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, warnings, err := materializeChatImages(ctx, root, record, document, chatImageOptions{SourceID: "picsum"}, func(map[string]any) {})
			if err != nil || len(warnings) != 1 || job.snapshot(time.Now())["generatedImages"] != nil {
				t.Fatal("failed image save was published", warnings, err)
			}
		})
	}
}

func TestCompanionPrototypeImagesStayBoundedAndBoundToJob(t *testing.T) {
	job := companionImageProgressJob("/shared/project")
	ctx := context.WithValue(context.Background(), companionPrototypeImageContextKey{}, job)
	asset := studioImageRecord{ID: randomUUIDString(), Timestamp: "2026-10-07T08:00:00Z", MediaType: "image",
		Prompt: "Harbor api_key=private-test-key\n<img src=\"data:image/png;base64,private-pixels\">", SourceService: "Test source", DataBase64: "private-bytes"}
	for _, target := range []string{"/unshared/project", ""} {
		recordCompanionPrototypeImage(ctx, target, asset)
	}
	recordCompanionPrototypeImage(context.Background(), job.projectPath, asset)
	for _, media := range []string{"audio", "video"} {
		other := asset
		other.MediaType = media
		recordCompanionPrototypeImage(ctx, job.projectPath, other)
	}
	writer := &companionDesktopPrototypeWriter{job: job}
	forged, _ := json.Marshal(map[string]any{"imageReady": asset, "generatedImages": []studioImageRecord{asset}})
	writer.event(forged)
	if job.snapshot(time.Now())["generatedImages"] != nil {
		t.Fatal("unrelated data or stream fields published a generated image")
	}
	recordCompanionPrototypeImage(ctx, job.projectPath, asset)
	recordCompanionPrototypeImage(ctx, job.projectPath, asset)
	for i := 0; i < 8; i++ {
		asset.ID = randomUUIDString()
		recordCompanionPrototypeImage(ctx, job.projectPath, asset)
	}
	images := job.snapshot(time.Now())["generatedImages"].([]companionPrototypeImage)
	encoded, _ := json.Marshal(images)
	if len(images) != maxChatImages || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "data:image") || strings.Contains(string(encoded), "dataBase64") {
		t.Fatal("generated image metadata exceeded its bound or exposed payload", string(encoded))
	}
	job.mu.Lock()
	job.generatedImages = nil
	job.status = "canceled"
	job.mu.Unlock()
	recordCompanionPrototypeImage(ctx, job.projectPath, asset)
	if job.snapshot(time.Now())["generatedImages"] != nil {
		t.Fatal("late image changed canceled progress")
	}
	job.mu.Lock()
	job.status = "running"
	job.mu.Unlock()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	recordCompanionPrototypeImage(canceled, job.projectPath, asset)
	if job.snapshot(time.Now())["generatedImages"] != nil {
		t.Fatal("canceled request published an image")
	}
}
