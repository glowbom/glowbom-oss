package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func companionAudioTestSession(t *testing.T) (*companionSession, *int) {
	t.Helper()
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	t.Setenv("GLOWBOM_SERVER_TOKEN", "synthetic-audio-desktop-token")
	calls := 0
	session := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.RemoteAddr != "127.0.0.1:0" || r.Host != "127.0.0.1" || r.Header.Get("Authorization") != "Bearer synthetic-audio-desktop-token" || r.Header.Get("Origin") != "" {
			t.Fatal("audio escaped the authenticated Desktop dispatch")
		}
		switch r.URL.Path {
		case "/studio/audio":
			studioAudioHandler(w, r)
		case "/studio/audio/content":
			if r.URL.RawQuery != "id="+url.QueryEscape(normalizedStudioUUID(r.URL.Query().Get("id"))) {
				t.Fatal("audio content forwarded an unchecked identifier")
			}
			studioAudioContentHandler(w, r)
		default:
			t.Fatal("audio reached an unrelated Desktop route", r.URL.Path)
		}
	}))
	return session, &calls
}

func TestCompanionAudioCatalogListsAllTypesAndPages(t *testing.T) {
	session, _ := companionAudioTestSession(t)
	want := map[string]string{}
	for _, audioType := range []string{"voice", "music", "sound"} {
		asset := studioTestAudioAsset(t, audioType, "")
		want[asset.ID] = audioType
	}
	if _, err := saveStudioAsset(studioSaveOptions{Prompt: "Separate image", MediaType: "image", DataURI: "data:image/png;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for offset := 0; offset < 3; offset++ {
		response := httptest.NewRecorder()
		session.ServeHTTP(response, companionRequest(session, http.MethodGet, fmt.Sprintf("/audio?offset=%d&limit=1", offset), ""))
		var page struct {
			Audio   []studioAudioSummary `json:"audio"`
			HasMore bool                 `json:"hasMore"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Audio) != 1 || page.HasMore != (offset < 2) {
			t.Fatalf("invalid audio page %d: %d %s", offset, response.Code, response.Body.String())
		}
		asset := page.Audio[0]
		if seen[asset.ID] || asset.AudioType != want[asset.ID] || asset.MediaType != "audio" || asset.MimeType != "audio/mpeg" || asset.Prompt == "" || asset.Model == "" || asset.Timestamp == "" {
			t.Fatal("audio page mixed media or lost saved metadata", asset)
		}
		if strings.Contains(response.Body.String(), "dataBase64") || strings.Contains(response.Body.String(), "synthetic-audio-desktop-token") {
			t.Fatal("audio list exposed content or credentials")
		}
		seen[asset.ID] = true
	}
	response := httptest.NewRecorder()
	session.ServeHTTP(response, companionRequest(session, http.MethodGet, "/audio", ""))
	if response.Code != http.StatusOK {
		t.Fatal("default audio page failed", response.Code)
	}
}

func TestCompanionAudioContentPreservesBytesRangesAndHead(t *testing.T) {
	session, _ := companionAudioTestSession(t)
	asset := studioTestAudioAsset(t, "music", "")
	for _, scenario := range []struct {
		method, byteRange, body string
		status                  int
	}{
		{http.MethodGet, "", "ID3saved audio", http.StatusOK},
		{http.MethodGet, "bytes=0-2", "ID3", http.StatusPartialContent},
		{http.MethodHead, "", "", http.StatusOK},
	} {
		request := companionRequest(session, scenario.method, "/audio/"+strings.ToLower(asset.ID)+"/content", "")
		request.Header.Set("Range", scenario.byteRange)
		response := httptest.NewRecorder()
		session.ServeHTTP(response, request)
		if response.Code != scenario.status || response.Body.String() != scenario.body || response.Header().Get("Content-Type") != "audio/mpeg" || response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Location") != "" {
			t.Fatalf("unsafe or changed audio content: %d %s", response.Code, response.Body.String())
		}
		if scenario.byteRange != "" && response.Header().Get("Content-Range") != "bytes 0-2/14" {
			t.Fatal("audio lost range metadata", response.Header().Get("Content-Range"))
		}
	}
}

func TestCompanionAudioPreservesPairingAndQueryBoundaries(t *testing.T) {
	session, calls := companionAudioTestSession(t)
	id := "AAAAAAAA-1111-4222-8333-444444444444"
	for _, scenario := range []struct {
		path   string
		change func(*http.Request)
		status int
	}{
		{"/audio", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"/audio?offset=0&limit=24", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"/audio/" + id + "/content", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }, 401},
		{"/audio", func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.example") }, 403},
		{"/audio", func(r *http.Request) { r.RemoteAddr = "8.8.8.8:50000" }, 403},
		{"/audio", func(r *http.Request) { r.Host = "untrusted.example" }, 403},
		{"/audio?token=secret", nil, 403},
		{"/audio?projectId=" + id, nil, 403},
		{"/audio?offset=0&offset=1", nil, 403},
		{"/audio?limit=0", nil, 403},
		{"/audio?limit=49", nil, 403},
		{"/audio?offset=-1", nil, 403},
		{"/audio?offset=999999999999999999999999", nil, 403},
		{"/audio?offset=", nil, 403},
		{"/audio?url=https://untrusted.example", nil, 403},
		{"/audio/" + id + "/content?offset=0", nil, 403},
		{"/images?offset=0", nil, 403},
		{"/audio/%2e%2e/content", nil, 403},
		{"/audio/not-an-id/content", nil, 404},
		{"/audio/" + id + "/content/other", nil, 404},
	} {
		request := companionRequest(session, http.MethodGet, scenario.path, "")
		if scenario.change != nil {
			scenario.change(request)
		}
		response := httptest.NewRecorder()
		session.ServeHTTP(response, request)
		if response.Code != scenario.status {
			t.Errorf("%s status %d, want %d", scenario.path, response.Code, scenario.status)
		}
	}
	if *calls != 0 {
		t.Fatal("unapproved request reached the Desktop API")
	}
	for _, state := range []string{"expired", "revoked"} {
		if state == "expired" {
			session.expires = time.Now().Add(-time.Second)
		} else {
			session.expires = time.Now().Add(time.Hour)
			session.close()
		}
		for _, path := range []string{"/audio?offset=0", "/audio/" + id + "/content"} {
			response := httptest.NewRecorder()
			session.ServeHTTP(response, companionRequest(session, http.MethodGet, path, ""))
			if response.Code != http.StatusUnauthorized {
				t.Fatal(state, "pairing could read audio")
			}
		}
	}
}

func TestCompanionAudioRejectsOtherMediaAndSymlinkRecords(t *testing.T) {
	session, _ := companionAudioTestSession(t)
	image, err := saveStudioAsset(studioSaveOptions{Prompt: "Separate image", MediaType: "image", DataURI: "data:image/png;base64," + base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))})
	if err != nil {
		t.Fatal(err)
	}
	asset := studioTestAudioAsset(t, "voice", "")
	path, _, data, err := readStudioAudio(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if os.WriteFile(outside, data, 0600) != nil || os.Remove(path) != nil || os.Symlink(outside, path) != nil {
		t.Fatal("could not prepare symlink fixture")
	}
	for _, id := range []string{image.ID, asset.ID, "AAAAAAAA-1111-4222-8333-444444444444"} {
		response := httptest.NewRecorder()
		session.ServeHTTP(response, companionRequest(session, http.MethodGet, "/audio/"+id+"/content", ""))
		if response.Code != http.StatusNotFound {
			t.Fatal("audio exposed other media or external record", response.Code)
		}
	}
}

func TestCompanionProjectCatalogNewestFirstWithoutLimitOrScopeExpansion(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	session := testCompanion(t, http.NotFoundHandler())
	wantNewest := ""
	for i := 0; i < 12; i++ {
		project := sharedCompanionProject(t, session)
		stamp := fmt.Sprintf("2026-10-%02dT10:00:00Z", i+1)
		manifest := &GlowbomProject{Name: fmt.Sprintf("Project %02d", 12-i), CreatedAt: stamp}
		if err := SaveProject(filepath.Join(project.path, "glowbom.json"), manifest); err != nil {
			t.Fatal(err)
		}
		project.Name = manifest.Name
		session.projects[project.ID] = project
		wantNewest = project.ID
	}
	undated := sharedCompanionProject(t, session)
	foreign := sharedCompanionProject(t, testCompanion(t, http.NotFoundHandler()))
	if _, err := registerStudioProject(foreign.path); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	session.ServeHTTP(response, companionRequest(session, http.MethodGet, "/projects", ""))
	var result struct {
		Projects []companionProject `json:"projects"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Projects) != 13 {
		t.Fatal("shared project list was limited", response.Code, response.Body.String())
	}
	if result.Projects[0].ID != wantNewest || result.Projects[12].ID != undated.ID || result.Projects[12].CreatedAt != "" {
		t.Fatal("newest-first ordering or undated legacy fallback failed", result.Projects)
	}
	for _, project := range result.Projects {
		if project.ID == foreign.ID || strings.Contains(response.Body.String(), foreign.path) {
			t.Fatal("catalog discovered an unshared project")
		}
	}
}

func TestCompanionProjectDatesAreNormalizedAndPreservedByRename(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	session := testCompanion(t, http.NotFoundHandler())
	project := sharedCompanionProject(t, session)
	manifest := &GlowbomProject{Name: project.Name, CreatedAt: "2026-10-07T10:00:00.123+03:00"}
	if err := SaveProject(filepath.Join(project.path, "glowbom.json"), manifest); err != nil {
		t.Fatal(err)
	}
	paired, err := companionProjects([]string{project.path})
	if err != nil || paired[project.ID].CreatedAt != "2026-10-07T07:00:00.123Z" {
		t.Fatal("pairing did not expose true normalized creation date", paired, err)
	}
	response := companionSettingsCall(t, session, http.MethodPatch, "/projects/"+project.ID+"/settings", map[string]string{"name": "New name", "expectedName": project.Name})
	var settings struct {
		Project companionProject `json:"project"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &settings) != nil || settings.Project.CreatedAt != paired[project.ID].CreatedAt {
		t.Fatal("rename lost creation date", response.Code, response.Body.String())
	}
	manifest.CreatedAt = "invalid"
	if err := SaveProject(filepath.Join(project.path, "glowbom.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if catalog := session.projectCatalog(); len(catalog) != 1 || catalog[0].CreatedAt != "" {
		t.Fatal("invalid legacy date was treated as creation time", catalog)
	}
}

func TestCompanionProjectMetadataStaysInsideSharedFolder(t *testing.T) {
	session := testCompanion(t, http.NotFoundHandler())
	project := sharedCompanionProject(t, session)
	outside := sharedCompanionProject(t, testCompanion(t, http.NotFoundHandler()))
	if err := SaveProject(filepath.Join(outside.path, "glowbom.json"), &GlowbomProject{Name: "Unshared", CreatedAt: "2026-10-07T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(project.path, "glowbom.json")
	if os.Remove(manifestPath) != nil || os.Symlink(filepath.Join(outside.path, "glowbom.json"), manifestPath) != nil {
		t.Fatal("could not prepare manifest symlink fixture")
	}
	if updated := companionProjectMetadata(project); updated.CreatedAt != "" {
		t.Fatal("metadata escaped through a manifest symlink")
	}
	if os.Remove(manifestPath) != nil || os.Remove(project.path) != nil || os.Symlink(outside.path, project.path) != nil {
		t.Fatal("could not prepare project symlink fixture")
	}
	if updated := companionProjectMetadata(project); updated.CreatedAt != "" {
		t.Fatal("metadata escaped through a changed project folder")
	}
}
