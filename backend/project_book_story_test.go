package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBookStoryParsesBoundedArticle(t *testing.T) {
	story := parseBookStory("```json\n" + `{"story":{"title":"  You can now sign in  ","body":"A sign-in screen now lets people enter an email and password.\n\nThe form was built into the saved browser prototype so a visitor can begin from a familiar entry point."}}` + "\n```")
	if story == nil || story.Title != "You can now sign in" || !strings.Contains(story.Body, "browser prototype") {
		t.Fatalf("valid article rejected: %+v", story)
	}
	for _, text := range []string{
		`{"story":{"title":"","body":"A long enough body that still lacks a readable headline for this result."}}`,
		`{"story":{"title":"Headline","body":"Too short."}}`,
		`{"story":{"title":"Headline","body":"<script>bad</script> and many more words to satisfy the length check for this article."}}`,
	} {
		if parseBookStory(text) != nil {
			t.Fatal("unsafe or incomplete story accepted", text)
		}
	}
}

func TestBookStoryActionRequiresUntouchedCompletedRun(t *testing.T) {
	base := bookEntry{Title: "Work on a garden page", Body: "An honest fallback story.", Status: "completed", Model: "provider/model", Contributor: "OpenCode"}
	base.AutoStoryHash = bookStoryHash(base.Title, base.Body)
	if !bookAutoStoryEligible(base) {
		t.Fatal("untouched completed story was not eligible")
	}
	for _, model := range []string{"", "auto", "cursor/model", "claude-code/sonnet", "opencode/big-pickle"} {
		entry := base
		entry.Model = model
		if !bookAutoStoryEligible(entry) {
			t.Fatal("build model prevented independently written Book story", model)
		}
	}

	for name, mutate := range map[string]func(*bookEntry){
		"failed run":   func(entry *bookEntry) { entry.Status = "failed" },
		"owner edit":   func(entry *bookEntry) { entry.Body = "An owner's revised story." },
		"owner review": func(entry *bookEntry) { entry.Reviewed = true },
		"written story": func(entry *bookEntry) {
			entry.AutoStoryHash = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			entry := base
			mutate(&entry)
			if bookAutoStoryEligible(entry) {
				t.Fatal("write story was offered for an ineligible entry")
			}
		})
	}
}

func TestProjectBookUpgradesOnlyUntouchedLegacyProse(t *testing.T) {
	project := bookTestProject(t)
	bookTestBuild(t, project)
	root, _, err := openProjectBookProject(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeProjectBook(root); err != nil {
		t.Fatal(err)
	}
	item, warning := importBookBuild(root, "history/2026-09-27_120000_first")
	if warning != "" {
		t.Fatal(warning)
	}
	legacy := item.entry
	legacy.Title, legacy.Body = item.legacyTitle, item.legacyBody
	if err := saveBookEntry(root, legacy); err != nil {
		t.Fatal(err)
	}
	root.Close()
	_, book := bookTestCall(t, project, "sync", nil)
	if book.Entries[0].Title != item.entry.Title || book.Entries[0].Body != item.entry.Body || !book.Entries[0].CanWriteStory {
		t.Fatalf("untouched legacy entry was not upgraded: %+v", book.Entries[0])
	}
	entry := book.Entries[0]
	files, err := filepath.Glob(filepath.Join(project, "project-book/history", entry.ID, "*", "entry.md"))
	if err != nil || len(files) != 2 {
		t.Fatal("expected the old and new revisions", err, files)
	}
	latest := files[0]
	if files[1] > files[0] {
		latest = files[1]
	}
	if err := os.WriteFile(latest, []byte("# Owner headline\n\nAn owner edited the story outside Glowbom.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, again := bookTestCall(t, project, "sync", nil)
	if again.Entries[0].Title != "Owner headline" || again.Entries[0].CanWriteStory {
		t.Fatal("sync replaced an owner-edited story")
	}
}

func bookStoryModelServer(t *testing.T, answer *string, calls *atomic.Int32, expectedPrompt ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			w.Write([]byte(`{"connected":["provider"],"all":[{"id":"provider","name":"Provider","models":{"model":{"name":"Model","status":"active","variants":{"low":{"reasoningEffort":"low"}}}}}]}`))
		case "/session":
			w.Write([]byte(`{"id":"book-session","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`))
		case "/experimental/tool/ids":
			w.Write([]byte(`[]`))
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
		case "/session/book-session/message":
			calls.Add(1)
			var request struct {
				System  string          `json:"system"`
				Variant string          `json:"variant"`
				Tools   map[string]bool `json:"tools"`
				Model   struct {
					ProviderID string `json:"providerID"`
					ModelID    string `json:"modelID"`
				} `json:"model"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.Tools["*"] || request.Variant != "low" || request.Model.ProviderID != "provider" || request.Model.ModelID != "model" || len(request.Parts) == 0 || !strings.Contains(request.Parts[0].Text, "Added the garden page") || !strings.Contains(request.Parts[0].Text, "Plant") {
				t.Error("story or sketch request used the wrong model, tools, or evidence")
			}
			for _, expected := range expectedPrompt {
				if !strings.Contains(request.System, expected) {
					t.Errorf("model prompt missing %q", expected)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"parts": []map[string]string{{"type": "text", "text": *answer}}})
		default:
			w.Write([]byte(`{}`))
		}
	}))
}

func TestProjectBookStoryAndSketchPersistIndependently(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
	_, imported := bookTestCall(t, project, "sync", nil)
	entry := imported.Entries[0]
	answer := `{"story":{"title":"A garden page for the project","body":"The project now has a garden page with a Plant button. The saved page gives visitors a place to begin the garden experience.\n\nThe change lives in the web view, with a simple heading and action that can be refined in a later run."},"description":"Invalid sketch","annotations":[]}`
	var calls atomic.Int32
	server := bookStoryModelServer(t, &answer, &calls)
	defer server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	book, changed, err := drawProjectBookVisual(ctx, project, entry.Source, "", "")
	if err == nil || !changed || book.Entries[0].Title != "A garden page for the project" || book.Entries[0].Sketch.Visual != nil || calls.Load() != 2 {
		t.Fatalf("valid story did not survive bad sketch: changed=%t err=%v entry=%+v", changed, err, book.Entries[0])
	}
	answer = `{"story":{"title":"An unrelated rewrite","body":"This body is long enough to pass the article parser but must not replace the first story. It should be ignored because the first story was already saved in its own revision."},"description":"A garden page.","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.8,"y":0.8},"color":"#111111"},{"kind":"text","text":"Plant","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`
	book, changed, err = drawProjectBookVisual(ctx, project, entry.Source, "", "")
	if err != nil || !changed || book.Entries[0].Sketch.Visual == nil || book.Entries[0].Title != "A garden page for the project" || calls.Load() != 3 {
		t.Fatalf("sketch retry replaced story: changed=%t err=%v calls=%d entry=%+v", changed, err, calls.Load(), book.Entries[0])
	}
	root, _, err := openProjectBookProject(project)
	if err != nil {
		t.Fatal(err)
	}
	input := bookVisualInput(root, book.Entries[0])
	root.Close()
	if strings.Contains(input, "garden experience") || !strings.Contains(input, "Added the garden page") {
		t.Fatal("the saved article replaced original evidence in sketch input")
	}
}

func TestProjectBookWritesSelectedLegacyStoryAndKeepsOwnerEdit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
	_, imported := bookTestCall(t, project, "sync", nil)
	entry := imported.Entries[0]
	visual, err := parseBookVisual(`{"description":"Garden page","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.8,"y":0.8},"color":"#111111"},{"kind":"text","text":"Plant","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`, "provider/model")
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := openProjectBookProject(project)
	if err != nil {
		t.Fatal(err)
	}
	entry.Sketch.Visual = visual
	if err := saveBookEntry(root, entry); err != nil {
		t.Fatal(err)
	}
	root.Close()
	answer := `{"story":{"title":"A garden page takes shape","body":"The project now includes a garden page with a visible Plant button. This gives visitors a clear starting point for the garden experience.\n\nThe page was built in the web view and saved with this run, ready for the owner to inspect and refine."}}`
	var calls atomic.Int32
	server := bookStoryModelServer(t, &answer, &calls)
	defer server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	requestBody, _ := json.Marshal(map[string]string{"projectPath": project, "entryId": entry.ID})
	response := httptest.NewRecorder()
	projectBookStoryHandler(response, httptest.NewRequest("POST", "/project-book/story", bytes.NewReader(requestBody)))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var written projectBook
	if json.Unmarshal(response.Body.Bytes(), &written) != nil || written.Entries[0].Title != "A garden page takes shape" || written.Entries[0].CanWriteStory || written.Entries[0].Sketch.Visual == nil || len(written.Entries[0].Sketch.Nodes) == 0 || calls.Load() != 1 {
		t.Fatal("selected story was not saved", response.Body.String())
	}
	response = httptest.NewRecorder()
	projectBookStoryHandler(response, httptest.NewRequest("POST", "/project-book/story", bytes.NewReader(requestBody)))
	if response.Code == http.StatusOK || calls.Load() != 1 {
		t.Fatal("model was asked to overwrite an existing story")
	}
	_, current := bookTestCall(t, project, "", nil)
	if current.Entries[0].Title != "A garden page takes shape" {
		t.Fatal("the second write changed the saved story")
	}
	bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "Owner's title", "body": "Owner's words"})
	response = httptest.NewRecorder()
	projectBookStoryHandler(response, httptest.NewRequest("POST", "/project-book/story", bytes.NewReader(requestBody)))
	if response.Code == http.StatusOK || calls.Load() != 1 {
		t.Fatal("owner edit was not protected")
	}
}
