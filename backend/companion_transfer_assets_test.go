package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const companionImageProjectID = "50D3FAF1-B123-4C12-8CCA-1CA49862CD43"
const companionImageInputID = "F6C26979-7C1D-4910-B3F4-8FD9A67E224C"
const companionImageResultID = "78D9F9D3-0D88-4E2F-8ECF-456B891E13C0"

const companionPhoneImageCSP = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'none'; media-src 'none'; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'none'">`

func companionProjectImageFixture(t *testing.T) companionPrototypeImport {
	t.Helper()
	input := companionImageFixture(t, "png", 30, 20)
	result := companionImageFixture(t, "jpeg", 40, 30)
	html := `<!doctype html><html><head>` + companionPhoneImageCSP + `<style>body{font:18px sans-serif;padding:24px}img{width:160px;border:1px solid #aaa}</style></head><body><h1>Phone image transfer</h1><p>Original input and generated result</p><img id="sketch" src="assets/` + companionImageInputID + `.png"><img id="result" src="assets/` + companionImageResultID + `.jpg"><img id="embedded" src="data:image/jpeg;base64,` + base64.StdEncoding.EncodeToString(result) + `"></body></html>`
	snapshot, _ := json.Marshal(map[string]any{"id": companionImageProjectID, "referenceAssetIDs": []string{companionImageInputID}, "generatedAssetIDs": []string{companionImageResultID}, "canvasWidth": 390, "canvasHeight": 844, "aiModel": "local-phone-model", "generations": []any{map[string]any{"kind": "sketch", "inputAssetIDs": []string{companionImageInputID}}}})
	return companionPrototypeImport{RequestID: companionTransferTestID, Name: "Phone picture fixture", Prompt: "Create a clock from my sketch", HTML: html, OriginalHTML: `<!doctype html><html><body><img src="glowbyimage:Clock art"></body></html>`, SourceProjectID: companionImageProjectID, SourceSnapshot: string(snapshot), Assets: []companionProjectImportAsset{{ID: companionImageInputID, Filename: strings.ToLower(companionImageInputID) + ".png", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(input), Role: "input", Prompt: "Original sketch", SourceType: "uploaded", SourceService: "Glowbom iPhone"}, {ID: companionImageResultID, Filename: strings.ToLower(companionImageResultID) + ".jpg", MimeType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(result), Role: "hero", Prompt: "Clock art", SourceType: "generated", SourceService: "Phone image model", SourceAssetID: companionImageInputID, SourceProjectID: companionImageProjectID}}}
}

func TestCompanionTransferSuppliedOriginalsAreLinkedToStudioAndBook(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("transfer called a model", r.URL.Path) }))
	request := companionProjectImageFixture(t)
	w := companionTransferCall(t, s, "/projects/import", request)
	var response struct {
		companionProject
		AssetsSaved        bool   `json:"assetsSaved"`
		SuppliedAssetCount int    `json:"suppliedAssetCount"`
		InputCount         int    `json:"inputCount"`
		SourceProjectID    string `json:"sourceProjectId"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &response) != nil || !response.AssetsSaved || response.SuppliedAssetCount != 2 || response.InputCount != 1 || response.AssetCount != 2 || response.SourceProjectID != request.SourceProjectID {
		t.Fatal("unconfirmed project images", w.Code, w.Body.String())
	}
	shared, _ := s.project(response.ID)
	fields, _, err := readStudioProjectState(shared.path)
	if err != nil {
		t.Fatal(err)
	}
	projectID := studioJSONString(fields, "projectID")
	var mapping map[string]string
	if json.Unmarshal(fields["phoneAssetIds"], &mapping) != nil || len(mapping) != 2 {
		t.Fatal("source identities lost", mapping)
	}
	for _, asset := range request.Assets {
		original, _ := base64.StdEncoding.DecodeString(asset.DataBase64)
		copied, err := os.ReadFile(filepath.Join(shared.path, "prototype/assets", asset.Filename))
		if err != nil || sha256.Sum256(copied) != sha256.Sum256(original) {
			t.Fatal("prototype changed original", asset.ID, err)
		}
		studio, err := readStudioAssetForLink(mapping[asset.ID])
		data, _ := base64.StdEncoding.DecodeString(studio.DataBase64)
		if err != nil || !bytes.Equal(data, original) || !studioHasProject(studio.UsedInProjects, projectID) || studio.SourceProjectID != projectID || studio.SourceService != asset.SourceService || studio.SourceAssetID != asset.SourceAssetID {
			t.Fatal("Studio lost original bytes or provenance", asset.ID, err, studio)
		}
		if companionImportInputRole(asset.Role) {
			copied, err = os.ReadFile(filepath.Join(shared.path, "inputs", asset.Filename))
			if err != nil || !bytes.Equal(copied, original) {
				t.Fatal("original input missing", err)
			}
		}
	}
	for filename, expected := range map[string]string{"saved-preview.html": request.HTML, "source-project.json": request.SourceSnapshot, "original.html": request.OriginalHTML} {
		data, err := os.ReadFile(filepath.Join(shared.path, ".glowbom/phone-import", filename))
		if err != nil || string(data) != expected {
			t.Fatal("source archive changed", filename, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(shared.path, "prototype/index.html"))
	if !bytes.Contains(data, []byte("img-src data: 'self';")) || !bytes.Contains(data, []byte("connect-src 'none';")) || !bytes.Contains(data, []byte("script-src 'unsafe-inline';")) || bytes.Contains(data, []byte("data:image")) || !bytes.Contains(data, []byte("assets/"+request.Assets[0].Filename)) {
		t.Fatal("image references/CSP broken", string(data))
	}
	w = httptest.NewRecorder()
	studioAssetsHandler(w, httptest.NewRequest("GET", "/studio/assets?projectId="+projectID, nil))
	var catalog struct {
		Images []studioImageSummary `json:"images"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil || len(catalog.Images) != 2 {
		t.Fatal("Desktop Studio still empty", w.Code, w.Body.String())
	}
	bookRoot, name, err := openProjectBookProject(shared.path)
	if err != nil {
		t.Fatal(err)
	}
	book, _, err := readProjectBook(bookRoot, name)
	bookRoot.Close()
	if err != nil || len(book.Entries) != 1 || len(book.Entries[0].Images) != 1 || book.Entries[0].Images[0].Role != "input" {
		t.Fatal("Book lost original sketch", err, book)
	}
	next := testCompanion(t, http.NotFoundHandler())
	w = companionTransferCall(t, next, "/projects/import", request)
	if w.Code != 200 {
		t.Fatal("re-pair receipt not recovered", w.Code, w.Body.String())
	}
	images, _, _, _, _ := listStudioProjectCatalogPage(projectID, 0, 48, 0, 24)
	if len(images) != 2 {
		t.Fatal("retry duplicated images", len(images))
	}
	request.Assets[0].Prompt = "Changed review"
	if w = companionTransferCall(t, next, "/projects/import", request); w.Code != 409 {
		t.Fatal("changed originals reused request", w.Code)
	}
	request.Assets[0].Prompt = "Original sketch"
	assetPath, _, _, err := studioAssetRaw(mapping[companionImageInputID])
	if err != nil {
		t.Fatal(err)
	}
	if os.Remove(assetPath) != nil {
		t.Fatal("could not remove fixture original")
	}
	if w = companionTransferCall(t, next, "/projects/import", request); w.Code != 409 {
		t.Fatal("deleted original silently recreated", w.Code)
	}
}

func TestCompanionTransferOldClientInlineImagesAreIndexed(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	image := companionImageFixture(t, "jpeg", 12, 10)
	request := companionPrototypeImport{RequestID: companionTransferTestID, Name: "Old phone", Prompt: "An image", HTML: `<!doctype html><html><head>` + companionPhoneImageCSP + `</head><body><img src="data:image/jpeg;base64,` + base64.StdEncoding.EncodeToString(image) + `"></body></html>`}
	w := companionTransferCall(t, s, "/projects/import", request)
	var project companionProject
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &project) != nil || project.AssetCount != 1 {
		t.Fatal("old phone lost inline Studio image", w.Code, w.Body.String())
	}
	shared, _ := s.project(project.ID)
	registered, err := registerStudioProject(shared.path)
	if err != nil {
		t.Fatal(err)
	}
	images, _, _, _, err := listStudioProjectCatalogPage(registered.ID, 0, 48, 0, 24)
	if err != nil || len(images) != 1 {
		t.Fatal("inline asset missing from project", err, len(images))
	}
	asset, err := readStudioAssetForLink(images[0].ID)
	raw, _ := base64.StdEncoding.DecodeString(asset.DataBase64)
	if err != nil || !bytes.Equal(raw, image) {
		t.Fatal("old inline bytes changed", err)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, "GET", "/projects/import", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"projectAssets":true`) {
		t.Fatal("missing upload capability", w.Code, w.Body.String())
	}
}

func companionLegacyPhoneFixture(t *testing.T, studio string, receipt bool) (string, string, []string) {
	t.Helper()
	canonical, _ := filepath.EvalSymlinks(studio)
	path := filepath.Join(canonical, "PhoneProjects", strings.ToLower(companionTransferTestID))
	if os.MkdirAll(filepath.Join(path, "prototype/assets"), 0700) != nil {
		t.Fatal("fixture mkdir")
	}
	manifest := &GlowbomProject{Name: "Earlier phone import", Version: "1", Description: "Three generated pictures", Targets: map[string]Target{}}
	if err := SaveProject(filepath.Join(path, "glowbom.json"), manifest); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	html := `<!doctype html><html><head>` + companionPhoneImageCSP + `</head><body><h1>User's saved prototype</h1>`
	for i := 0; i < 3; i++ {
		data := companionImageFixture(t, "jpeg", 12+i, 10+i)
		name := "phone-inline-" + chatSourceHash(string(data))[:24] + ".jpg"
		if os.WriteFile(filepath.Join(path, "prototype/assets", name), data, 0600) != nil {
			t.Fatal("fixture write")
		}
		names = append(names, name)
		html += `<img src="assets/` + name + `">`
	}
	html += "</body></html>"
	if os.WriteFile(filepath.Join(path, "prototype/index.html"), []byte(html), 0600) != nil {
		t.Fatal("fixture HTML")
	}
	registered, err := registerStudioProject(path)
	if err != nil {
		t.Fatal(err)
	}
	if receipt {
		directory, err := companionTransferDirectory()
		if err != nil {
			t.Fatal(err)
		}
		record := companionTransferRecord{Kind: "project", Hash: "original-fixture", Project: &companionProject{ID: companionProjectID(path), Name: manifest.Name, Available: true}, ProjectPath: path}
		if err = writeCompanionTransfer(directory, companionTransferTestID, record); err != nil {
			t.Fatal(err)
		}
	}
	return path, registered.ID, names
}

func TestCompanionTransferLegacyStudioRefreshRepairsOnlyVerifiedImport(t *testing.T) {
	studio := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	path, projectID, names := companionLegacyPhoneFixture(t, studio, true)
	before, _ := os.ReadFile(filepath.Join(path, "prototype/index.html"))
	list, err := listStudioProjects()
	if err != nil || len(list) != 1 || list[0].AssetCount != 3 {
		t.Fatal("earlier imported project still says No images", err, list)
	}
	images, _, _, _, err := listStudioProjectCatalogPage(projectID, 0, 48, 0, 24)
	if err != nil || len(images) != 3 {
		t.Fatal("scoped Studio filter empty", err, len(images))
	}
	for _, image := range images {
		record, err := readStudioAssetForLink(image.ID)
		if err != nil || !studioHasProject(record.UsedInProjects, projectID) {
			t.Fatal("legacy image association missing", err)
		}
	}
	html, _ := os.ReadFile(filepath.Join(path, "prototype/index.html"))
	backup, _ := os.ReadFile(filepath.Join(path, ".glowbom/phone-import/legacy-preview.html"))
	if !bytes.Equal(before, backup) || !bytes.Contains(html, []byte("img-src data: 'self';")) || !bytes.Contains(html, []byte("User's saved prototype")) {
		t.Fatal("repair lost original or edits")
	}
	for i := 0; i < 3; i++ {
		list, err = listStudioProjects()
		if err != nil || list[0].AssetCount != 3 {
			t.Fatal("repair duplicated originals", err, list)
		}
	}
	imagePath, _, _, err := studioAssetRaw(images[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if os.Remove(imagePath) != nil {
		t.Fatal("fixture deletion")
	}
	list, err = listStudioProjects()
	if err != nil || list[0].AssetCount != 2 {
		t.Fatal("repair recreated deliberately deleted media", err, list)
	}
	original, err := os.ReadFile(filepath.Join(path, "prototype/assets", names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, nil) {
		t.Fatal("missing original")
	}
}

func TestCompanionTransferLegacyRepairRejectsUnprovenOrChangedMedia(t *testing.T) {
	for _, scenario := range []string{"missing receipt", "nil project", "wrong project identity", "changed bytes", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			studio := t.TempDir()
			t.Setenv("GLOWBOM_STUDIO_DIR", studio)
			path, projectID, names := companionLegacyPhoneFixture(t, studio, scenario != "missing receipt")
			receiptPath := filepath.Join(studio, "PhoneImports", strings.ToLower(companionTransferTestID)+".json")
			if scenario == "nil project" || scenario == "wrong project identity" {
				_, data, _ := readStudioJSON(receiptPath, 32<<10)
				var record companionTransferRecord
				_ = json.Unmarshal(data, &record)
				if scenario == "nil project" {
					record.Project = nil
				} else {
					record.Project.ID = "wrong"
				}
				data, _ = json.Marshal(record)
				if os.WriteFile(receiptPath, data, 0600) != nil {
					t.Fatal("fixture record change")
				}
			}
			if scenario == "changed bytes" {
				data := companionImageFixture(t, "jpeg", 20, 20)
				if os.WriteFile(filepath.Join(path, "prototype/assets", names[0]), data, 0600) != nil {
					t.Fatal("fixture media change")
				}
			}
			if scenario == "symlink" {
				outside := filepath.Join(t.TempDir(), "outside.jpg")
				if os.WriteFile(outside, companionImageFixture(t, "jpeg", 12, 10), 0600) != nil {
					t.Fatal("fixture outside write")
				}
				file := filepath.Join(path, "prototype/assets", names[0])
				if os.Remove(file) != nil || os.Symlink(outside, file) != nil {
					t.Fatal("fixture symlink")
				}
			}
			before, _ := os.ReadFile(filepath.Join(path, "prototype/index.html"))
			_, _ = listStudioProjects()
			images, _, _, _, err := listStudioProjectCatalogPage(projectID, 0, 48, 0, 24)
			if err != nil || len(images) != 0 {
				t.Fatal("unproven/changed media registered", err, len(images))
			}
			after, _ := os.ReadFile(filepath.Join(path, "prototype/index.html"))
			if !bytes.Equal(before, after) {
				t.Fatal("unsafe repair changed prototype")
			}
		})
	}
}

func TestCompanionTransferAssetsValidateBeforeProjectCreation(t *testing.T) {
	studio := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", studio)
	s := testCompanion(t, http.NotFoundHandler())
	for _, change := range []func(*companionPrototypeImport){func(r *companionPrototypeImport) { r.Assets = r.Assets[:1] }, func(r *companionPrototypeImport) { r.Assets[0].Filename = "../../input.png" }, func(r *companionPrototypeImport) {
		r.HTML = strings.Replace(r.HTML, "</body>", `<img src="assets/missing.png"></body>`, 1)
	}, func(r *companionPrototypeImport) {
		r.HTML = strings.Replace(r.HTML, "</body>", `<img src="blob:temporary"></body>`, 1)
	}, func(r *companionPrototypeImport) {
		r.Assets[1].ID = strings.ToLower(r.Assets[0].ID)
	}} {
		request := companionProjectImageFixture(t)
		request.RequestID = randomUUIDString()
		change(&request)
		w := companionTransferCall(t, s, "/projects/import", request)
		if w.Code != 400 {
			t.Fatal("missing original accepted", w.Code, w.Body.String())
		}
	}
	files, _ := os.ReadDir(filepath.Join(studio, "PhoneProjects"))
	if len(files) != 0 {
		t.Fatal("incomplete media created project", len(files))
	}
	if s.transfersActive != 0 || s.transferReservedBytes != 0 {
		t.Fatal("rejected media held transfer capacity")
	}
}

func TestCompanionTransferStudioRefreshHasNoTransferLockDeadlock(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, http.NotFoundHandler())
	var workers sync.WaitGroup
	workers.Add(2)
	errors := make(chan error, 2)
	go func() {
		defer workers.Done()
		for i := 0; i < 5; i++ {
			if _, err := listStudioProjects(); err != nil {
				errors <- err
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 3; i++ {
			request := companionProjectImageFixture(t)
			request.RequestID = randomUUIDString()
			w := companionTransferCall(t, s, "/projects/import", request)
			if w.Code != 201 {
				errors <- &companionTransferFixtureError{w.Body.String()}
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Studio refresh and phone import deadlocked")
	}
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
}

type companionTransferFixtureError struct{ message string }

func (e *companionTransferFixtureError) Error() string { return e.message }

func TestCompanionTransferImageBrowserFixture(t *testing.T) {
	if os.Getenv("GLOWBOM_PHONE_IMAGE_FIXTURE") != "1" {
		t.Skip("optional browser fixture")
	}
	directory := "/private/tmp/glowbom-phone-image-transfer-fixture"
	if os.MkdirAll(directory, 0700) != nil {
		t.Fatal("fixture root unavailable")
	}
	t.Setenv("GLOWBOM_STUDIO_DIR", directory)
	s := testCompanion(t, http.NotFoundHandler())
	request := companionProjectImageFixture(t)
	request.RequestID = randomUUIDString()
	w := companionTransferCall(t, s, "/projects/import", request)
	var project companionProject
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &project) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	shared, _ := s.project(project.ID)
	t.Log("Synthetic prototype:", filepath.Join(shared.path, "prototype"))
}
