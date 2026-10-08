package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const companionTransferTestID = "C8B57D59-3D53-47A3-8292-8663B99A090A"

func companionTransferCall(t *testing.T, s *companionSession, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "POST", path, string(body)))
	return w
}

func TestCompanionTransferPrototypePortableAndIdempotentAcrossPairing(t *testing.T) {
	directory := t.TempDir()
	directory, _ = filepath.EvalSymlinks(directory)
	t.Setenv("GLOWBOM_STUDIO_DIR", directory)
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("import invoked a model or Build", r.URL.Path) }))
	request := companionPrototypeImport{RequestID: companionTransferTestID, Name: "../../A phone idea", Prompt: "A simple clock", HTML: "<!doctype html><html><body>Phone prototype</body></html>"}
	w := companionTransferCall(t, s, "/projects/import", request)
	var project companionProject
	if w.Code != 201 || w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &project) != nil || project.ID == "" {
		t.Fatal("unsafe prototype import", w.Code, w.Body.String())
	}
	shared, ok := s.project(project.ID)
	if !ok || !isPathWithin(directory, shared.path) {
		t.Fatal("imported project is not in managed storage", shared)
	}
	var receipt struct {
		SavedPath string `json:"savedPath"`
	}
	if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.SavedPath != shared.path {
		t.Fatal("save location missing from receipt", w.Body.String())
	}
	html, err := os.ReadFile(filepath.Join(shared.path, "prototype/index.html"))
	if err != nil || string(html) != request.HTML {
		t.Fatal("prototype not portable", err, string(html))
	}
	manifest, err := LoadProject(filepath.Join(shared.path, "glowbom.json"))
	if err != nil || manifest.Name != request.Name || manifest.Description != request.Prompt {
		t.Fatal("lost original intent", err, manifest)
	}
	if project.CreatedAt == "" || project.CreatedAt != companionProjectDate(manifest.CreatedAt) {
		t.Fatal("import receipt lost the saved creation date")
	}
	bookRoot, name, err := openProjectBookProject(shared.path)
	if err != nil {
		t.Fatal(err)
	}
	book, exists, err := readProjectBook(bookRoot, name)
	bookRoot.Close()
	if err != nil || !exists || len(book.Entries) != 1 || book.Entries[0].Request != request.Prompt {
		t.Fatal("imported prototype did not assemble factual Book", err, book)
	}
	next := testCompanion(t, http.NotFoundHandler())
	w = companionTransferCall(t, next, "/projects/import", request)
	var recovered companionProject
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &recovered) != nil || recovered.ID != project.ID {
		t.Fatal("re-pair retry duplicated import", w.Code, w.Body.String())
	}
	if recovered.CreatedAt != project.CreatedAt {
		t.Fatal("retry changed the project creation date")
	}
	request.HTML = "<!doctype html><html>Different review</html>"
	w = companionTransferCall(t, next, "/projects/import", request)
	if w.Code != 409 {
		t.Fatal("changed request reused receipt", w.Code, w.Body.String())
	}
	folders, err := os.ReadDir(filepath.Join(directory, "PhoneProjects"))
	if err != nil || len(folders) != 1 {
		t.Fatal("retry created extra project", err, len(folders))
	}
}

func TestCompanionTransferExtractsEmbeddedImagesWithoutDroppingOriginals(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	image := companionImageFixture(t, "jpeg", 4, 3)
	uri := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(image)
	html := "<!doctype html><html><body>" + strings.Repeat(`<img src="`+uri+`">`, 4000) + "</body></html>"
	if len(html) <= maxChatResultBytes {
		t.Fatal("fixture must exceed ordinary model output limit")
	}
	w := companionTransferCall(t, s, "/projects/import", companionPrototypeImport{RequestID: companionTransferTestID, Name: "Photos", Prompt: "Keep generated images", HTML: html})
	var project companionProject
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &project) != nil {
		t.Fatal("large prototype import", w.Code, w.Body.String())
	}
	shared, _ := s.project(project.ID)
	saved, err := os.ReadFile(filepath.Join(shared.path, "prototype/index.html"))
	if err != nil || bytes.Contains(saved, []byte("data:image/jpeg")) || !bytes.Contains(saved, []byte("assets/phone-inline-")) {
		t.Fatal("embedded image references not preserved", err)
	}
	assets, err := os.ReadDir(filepath.Join(shared.path, "prototype/assets"))
	if err != nil || len(assets) != 1 {
		t.Fatal("identical images not deduplicated", err, len(assets))
	}
	original, err := os.ReadFile(filepath.Join(shared.path, "prototype/assets", assets[0].Name()))
	if err != nil || !bytes.Equal(original, image) {
		t.Fatal("original generated image changed", err)
	}
}

func TestCompanionTransferOriginalStudioMediaAndSharedProjectLinks(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", directory)
	s := testCompanion(t, http.NotFoundHandler())
	project := sharedCompanionProject(t, s)
	image := companionImageFixture(t, "jpeg", 4, 3)
	request := companionMediaImport{RequestID: companionTransferTestID, Filename: "original.jpg", MimeType: "image/jpeg", Prompt: "Phone reference", DataBase64: base64.StdEncoding.EncodeToString(image)}
	path := "/projects/" + project.ID + "/studio/import"
	w := companionTransferCall(t, s, path, request)
	var result companionMediaImported
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ProjectID != project.ID || result.Filename != request.Filename || result.ByteCount != int64(len(image)) || strings.Contains(w.Body.String(), directory) {
		t.Fatal("invalid Studio import", w.Code, w.Body.String())
	}
	asset, err := readStudioAssetForLink(result.ID)
	if err != nil || asset.DataBase64 != request.DataBase64 || asset.SourceProjectID == "" || asset.Dimensions.Width != 4 {
		t.Fatal("original Studio bytes or project ownership lost", err, asset)
	}
	copy, err := os.ReadFile(filepath.Join(project.path, "prototype/assets/phone-studio-"+strings.ToLower(asset.ID)+".jpg"))
	if err != nil || !bytes.Equal(copy, image) {
		t.Fatal("project original changed", err)
	}
	next := testCompanion(t, http.NotFoundHandler())
	next.projects[project.ID] = project
	w = companionTransferCall(t, next, path, request)
	var retry companionMediaImported
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &retry) != nil || retry.ID != result.ID {
		t.Fatal("media receipt did not survive re-pair", w.Code, w.Body.String())
	}
	files, err := os.ReadDir(filepath.Join(directory, "Assets"))
	if err != nil || len(files) != 1 {
		t.Fatal("retry duplicated media", err, len(files))
	}
	request.Prompt = "Changed review"
	if w = companionTransferCall(t, next, path, request); w.Code != 409 {
		t.Fatal("modified media request reused ID", w.Code)
	}
	request.Prompt = "Phone reference"
	assetPath, _, _, err := studioAssetRaw(result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(assetPath); err != nil {
		t.Fatal(err)
	}
	if w = companionTransferCall(t, next, path, request); w.Code != http.StatusGone {
		t.Fatal("deleted original reported a successful receipt or was duplicated", w.Code)
	}
}

func companionTransferVideoFixture(brand string) []byte {
	box := func(kind string, payload []byte) []byte {
		result := make([]byte, 8+len(payload))
		binary.BigEndian.PutUint32(result, uint32(len(result)))
		copy(result[4:8], kind)
		copy(result[8:], payload)
		return result
	}
	result := box("ftyp", []byte(brand+"\x00\x00\x00\x00"+brand))
	result = append(result, box("moov", []byte("fixture"))...)
	return append(result, box("mdat", []byte("fixture-video"))...)
}

func TestCompanionTransferVideoValidationAndRequestBoundary(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	for _, test := range []struct{ brand, mime, filename string }{{"isom", "video/mp4", "clip.mp4"}, {"qt  ", "video/quicktime", "clip.mov"}} {
		data := companionTransferVideoFixture(test.brand)
		request := companionMediaImport{RequestID: randomUUIDString(), Filename: test.filename, MimeType: test.mime, DataBase64: base64.StdEncoding.EncodeToString(data)}
		w := companionTransferCall(t, s, "/studio/import", request)
		var result companionMediaImported
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.MediaType != "video" || result.MimeType != test.mime || result.ProjectID != "" {
			t.Fatal("valid video rejected", w.Code, w.Body.String())
		}
		request.RequestID = randomUUIDString()
		request.DataBase64 = base64.StdEncoding.EncodeToString(data[:len(data)-1])
		if w = companionTransferCall(t, s, "/studio/import", request); w.Code != 400 {
			t.Fatal("truncated video accepted", w.Code)
		}
	}
	for _, payload := range []any{
		map[string]any{"requestId": companionTransferTestID, "name": "Project", "prompt": "Idea", "html": "<!doctype html><html></html>", "projectPath": "/private"},
		map[string]any{"requestId": "../escape", "name": "Project", "prompt": "Idea", "html": "<!doctype html><html></html>"},
	} {
		if w := companionTransferCall(t, s, "/projects/import", payload); w.Code != 400 {
			t.Fatal("unsafe project import accepted", w.Code)
		}
	}
	r := companionRequest(s, "POST", "/projects/import", `{}`)
	r.Header.Del("Authorization")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unpaired project transfer accepted", w.Code)
	}
}

func TestCompanionTransferConcurrentProjectReaders(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	var readers sync.WaitGroup
	readers.Add(1)
	stop := make(chan struct{})
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, companionRequest(s, "GET", "/projects", ""))
			if w.Code != 200 {
				t.Error("project list failed during transfer", w.Code)
			}
			_ = s.buildModelList(false)
			_, _ = s.project("not-shared")
		}
	}()
	for index := 0; index < 3; index++ {
		w := companionTransferCall(t, s, "/projects/import", companionPrototypeImport{RequestID: randomUUIDString(), Name: "Concurrent phone project", Prompt: "An idea", HTML: "<!doctype html><html>Complete</html>"})
		if w.Code != 201 {
			t.Error("project transfer failed", w.Code, w.Body.String())
		}
	}
	close(stop)
	readers.Wait()
}
