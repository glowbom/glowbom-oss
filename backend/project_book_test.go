package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bookTestProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bookTestWrite(t, dir, "glowbom.json", []byte(`{"name":"Garden","version":"1.0.0","custom":"keep"}`))
	return dir
}
func bookTestWrite(t *testing.T, root, name string, data []byte) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
func bookTestPNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func bookTestCall(t *testing.T, project, action string, values map[string]any) (*httptest.ResponseRecorder, projectBook) {
	t.Helper()
	var request *http.Request
	if action == "" {
		request = httptest.NewRequest("GET", "/project-book?path="+url.QueryEscape(project), nil)
	} else {
		if values == nil {
			values = map[string]any{}
		}
		values["projectPath"], values["action"] = project, action
		data, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		request = httptest.NewRequest("POST", "/project-book", bytes.NewReader(data))
	}
	response := httptest.NewRecorder()
	projectBookHandler(response, request)
	var book projectBook
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &book); err != nil {
			t.Fatal(err)
		}
	}
	return response, book
}
func bookTestBuild(t *testing.T, project string) {
	t.Helper()
	bookTestWrite(t, project, "history/2026-09-27_120000_first/entry.json", []byte(`{"id":"original","timestamp":"2026-09-27T12:00:00Z","instructions":"prepared instructions","request":"Add a garden page","taskType":"refine","status":"completed","outputSummary":"Added the garden page.\nValidation was not recorded.","model":"provider/model","contributor":"OpenCode","changedFiles":["web/page.tsx"],"attachments":[{"id":"input","filename":"drawing.png","mediaType":"image","mimeType":"image/png"}]}`))
	bookTestWrite(t, project, "history/2026-09-27_120000_first/drawing.png", bookTestPNG(t))
}

func TestProjectBookReadsWithoutCreationAndImportsLegacySources(t *testing.T) {
	project := bookTestProject(t)
	response, book := bookTestCall(t, project, "", nil)
	if response.Code != 200 || book.Version != 1 || len(book.Entries) != 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(project, "project-book")); !os.IsNotExist(err) {
		t.Fatal("GET created a Book", err)
	}
	bookTestBuild(t, project)
	bookTestWrite(t, project, ".glowbom/prototypes/version-legacy/request.json", []byte(`{"messages":[{"role":"user","text":"Make a garden"}],"createdAt":"2026-09-26T12:00:00Z"}`))
	bookTestWrite(t, project, ".glowbom/prototypes/version-legacy/result.html", []byte("<!doctype html><html>garden</html>"))
	bookTestWrite(t, project, ".glowbom/prototypes/version-broken/request.json", []byte("{broken"))
	response, book = bookTestCall(t, project, "sync", nil)
	if response.Code != 200 || len(book.Entries) != 2 || len(book.Warnings) == 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	entry := book.Entries[0]
	if entry.Request != "Add a garden page" || entry.Contributor != "OpenCode" || entry.Model != "provider/model" || len(entry.Images) != 1 || len(entry.Sketch.Nodes) != 4 {
		t.Fatalf("build = %+v", entry)
	}
	if !strings.Contains(entry.Body, "garden page") || strings.Contains(entry.Body, "## What we asked for") {
		t.Fatal("the imported story was not written for a reader")
	}
	record, err := os.ReadFile(filepath.Join(project, "history/2026-09-27_120000_first/entry.json"))
	if err != nil || !strings.Contains(string(record), `Validation was not recorded.`) {
		t.Fatal("the original result evidence was lost", err)
	}
	if !entry.CanWriteStory {
		t.Fatal("fallback story cannot be improved with the recorded model")
	}
	legacy := book.Entries[1]
	if legacy.Model != "" || legacy.Contributor != "" {
		t.Fatalf("legacy metadata invented: %+v", legacy)
	}
	response, again := bookTestCall(t, project, "sync", nil)
	if response.Code != 200 || len(again.Entries) != 2 || again.Entries[0].UpdatedAt != entry.UpdatedAt || again.Entries[0].Images[0].ID != entry.Images[0].ID {
		t.Fatal("sync was not idempotent", response.Body.String())
	}
	manifest, _ := os.ReadFile(filepath.Join(project, "glowbom.json"))
	if !bytes.Contains(manifest, []byte(`"custom":"keep"`)) {
		t.Fatal("manifest metadata changed")
	}
}

func TestProjectBookMarkdownAndImageEditsSurviveSync(t *testing.T) {
	project := bookTestProject(t)
	bookTestBuild(t, project)
	response, book := bookTestCall(t, project, "sync", nil)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	entry := book.Entries[0]
	files, err := filepath.Glob(filepath.Join(project, "project-book/history", entry.ID, "*", "entry.md"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if err := os.WriteFile(files[0], []byte("# My corrected title\n\nI changed this with a text editor.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	response, book = bookTestCall(t, project, "sync", nil)
	if response.Code != 200 || book.Entries[0].Title != "My corrected title" || book.Entries[0].Body != "I changed this with a text editor." {
		t.Fatal(response.Body.String())
	}
	response, book = bookTestCall(t, project, "remove-image", map[string]any{"entryId": entry.ID, "imageId": entry.Images[0].ID})
	if response.Code != 200 || len(book.Entries[0].Images) != 0 {
		t.Fatal(response.Body.String())
	}
	response, book = bookTestCall(t, project, "sync", nil)
	if response.Code != 200 || len(book.Entries[0].Images) != 0 {
		t.Fatal("removed input returned", response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(project, "history/2026-09-27_120000_first/drawing.png")); err != nil {
		t.Fatal("source input removed", err)
	}
	data := bookTestPNG(t)
	response, book = bookTestCall(t, project, "add-image", map[string]any{"entryId": entry.ID, "filename": "garden.png", "dataURL": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), "caption": "A new illustration"})
	if response.Code != 200 || len(book.Entries[0].Images) != 1 || book.Entries[0].Images[0].Role != "image" {
		t.Fatal(response.Body.String())
	}
	image := book.Entries[0].Images[0]
	image.Caption = "Revised caption"
	response, book = bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "Owner title", "body": "Owner explanation", "images": []bookImage{image}})
	if response.Code != 200 || !book.Entries[0].Reviewed {
		t.Fatal(response.Body.String())
	}
	response, book = bookTestCall(t, project, "sync", nil)
	if response.Code != 200 || book.Entries[0].Title != "Owner title" || book.Entries[0].Images[0].Caption != "Revised caption" {
		t.Fatal(response.Body.String())
	}
	media := httptest.NewRecorder()
	projectBookMediaHandler(media, httptest.NewRequest("GET", "/project-book/media?path="+url.QueryEscape(project)+"&asset="+url.QueryEscape(image.Path), nil))
	if media.Code != 200 || media.Header().Get("Content-Type") != "image/png" || !bytes.Equal(media.Body.Bytes(), data) {
		t.Fatal(media.Code, media.Body.String())
	}
}

func TestProjectBookRejectsUnsupportedBooksAndEscapingPaths(t *testing.T) {
	t.Run("existing format", func(t *testing.T) {
		project := bookTestProject(t)
		bookTestWrite(t, project, "project-book/README.md", []byte("Owner's existing book"))
		response, _ := bookTestCall(t, project, "sync", nil)
		if response.Code != 409 {
			t.Fatal(response.Code, response.Body.String())
		}
		data, _ := os.ReadFile(filepath.Join(project, "project-book/README.md"))
		if string(data) != "Owner's existing book" {
			t.Fatal("existing Book overwritten")
		}
	})
	t.Run("book symlink", func(t *testing.T) {
		project, outside := bookTestProject(t), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(project, "project-book")); err != nil {
			t.Fatal(err)
		}
		response, _ := bookTestCall(t, project, "sync", nil)
		if response.Code == 200 {
			t.Fatal("accepted linked Book")
		}
		files, _ := os.ReadDir(outside)
		if len(files) != 0 {
			t.Fatal("wrote outside project")
		}
	})
	t.Run("source image symlink", func(t *testing.T) {
		project := bookTestProject(t)
		bookTestBuild(t, project)
		outside := filepath.Join(t.TempDir(), "image.png")
		if err := os.WriteFile(outside, bookTestPNG(t), 0644); err != nil {
			t.Fatal(err)
		}
		input := filepath.Join(project, "history/2026-09-27_120000_first/drawing.png")
		if err := os.Remove(input); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, input); err != nil {
			t.Fatal(err)
		}
		response, book := bookTestCall(t, project, "sync", nil)
		if response.Code != 200 || len(book.Entries[0].Images) != 0 || len(book.Warnings) == 0 {
			t.Fatal(response.Body.String())
		}
		media := httptest.NewRecorder()
		projectBookMediaHandler(media, httptest.NewRequest("GET", "/project-book/media?path="+url.QueryEscape(project)+"&asset=../glowbom.json", nil))
		if media.Code != 400 {
			t.Fatal("media traversal accepted")
		}
	})
}

func TestProjectBookRejectsInvalidEditsWithoutLosingEntry(t *testing.T) {
	project := bookTestProject(t)
	bookTestBuild(t, project)
	_, book := bookTestCall(t, project, "sync", nil)
	entry := book.Entries[0]
	for _, values := range []map[string]any{
		{"entryId": entry.ID, "title": strings.Repeat("x", 241), "body": "body"},
		{"entryId": entry.ID, "title": "title", "body": "body", "images": []bookImage{{ID: "fake", Path: "../../secret", Name: "x", Role: "image"}}},
		{"entryId": "../outside", "title": "title", "body": "body"},
	} {
		response, _ := bookTestCall(t, project, "update-entry", values)
		if response.Code != 400 {
			t.Fatal("invalid edit accepted", response.Body.String())
		}
	}
	response, _ := bookTestCall(t, project, "add-image", map[string]any{"entryId": entry.ID, "filename": "image.png", "dataURL": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("<script>bad</script>"))})
	if response.Code != 400 {
		t.Fatal("disguised image accepted")
	}
	response, book = bookTestCall(t, project, "", nil)
	if response.Code != 200 || book.Entries[0].Title != entry.Title || len(book.Entries[0].Images) != 1 {
		t.Fatal("invalid edit changed saved entry", response.Body.String())
	}
	unfinished := filepath.Join(project, "project-book/history", entry.ID, "r99999999999999999999-unfinished")
	if err := os.Mkdir(unfinished, 0755); err != nil {
		t.Fatal(err)
	}
	response, book = bookTestCall(t, project, "", nil)
	if response.Code != 200 || book.Entries[0].Title != entry.Title {
		t.Fatal("unfinished revision hid prior entry", response.Body.String())
	}
}

func TestProjectBookImportsCommittedPrototypeInputsOnly(t *testing.T) {
	project := bookTestProject(t)
	for _, version := range []string{"pending", "complete"} {
		saved := "false"
		if version == "complete" {
			saved = "true"
		}
		base := ".glowbom/prototypes/version-" + version
		bookTestWrite(t, project, base+"/request.json", []byte(`{"saved":`+saved+`,"model":"test/model","createdAt":"2026-09-27T11:00:00Z","messages":[{"role":"user","text":"Sketch a garden"}],"attachments":[{"filename":"sketch.png","path":"inputs/001-sketch.png","mime":"image/png"}],"inputSketchPath":"input-sketch.json"}`))
		bookTestWrite(t, project, base+"/result.html", []byte("<!doctype html><html>Garden</html>"))
		bookTestWrite(t, project, base+"/inputs/001-sketch.png", bookTestPNG(t))
	}
	response, book := bookTestCall(t, project, "sync", nil)
	if response.Code != 200 || len(book.Entries) != 1 || len(book.Entries[0].Images) != 1 || len(book.Warnings) != 1 || !strings.Contains(book.Entries[0].Source, "complete") {
		t.Fatal(response.Body.String())
	}
}

func TestProjectBookBuildAttributionPersistsCompatibly(t *testing.T) {
	project := bookTestProject(t)
	bookTestWrite(t, project, "current_instructions/instructions.txt", []byte("Prepared instructions"))
	meta := agentHistoryMetadata{Model: "provider/model", RequestedModel: "provider/requested", Provider: "provider", Contributor: "OpenCode", RunID: "run-123", Request: "My original words", ChangedFiles: []string{"web/page.tsx"}}
	if err := persistCurrentInstructionsHistory(project, "Prepared instructions", "completed", "Saved.", meta); err != nil {
		t.Fatal(err)
	}
	entries, err := loadProjectHistoryEntries(project)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	if entries[0].Model != meta.Model || entries[0].RunID != meta.RunID || entries[0].Contributor != meta.Contributor || len(entries[0].ChangedFiles) != 1 {
		t.Fatalf("metadata missing: %+v", entries[0])
	}
	response, book := bookTestCall(t, project, "sync", nil)
	if response.Code != 200 || book.Entries[0].Request != meta.Request || book.Entries[0].RunID != meta.RunID {
		t.Fatal(response.Body.String())
	}
}

func TestProjectBookRejectsStaleEdits(t *testing.T) {
	project := bookTestProject(t)
	bookTestBuild(t, project)
	_, book := bookTestCall(t, project, "sync", nil)
	entry := book.Entries[0]
	response, _ := bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "First window", "body": "Saved first", "expectedUpdatedAt": entry.UpdatedAt})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response, _ = bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "Second window", "body": "Stale text", "expectedUpdatedAt": entry.UpdatedAt})
	if response.Code != 409 {
		t.Fatal("stale text replaced newer edit", response.Code, response.Body.String())
	}
	_, current := bookTestCall(t, project, "", nil)
	if current.Entries[0].Title != "First window" {
		t.Fatal(current.Entries[0].Title)
	}
}

func TestProjectBookProtectsExternalMarkdownEdits(t *testing.T) {
	project := bookTestProject(t)
	bookTestBuild(t, project)
	_, book := bookTestCall(t, project, "sync", nil)
	entry := book.Entries[0]
	_, opened := bookTestCall(t, project, "", nil)
	if entry.ContentVersion == "" || entry.ContentVersion != opened.Entries[0].ContentVersion {
		t.Fatal("content version was unstable after saving")
	}
	files, _ := filepath.Glob(filepath.Join(project, "project-book/history", entry.ID, "*", "entry.md"))
	if err := os.WriteFile(files[0], []byte("# External title\n\nChanged outside Glowbom.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	response, _ := bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "Stale browser title", "body": "Stale text", "expectedUpdatedAt": entry.UpdatedAt, "expectedContentVersion": entry.ContentVersion})
	if response.Code != 409 {
		t.Fatal("external edit overwritten", response.Code, response.Body.String())
	}
	_, current := bookTestCall(t, project, "", nil)
	if current.Entries[0].Title != "External title" || current.Entries[0].ContentVersion == entry.ContentVersion {
		t.Fatal(current.Entries[0])
	}
}

func TestProjectBookCorruptEntryDoesNotHideOtherEntriesOrGetReimported(t *testing.T) {
	project := bookTestProject(t)
	bookTestBuild(t, project)
	bookTestWrite(t, project, ".glowbom/prototypes/version-other/request.json", []byte(`{"messages":[{"role":"user","text":"Other work"}],"createdAt":"2026-09-25T12:00:00Z"}`))
	bookTestWrite(t, project, ".glowbom/prototypes/version-other/result.html", []byte("<!doctype html><html>Other</html>"))
	_, book := bookTestCall(t, project, "sync", nil)
	brokenID := book.Entries[0].ID
	files, _ := filepath.Glob(filepath.Join(project, "project-book/history", brokenID, "*", "entry.md"))
	if err := os.WriteFile(files[0], []byte("Owner is editing this title"), 0644); err != nil {
		t.Fatal(err)
	}
	bookTestWrite(t, project, "project-book/history/.DS_Store", []byte("desktop metadata"))
	response, book := bookTestCall(t, project, "sync", nil)
	if response.Code != 200 || len(book.Entries) != 1 || len(book.Warnings) == 0 {
		t.Fatal(response.Body.String())
	}
	if book.Entries[0].ID == brokenID {
		t.Fatal("reimported over a broken owner edit")
	}
	data, _ := os.ReadFile(files[0])
	if string(data) != "Owner is editing this title" {
		t.Fatal("owner edit overwritten")
	}
}

func TestProjectBookNormalizesNullCollectionsAndRejectsOversizedSnapshots(t *testing.T) {
	project := bookTestProject(t)
	bookTestBuild(t, project)
	_, book := bookTestCall(t, project, "sync", nil)
	entry := book.Entries[0]
	sources, _ := filepath.Glob(filepath.Join(project, "project-book/history", entry.ID, "*", "source.json"))
	data, _ := os.ReadFile(sources[0])
	var source map[string]any
	if err := json.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	source["images"], source["changedFiles"] = nil, nil
	data, _ = json.Marshal(source)
	if err := os.WriteFile(sources[0], data, 0644); err != nil {
		t.Fatal(err)
	}
	sketch := filepath.Join(filepath.Dir(sources[0]), "sketch.json")
	if err := os.WriteFile(sketch, []byte(`{"version":1,"nodes":null,"edges":null}`), 0644); err != nil {
		t.Fatal(err)
	}
	response, book := bookTestCall(t, project, "", nil)
	if response.Code != 200 || len(book.Entries) != 1 || book.Entries[0].Images == nil || book.Entries[0].ChangedFiles == nil || book.Entries[0].Sketch.Nodes == nil || book.Entries[0].Sketch.Edges == nil {
		t.Fatal(response.Body.String())
	}
	root, _, err := openProjectBookProject(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entry.ChangedFiles = make([]string, 2000)
	for i := range entry.ChangedFiles {
		entry.ChangedFiles[i] = strings.Repeat("x", 2000)
	}
	if err := saveBookEntry(root, entry); err == nil {
		t.Fatal("wrote an entry too large to reopen")
	}
	_, found, err := readBookEntry(root, entry.ID)
	if err != nil || !found {
		t.Fatal("oversized write damaged previous revision", err)
	}
}
