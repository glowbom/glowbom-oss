package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBookVisualParsesUsefulDrawingAndRejectsUnsafeMarks(t *testing.T) {
	valid := `{"description":"A song queue beside the player.","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.8,"y":0.8},"color":"#111111","width":0.002},{"kind":"text","text":"Up next","x":0.55,"y":0.2,"width":0.2,"fontSize":0.025,"color":"#111111"}]}`
	visual, err := parseBookVisual(valid, "example/model")
	if err != nil || visual.Document.Version != 1 || visual.Document.Width != 1000 || len(visual.Document.Annotations) != 2 || visual.Model != "example/model" {
		t.Fatalf("valid drawing rejected: %+v, %v", visual, err)
	}
	for _, value := range []string{
		strings.Replace(valid, `"x":0.1`, `"x":1.5`, 1),
		strings.Replace(valid, `"rectangle"`, `"script"`, 1),
		strings.Replace(valid, `"description":"A song queue beside the player."`, `"description":""`, 1),
		valid + `{"extra":true}`,
		`{"description":"Only words.","annotations":[{"kind":"text","text":"Title","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"},{"kind":"text","text":"More words","x":0.2,"y":0.3,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`,
	} {
		if _, err := parseBookVisual(value, "example/model"); err == nil {
			t.Fatal("invalid drawing accepted", value)
		}
	}
}

func TestCompletedSessionModelUsesActualAssistantModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/build-session/message" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`[{"info":{"role":"user","model":{"providerID":"fireworks","modelID":"other"}}},{"info":{"role":"assistant","providerID":"xai","modelID":"grok-4.7"}}]`))
	}))
	defer server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	model, err := completedSessionModel(context.Background(), t.TempDir(), "build-session")
	if err != nil || model != "xai/grok-4.7" {
		t.Fatalf("completed model = %q, %v", model, err)
	}
}

func TestBookVisualPersistsAlongsideRunOutlineAndStory(t *testing.T) {
	project := bookTestProject(t)
	bookTestBuild(t, project)
	_, book := bookTestCall(t, project, "sync", nil)
	entry := book.Entries[0]
	visual, err := parseBookVisual(`{"description":"A garden screen.","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.8,"y":0.8},"color":"#111111"},{"kind":"text","text":"Garden","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`, "example/model")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	entry.Sketch.Visual = visual
	if err := saveBookEntry(root, entry); err != nil {
		t.Fatal(err)
	}
	root.Close()
	_, loaded := bookTestCall(t, project, "", nil)
	if len(loaded.Entries) != 1 || loaded.Entries[0].Sketch.Visual == nil || loaded.Entries[0].Sketch.Visual.Description != "A garden screen." || loaded.Entries[0].Title != entry.Title || len(loaded.Entries[0].Sketch.Nodes) == 0 {
		t.Fatalf("visual did not preserve the story and outline: %+v", loaded.Entries)
	}
	_, again := bookTestCall(t, project, "sync", nil)
	if again.Entries[0].Sketch.Visual == nil {
		t.Fatal("sync replaced a saved visual drawing")
	}
	revisionFolder := filepath.Join(project, "project-book/history", entry.ID)
	revisions, err := os.ReadDir(revisionFolder)
	if err != nil || len(revisions) < 2 {
		t.Fatal("drawing did not create a new Book revision", err)
	}
	for _, revision := range revisions {
		data, err := os.ReadFile(filepath.Join(revisionFolder, revision.Name(), "sketch.json"))
		if err != nil {
			t.Fatal(err)
		}
		var sketch bookSketch
		if json.Unmarshal(data, &sketch) != nil {
			t.Fatal("revision sketch is not portable JSON")
		}
	}
}

func TestBookVisualDrawsFromTheExactSavedResultWithoutTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
	bookTestCall(t, project, "sync", nil)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			w.Write([]byte(`{"connected":["fireworks","provider"],"all":[{"id":"fireworks","name":"Fireworks","models":{"other":{"name":"Other","status":"active"}}},{"id":"provider","name":"Provider","models":{"model":{"name":"Model","status":"active","variants":{"low":{"reasoningEffort":"low"}}}}}]}`))
		case "/session":
			w.Write([]byte(`{"id":"sketch-session","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`))
		case "/experimental/tool/ids":
			w.Write([]byte(`[]`))
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
		case "/session/sketch-session/message":
			calls.Add(1)
			var body struct {
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
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Tools["*"] || body.Variant != "low" || !strings.Contains(body.System, "wireframe") || !strings.Contains(body.Parts[0].Text, "Plant") || body.Model.ProviderID != "provider" || body.Model.ModelID != "model" {
				t.Error("drawing was not grounded in saved source or tools were enabled")
			}
			w.Write([]byte(`{"parts":[{"type":"text","text":"{\"description\":\"A garden screen with a Plant button.\",\"annotations\":[{\"kind\":\"shape\",\"shape\":\"rectangle\",\"start\":{\"x\":0.1,\"y\":0.1},\"end\":{\"x\":0.8,\"y\":0.8},\"color\":\"#111111\"},{\"kind\":\"text\",\"text\":\"Plant\",\"x\":0.2,\"y\":0.2,\"width\":0.3,\"fontSize\":0.03,\"color\":\"#111111\"}]}"}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	t.Setenv("OPENCODE_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source := "history/2026-09-27_120000_first/entry.json"
	if _, changed, err := drawProjectBookVisual(ctx, project, source, "", "cursor/model"); err == nil || changed || calls.Load() != 0 {
		t.Fatal("Cursor result was silently sent to an unrelated provider")
	}
	if _, changed, err := drawProjectBookVisual(ctx, project, source, "", "fireworks/missing"); err == nil || changed || calls.Load() != 0 {
		t.Fatal("An unavailable explicit model was silently replaced")
	}
	book, changed, err := drawProjectBookVisual(ctx, project, source, "", "", "drawing")
	if err != nil || !changed || len(book.Entries) != 1 || book.Entries[0].Sketch.Visual == nil || book.Entries[0].Sketch.Visual.Description != "A garden screen with a Plant button." || book.Entries[0].Sketch.Visual.Model != "provider/model" {
		t.Fatalf("result sketch was not saved: changed=%t err=%v book=%+v", changed, err, book)
	}
	_, changed, err = drawProjectBookVisual(ctx, project, source, "", "", "drawing")
	if err != nil || changed || calls.Load() != 1 {
		t.Fatalf("same run was drawn more than once: changed=%t err=%v calls=%d", changed, err, calls.Load())
	}
}

func TestBookModelPreparationReturnsWhenCancelled(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- prepareBookModel(ctx, func() error { close(started); <-release; return nil })
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation was not preserved", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Book request remained blocked in setup")
	}
	called := false
	if err := prepareBookModel(ctx, func() error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("an already cancelled request started setup", err)
	}
}

func TestBookModelPreparationHonorsTheRequestDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := prepareBookModel(ctx, func() error { <-release; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("setup ignored the request deadline", err)
	}
	if options := bookCompletionOptions(chatModel{ID: "provider/plain"}); options.Variant != "" {
		t.Fatal("unsupported effort variant was sent", options)
	}
}

func TestBookVisualModelTimeoutPreservesTheSavedResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	_, imported := bookTestCall(t, project, "sync", nil)
	entry := imported.Entries[0]
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			w.Write([]byte(`{"connected":["provider"],"all":[{"id":"provider","models":{"model":{"name":"Model"}}}]}`))
		case "/session":
			w.Write([]byte(`{"id":"timeout-session","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`))
		case "/experimental/tool/ids":
			w.Write([]byte(`[]`))
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
		case "/session/timeout-session/message":
			io.Copy(io.Discard, r.Body)
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer func() { close(release); server.Close() }()
	t.Setenv("OPENCODE_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, changed, err := drawProjectBookVisual(ctx, project, entry.Source, "", "")
	if changed || !errors.Is(err, context.DeadlineExceeded) || bookGenerationHTTPStatus(err) != http.StatusGatewayTimeout || !strings.Contains(err.Error(), "took too long") {
		t.Fatalf("timeout did not report accurately: changed=%t err=%v", changed, err)
	}
	select {
	case <-started:
	default:
		t.Fatal("model completion was never reached")
	}
	_, current := bookTestCall(t, project, "", nil)
	if current.Entries[0].ContentVersion != entry.ContentVersion || current.Entries[0].Sketch.Visual != nil {
		t.Fatal("timeout changed the saved Book entry")
	}
}
