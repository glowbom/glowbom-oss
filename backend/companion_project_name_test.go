package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func projectNameTestSession(t *testing.T, preferences bookWritingPreferences) *companionSession {
	t.Helper()
	service := &chatService{directory: t.TempDir(), prepare: func() error {
		t.Error("Codex naming depended on an unrelated OpenCode runtime")
		return errors.New("unrelated runtime unavailable")
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/project-name", func(w http.ResponseWriter, r *http.Request) {
		service.writeProjectName(w, r, preferences)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Error("naming used a combined catalog or coding route", r.URL.Path)
		http.NotFound(w, r)
	})
	return testCompanion(t, mux)
}

func projectNameTestResult(t *testing.T, s *companionSession, model string) projectNameResult {
	t.Helper()
	w := companionTransferCall(t, s, "/projects/prototype/name", map[string]string{
		"prompt": "Private reach-out list people you want to stay in touch with. Remind me each week.", "model": model})
	var result projectNameResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("invalid name response", w.Code, w.Body.String())
	}
	return result
}

func TestCompanionProjectNameUsesPreferredBookModelWithoutCreatingProject(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/book", ReasoningEfforts: []string{"low", "high"}}, {ID: "codex/build"}})
	var calls atomic.Int32
	withCodexTurn(t, func(ctx context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		calls.Add(1)
		if options.Model != "book" || !options.ChatOnly || options.ReasoningEffort != "low" || options.ThreadID != "" ||
			options.Instructions != projectNamingInstructions || len(options.Input) != 1 ||
			!strings.Contains(options.Input[0]["text"].(string), "Remind me each week.") {
			t.Error("naming lost the Book model, complete idea, or tool-disabled scope")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > companionProjectNameTimeout {
			t.Error("naming has no bounded deadline")
		}
		return emitCodexBookTestAnswer(emit, "“Reach Out”")
	})
	s := projectNameTestSession(t, bookWritingPreferences{Model: "codex/book"})
	result := projectNameTestResult(t, s, "codex/build")
	if result.Name != "Reach Out" || result.BundleID != "app.glowbom.reach.out" || result.Source != "model" ||
		result.Model != "codex/book" || result.ModelSource != "project-book" || calls.Load() != 1 {
		t.Fatal("unexpected Book naming result", result)
	}
	if len(s.projects) != 0 || len(s.prototypeJobs) != 0 || s.localChatPath != "" {
		t.Fatal("naming created a project, build, or local workspace")
	}
}

func TestCompanionProjectNameFallsBackToSelectedChatModel(t *testing.T) {
	for _, failure := range []string{"unavailable", "provider failure", "invalid name"} {
		t.Run(failure, func(t *testing.T) {
			models := []chatModel{{ID: "codex/build", ReasoningEfforts: []string{"low", "high"}}}
			if failure != "unavailable" {
				models = append(models, chatModel{ID: "codex/book"})
			}
			withCodexModels(t, models)
			var called []string
			withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
				called = append(called, options.Model)
				if options.Model == "book" {
					if failure == "provider failure" {
						return "", errors.New("private account failure detail")
					}
					return emitCodexBookTestAnswer(emit, "Here is a long sentence instead of a project name")
				}
				if !options.ChatOnly || options.Model != "build" || options.ReasoningEffort != "low" {
					t.Error("fallback was not a low-effort chat-only turn on selected model")
				}
				return emitCodexBookTestAnswer(emit, "Friendship Journal")
			})
			result := projectNameTestResult(t, projectNameTestSession(t, bookWritingPreferences{Model: "codex/book"}), "codex/build")
			if result.Name != "Friendship Journal" || result.Source != "model" || result.Model != "codex/build" || result.ModelSource != "selected" || result.Reason != "" {
				t.Fatal("selected chat fallback failed", result)
			}
			expected := []string{"book", "build"}
			if failure == "unavailable" {
				expected = []string{"build"}
			}
			if !reflect.DeepEqual(called, expected) {
				t.Fatal("model was repeated or substituted", called)
			}
		})
	}
}

type projectNameTestTransport func(*http.Request) (*http.Response, error)

func (transport projectNameTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func TestProjectNameUsesPaidOpenCodeBookModelWithEveryToolDenied(t *testing.T) {
	var calls atomic.Int32
	service := &chatService{directory: t.TempDir(), serverURL: "http://naming-fixture", prepare: func() error { return nil }}
	service.client = &http.Client{Transport: projectNameTestTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/provider":
			return chatImageReply(`{"connected":["opencode"],"all":[{"id":"opencode","models":{"paid":{"modalities":{"output":["text"]},"variants":{"low":{}}}}}]}`), nil
		case "/session":
			var request struct {
				Permission []map[string]string `json:"permission"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || !reflect.DeepEqual(request.Permission, []map[string]string{{"permission": "*", "pattern": "*", "action": "deny"}}) {
				t.Error("naming session did not deny all permissions")
			}
			return chatImageReply(`{"id":"name-only","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`), nil
		case "/experimental/tool/ids":
			return chatImageReply(`["bash","read","edit"]`), nil
		case "/event":
			return chatImageReply("data: {}\n\n"), nil
		case "/session/name-only/message":
			calls.Add(1)
			var request struct {
				Model   map[string]string `json:"model"`
				System  string            `json:"system"`
				Variant string            `json:"variant"`
				Parts   []map[string]any  `json:"parts"`
				Tools   map[string]bool   `json:"tools"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model["providerID"] != "opencode" || request.Model["modelID"] != "paid" ||
				request.System != projectNamingInstructions || request.Variant != "low" || len(request.Parts) != 1 {
				t.Error("naming did not preserve Book model and text-only scope")
			}
			for _, tool := range []string{"*", "bash", "read", "edit"} {
				if enabled, present := request.Tools[tool]; !present || enabled {
					t.Error("naming enabled a tool", tool)
				}
			}
			return chatImageReply(`{"info":{},"parts":[{"type":"text","text":"Reach Out"},{"type":"reasoning","text":"private reasoning"}]}`), nil
		case "/session/name-only/abort", "/session/name-only":
			return chatImageReply(`true`), nil
		default:
			t.Error("unexpected naming request", r.URL.Path)
			return nil, errors.New("unexpected route")
		}
	})}
	result := service.prepareProjectName(context.Background(), projectNameRequest{Prompt: "An app to remember friends", Model: "cursor/auto"}, bookWritingPreferences{Model: "opencode/paid"})
	if result.Name != "Reach Out" || result.Model != "opencode/paid" || result.ModelSource != "project-book" || calls.Load() != 1 {
		t.Fatal("paid Book model was skipped or the reply was not parsed", result)
	}
}

func TestCompanionProjectNameWaitsBeyondOldTwoSecondCutoff(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/test"}})
	withCodexTurn(t, func(ctx context.Context, _ codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		select {
		case <-time.After(2100 * time.Millisecond):
			return emitCodexBookTestAnswer(emit, "Reach Out")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	result := projectNameTestResult(t, projectNameTestSession(t, bookWritingPreferences{}), "codex/test")
	if result.Name != "Reach Out" || result.Source != "model" {
		t.Fatal("normal model reply was replaced with a starter name", result)
	}
}

func TestCompanionProjectNameNeverRunsBuildOnlyModels(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/book"}})
	withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		if options.Model != "book" || !options.ChatOnly {
			t.Error("a coding agent was used to name a project")
		}
		return emitCodexBookTestAnswer(emit, "Reach Out")
	})
	for _, selected := range []string{"cursor/auto", "claude-code/sonnet", "acp/test", "opencode/big-pickle"} {
		result := projectNameTestResult(t, projectNameTestSession(t, bookWritingPreferences{Model: "codex/book"}), selected)
		if result.Model != "codex/book" || result.Name != "Reach Out" {
			t.Fatal("Book naming was blocked by build-only choice", selected, result)
		}
		result = projectNameTestResult(t, projectNameTestSession(t, bookWritingPreferences{}), selected)
		if result.Source != "fallback" || result.Model != "" {
			t.Fatal("build-only model was invoked", selected, result)
		}
	}
}

func TestProjectNameCandidatesPreserveConfiguredPrecedenceAndDeduplicate(t *testing.T) {
	cases := []struct {
		selected    string
		preferences bookWritingPreferences
		expected    []projectNameCandidate
	}{
		{"codex/build", bookWritingPreferences{Model: "opencode/paid"}, []projectNameCandidate{{"opencode/paid", "project-book"}, {"codex/build", "selected"}}},
		{"codex/book", bookWritingPreferences{Model: "codex/book"}, []projectNameCandidate{{"codex/book", "project-book"}}},
		{"cursor/auto", bookWritingPreferences{FallbackModel: "codex/chat"}, []projectNameCandidate{{"codex/chat", "project-book-fallback"}}},
		{"codex/build", bookWritingPreferences{FallbackModel: "codex/chat"}, []projectNameCandidate{{"codex/build", "selected"}, {"codex/chat", "project-book-fallback"}}},
	}
	for _, fixture := range cases {
		if got := projectNameCandidates(fixture.selected, fixture.preferences); !reflect.DeepEqual(got, fixture.expected) {
			t.Fatal("unexpected candidates", got)
		}
	}
}

func TestCompanionProjectNameFailureReturnsHonestGenericStarter(t *testing.T) {
	for _, reply := range []string{"One\nTwo", "Here is a detailed response containing much more than eight words", "<b></b>", ".", strings.Repeat("x", 4097)} {
		t.Run(reply[:min(len(reply), 30)], func(t *testing.T) {
			withCodexModels(t, []chatModel{{ID: "codex/test"}})
			withCodexTurn(t, func(_ context.Context, _ codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
				return emitCodexBookTestAnswer(emit, reply)
			})
			result := projectNameTestResult(t, projectNameTestSession(t, bookWritingPreferences{}), "codex/test")
			if result.Name != "My new app" || result.Source != "fallback" || result.Reason != "invalid_name" || result.Model != "" {
				t.Fatal("failed naming disguised a prompt fragment as a name", result)
			}
		})
	}
}

func TestProjectNameCancellationIsBoundedAndDoesNotTryAnotherModel(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/book"}, {ID: "codex/build"}})
	stopped := make(chan struct{})
	var calls atomic.Int32
	withCodexTurn(t, func(ctx context.Context, _ codexRunOptions, _ func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		close(stopped)
		return "", ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	service := &chatService{}
	result := service.prepareProjectName(ctx, projectNameRequest{Prompt: "Build a meaningful app", Model: "codex/build"}, bookWritingPreferences{Model: "codex/book"})
	<-stopped
	if result.Source != "fallback" || result.Reason != "timeout" || calls.Load() != 1 {
		t.Fatal("canceled naming started another model", result, calls.Load())
	}
}

func TestCompanionProjectNameRejectsUnknownFieldsAndRequiresPairing(t *testing.T) {
	s := projectNameTestSession(t, bookWritingPreferences{})
	for _, body := range []string{
		`{"prompt":"Build a garden journal","model":"provider/exact","projectPath":"/private"}`,
		`{"prompt":"Build a garden journal","model":"provider/exact","attachmentId":"private"}`,
		`{"prompt":"Build a garden journal","model":""}`,
		`{"prompt":"` + strings.Repeat("x", companionDesktopPrototypePromptBytes+1) + `","model":"provider/exact"}`,
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/prototype/name", body))
		if w.Code < 400 {
			t.Fatal("invalid naming request accepted", w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := companionRequest(s, http.MethodPost, "/projects/prototype/name", `{"prompt":"Build a garden journal","model":"codex/test"}`)
	r.Header.Del("Authorization")
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("naming bypassed pairing authentication", w.Code)
	}
}

func TestProjectNameBundleIDsMatchSavedDesktopIdentifiers(t *testing.T) {
	for input, expected := range map[string]string{"Garden Journal": "app.glowbom.garden.journal", "3D Ship Sim": "app.glowbom.app3d.ship.sim", "42 / My App!": "app.glowbom.app42.my.app", "🌱": "app.glowbom.myapp", "": "app.glowbom.myapp"} {
		if actual := suggestProjectBundleID(input); actual != expected {
			t.Errorf("bundle ID for %q: got %q, want %q", input, actual, expected)
		}
	}
}

func TestCompanionDesktopPrototypeSavesBundleIDFromReviewedName(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := testCompanion(t, desktopPrototypeTestAPI(t, func(w http.ResponseWriter, r *http.Request, request chatRequest) {
		emitCompanionChatTest(w, map[string]any{"done": true, "success": true})
	}))
	s.pickImportFolder = func(context.Context) (string, bool, error) {
		t.Error("remote project creation opened the native folder chooser")
		return "", false, nil
	}
	grant := companionDefaultDestinationFixture(t, s, companionTransferTestID)
	request := companionDesktopPrototypeRequest{RequestID: companionTransferTestID, DestinationID: grant.ID,
		Name: "3D Garden Journal", Prompt: "Make a garden journal", Model: "provider/exact"}
	w := companionTransferCall(t, s, "/projects/prototype", request)
	if w.Code != http.StatusAccepted {
		t.Fatal("creation not accepted", w.Code, w.Body.String())
	}
	snapshot := waitDesktopPrototype(t, s, strings.ToLower(request.RequestID), "completed")
	manifest, err := LoadProject(filepath.Join(snapshot["projectPath"].(string), "glowbom.json"))
	if err != nil || manifest.Name != request.Name || manifest.DisplayName != request.Name || manifest.BundleID != "app.glowbom.app3d.garden.journal" {
		t.Fatal("creation did not preserve reviewed name and derive its app ID", manifest, err)
	}
	if _, err := os.Stat(filepath.Join(snapshot["projectPath"].(string), "apple/Custom/ContentView.swift")); err != nil {
		t.Fatal("naming lost the full Desktop starter", err)
	}
}
