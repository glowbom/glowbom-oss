package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBookCompletionOptionsKeepDriverEffortSeparate(t *testing.T) {
	for _, test := range []struct {
		name    string
		model   chatModel
		effort  string
		variant string
	}{
		{name: "Codex supported low", model: chatModel{ID: "codex/test", DefaultReasoningEffort: "high", ReasoningEfforts: []string{"low", "medium", "high", "ultra"}}, effort: "low"},
		{name: "Codex unsupported low", model: chatModel{ID: "codex/test", DefaultReasoningEffort: "high", ReasoningEfforts: []string{"high", "ultra"}}},
		{name: "Codex older catalog", model: chatModel{ID: "codex/test", LowEffort: true}},
		{name: "OpenCode low variant", model: chatModel{ID: "provider/model", LowEffort: true}, variant: "low"},
		{name: "OpenCode default", model: chatModel{ID: "provider/model"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := bookCompletionOptions(test.model)
			if options.ReasoningEffort != test.effort || options.Variant != test.variant {
				t.Fatalf("unexpected Book effort: effort=%q variant=%q", options.ReasoningEffort, options.Variant)
			}
		})
	}
}

func codexBookTestProject(t *testing.T) (string, bookEntry) {
	t.Setenv("HOME", t.TempDir())
	t.Helper()
	project := bookTestProject(t)
	bookTestBuild(t, project)
	source := "history/2026-09-27_120000_first/entry.json"
	data, err := os.ReadFile(filepath.Join(project, source))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte(`"provider/model"`), []byte(`"codex/test"`))
	data = bytes.ReplaceAll(data, []byte(`"OpenCode"`), []byte(`"Codex"`))
	bookTestWrite(t, project, source, data)
	bookTestWrite(t, project, "web/page.tsx", []byte("export function Garden() { return <main><h1>Garden</h1><button>Plant</button></main> }"))
	_, imported := bookTestCall(t, project, "sync", nil)
	if len(imported.Entries) != 1 || imported.Entries[0].Model != "codex/test" || imported.Entries[0].Contributor != "Codex" {
		t.Fatal("Codex build did not import with its recorded model")
	}
	withCodexModels(t, []chatModel{{ID: "codex/test", ReasoningEfforts: []string{"low", "high", "ultra"}, DefaultReasoningEffort: "high"}})
	// Native Book turns must not depend on the OpenCode server being available.
	t.Setenv("OPENCODE_URL", "http://127.0.0.1:1")
	return project, imported.Entries[0]
}

func emitCodexBookTestAnswer(emit func(codexRPCMessage) error, text string) (string, error) {
	params, _ := json.Marshal(map[string]any{"item": map[string]any{"id": "book-answer", "type": "agentMessage", "phase": "final_answer", "text": text}})
	return "book-thread", emit(codexRPCMessage{Method: "item/completed", Params: params})
}

func TestCodexBookGeneratesAndRepairsWithSavedStyle(t *testing.T) {
	project, entry := codexBookTestProject(t)
	preferences := readBookWritingPreferences()
	first := strings.Replace(bookParserTestResponse, `"shape":"rectangle"`, `"shape":"circle"`, 1)
	var calls int
	withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		calls++
		if !options.ChatOnly || options.Directory != project || options.Model != "test" || options.ThreadID != "" || options.ReasoningEffort != "low" {
			t.Fatal("Book did not use a separate tool-disabled, low-effort turn with the recorded Codex model")
		}
		if len(options.Input) != 1 || !strings.Contains(options.Input[0]["text"].(string), "Added the garden page") || !strings.Contains(options.Input[0]["text"].(string), "Plant") {
			t.Fatal("Book turn lost the saved build evidence")
		}
		if !strings.HasPrefix(options.Instructions, bookGenerationPrompt(preferences, calls == 1, true)) {
			t.Fatal("native generation or repair lost the saved writing and drawing style")
		}
		if calls == 1 {
			return emitCodexBookTestAnswer(emit, first)
		}
		if calls != 2 || !strings.Contains(options.Instructions, "Generate only the drawing") {
			t.Fatal("Codex repair did not keep the valid story")
		}
		return emitCodexBookTestAnswer(emit, strings.Replace(bookParserTestResponse, "A clearer garden", "Unwanted story rewrite", 1))
	})
	book, changed, err := drawProjectBookVisual(context.Background(), project, entry.Source, "", entry.Model)
	if err != nil || !changed || calls != 2 || len(book.Entries) != 1 {
		t.Fatalf("Codex Book generation failed: changed=%t calls=%d err=%v", changed, calls, err)
	}
	got := book.Entries[0]
	if got.Title != "A clearer garden" || got.Sketch.Visual == nil || got.Sketch.Visual.Model != entry.Model || got.CanWriteStory || len(got.Images) != 1 || got.Images[0] != entry.Images[0] {
		t.Fatal("Codex generation lost its valid story, drawing, or original illustration")
	}
	for _, path := range []string{"history/2026-09-27_120000_first/drawing.png", "project-book/" + entry.Images[0].Path} {
		image, err := os.ReadFile(filepath.Join(project, path))
		if err != nil || !bytes.Equal(image, bookTestPNG(t)) {
			t.Fatal("Book generation changed an original input image", err)
		}
	}
	_, loaded := bookTestCall(t, project, "sync", nil)
	if loaded.Entries[0].Title != got.Title || loaded.Entries[0].Sketch.Visual == nil {
		t.Fatal("Codex story and sketch did not survive reopening the Book")
	}
	if _, changed, err = drawProjectBookVisual(context.Background(), project, entry.Source, "", entry.Model); err != nil || changed || calls != 2 {
		t.Fatal("completed Codex Book content was generated again", err)
	}
}

func TestCodexBookStoryPreviewUsesRequestedVoiceAndPreservesOwnerEdits(t *testing.T) {
	project, entry := codexBookTestProject(t)
	_, edited := bookTestCall(t, project, "update-entry", map[string]any{"entryId": entry.ID, "title": "Owner title", "body": "The owner wrote this story.", "expectedContentVersion": entry.ContentVersion})
	entry = edited.Entries[0]
	var calls int
	var expected bookWritingPreferences
	withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		calls++
		if !options.ChatOnly || options.Model != "test" || options.ReasoningEffort != "low" || options.Instructions != bookWritingPrompt(bookStoryPrompt, expected) {
			t.Fatal("Codex story preview lost its selected voice or separate low-effort turn")
		}
		if expected.Style == "custom" {
			return emitCodexBookTestAnswer(emit, `{"story":{"title":"Garden update","body":"A Plant button takes root."}}`)
		}
		return emitCodexBookTestAnswer(emit, bookParserTestResponse)
	})
	for _, preferences := range []bookWritingPreferences{{Style: "announcement"}, {Style: "custom", CustomPrompt: "Write a warm announcement under 100 characters."}} {
		expected = preferences
		preview, changed, err := writeProjectBookStory(context.Background(), project, entry.ID, bookStoryOptions{Preview: true, Style: preferences.Style, CustomPrompt: preferences.CustomPrompt})
		if err != nil || changed || preview.Entries[0].Title == entry.Title {
			t.Fatal("Codex story preview failed", err)
		}
		_, saved := bookTestCall(t, project, "", nil)
		if saved.Entries[0].Title != entry.Title || saved.Entries[0].ContentVersion != entry.ContentVersion || saved.Entries[0].Sketch.Visual != nil {
			t.Fatal("preview changed the owner's saved story or sketch")
		}
	}
	if _, _, err := writeProjectBookStory(context.Background(), project, entry.ID); err == nil || calls != 2 {
		t.Fatal("automatic writing replaced an owner edit")
	}
}
