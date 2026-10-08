package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

func TestBuildAgentStatusRequiresCompleteMarkedLine(t *testing.T) {
	content := "I am making a change.\nGLOWBOM_STATUS: Updating the welcome screen\nGLOWBOM_STATUS: Testing the"
	lines := buildAgentStatusLines(content, false)
	if len(lines) != 1 || lines[0] != "Updating the welcome screen" {
		t.Fatalf("unexpected in-progress statuses: %#v", lines)
	}
	lines = buildAgentStatusLines(content, true)
	if len(lines) != 2 || lines[1] != "Testing the" {
		t.Fatalf("missing final status: %#v", lines)
	}
	if got := buildAgentStatusLines("Status: unrelated prose\n", true); len(got) != 0 {
		t.Fatalf("unmarked prose became a status: %#v", got)
	}
	if got := cleanBuildStatus("  Updated\n the\tapp  "); got != "Updated the app" {
		t.Fatalf("status was not normalized: %q", got)
	}
	stacked := "GLOWBOM_STATUS: Prototype verified to compile. GLOWBOM_STATUS: Purple mode button is ready. GLOWBOM_STATUS:"
	if got := cleanBuildStatus(stacked); got != "Purple mode button is ready." {
		t.Fatalf("repeated markers remained in status: %q", got)
	}
	if got := buildAgentStatusLines(stacked+"\n", false); len(got) != 1 || got[0] != "Purple mode button is ready." {
		t.Fatalf("repeated markers remained in agent update: %#v", got)
	}
}

func TestBuildAgentResultLineSelectsFinalPlainLanguageConclusion(t *testing.T) {
	content := "GLOWBOM_STATUS: Verifying the welcome screen\nDetailed notes.\nGLOWBOM_RESULT: People can now sign in, and the iOS build passed."
	if got := buildAgentResultLine(content); got != "People can now sign in, and the iOS build passed." {
		t.Fatalf("wrong conclusion: %q", got)
	}
}

func TestBuildStatusStreamAcceptsOnlyAssistantMarkedUpdates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message.updated","properties":{"info":{"id":"user","role":"user","sessionID":"session-test","parts":[{"type":"text","text":"GLOWBOM_STATUS: Do not show this\n"}]}}}`,
			`{"type":"message.part.updated","properties":{"part":{"id":"user-steer","type":"text","text":"[Glowbom steer] Private correction","messageID":"user","sessionID":"session-test"}}}`,
			`{"type":"message.part.updated","properties":{"part":{"id":"tool-read","type":"tool","tool":"read","messageID":"assistant","sessionID":"session-test","state":{"status":"running","input":{"path":"/private/project/App.tsx"}}}}}`,
			`{"type":"message.updated","properties":{"info":{"id":"assistant","role":"assistant","sessionID":"session-test","parts":[{"type":"text","text":"GLOWBOM_STATUS: Updating the app\n"}]}}}`,
			`{"type":"message.updated","properties":{"info":{"id":"assistant","role":"assistant","sessionID":"session-test","parts":[{"type":"text","text":"GLOWBOM_STATUS: Updating the app\nGLOWBOM_RESULT: People can now use the welcome screen.\n"}]}}}`,
			`{"type":"session.idle","properties":{"sessionID":"session-test"}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	driver := &OpenCodeDriver{client: opencode.NewClient(option.WithBaseURL(server.URL))}
	dispatched := make(chan struct{})
	close(dispatched)
	response := httptest.NewRecorder()
	completed, _, _, errMessage := driver.streamEventsAndWaitForCompletion(context.Background(), response, response, "/project", "session-test", dispatched)
	if !completed || errMessage != "" {
		t.Fatalf("stream incomplete: %t %q", completed, errMessage)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"text":"Updating the app"`) || !strings.Contains(body, `"source":"agent"`) || !strings.Contains(body, `reading App.tsx`) || strings.Contains(body, `"text":"Do not show this"`) || strings.Contains(body, "Private correction") {
		t.Fatalf("assistant status filtering failed: %s", body)
	}
	if !strings.Contains(body, `"resultText":"People can now use the welcome screen."`) {
		t.Fatalf("final agent result was not streamed: %s", body)
	}
}

func TestRoutineIdleStatusIsNotShownAsBuildProgress(t *testing.T) {
	if !isIgnorableSessionStatus(`{"type":"idle"}`) || !isIgnorableSessionStatus(`{"type":"retry"}`) {
		t.Fatal("routine OpenCode session states reached Build progress")
	}
}

func TestBuildToolStatusesUseSafeEvidence(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]interface{}
		want  string
	}{
		{"read file", "read", map[string]interface{}{"path": "/private/project/App.tsx"}, "reading App.tsx"},
		{"edit file", "edit", map[string]interface{}{"file": "src/SignIn.swift"}, "updating SignIn.swift"},
		{"test", "bash", map[string]interface{}{"command": "go test ./..."}, "running tests"},
		{"build", "bash", map[string]interface{}{"command": "bun run build"}, "building the app"},
		{"search", "grep", map[string]interface{}{"pattern": "SECRET_VALUE"}, "tracing existing references"},
		{"sensitive file", "read", map[string]interface{}{"path": "/private/project/.env"}, "reading existing files"},
		{"unknown command", "bash", map[string]interface{}{"command": "deploy --token private-value"}, "its details are available in Build"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := buildToolStatus(test.tool, test.input)
			if !strings.Contains(got, test.want) {
				t.Fatalf("status %q does not contain %q", got, test.want)
			}
			for _, sensitive := range []string{"/private/project", "SECRET_VALUE", "private-value", ".env"} {
				if strings.Contains(got, sensitive) {
					t.Fatalf("status exposed raw input %q: %s", sensitive, got)
				}
			}
		})
	}
}

func TestBuildPromptUsesNewChatMessages(t *testing.T) {
	project := translationProject(t)
	dir, err := chatWriteDirectory(project, ".glowbom")
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicChatFile(dir, "chat.json", []byte(`[{"role":"user","text":"Start"},{"role":"assistant","text":"Okay"}]`)); err != nil {
		t.Fatal(err)
	}
	prompt := buildRunUpdatesPrompt(project)
	for _, expected := range []string{"GLOWBOM_STATUS:", ".glowbom/chat.json", "between major steps", "had 2 array entries", "user messages added after those entries", "12 to 18 words", "actual screen, file, test"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q", expected)
		}
	}
	if strings.Contains(prompt, "build-corrections") {
		t.Fatal("prompt still refers to the removed correction inbox")
	}
	if err := atomicChatFile(dir, "chat.json", []byte(`{broken`)); err != nil {
		t.Fatal(err)
	}
	prompt = buildRunUpdatesPrompt(project)
	if !strings.Contains(prompt, "baseline could not be read") || strings.Contains(prompt, "had 0 array entries") {
		t.Fatal("unreadable history was treated as an empty baseline")
	}
}

func TestBuildChatDoesNotAppearAsChangedAppFile(t *testing.T) {
	project := translationProject(t)
	before, err := captureProjectFileSnapshot(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".glowbom"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".glowbom", "chat.json"), []byte(`[{"role":"user","text":"Use a warmer color"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".glowbom", "kept.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := detectChangedFilesFromSnapshot(project, before)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != ".glowbom/kept.json" {
		t.Fatalf("unexpected changed files: %#v", changed)
	}
	merged := mergeChangedFiles([]string{"prototype/index.html"}, []string{".glowbom/chat.json"})
	if len(merged) != 1 || merged[0] != "prototype/index.html" {
		t.Fatalf("chat entered changed file list: %#v", merged)
	}
}
