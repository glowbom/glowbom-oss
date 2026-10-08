package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type companionVideoFixture struct {
	session    *companionSession
	video      studioImageRecord
	image      studioImageRecord
	audio      studioImageRecord
	videoBytes []byte
	imageBytes []byte
	apiCalls   int
	studioRoot string
}

func newCompanionVideoFixture(t *testing.T) *companionVideoFixture {
	t.Helper()
	fixture := &companionVideoFixture{studioRoot: t.TempDir()}
	t.Setenv("GLOWBOM_STUDIO_DIR", fixture.studioRoot)
	t.Setenv("GLOWBOM_SERVER_TOKEN", "synthetic-desktop-token")
	fixture.videoBytes = []byte{
		0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm',
		0, 0, 0, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '2',
		0, 0, 0, 9, 'm', 'd', 'a', 't', 0x7f,
	}
	fixture.imageBytes = projectIconTestImage(t, "png")
	var err error
	fixture.video, err = saveStudioAsset(studioSaveOptions{
		Prompt: "Synthetic video", DataURI: "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(fixture.videoBytes),
		MediaType: "video", Source: "Test fixture", AspectRatio: "16:9", Duration: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.image, err = saveStudioAsset(studioSaveOptions{
		Prompt: "Synthetic still", DataURI: "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixture.imageBytes),
		MediaType: "image", Source: "Test fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.audio, err = saveStudioAsset(studioSaveOptions{
		Prompt: "Synthetic audio", DataURI: "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString([]byte("synthetic audio")),
		MediaType: "audio", Source: "Test fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.session = testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.apiCalls++
		if r.Method != http.MethodGet || r.RemoteAddr != "127.0.0.1:0" || r.Host != "127.0.0.1" ||
			r.Header.Get("Authorization") != "Bearer synthetic-desktop-token" || r.Header.Get("Origin") != "" {
			t.Fatal("companion forwarded an unsafe Studio request")
		}
		switch r.URL.Path {
		case "/studio/videos":
			studioVideosHandler(w, r)
		case "/studio/videos/content":
			if r.URL.RawQuery != "id="+url.QueryEscape(normalizedStudioUUID(r.URL.Query().Get("id"))) {
				t.Fatal("video content forwarded more than a normalized asset identifier")
			}
			studioVideoContentHandler(w, r)
		case "/studio/images":
			studioImagesHandler(w, r)
		case "/studio/images/content":
			studioImageContentHandler(w, r)
		default:
			t.Fatalf("unexpected internal route %q", r.URL.Path)
		}
	}))
	return fixture
}

func TestCompanionVideoContentPreservesOriginalMP4(t *testing.T) {
	fixture := newCompanionVideoFixture(t)
	for _, id := range []string{fixture.video.ID, strings.ToLower(fixture.video.ID)} {
		response := httptest.NewRecorder()
		fixture.session.ServeHTTP(response, companionRequest(fixture.session, http.MethodGet, "/videos/"+id+"/content", ""))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "video/mp4" {
			t.Fatalf("video content status=%d type=%q", response.Code, response.Header().Get("Content-Type"))
		}
		if !bytes.Equal(response.Body.Bytes(), fixture.videoBytes) {
			t.Fatal("companion changed the original MP4 bytes")
		}
		if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Location") != "" {
			t.Fatal("video download lost safe content headers or redirected")
		}
	}
}

func TestCompanionVideoCatalogSeparatesImagesAndVideos(t *testing.T) {
	fixture := newCompanionVideoFixture(t)
	for _, test := range []struct {
		path, key string
		want      studioImageRecord
	}{
		{"/videos", "videos", fixture.video},
		{"/images", "images", fixture.image},
	} {
		response := httptest.NewRecorder()
		fixture.session.ServeHTTP(response, companionRequest(fixture.session, http.MethodGet, test.path, ""))
		var catalog map[string][]studioImageSummary
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &catalog) != nil {
			t.Fatalf("catalog %s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
		if len(catalog) != 1 || len(catalog[test.key]) != 1 || catalog[test.key][0].ID != test.want.ID || catalog[test.key][0].Prompt != test.want.Prompt {
			t.Fatalf("catalog %s mixed media or lost metadata: %+v", test.path, catalog)
		}
		if strings.Contains(response.Body.String(), "dataBase64") || strings.Contains(response.Body.String(), fixture.studioRoot) {
			t.Fatal("catalog exposed media payloads or local paths")
		}
		if test.key == "videos" {
			video := catalog[test.key][0]
			if video.Duration == nil || *video.Duration != 8 || video.Dimensions == nil {
				t.Fatal("video catalog lost playback metadata")
			}
		}
	}
	response := httptest.NewRecorder()
	fixture.session.ServeHTTP(response, companionRequest(fixture.session, http.MethodGet, "/images/"+fixture.image.ID+"/content", ""))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" || !bytes.Equal(response.Body.Bytes(), fixture.imageBytes) {
		t.Fatal("adding video content changed image downloads")
	}
}

func TestCompanionVideoContentRejectsOtherMediaAndUnknownIDs(t *testing.T) {
	fixture := newCompanionVideoFixture(t)
	for _, path := range []string{
		"/videos/" + fixture.image.ID + "/content",
		"/videos/" + fixture.audio.ID + "/content",
		"/images/" + fixture.video.ID + "/content",
		"/videos/AAAAAAAA-0000-4000-8000-000000000000/content",
	} {
		response := httptest.NewRecorder()
		fixture.session.ServeHTTP(response, companionRequest(fixture.session, http.MethodGet, path, ""))
		if response.Code != http.StatusNotFound {
			t.Fatalf("wrong media or unknown asset accepted: %s status=%d", path, response.Code)
		}
	}
}

func TestCompanionVideoPairingSecurity(t *testing.T) {
	fixture := newCompanionVideoFixture(t)
	contentPath := "/videos/" + fixture.video.ID + "/content"
	for _, test := range []struct {
		name, path string
		change     func(*http.Request)
		want       int
	}{
		{"missing catalog token", "/videos", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"missing content token", contentPath, func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"wrong token", contentPath, func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") }, 401},
		{"public peer", contentPath, func(r *http.Request) { r.RemoteAddr = "8.8.8.8:50000" }, 403},
		{"origin", contentPath, func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, 403},
		{"other host", contentPath, func(r *http.Request) { r.Host = "attacker.example" }, 403},
		{"userinfo URL", contentPath, func(r *http.Request) { r.URL.User = url.User("attacker") }, 403},
		{"catalog query", "/videos?token=private", nil, 403},
		{"content query", contentPath + "?id=" + fixture.image.ID, nil, 403},
		{"redirect query", contentPath + "?url=https://attacker.example/video.mp4", nil, 403},
		{"invalid id", "/videos/not-an-id/content", nil, 404},
		{"traversal", "/videos/../content", nil, 404},
		{"encoded traversal", "/videos/%2e%2e/content", nil, 403},
		{"encoded slash", "/videos/" + fixture.video.ID + "%2foutside/content", nil, 403},
		{"encoded external URL", "/videos/https%3a%2f%2fattacker.example/content", nil, 403},
		{"external URL path", "/videos/https://attacker.example/video.mp4/content", nil, 404},
		{"extra path", contentPath + "/outside", nil, 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := companionRequest(fixture.session, http.MethodGet, test.path, "")
			if test.change != nil {
				test.change(request)
			}
			response := httptest.NewRecorder()
			fixture.session.ServeHTTP(response, request)
			if response.Code != test.want || response.Header().Get("Location") != "" {
				t.Fatalf("unsafe video request status=%d want=%d location=%q", response.Code, test.want, response.Header().Get("Location"))
			}
		})
	}
	if fixture.apiCalls != 0 {
		t.Fatal("an unsafe video request reached the Desktop API")
	}
}

func TestCompanionVideoExpiredAndRevokedPairings(t *testing.T) {
	for _, invalidation := range []string{"expired", "revoked"} {
		t.Run(invalidation, func(t *testing.T) {
			fixture := newCompanionVideoFixture(t)
			if invalidation == "expired" {
				fixture.session.expires = time.Now().Add(-time.Second)
			} else {
				fixture.session.close()
			}
			for _, path := range []string{"/videos", "/videos/" + fixture.video.ID + "/content"} {
				response := httptest.NewRecorder()
				fixture.session.ServeHTTP(response, companionRequest(fixture.session, http.MethodGet, path, ""))
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("%s pairing still reads %s: %d", invalidation, path, response.Code)
				}
			}
			if fixture.apiCalls != 0 {
				t.Fatal("invalidated pairing reached Studio")
			}
		})
	}
}

func TestCompanionVideoCatalogAndContentRejectMutations(t *testing.T) {
	fixture := newCompanionVideoFixture(t)
	for _, path := range []string{"/videos", "/videos/" + fixture.video.ID + "/content", "/videos/generate", "/videos/generations"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			if path == "/videos" && method == http.MethodPost {
				continue
			}
			response := httptest.NewRecorder()
			fixture.session.ServeHTTP(response, companionRequest(fixture.session, method, path, `{"prompt":"Do not generate"}`))
			if response.Code != http.StatusNotFound {
				t.Fatalf("unsupported video operation accepted: %s %s status=%d", method, path, response.Code)
			}
		}
	}
	if fixture.apiCalls != 0 || len(fixture.session.jobs) != 0 {
		t.Fatal("a video generation or mutation reached Desktop")
	}
}

func TestCompanionVideoContentReportsStoredFormatAndInvalidPayload(t *testing.T) {
	fixture := newCompanionVideoFixture(t)
	const id = "BA9B8D05-A22B-4F9E-84B4-26B6F789ACB3"
	for _, test := range []struct {
		name, payload, wantType string
		wantStatus              int
		wantBody                []byte
	}{
		{"mislabeled MP4", "data:video/quicktime;base64," + base64.StdEncoding.EncodeToString(fixture.videoBytes), "video/mp4", 200, fixture.videoBytes},
		{"nonmedia declaration", "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte("synthetic payload")), "video/mp4", 200, []byte("synthetic payload")},
		{"invalid base64", "invalid-base64!", "", 500, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := json.Marshal(studioImageRecord{ID: id, MediaType: "video", DataBase64: test.payload})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fixture.studioRoot, "Assets", id+".json"), record, 0600); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			fixture.session.ServeHTTP(response, companionRequest(fixture.session, http.MethodGet, "/videos/"+id+"/content", ""))
			if response.Code != test.wantStatus {
				t.Fatalf("stored video status=%d want=%d", response.Code, test.wantStatus)
			}
			if test.wantStatus == http.StatusOK && (response.Header().Get("Content-Type") != test.wantType || !bytes.Equal(response.Body.Bytes(), test.wantBody)) {
				t.Fatalf("stored video format or bytes changed: type=%q", response.Header().Get("Content-Type"))
			}
		})
	}
}
