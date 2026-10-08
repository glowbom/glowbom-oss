package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildBookCompletionUsesIndependentWriterForEveryBuilder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENCODE_URL", "http://127.0.0.1:1")
	withCodexModels(t, []chatModel{{ID: "codex/book-writer", ReasoningEfforts: []string{"low"}}})
	if err := saveBookWritingPreferences(bookWritingPreferences{Style: "announcement", Model: "codex/book-writer"}); err != nil {
		t.Fatal(err)
	}
	for _, builder := range []struct{ model, contributor string }{
		{"opencode/big-pickle", "OpenCode"}, {"auto", "Cursor"},
		{"claude-code/sonnet", "Claude Code"}, {"codex/build-model", "Codex"},
	} {
		t.Run(builder.contributor, func(t *testing.T) {
			project := bookTestProject(t)
			bookTestBuild(t, project)
			path := filepath.Join(project, "history/2026-09-27_120000_first/entry.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			record["model"], record["contributor"], record["runId"] = builder.model, builder.contributor, "saved-run"
			original, _ := json.Marshal(record)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
				calls++
				if options.Model != "book-writer" || !options.ChatOnly || options.Directory != project {
					t.Fatal("Book changed the builder or used a coding turn", options.Model)
				}
				return emitCodexBookTestAnswer(emit, bookParserTestResponse)
			})
			output := httptest.NewRecorder()
			writeBuildProjectBook(context.Background(), output, output, project, "saved-run")
			_, book := bookTestCall(t, project, "", nil)
			if calls != 1 || len(book.Entries) != 1 {
				t.Fatalf("Book completion did not use its writer: calls=%d entries=%d output=%s", calls, len(book.Entries), output.Body.String())
			}
			entry := book.Entries[0]
			if entry.Model != builder.model || entry.Contributor != builder.contributor || entry.StoryModel != "codex/book-writer" || entry.Sketch.Visual == nil || entry.Sketch.Visual.Model != "codex/book-writer" {
				t.Fatal("Book lost independent builder and writer attribution", entry)
			}
			writeBuildProjectBook(context.Background(), output, output, project, "saved-run")
			if calls != 1 {
				t.Fatal("completion regenerated an existing story or drawing")
			}
			saved, _ := os.ReadFile(path)
			if !bytes.Equal(saved, original) {
				t.Fatal("Book changed the original build record")
			}
		})
	}
}

func TestUnavailableBookWriterLeavesCompletedBuildRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := bookTestProject(t)
	bookTestBuild(t, project)
	path := filepath.Join(project, "history/2026-09-27_120000_first/entry.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"model":"provider/model"`), []byte(`"model":"codex/test","runId":"saved-run"`), 1)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	withCodexModels(t, []chatModel{{ID: "codex/test"}})
	_, imported := bookTestCall(t, project, "sync", nil)
	entry := imported.Entries[0]
	if err := saveBookWritingPreferences(bookWritingPreferences{Style: "announcement", Model: "codex/unavailable"}); err != nil {
		t.Fatal(err)
	}
	output := httptest.NewRecorder()
	writeBuildProjectBook(context.Background(), output, output, project, entry.RunID)
	_, book := bookTestCall(t, project, "", nil)
	if !strings.Contains(output.Body.String(), "The build was saved") || len(book.Entries) != 1 || book.Entries[0].Status != "completed" || book.Entries[0].Body != entry.Body || book.Entries[0].Sketch.Visual != nil {
		t.Fatal("an unavailable Book writer changed the completed build", output.Body.String())
	}
}
