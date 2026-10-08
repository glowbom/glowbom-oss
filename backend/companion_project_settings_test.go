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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const companionIconTestID = "11111111-2222-4333-8444-555555555555"

func companionSettingsCall(t *testing.T, s *companionSession, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, method, path, string(data)))
	return w
}

func companionIconTestAPI(t *testing.T, transport projectIconTestTransport) (http.Handler, *atomic.Int32) {
	t.Helper()
	isolateProjectIconCredentials(t)
	t.Setenv(grokSubscriptionMediaFlag, "0")
	t.Setenv("GLOWBOM_CHATGPT_IMAGE_TRANSPORT", "qa-disabled")
	t.Setenv("XAI_API_KEY", "mock-private-key")
	guard := projectIconTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.x.ai" {
			t.Error("unexpected secondary network transport", r.URL.Host)
			return nil, errors.New("mock transport rejected")
		}
		return transport(r)
	})
	mockProjectIconProvider(t, guard)
	previous := http.DefaultTransport
	http.DefaultTransport = guard
	t.Cleanup(func() { http.DefaultTransport = previous })
	catalogCalls := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("/opencode/project/icon/sources", func(w http.ResponseWriter, r *http.Request) {
		catalogCalls.Add(1)
		openCodeIconSourcesHandler(w, r)
	})
	mux.HandleFunc("/opencode/project/icon/generate", openCodeGenerateIconHandler)
	return mux, catalogCalls
}

func waitCompanionIcon(t *testing.T, s *companionSession, p companionProject, id, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w := companionSettingsCall(t, s, "GET", "/projects/"+p.ID+"/icon/generations/"+id, nil)
		var value map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil {
			t.Fatal("missing icon progress", w.Code)
		}
		if value["status"] == status {
			return value
		}
		if value["status"] != "running" {
			t.Fatal("unexpected icon status", value["status"])
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("icon did not reach", status)
	return nil
}

func TestCompanionProjectSettingsRenameIsRootedAndRetrySafe(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	file := filepath.Join(p.path, "glowbom.json")
	before, _ := os.ReadFile(file)
	var manifest map[string]any
	_ = json.Unmarshal(before, &manifest)
	manifest["unknownMetadata"] = map[string]any{"keep": true}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	path := "/projects/" + p.ID + "/settings"
	request := map[string]string{"name": "  New name  ", "expectedName": "Shared"}
	for i := 0; i < 2; i++ {
		w := companionSettingsCall(t, s, "PATCH", path, request)
		if w.Code != 200 || strings.Contains(w.Body.String(), p.path) || strings.Contains(w.Body.String(), "unknownMetadata") {
			t.Fatal("rename failed or leaked manifest", w.Code)
		}
	}
	after, _ := os.ReadFile(file)
	_ = json.Unmarshal(after, &manifest)
	if manifest["name"] != "New name" || manifest["unknownMetadata"] == nil || manifest["targets"] == nil {
		t.Fatal("rename discarded project metadata")
	}
	if _, err := os.Stat(p.path); err != nil {
		t.Fatal("metadata rename moved the project folder")
	}
	if project, _ := s.project(p.ID); project.Name != "New name" {
		t.Fatal("shared project name was not updated")
	}
	w := companionSettingsCall(t, s, "PATCH", path, map[string]string{"name": "A conflicting name", "expectedName": "Shared"})
	if w.Code != 409 {
		t.Fatal("stale rename overwrote newer name", w.Code)
	}
	for _, name := range []string{"", "bad\x00name", strings.Repeat("é", 61)} {
		if w := companionSettingsCall(t, s, "PATCH", path, map[string]string{"name": name, "expectedName": "New name"}); w.Code != 400 {
			t.Fatal("unsafe name accepted", w.Code)
		}
	}
}

func TestCompanionProjectSettingsRejectsForeignScopeSecretsAndSymlinks(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	for _, path := range []string{"/projects/not-shared/settings", "/projects/not-shared/icon/content", "/projects/not-shared/icon/generations"} {
		if w := companionSettingsCall(t, s, "GET", path, nil); w.Code != 404 {
			t.Fatal("unshared project exposed settings", w.Code)
		}
	}
	w := companionSettingsCall(t, s, "PATCH", "/projects/"+p.ID+"/settings", map[string]string{"name": "New", "expectedName": "Shared", "path": t.TempDir()})
	if w.Code != 400 {
		t.Fatal("arbitrary project path accepted")
	}
	r := companionRequest(s, "PATCH", "/projects/"+p.ID+"/settings", `{"name":"New","expectedName":"Shared"}`)
	r.Header.Set("Authorization", "Bearer wrong")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unsigned pairing mutated settings")
	}
	r.Header.Set("Authorization", "Bearer "+s.pairing.Token)
	r.Header.Set("Origin", "https://untrusted.invalid")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("browser origin mutated settings")
	}
	outside := filepath.Join(t.TempDir(), "private.json")
	if err := os.WriteFile(outside, []byte(`{"name":"private secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(p.path, "glowbom.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(p.path, "glowbom.json")); err != nil {
		t.Fatal(err)
	}
	w = companionSettingsCall(t, s, "GET", "/projects/"+p.ID+"/settings", nil)
	if w.Code == 200 || strings.Contains(w.Body.String(), "private secret") {
		t.Fatal("manifest symlink escaped project")
	}
	w = companionSettingsCall(t, s, "PATCH", "/projects/"+p.ID+"/settings", map[string]string{"name": "New", "expectedName": "private secret"})
	if w.Code == 200 {
		t.Fatal("manifest symlink was renamed")
	}
}

func TestCompanionProjectSettingsConcurrentRenamesKeepOneExpectedVersion(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	var group sync.WaitGroup
	codes := make(chan int, 2)
	for _, name := range []string{"One", "Two"} {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			codes <- companionSettingsCall(t, s, "PATCH", "/projects/"+p.ID+"/settings", map[string]string{"name": name, "expectedName": "Shared"}).Code
		}(name)
	}
	group.Wait()
	close(codes)
	accepted, conflicts := 0, 0
	for code := range codes {
		if code == 200 {
			accepted++
		}
		if code == 409 {
			conflicts++
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatal("concurrent rename discarded expected version", accepted, conflicts)
	}
}

func TestCompanionProjectIconGenerationUsesExactProviderAndPrivateReference(t *testing.T) {
	image := projectIconTestImage(t, "png")
	var calls atomic.Int32
	api, catalog := companionIconTestAPI(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer mock-private-key" {
			t.Error("wrong Desktop image credential")
		}
		data, _ := io.ReadAll(r.Body)
		if !bytes.Contains(data, []byte("Recognizable robot")) || !bytes.Contains(data, []byte("data:image/png;base64,")) {
			t.Error("selected reference or prompt missing")
		}
		return iconProviderResponse(image), nil
	})
	s := testCompanion(t, api)
	p := sharedCompanionProject(t, s)
	attachment := uploadCompanionImage(t, s, p, "private-reference.jpg", projectIconTestImage(t, "jpeg"))
	request := companionProjectIconRequest{RequestID: strings.ToUpper(companionIconTestID), Prompt: "Recognizable robot", SourceID: "xai-api", ReferenceID: strings.ToUpper(attachment.ID)}
	path := "/projects/" + p.ID + "/icon/generations"
	w := companionSettingsCall(t, s, "POST", path, request)
	if w.Code != 202 {
		t.Fatal("icon not accepted", w.Code, w.Body.String())
	}
	value := waitCompanionIcon(t, s, p, companionIconTestID, "completed")
	public, _ := json.Marshal(value)
	for _, private := range []string{p.path, "mock-private-key", "data:image", attachment.Filename, attachment.ID, "iconPath"} {
		if bytes.Contains(public, []byte(private)) {
			t.Fatal("icon snapshot exposed private provider data")
		}
	}
	if data, err := os.ReadFile(filepath.Join(p.path, "icon.png")); err != nil || !bytes.Equal(data, image) {
		t.Fatal("core icon save was not reused", err)
	}
	t.Setenv("XAI_API_KEY", "")
	w = companionSettingsCall(t, s, "POST", path, request)
	if w.Code != 200 || calls.Load() != 1 || catalog.Load() != 1 {
		t.Fatal("retry charged or refreshed an accepted generation", w.Code, calls.Load())
	}
	request.Prompt = "Changed icon"
	if w := companionSettingsCall(t, s, "POST", path, request); w.Code != 409 {
		t.Fatal("changed retry replaced request")
	}
	other := sharedCompanionProject(t, s)
	request.Prompt = "Recognizable robot"
	if w := companionSettingsCall(t, s, "POST", "/projects/"+other.ID+"/icon/generations", request); w.Code != 409 {
		t.Fatal("request replayed for another project")
	}
	if w := companionSettingsCall(t, s, "DELETE", "/projects/"+other.ID+"/icon/generations/"+companionIconTestID, nil); w.Code != 404 {
		t.Fatal("foreign project canceled icon")
	}
}

func TestCompanionProjectIconCancellationKeepsPreviousIcon(t *testing.T) {
	entered, released, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	image := projectIconTestImage(t, "png")
	api, _ := companionIconTestAPI(t, func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		close(released)
		return nil, r.Context().Err()
	})
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/generate") {
			defer close(finished)
		}
		api.ServeHTTP(w, r)
	})
	s := testCompanion(t, wrapped)
	p := sharedCompanionProject(t, s)
	if err := os.WriteFile(filepath.Join(p.path, "icon.png"), image, 0600); err != nil {
		t.Fatal(err)
	}
	request := companionProjectIconRequest{RequestID: companionIconTestID, Prompt: "Create new icon", SourceID: "xai-api"}
	data, _ := json.Marshal(request)
	ctx, cancel := context.WithCancel(context.Background())
	r := companionRequest(s, "POST", "/projects/"+p.ID+"/icon/generations", string(data)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 202 {
		cancel()
		t.Fatal("icon not accepted", w.Code)
	}
	cancel()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not outlive initial phone request")
	}
	attachment := uploadCompanionImage(t, s, p, "selected.png", image)
	if w := companionSettingsCall(t, s, "PUT", "/projects/"+p.ID+"/icon/content", map[string]string{"attachmentId": attachment.ID}); w.Code != 409 {
		t.Fatal("selected icon raced with running generation")
	}
	w = companionSettingsCall(t, s, "DELETE", "/projects/"+p.ID+"/icon/generations/"+companionIconTestID, nil)
	if w.Code != 200 {
		t.Fatal("cancellation failed")
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("provider context was not canceled")
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("provider cleanup did not finish")
	}
	value := waitCompanionIcon(t, s, p, companionIconTestID, "canceled")
	if value["error"] != nil {
		t.Fatal("canceled job exposed provider errors")
	}
	if after, _ := os.ReadFile(filepath.Join(p.path, "icon.png")); !bytes.Equal(after, image) {
		t.Fatal("cancellation changed previous icon")
	}
}

func TestCompanionProjectIconFailuresNeverExposeProviderErrorBodies(t *testing.T) {
	image := projectIconTestImage(t, "png")
	api, _ := companionIconTestAPI(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("private-provider-key /private/project data:image/png;base64,private"))}, nil
	})
	s := testCompanion(t, api)
	p := sharedCompanionProject(t, s)
	_ = os.WriteFile(filepath.Join(p.path, "icon.png"), image, 0600)
	w := companionSettingsCall(t, s, "POST", "/projects/"+p.ID+"/icon/generations", companionProjectIconRequest{RequestID: companionIconTestID, Prompt: "New icon", SourceID: "xai-api"})
	if w.Code != 202 {
		t.Fatal("request failed", w.Code)
	}
	value := waitCompanionIcon(t, s, p, companionIconTestID, "failed")
	public, _ := json.Marshal(value)
	if bytes.Contains(public, []byte("private-provider")) || bytes.Contains(public, []byte("/private/project")) || bytes.Contains(public, []byte("data:image")) {
		t.Fatal("provider error leaked into snapshot")
	}
	if after, _ := os.ReadFile(filepath.Join(p.path, "icon.png")); !bytes.Equal(after, image) {
		t.Fatal("failed generation replaced previous icon")
	}
}

func TestCompanionProjectIconSelectionRequiresOwnedUnchangedSquareImage(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	image := projectIconTestImage(t, "jpeg")
	attachment := uploadCompanionImage(t, s, p, "chosen.jpg", image)
	path := "/projects/" + p.ID + "/icon/content"
	for i := 0; i < 2; i++ {
		w := companionSettingsCall(t, s, "PUT", path, map[string]string{"attachmentId": strings.ToUpper(attachment.ID)})
		if w.Code != 200 || strings.Contains(w.Body.String(), p.path) || !strings.Contains(w.Body.String(), `"hasIcon":true`) {
			t.Fatal("selected icon was not saved", w.Code)
		}
	}
	w := companionSettingsCall(t, s, "GET", path, nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("icon content was not safe PNG")
	}
	other := sharedCompanionProject(t, s)
	if w := companionSettingsCall(t, s, "PUT", "/projects/"+other.ID+"/icon/content", map[string]string{"attachmentId": attachment.ID}); w.Code != 410 {
		t.Fatal("foreign attachment accepted")
	}
	rectangle := uploadCompanionImage(t, s, p, "rectangle.png", companionImageFixture(t, "png", 4, 3))
	if w := companionSettingsCall(t, s, "PUT", path, map[string]string{"attachmentId": rectangle.ID}); w.Code != 400 {
		t.Fatal("non-square icon accepted")
	}
	s.mu.Lock()
	stored := s.attachments[attachment.ID]
	s.mu.Unlock()
	if err := os.WriteFile(filepath.Join(p.path, stored.relativePath), projectIconTestImage(t, "png"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := companionSettingsCall(t, s, "PUT", path, map[string]string{"attachmentId": attachment.ID}); w.Code != 410 {
		t.Fatal("tampered attachment accepted")
	}
	if w := companionSettingsCall(t, s, "PUT", path, map[string]string{"attachmentId": rectangle.ID, "apiKey": "private"}); w.Code != 400 {
		t.Fatal("credential accepted in icon request")
	}
}

func TestCompanionProjectIconConcurrentRetriesStartOnlyOneGeneration(t *testing.T) {
	var catalog sync.WaitGroup
	catalog.Add(2)
	releaseCatalog, releaseGeneration, entered, finished := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var generations atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/opencode/project/icon/sources", func(w http.ResponseWriter, r *http.Request) {
		catalog.Done()
		<-releaseCatalog
		writeJSON(w, map[string]any{"sources": []projectIconSource{{ID: "mock-source", Available: true}}})
	})
	mux.HandleFunc("/opencode/project/icon/generate", func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		generations.Add(1)
		close(entered)
		<-releaseGeneration
		writeJSON(w, map[string]any{"success": true, "image": "private image response", "iconPath": "/private/source"})
	})
	s := testCompanion(t, mux)
	p := sharedCompanionProject(t, s)
	request := companionProjectIconRequest{RequestID: companionIconTestID, Prompt: "Mock icon", SourceID: "mock-source"}
	var callers sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			codes <- companionSettingsCall(t, s, "POST", "/projects/"+p.ID+"/icon/generations", request).Code
		}()
	}
	doneCatalog := make(chan struct{})
	go func() { catalog.Wait(); close(doneCatalog) }()
	select {
	case <-doneCatalog:
	case <-time.After(3 * time.Second):
		close(releaseCatalog)
		close(releaseGeneration)
		t.Fatal("concurrent preflight did not start")
	}
	close(releaseCatalog)
	callers.Wait()
	close(codes)
	accepted, replayed := 0, 0
	for code := range codes {
		if code == 202 {
			accepted++
		}
		if code == 200 {
			replayed++
		}
	}
	if accepted != 1 || replayed != 1 {
		close(releaseGeneration)
		t.Fatal("retry did not reuse accepted job", accepted, replayed)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(releaseGeneration)
		t.Fatal("generation did not start")
	}
	close(releaseGeneration)
	<-finished
	value := waitCompanionIcon(t, s, p, companionIconTestID, "completed")
	public, _ := json.Marshal(value)
	if generations.Load() != 1 || bytes.Contains(public, []byte("private image response")) || bytes.Contains(public, []byte("/private/source")) {
		t.Fatal("concurrent retry generated twice or leaked private response")
	}
}

func TestCompanionProjectIconRevocationDuringPreflightStartsNoJob(t *testing.T) {
	entered := make(chan struct{})
	var generations atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/opencode/project/icon/sources", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		writeJSON(w, map[string]any{"sources": []projectIconSource{{ID: "mock-source", Available: true}}})
	})
	mux.HandleFunc("/opencode/project/icon/generate", func(w http.ResponseWriter, r *http.Request) { generations.Add(1) })
	s := testCompanion(t, mux)
	p := sharedCompanionProject(t, s)
	done := make(chan int, 1)
	go func() {
		done <- companionSettingsCall(t, s, "POST", "/projects/"+p.ID+"/icon/generations", companionProjectIconRequest{RequestID: companionIconTestID, Prompt: "Icon", SourceID: "mock-source"}).Code
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		s.cancel()
		t.Fatal("metadata request did not start")
	}
	s.cancel()
	select {
	case code := <-done:
		if code != 409 {
			t.Fatal("revoked preflight accepted", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not cancel preflight")
	}
	s.mu.Lock()
	count := len(s.iconJobs)
	s.mu.Unlock()
	if count != 0 || generations.Load() != 0 {
		t.Fatal("revoked pairing started paid icon work")
	}
}

func TestCompanionProjectIconReferenceMustBelongToTheSharedProject(t *testing.T) {
	var generations atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/opencode/project/icon/sources", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sources": []projectIconSource{{ID: "mock-source", Available: true}}})
	})
	mux.HandleFunc("/opencode/project/icon/generate", func(w http.ResponseWriter, r *http.Request) { generations.Add(1) })
	s := testCompanion(t, mux)
	p, foreign := sharedCompanionProject(t, s), sharedCompanionProject(t, s)
	attachment := uploadCompanionImage(t, s, foreign, "other.png", projectIconTestImage(t, "png"))
	w := companionSettingsCall(t, s, "POST", "/projects/"+p.ID+"/icon/generations", companionProjectIconRequest{RequestID: companionIconTestID, Prompt: "Icon", SourceID: "mock-source", ReferenceID: attachment.ID})
	if w.Code != 400 || generations.Load() != 0 {
		t.Fatal("foreign reference reached provider")
	}
}

func TestCompanionProjectSettingsStoppedOrMovedRootKeepsName(t *testing.T) {
	s := testCompanion(t, http.NotFoundHandler())
	p := sharedCompanionProject(t, s)
	root, err := os.OpenRoot(p.path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := renameCompanionProject(root, "New name", "Shared", ctx); err == nil {
		t.Fatal("canceled rename wrote manifest")
	}
	moved := p.path + "-moved"
	if err := os.Rename(p.path, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := renameCompanionProject(root, "New name", "Shared"); err == nil {
		t.Fatal("moved folder was renamed through a stale path")
	}
	data, err := os.ReadFile(filepath.Join(moved, "glowbom.json"))
	if err != nil || !bytes.Contains(data, []byte(`"name": "Shared"`)) {
		t.Fatal("failed rename changed project metadata")
	}
}
