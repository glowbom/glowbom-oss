package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBookModelSelectionPrecedenceAndRestrictions(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/build"}, {ID: "codex/writer"}, {ID: "codex/chat"}})
	service := &chatService{prepare: func() error { return errors.New("OpenCode unavailable") }}
	for _, test := range []struct{ name, build, override, saved, fallback, want string }{
		{"automatic build", "codex/build", "", "", "codex/chat", "codex/build"},
		{"automatic disconnected build", "codex/gone", "", "", "codex/chat", "codex/chat"},
		{"automatic OpenCode unavailable", "provider/build", "", "", "codex/chat", "codex/chat"},
		{"Cursor", "cursor/default", "", "", "codex/chat", "codex/chat"},
		{"Claude Code", "claude-code/sonnet", "", "", "codex/chat", "codex/chat"},
		{"BigPickle", "opencode/big-pickle", "", "", "codex/chat", "codex/chat"},
		{"saved selection", "codex/build", "", "codex/writer", "codex/chat", "codex/writer"},
		{"override", "codex/build", "codex/chat", "codex/writer", "", "codex/chat"},
		{"saved unavailable", "codex/build", "", "codex/gone", "codex/chat", ""},
		{"override unavailable", "codex/build", "codex/gone", "codex/writer", "codex/chat", ""},
		{"unsupported explicit", "codex/build", "opencode/big-pickle", "", "codex/chat", ""},
		{"no implicit connected fallback", "cursor/default", "", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, err := resolveBookModel(context.Background(), service, test.build, test.override, bookWritingPreferences{Model: test.saved, FallbackModel: test.fallback})
			if (err != nil) != (test.want == "") || model.ID != test.want {
				t.Fatalf("got %q, %v; want %q", model.ID, err, test.want)
			}
		})
	}
}

func TestBookModelCatalogFiltersUnsupportedAndDisconnectedModels(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/writer", Name: "Writer"}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"connected":["opencode","provider","apple-intelligence","glowbom-ollama","cursor","claude-code"],"all":[{"id":"opencode","models":{"big-pickle":{},"demo-free":{},"paid":{}}},{"id":"provider","models":{"text":{"modalities":{"output":["text"]},"variants":{"low":{}}},"no-tools":{"tool_call":false},"image":{"modalities":{"output":["image"]}},"old":{"status":"deprecated"}}},{"id":"offline","models":{"model":{}}},{"id":"apple-intelligence","models":{"apple-foundationmodel":{}}},{"id":"glowbom-ollama","models":{"maternion/mimo-v2.6:9b":{}}},{"id":"cursor","models":{"default":{}}},{"id":"claude-code","models":{"sonnet":{}}}]}`))
	}))
	defer server.Close()
	service := &chatService{serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
	models, err := bookConnectedModels(context.Background(), service)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	if !reflect.DeepEqual(ids, []string{"codex/writer", "opencode/paid", "provider/no-tools", "provider/text"}) {
		t.Fatal("unexpected Book catalog", ids)
	}
	if !models[len(models)-1].LowEffort {
		t.Fatal("lost low effort capability")
	}
	service.prepare = func() error { return errors.New("OpenCode unavailable") }
	models, err = bookConnectedModels(context.Background(), service)
	if err != nil || len(models) != 1 || models[0].ID != "codex/writer" {
		t.Fatal("OpenCode failure hid Codex", models, err)
	}
}

func TestBookModelCatalogKeepsAvailableRuntimeWhenOtherTimesOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, slowRuntime := range []string{"opencode", "codex"} {
		t.Run(slowRuntime, func(t *testing.T) {
			withCodexModels(t, nil)
			codexLoadChatModels = func(ctx context.Context) ([]chatModel, error) {
				if slowRuntime == "codex" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return []chatModel{{ID: "codex/writer"}}, nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if slowRuntime == "opencode" {
					<-r.Context().Done()
					return
				}
				w.Write([]byte(`{"connected":["provider"],"all":[{"id":"provider","models":{"writer":{}}}]}`))
			}))
			defer server.Close()
			service := &chatService{serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			models, err := bookConnectedModels(ctx, service)
			want := "codex/writer"
			if slowRuntime == "codex" {
				want = "provider/writer"
			}
			if err != nil || len(models) != 1 || models[0].ID != want {
				t.Fatal("timed-out runtime hid the available writer", models, err)
			}
		})
	}
}

func TestBookFallbackPatchUsesDesktopOriginAndTokenProtection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GLOWBOM_DESKTOP", "1")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	t.Setenv("GLOWBOM_PORT", "4587")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "book-patch-test-token")
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://127.0.0.1:4587")
	web := t.TempDir()
	bookTestWrite(t, web, "index.html", []byte("test"))
	t.Setenv("GLOWBOM_WEB_DIR", web)
	mux := http.NewServeMux()
	mux.HandleFunc("/settings/project-book", projectBookWritingSettingsHandler)
	handler, err := desktopHandler(mux)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, origin, token string
		status              int
	}{
		{"/api/settings/project-book", "http://127.0.0.1:4587", "book-patch-test-token", http.StatusOK},
		{"/settings/project-book", "http://127.0.0.1:4587", "book-patch-test-token", http.StatusOK},
		{"/api/settings/project-book", "http://127.0.0.1:4587", "", http.StatusUnauthorized},
		{"/api/settings/project-book", "https://untrusted.example", "book-patch-test-token", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPatch, "http://127.0.0.1:4587"+test.path, strings.NewReader(`{"fallbackModel":"codex/chat"}`))
		request.Header.Set("Origin", test.origin)
		request.Header.Set("Authorization", "Bearer "+test.token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatal(test.path, test.origin, response.Code, response.Body.String())
		}
	}
}

func TestBookModelSettingsPreserveStyleOnChatFallbackPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := bookWritingPreferences{Style: "custom", CustomPrompt: "Warm and brief.", Model: "codex/writer", FallbackModel: "provider/old"}
	data, _ := json.Marshal(want)
	response := httptest.NewRecorder()
	projectBookWritingSettingsHandler(response, httptest.NewRequest("POST", "/settings/project-book", bytes.NewReader(data)))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	projectBookWritingSettingsHandler(response, httptest.NewRequest("PATCH", "/settings/project-book", strings.NewReader(`{"fallbackModel":"codex/chat"}`)))
	want.FallbackModel = "codex/chat"
	if response.Code != 200 || readBookWritingPreferences() != want {
		t.Fatal("fallback patch changed preferences", response.Code, readBookWritingPreferences())
	}
	response = httptest.NewRecorder()
	projectBookWritingSettingsHandler(response, httptest.NewRequest("POST", "/settings/project-book", strings.NewReader(`{"style":"epic","profanity":false,"customPrompt":"","model":"codex/writer"}`)))
	want.Style, want.CustomPrompt = "epic", ""
	if response.Code != 200 || readBookWritingPreferences() != want {
		t.Fatal("saving settings from an older view cleared the latest Chat fallback", readBookWritingPreferences())
	}
	for _, body := range []string{`{}`, `{"fallbackModel":"codex/chat","style":"roast"}`, `{"fallbackModel":"bad"}`} {
		response = httptest.NewRecorder()
		projectBookWritingSettingsHandler(response, httptest.NewRequest("PATCH", "/settings/project-book", strings.NewReader(body)))
		if response.Code != 400 || readBookWritingPreferences() != want {
			t.Fatal("invalid patch changed preferences")
		}
	}
	want.Model = "opencode/big-pickle"
	data, _ = json.Marshal(want)
	response = httptest.NewRecorder()
	projectBookWritingSettingsHandler(response, httptest.NewRequest("POST", "/settings/project-book", bytes.NewReader(data)))
	if response.Code != 400 || readBookWritingPreferences().Model != "codex/writer" {
		t.Fatal("Build-only writer setting accepted")
	}
}

func bookIndependentWriterProject(t *testing.T, builder, contributor string) (string, bookEntry) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	source := "history/2026-09-27_120000_first/entry.json"
	data, err := os.ReadFile(filepath.Join(project, source))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte(`"provider/model"`), []byte(`"`+builder+`"`))
	data = bytes.ReplaceAll(data, []byte(`"OpenCode"`), []byte(`"`+contributor+`"`))
	bookTestWrite(t, project, source, data)
	_, book := bookTestCall(t, project, "sync", nil)
	return project, book.Entries[0]
}

func TestBookIndependentWriterPreservesBuilderEvidenceAndExistingContent(t *testing.T) {
	for _, test := range []struct{ model, contributor string }{{"cursor/default", "Cursor"}, {"claude-code/sonnet", "Claude Code"}, {"opencode/big-pickle", "OpenCode"}} {
		t.Run(test.contributor, func(t *testing.T) {
			project, entry := bookIndependentWriterProject(t, test.model, test.contributor)
			withCodexModels(t, []chatModel{{ID: "codex/writer", ReasoningEfforts: []string{"low"}}})
			if err := saveBookWritingPreferences(bookWritingPreferences{Style: "announcement", Model: "codex/writer"}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
				calls++
				if options.Model != "writer" || !options.ChatOnly || options.ReasoningEffort != "low" || !strings.Contains(options.Input[0]["text"].(string), "Added the garden page") {
					t.Fatal("writer lost evidence, low effort or tool restrictions")
				}
				return emitCodexBookTestAnswer(emit, bookParserTestResponse)
			})
			book, changed, err := drawProjectBookVisual(context.Background(), project, entry.Source, "", "")
			if err != nil || !changed || calls != 1 {
				t.Fatal(changed, calls, err)
			}
			got := book.Entries[0]
			if got.Model != test.model || got.Contributor != test.contributor || got.StoryModel != "codex/writer" || got.StoryGeneration == nil || got.StoryGeneration.SourceVersion != entry.ContentVersion || got.Sketch.Visual == nil || got.Sketch.Visual.Model != "codex/writer" || got.Sketch.Visual.SourceVersion != entry.ContentVersion || !reflect.DeepEqual(got.Images, entry.Images) {
				t.Fatal("builder attribution, Book provenance or assets lost", got)
			}
			_, loaded := bookTestCall(t, project, "", nil)
			if loaded.Entries[0].StoryModel != got.StoryModel || loaded.Entries[0].StoryGeneration.SourceVersion != entry.ContentVersion {
				t.Fatal("provenance did not persist")
			}
			_, changed, err = drawProjectBookVisual(context.Background(), project, entry.Source, "", "codex/another", "both")
			if err != nil || changed || calls != 1 {
				t.Fatal("writer change replaced saved content", err)
			}
		})
	}
}

func TestBookStoryPreviewOverrideKeepsProvenanceOnReviewedSave(t *testing.T) {
	project, entry := bookIndependentWriterProject(t, "claude-code/opus", "Claude Code")
	withCodexModels(t, []chatModel{{ID: "codex/writer"}})
	withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		if options.Model != "writer" {
			t.Fatal("preview ignored override")
		}
		return emitCodexBookTestAnswer(emit, bookParserTestResponse)
	})
	preview, changed, err := writeProjectBookStory(context.Background(), project, entry.ID, bookStoryOptions{Preview: true, Style: "announcement", Model: "codex/writer"})
	if err != nil || changed || preview.Entries[0].StoryModel != "codex/writer" {
		t.Fatal("preview failed", err)
	}
	_, saved := bookTestCall(t, project, "", nil)
	if saved.Entries[0].StoryModel != "" || saved.Entries[0].ContentVersion != entry.ContentVersion {
		t.Fatal("preview changed stored entry")
	}
	draft := preview.Entries[0]
	response, saved := bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": draft.Title, "body": draft.Body, "expectedContentVersion": entry.ContentVersion, "storyModel": draft.StoryModel, "storyGeneration": draft.StoryGeneration})
	if response.Code != 200 || saved.Entries[0].StoryModel != "codex/writer" || !saved.Entries[0].Reviewed || saved.Entries[0].Model != entry.Model {
		t.Fatal("reviewed preview lost writer or builder", response.Code)
	}
	_, saved = bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "Owner edit", "body": "The owner clarified the result.", "expectedContentVersion": saved.Entries[0].ContentVersion})
	if saved.Entries[0].StoryModel != "codex/writer" {
		t.Fatal("manual edit lost existing writer attribution")
	}
}

func TestBookPreservesLegacySketchFromDifferentModel(t *testing.T) {
	project, entry := bookIndependentWriterProject(t, "cursor/default", "Cursor")
	visual, err := parseBookVisual(bookParserTestResponse, "provider/old-writer")
	if err != nil {
		t.Fatal(err)
	}
	entry.Sketch.Visual = visual
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveBookEntry(root, entry); err != nil {
		t.Fatal(err)
	}
	root.Close()
	withCodexTurn(t, func(context.Context, codexRunOptions, func(codexRPCMessage) error, func(codexRPCMessage) (any, error)) (string, error) {
		t.Fatal("existing drawing caused a new model request")
		return "", nil
	})
	book, changed, err := drawProjectBookVisual(context.Background(), project, entry.Source, "", "codex/new-writer", "drawing")
	wantJSON, _ := json.Marshal(visual)
	gotJSON, _ := json.Marshal(book.Entries[0].Sketch.Visual)
	if err != nil || changed || !bytes.Equal(gotJSON, wantJSON) {
		t.Fatal("legacy drawing replaced after model change", changed, err)
	}
}

func TestBookDrawingRequestUsesExplicitOverrideAndPreservesConcurrentOwnerEdit(t *testing.T) {
	project, entry := bookIndependentWriterProject(t, "opencode/big-pickle", "OpenCode")
	withCodexModels(t, []chatModel{{ID: "codex/writer"}, {ID: "codex/default"}})
	if err := saveBookWritingPreferences(bookWritingPreferences{Style: "announcement", Model: "codex/default"}); err != nil {
		t.Fatal(err)
	}
	withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		if options.Model != "writer" {
			t.Fatal("request override did not beat saved model")
		}
		bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "Owner story", "body": "The owner's correction.", "expectedContentVersion": entry.ContentVersion})
		return emitCodexBookTestAnswer(emit, bookParserTestResponse)
	})
	data, _ := json.Marshal(map[string]any{"projectPath": project, "entryId": entry.ID, "parts": "both", "model": "codex/writer"})
	response := httptest.NewRecorder()
	projectBookVisualHandler(response, httptest.NewRequest("POST", "/project-book/visual", bytes.NewReader(data)))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	_, book := bookTestCall(t, project, "", nil)
	if book.Entries[0].Title != "Owner story" || book.Entries[0].Sketch.Visual != nil || book.Entries[0].StoryModel != "" {
		t.Fatal("stale completion changed the owner's newer revision")
	}
}
