package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestACPEmptyCompletionIsFailedWithoutWritingBook(t *testing.T) {
	for _, mode := range []string{"empty", "whitespace", "tool-only"} {
		t.Run(mode, func(t *testing.T) {
			project, _ := acpBuildFixture(t)
			original := acpRunTurn
			t.Cleanup(func() { acpRunTurn = original })
			acpRunTurn = func(ctx context.Context, profile acpProfile, directory, session string, prompt []map[string]any, emit func(acpMessage) error, request func(context.Context, acpMessage) (any, error)) (string, error) {
				if err := emit(acpMessage{Method: "glowbom/session", Params: json.RawMessage(`{"sessionId":"empty-turn"}`)}); err != nil {
					return "", err
				}
				var update string
				switch mode {
				case "whitespace":
					update = `{"sessionId":"empty-turn","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" \n\t"}}}`
				case "tool-only":
					update = `{"sessionId":"empty-turn","update":{"sessionUpdate":"tool_call","title":"Read project files","status":"completed"}}`
				}
				if update != "" {
					if err := emit(acpMessage{Method: "session/update", Params: json.RawMessage(update)}); err != nil {
						return "", err
					}
				}
				return "empty-turn", nil
			}
			body, _ := json.Marshal(OpenCodeAgentRequest{AgentDriver: "acp", Model: "acp/acp-1", ProjectPath: project, Instructions: "Update the page", PersistCurrentInstructionsToHistory: true})
			writer := httptest.NewRecorder()
			openCodeRefineHandler(writer, acpSettingsRequest(http.MethodPost, string(body)))
			output := writer.Body.String()
			if writer.Code != http.StatusOK || !strings.Contains(output, `"success":false`) || strings.Contains(output, `"success":true`) || !strings.Contains(output, "ended without a response or file changes") {
				t.Fatalf("empty ACP turn did not report a useful failure: %d %s", writer.Code, output)
			}
			if strings.Contains(output, "Writing the story") || strings.Contains(output, "Writing the Project Book") {
				t.Fatal("an empty ACP turn started the Project Book", output)
			}
			entries, err := loadProjectHistoryEntries(project)
			if err != nil || len(entries) != 1 || entries[0].Status != "failed" || !strings.Contains(entries[0].OutputSummary, "ended without a response or file changes") {
				t.Fatalf("empty ACP turn was not saved as failed: %+v %v", entries, err)
			}
		})
	}
}

func TestACPCompletionAcceptsReplyOrFileChanges(t *testing.T) {
	for _, mode := range []string{"reply-only", "file-change-only"} {
		t.Run(mode, func(t *testing.T) {
			project, profile := acpBuildFixture(t)
			original := acpRunTurn
			t.Cleanup(func() { acpRunTurn = original })
			acpRunTurn = func(ctx context.Context, profile acpProfile, directory, session string, prompt []map[string]any, emit func(acpMessage) error, request func(context.Context, acpMessage) (any, error)) (string, error) {
				if mode == "reply-only" {
					return "session", emit(acpMessage{Method: "session/update", Params: json.RawMessage(`{"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"The requested layout is already present."}}}`)})
				}
				return "session", os.WriteFile(filepath.Join(project, "changed.txt"), []byte("Updated the page"), 0600)
			}
			writer := httptest.NewRecorder()
			callbackCount := 0
			status, summary := runACPRefine(writer, httptest.NewRequest(http.MethodPost, "/opencode/refine", nil), OpenCodeAgentRequest{ProjectPath: project}, profile, "Update the page", nil, func(status, summary string, changed []string) {
				callbackCount++
				if status != "completed" || (mode == "reply-only" && summary == "") || (mode == "file-change-only" && len(changed) != 1) {
					t.Fatalf("valid ACP result was rejected: %s %q %v", status, summary, changed)
				}
			})
			if callbackCount != 1 || status != "completed" || !strings.Contains(writer.Body.String(), `"success":true`) || strings.Contains(writer.Body.String(), `"success":false`) {
				t.Fatalf("valid ACP turn did not complete: %s %q %s", status, summary, writer.Body.String())
			}
		})
	}
}
