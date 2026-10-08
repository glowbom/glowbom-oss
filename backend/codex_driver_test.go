package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func codexTestMessage(method, params string) codexRPCMessage {
	return codexRPCMessage{Method: method, Params: json.RawMessage(params)}
}

func withCodexTurn(t *testing.T, run func(context.Context, codexRunOptions, func(codexRPCMessage) error, func(codexRPCMessage) (any, error)) (string, error)) {
	t.Helper()
	previous := codexRunTurn
	codexRunTurn = run
	t.Cleanup(func() { codexRunTurn = previous })
}

func withCodexModels(t *testing.T, models []chatModel) {
	t.Helper()
	previous := codexLoadChatModels
	codexLoadChatModels = func(context.Context) ([]chatModel, error) { return models, nil }
	t.Cleanup(func() { codexLoadChatModels = previous })
}

func TestCodexTextResultSeparatesProgressFromFinalAnswer(t *testing.T) {
	for _, test := range []struct {
		name     string
		messages []codexRPCMessage
		want     string
	}{
		{"phased", []codexRPCMessage{
			codexTestMessage("item/completed", `{"item":{"id":"progress","type":"agentMessage","phase":"commentary","text":"I'll inspect the project."}}`),
			codexTestMessage("item/agentMessage/delta", `{"itemId":"answer","delta":"Added the map"}`),
			codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","phase":"final_answer","text":"Added the map. Build passed."}}`),
			codexTestMessage("item/completed", `{"item":{"id":"later","type":"agentMessage","phase":"commentary","text":"Finishing up."}}`),
		}, "Added the map. Build passed."},
		{"legacy", []codexRPCMessage{
			codexTestMessage("item/completed", `{"item":{"id":"progress","type":"agentMessage","text":"I'll inspect the project."}}`),
			codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","phase":null,"text":"Added the map."}}`),
		}, "Added the map."},
		{"phase on start", []codexRPCMessage{
			codexTestMessage("item/started", `{"item":{"id":"progress","type":"agentMessage","phase":"commentary","text":""}}`),
			codexTestMessage("item/agentMessage/delta", `{"itemId":"progress","delta":"I'll inspect the project."}`),
			codexTestMessage("item/completed", `{"item":{"id":"progress","type":"agentMessage","text":"I'll inspect the project."}}`),
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := codexTextStream{}
			for _, message := range test.messages {
				if _, _, err := stream.accept(message); err != nil {
					t.Fatal(err)
				}
			}
			if got := stream.result(); got != test.want {
				t.Fatalf("result = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCodexChatStreamsWithoutToolsOrOpenCode(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/test", Name: "Test", Provider: "Codex", Build: true}})
	withCodexTurn(t, func(ctx context.Context, options codexRunOptions, emit func(codexRPCMessage) error, request func(codexRPCMessage) (any, error)) (string, error) {
		if !options.ChatOnly || options.Model != "test" || options.ThreadID != "" || len(options.Input) != 1 {
			t.Fatalf("chat options: %+v", options)
		}
		if _, err := request(codexTestMessage("item/commandExecution/requestApproval", `{}`)); err == nil {
			t.Fatal("chat permitted an interactive tool request")
		}
		for _, message := range []codexRPCMessage{
			codexTestMessage("thread/started", `{"thread":{"id":"thread-test"}}`),
			codexTestMessage("item/agentMessage/delta", `{"itemId":"message","delta":"Hello"}`),
			codexTestMessage("item/agentMessage/delta", `{"itemId":"message","delta":" there"}`),
			codexTestMessage("item/completed", `{"item":{"id":"message","type":"agentMessage","text":"Hello there"}}`),
		} {
			if err := emit(message); err != nil {
				return "", err
			}
		}
		return "thread-test", nil
	})
	service := &chatService{directory: t.TempDir(), prepare: func() error {
		t.Fatal("Codex chat tried to prepare OpenCode")
		return nil
	}}
	body := `{"mode":"chat","model":"codex/test","messages":[{"role":"user","text":"Hello"}]}`
	w := httptest.NewRecorder()
	service.streamHandler(w, httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(body)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"text":"Hello there"`) || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCodexChatKeepsImageInputAndPropagatesFailure(t *testing.T) {
	withCodexTurn(t, func(ctx context.Context, options codexRunOptions, emit func(codexRPCMessage) error, request func(codexRPCMessage) (any, error)) (string, error) {
		if len(options.Input) != 2 || options.Input[1]["type"] != "image" || options.Input[1]["url"] != "data:image/png;base64,fixture" || !options.ChatOnly {
			t.Fatalf("input changed: %+v", options)
		}
		return "", errors.New("Codex usage limit")
	})
	_, err := completeCodexChat(context.Background(), t.TempDir(), "codex/test", "Only chat", []map[string]any{{"type": "text", "text": "Describe this"}, {"type": "file", "url": "data:image/png;base64,fixture"}}, nil, chatCompletionOptions{})
	if err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Fatal(err)
	}
}

func TestCodexCatalogSurvivesMissingOpenCode(t *testing.T) {
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", filepath.Join(t.TempDir(), "missing"))
	withCodexModels(t, []chatModel{{ID: "codex/test", Name: "Test", Provider: "Codex", Build: true, ReasoningEfforts: []string{"medium", "high"}, DefaultReasoningEffort: "medium", IsDefault: true}})
	t.Setenv("GLOWBOM_CURSOR_BIN", filepath.Join(t.TempDir(), "missing"))
	service := &chatService{prepare: func() error { return errors.New("OpenCode CLI not found") }}
	w := httptest.NewRecorder()
	service.modelsHandler(w, httptest.NewRequest(http.MethodGet, "/chat/models", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"codex/test"`) || !strings.Contains(w.Body.String(), `"reasoningEfforts":["medium","high"]`) || !strings.Contains(w.Body.String(), `"defaultReasoningEffort":"medium"`) || !strings.Contains(w.Body.String(), `"isDefault":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCodexChatReasoningEffortSelection(t *testing.T) {
	for _, test := range []struct {
		name, effort string
		wantStatus   int
	}{
		{"default", "", http.StatusOK},
		{"explicit", "high", http.StatusOK},
		{"unsupported", "ultra", http.StatusBadRequest},
		{"unknown", "fastest", http.StatusBadRequest},
		{"untrimmed", " high ", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			withCodexModels(t, []chatModel{{ID: "codex/test", Name: "Test", Provider: "Codex", Build: true, ReasoningEfforts: []string{"medium", "high"}, DefaultReasoningEffort: "medium"}})
			called := false
			withCodexTurn(t, func(ctx context.Context, options codexRunOptions, emit func(codexRPCMessage) error, request func(codexRPCMessage) (any, error)) (string, error) {
				called = true
				if !options.ChatOnly || options.ReasoningEffort != test.effort {
					t.Fatalf("selected effort changed: %+v", options)
				}
				return "thread-test", emit(codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","text":"Hello"}}`))
			})
			service := &chatService{directory: t.TempDir(), prepare: func() error {
				t.Fatal("Codex chat tried to prepare OpenCode")
				return nil
			}}
			body, _ := json.Marshal(chatRequest{Model: "codex/test", Mode: "chat", ReasoningEffort: test.effort, Messages: []chatMessage{{Role: "user", Text: "Hello"}}})
			w := httptest.NewRecorder()
			service.streamHandler(w, httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(string(body))))
			if w.Code != test.wantStatus || called != (test.wantStatus == http.StatusOK) {
				t.Fatal(w.Code, called, w.Body.String())
			}
		})
	}
}

func TestCodexBuildPreservesHistoryAndIsolatesSession(t *testing.T) {
	for _, previousSession := range []string{"ses_opencode", "cursor-abc", "codex:thread-resume"} {
		t.Run(previousSession, func(t *testing.T) {
			project := t.TempDir()
			withCodexModels(t, []chatModel{{ID: "codex/test", ReasoningEfforts: []string{"low", "high"}}})
			bookCalls := 0
			if err := os.WriteFile(filepath.Join(project, "glowbom.json"), []byte(`{"name":"Codex fixture"}`), 0600); err != nil {
				t.Fatal(err)
			}
			withCodexTurn(t, func(ctx context.Context, options codexRunOptions, emit func(codexRPCMessage) error, request func(codexRPCMessage) (any, error)) (string, error) {
				if options.ChatOnly {
					bookCalls++
					entries, err := loadProjectHistoryEntries(project)
					if err != nil || len(entries) != 1 || entries[0].OutputSummary != "Updated the page" {
						t.Fatalf("Book started before the final build result was saved: %+v %v", entries, err)
					}
					if err := emit(codexTestMessage("item/completed", `{"item":{"id":"progress","type":"agentMessage","phase":"commentary","text":"I'll write the story."}}`)); err != nil {
						return "", err
					}
					result := `{"story":{"title":"A clearer page","body":"The saved page now has a clearer layout for exploring the project. Its main controls sit together so visitors can find the next step."},"description":"A page with a title.","annotations":[{"kind":"shape","shape":"rectangle","start":{"x":0.1,"y":0.1},"end":{"x":0.9,"y":0.9},"color":"#111111"},{"kind":"text","text":"Welcome","x":0.2,"y":0.2,"width":0.3,"fontSize":0.03,"color":"#111111"}]}`
					params, _ := json.Marshal(map[string]any{"item": map[string]any{"id": "book", "type": "agentMessage", "phase": "final_answer", "text": result}})
					return "book-thread", emit(codexTestMessage("item/completed", string(params)))
				}
				wantSession := ""
				if strings.HasPrefix(previousSession, "codex:") {
					wantSession = "thread-resume"
				}
				if options.ChatOnly || options.ThreadID != wantSession || options.Directory != project || options.Model != "test" || options.ReasoningEffort != "high" {
					t.Fatalf("build options: %+v", options)
				}
				prompt, _ := options.Input[0]["text"].(string)
				if !strings.Contains(prompt, "Update the page") || !strings.Contains(prompt, "automatic Glowbom media generation is unavailable") {
					t.Fatal("missing build instructions")
				}
				if err := os.WriteFile(filepath.Join(project, "changed.txt"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := emit(codexTestMessage("thread/started", `{"thread":{"id":"thread-resume"}}`)); err != nil {
					return "", err
				}
				if err := emit(codexTestMessage("item/completed", `{"item":{"id":"progress","type":"agentMessage","phase":"commentary","text":"I'll inspect the project."}}`)); err != nil {
					return "", err
				}
				return "thread-resume", emit(codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","phase":"final_answer","text":"Updated the page"}}`))
			})
			body, _ := json.Marshal(OpenCodeAgentRequest{AgentDriver: "codex", Model: "codex/test", ReasoningEffort: "high", ProjectPath: project, SessionID: previousSession, Instructions: "Update the page", PersistCurrentInstructionsToHistory: true})
			w := httptest.NewRecorder()
			openCodeRefineHandler(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(body))))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Session created: codex:thread-resume") || !strings.Contains(w.Body.String(), `"success":true`) || !strings.Contains(w.Body.String(), "changed.txt") {
				t.Fatal(w.Code, w.Body.String())
			}
			entries, err := loadProjectHistoryEntries(project)
			if err != nil || len(entries) != 1 || entries[0].Contributor != "Codex" || entries[0].Model != "codex/test" || entries[0].OutputSummary != "Updated the page" {
				t.Fatalf("history: %+v %v", entries, err)
			}
			_, book := bookTestCall(t, project, "", nil)
			if bookCalls != 1 || len(book.Entries) != 1 || book.Entries[0].RunID != entries[0].RunID || book.Entries[0].Sketch.Visual == nil || book.Entries[0].Title != "A clearer page" {
				t.Fatalf("Codex post-run Book was not saved: calls=%d, entries=%+v", bookCalls, book.Entries)
			}
			if strings.Index(w.Body.String(), "Writing the story") > strings.Index(w.Body.String(), `"done":true`) || !strings.Contains(w.Body.String(), `"resultText":"Updated the page"`) {
				t.Fatal("Codex completion lost the final answer or preceded its Book", w.Body.String())
			}
		})
	}
}

func TestCodexPostRunBookFailureKeepsTheBuildResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, failedBuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("failedBuild=%v", failedBuild), func(t *testing.T) {
			project := bookTestProject(t)
			withCodexModels(t, []chatModel{{ID: "codex/test"}})
			bookCalls := 0
			withCodexTurn(t, func(ctx context.Context, options codexRunOptions, emit func(codexRPCMessage) error, request func(codexRPCMessage) (any, error)) (string, error) {
				if options.ChatOnly {
					bookCalls++
					return "", context.DeadlineExceeded
				}
				if failedBuild {
					return "", errors.New("Build did not finish")
				}
				return "build-thread", emit(codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","phase":"final_answer","text":"Updated the page"}}`))
			})
			body, _ := json.Marshal(OpenCodeAgentRequest{AgentDriver: "codex", Model: "codex/test", ProjectPath: project, Instructions: "Update the page", PersistCurrentInstructionsToHistory: true})
			w := httptest.NewRecorder()
			openCodeRefineHandler(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(body))))
			entries, err := loadProjectHistoryEntries(project)
			if err != nil || len(entries) != 1 {
				t.Fatal(entries, err)
			}
			if failedBuild {
				if bookCalls != 0 || entries[0].Status != "failed" || !strings.Contains(w.Body.String(), `"success":false`) {
					t.Fatal("failed build generated a Book result", w.Body.String())
				}
			} else if bookCalls != 1 || entries[0].Status != "completed" || entries[0].OutputSummary != "Updated the page" || !strings.Contains(w.Body.String(), `"success":true`) || !strings.Contains(w.Body.String(), "Project Book story or drawing could not be completed") {
				t.Fatal("Book failure changed the saved build result", w.Body.String())
			}
		})
	}
}

func TestCodexApprovalRequiresMatchingProjectAndExplicitDecision(t *testing.T) {
	project, other := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request := codexTestMessage("item/commandExecution/requestApproval", `{"threadId":"thread-one","command":"bun run build","reason":"Build the project"}`)
	var capturedID string
	response, err := waitForCodexInput(ctx, project, request, func(event map[string]interface{}) {
		permission := event["permission"].(map[string]any)
		id, session := permission["id"].(string), permission["sessionID"].(string)
		capturedID = id
		for _, attempted := range []struct{ project, session, decision string }{
			{other, session, "once"}, {project, "codex:thread-two", "once"}, {project, session, "always"},
		} {
			if err := respondToCodexInput(attempted.project, attempted.session, id, "permission", attempted.decision, nil, nil); err == nil {
				t.Fatalf("accepted wrong authorization: %+v", attempted)
			}
		}
		body, _ := json.Marshal(OpenCodePermissionRespondRequest{ProjectPath: project, SessionID: session, PermissionID: id, Response: "once"})
		w := httptest.NewRecorder()
		openCodePermissionRespondHandler(w, httptest.NewRequest(http.MethodPost, "/opencode/permission/respond", strings.NewReader(string(body))))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
	})
	if err != nil || !reflect.DeepEqual(response, map[string]string{"decision": "accept"}) {
		t.Fatal(response, err)
	}
	if err := respondToCodexInput(project, "codex:thread-one", capturedID, "permission", "once", nil, nil); err == nil {
		t.Fatal("accepted duplicate approval")
	}
}

func TestCodexApprovalResponsesMatchNativeChoices(t *testing.T) {
	for _, test := range []struct {
		name, method, choices, response, decision string
		available, rejected                       []string
	}{
		{
			name: "offered session", choices: `["accept","acceptForSession","decline","cancel"]`,
			response: "session", decision: "acceptForSession",
			available: []string{"once", "session", "reject", "cancel"}, rejected: []string{"always"},
		},
		{
			name: "single use only", choices: `["accept","decline"]`,
			response: "once", decision: "accept", available: []string{"once", "reject"},
			rejected: []string{"always", "session", "cancel"},
		},
		{
			name: "cancel only", choices: `["cancel"]`, response: "cancel", decision: "cancel",
			available: []string{"cancel"}, rejected: []string{"once", "always", "session", "reject"},
		},
		{
			name:     "command prefix amendments are not session grants",
			choices:  `["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["npm"]}},{"applyNetworkPolicyAmendment":{"network_policy_amendment":{"host":"example.com","action":"allow"}}},"decline","futureDecision"]`,
			response: "once", decision: "accept", available: []string{"once", "execpolicy", "reject"},
			rejected: []string{"session", "always", "cancel"},
		},
		{
			name: "preserve order without duplicates", choices: `["decline","accept","accept"]`,
			response: "reject", decision: "decline", available: []string{"reject", "once"},
			rejected: []string{"session", "always"},
		},
		{
			name: "legacy command", response: "once", decision: "accept",
			available: []string{"once", "reject", "cancel"}, rejected: []string{"always", "session"},
		},
		{
			name: "null command choices", choices: `null`, response: "once", decision: "accept",
			available: []string{"once", "reject", "cancel"}, rejected: []string{"always", "session"},
		},
		{
			name: "file changes", method: "item/fileChange/requestApproval", response: "once", decision: "accept",
			available: []string{"once", "reject", "cancel"}, rejected: []string{"always", "session"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			method := test.method
			if method == "" {
				method = "item/commandExecution/requestApproval"
			}
			params := `{"threadId":"approval-thread","itemId":"item-one","command":"bun run build"`
			if test.choices != "" {
				params += `,"availableDecisions":` + test.choices
			}
			params += `}`
			response, err := waitForCodexInput(ctx, project, codexTestMessage(method, params), func(event map[string]interface{}) {
				permission := event["permission"].(map[string]any)
				if !reflect.DeepEqual(permission["availableResponses"], test.available) {
					t.Fatalf("available responses = %#v, want %#v", permission["availableResponses"], test.available)
				}
				id, session := permission["id"].(string), permission["sessionID"].(string)
				for _, rejected := range test.rejected {
					if err := respondToCodexInput(project, session, id, "permission", rejected, nil, nil); err == nil {
						t.Errorf("accepted unoffered response %q", rejected)
					}
				}
				body, _ := json.Marshal(OpenCodePermissionRespondRequest{ProjectPath: project, SessionID: session, PermissionID: id, Response: test.response})
				w := httptest.NewRecorder()
				openCodePermissionRespondHandler(w, httptest.NewRequest(http.MethodPost, "/opencode/permission/respond", strings.NewReader(string(body))))
				if w.Code != http.StatusOK {
					t.Fatal(w.Code, w.Body.String())
				}
			})
			if err != nil || !reflect.DeepEqual(response, map[string]string{"decision": test.decision}) {
				t.Fatal(response, err)
			}
		})
	}
}

func TestCodexApprovalWithNoSupportedChoicesFailsClosed(t *testing.T) {
	for _, choices := range []string{`[]`, `["futureDecision"]`, `[{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":[]}}]`} {
		request := codexTestMessage("item/commandExecution/requestApproval", `{"threadId":"approval-thread","availableDecisions":`+choices+`}`)
		if _, err := waitForCodexInput(context.Background(), t.TempDir(), request, func(map[string]interface{}) {
			t.Fatal("displayed unsupported approval choices")
		}); err == nil {
			t.Fatalf("accepted unsupported approval choices %s", choices)
		}
	}
}

func TestCodexQuestionAnswersKeepQuestionIDs(t *testing.T) {
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request := codexTestMessage("item/tool/requestUserInput", `{"threadId":"thread-one","questions":[{"id":"color","header":"Color","question":"Choose a color","options":[{"label":"Blue","description":"Ocean"}]},{"id":"title","header":"Title","question":"Choose a title"}]}`)
	response, err := waitForCodexInput(ctx, project, request, func(event map[string]interface{}) {
		question := event["question"].(map[string]any)
		id, session := question["id"].(string), question["sessionID"].(string)
		if err := respondToCodexInput(project, session, id, "question", "", nil, AnswerByQuestionID{"color": {"Blue"}}); err == nil {
			t.Fatal("accepted missing question")
		}
		body, _ := json.Marshal(OpenCodeQuestionRespondRequest{ProjectPath: project, SessionID: session, QuestionID: id, AnswerByQuestionID: AnswerByQuestionID{"color": {"Blue"}, "title": {"My App"}}})
		w := httptest.NewRecorder()
		openCodeQuestionRespondHandler(w, httptest.NewRequest(http.MethodPost, "/opencode/question/respond", strings.NewReader(string(body))))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(response)
	if string(encoded) != `{"answers":{"color":{"answers":["Blue"]},"title":{"answers":["My App"]}}}` {
		t.Fatal(string(encoded))
	}
}

func TestCodexPendingCancellationAndUnsupportedRequests(t *testing.T) {
	project := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var id string
	_, err := waitForCodexInput(ctx, project, codexTestMessage("item/fileChange/requestApproval", `{"threadId":"thread-one"}`), func(event map[string]interface{}) {
		id = event["permission"].(map[string]any)["id"].(string)
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := respondToCodexInput(project, "codex:thread-one", id, "permission", "once", nil, nil); err == nil {
		t.Fatal("accepted response after cancellation")
	}
	for _, request := range []codexRPCMessage{
		codexTestMessage("item/tool/call", `{"threadId":"thread-one"}`),
		codexTestMessage("item/fileChange/requestApproval", `{"threadId":"thread-one","grantRoot":"/private"}`),
		codexTestMessage("item/tool/requestUserInput", `{"threadId":"thread-one","questions":[{"id":"secret","question":"Password?","isSecret":true}]}`),
	} {
		if _, err := waitForCodexInput(context.Background(), project, request, func(map[string]interface{}) { t.Fatal("displayed unsupported request") }); err == nil {
			t.Fatal("accepted unsupported request")
		}
	}
}

func TestCodexPendingInputStopsWhenRuntimeDies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := codexTestMessage("item/fileChange/requestApproval", `{"threadId":"thread-one"}`)
	request.Context = ctx
	parent, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	_, err := waitForCodexInput(parent, t.TempDir(), request, func(map[string]interface{}) { cancel() })
	if !errors.Is(err, context.Canceled) || parent.Err() != nil {
		t.Fatal("pending input did not stop with the runtime", err)
	}
}

func TestCodexRunRestoresSessionWithoutSharingPrivateRouting(t *testing.T) {
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, m.api)
	project := sharedCompanionProject(t, s)
	job := m.newRun(project, "desktop", OpenCodeAgentRequest{ProjectPath: project.path, AgentDriver: "codex", Model: "test", ReasoningEffort: "high", Instructions: "Build"})
	job.mu.Lock()
	job.agentSessionID = "codex:thread-restore"
	job.status = "completed"
	job.resultText = "Done"
	job.mu.Unlock()
	m.saveRun(job)
	if job.snapshot(true)["sessionID"] != "codex:thread-restore" || job.snapshot(false)["sessionID"] != nil {
		t.Fatal("session routing leaked to public snapshot or was missing locally")
	}
	for _, local := range []bool{false, true} {
		if job.snapshot(local)["reasoningEffort"] != "high" {
			t.Fatal("selected effort was not included in the run snapshot")
		}
	}
	restored := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(restored.Shutdown)
	restored.loadProjectRuns(project.path)
	loaded := restored.run(job.id)
	if loaded == nil {
		t.Fatal("Codex run was not restored")
	}
	snapshot := loaded.snapshot(true)
	if snapshot["agentDriver"] != "codex" || snapshot["sessionID"] != "codex:thread-restore" || snapshot["model"] != "codex/test" || snapshot["reasoningEffort"] != "high" || snapshot["resultText"] != "Done" {
		t.Fatalf("restored Codex run: %+v", snapshot)
	}
}

func TestCodexRunEffortDoesNotChangeOtherWorkersOrInventDefaults(t *testing.T) {
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, m.api)
	project := sharedCompanionProject(t, s)
	for _, request := range []OpenCodeAgentRequest{
		{AgentDriver: "codex", ReasoningEffort: ""},
		{AgentDriver: "opencode", ReasoningEffort: "high"},
		{AgentDriver: "cursor", ReasoningEffort: "high"},
	} {
		job := m.newRun(project, "desktop", request)
		if job.reasoningEffort != "" || job.snapshot(true)["reasoningEffort"] != nil || job.snapshot(false)["reasoningEffort"] != nil {
			t.Fatalf("unexpected persisted effort for %+v", request)
		}
	}
}

func TestCodexProjectBookUsesRecordedRuntimeWithoutOpenCode(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/test", Name: "Test", Provider: "Codex", Build: true}})
	service := &chatService{prepare: func() error {
		t.Fatal("Codex Book generation tried to start OpenCode")
		return nil
	}}
	model, err := bookVisualModel(context.Background(), service, "codex/test")
	if err != nil || model.ID != "codex/test" {
		t.Fatal(model, err)
	}
	if _, err := bookVisualModel(context.Background(), service, "codex/disconnected"); err == nil {
		t.Fatal("Book silently substituted another model")
	}
}

func TestFourAgentsHaveIndependentBuildLanes(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "desktop-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	m := newCompanionManager(http.NotFoundHandler())
	t.Cleanup(m.Shutdown)
	s := testCompanion(t, m.api)
	project := sharedCompanionProject(t, s)
	started, finished := make(chan string, 4), make(chan string, 4)
	release := make(chan struct{})
	defer close(release)
	handler := m.guardBuild(func(w http.ResponseWriter, r *http.Request) {
		var req OpenCodeAgentRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"output\":\"Session created: %s:thread\"}\n\n", req.AgentDriver)
		started <- req.AgentDriver
		select {
		case <-release:
		case <-r.Context().Done():
		}
		fmt.Fprint(w, "data: {\"done\":true,\"success\":true}\n\n")
	})
	for _, driver := range []string{"opencode", "cursor", "claude-code", "codex"} {
		body, _ := json.Marshal(OpenCodeAgentRequest{ProjectPath: project.path, AgentDriver: driver, Model: "test", Instructions: "Build"})
		go func() {
			handler(httptest.NewRecorder(), registryLocalRequest(http.MethodPost, "/opencode/refine", string(body)))
			finished <- driver
		}()
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("an independent worker was blocked")
		}
	}
	w := httptest.NewRecorder()
	body, _ := json.Marshal(OpenCodeAgentRequest{ProjectPath: project.path, AgentDriver: "claude-code", Instructions: "Duplicate"})
	handler(w, registryLocalRequest(http.MethodPost, "/opencode/refine", string(body)))
	if w.Code != http.StatusConflict || len(m.runSnapshots(true)) != 4 {
		t.Fatal("Claude Code lane did not preserve other worker cards", w.Code, len(m.runSnapshots(true)))
	}
	var claudeJob *companionJob
	for _, job := range m.runList() {
		if job.agentDriver == "claude-code" {
			claudeJob = job
		}
	}
	if claudeJob == nil {
		t.Fatal("Claude Code worker missing")
	}
	w = httptest.NewRecorder()
	m.ServeHTTP(w, registryLocalRequest(http.MethodPost, "/companion/cancel", `{"jobId":"`+claudeJob.id+`"}`))
	if w.Code != http.StatusOK {
		t.Fatal("could not stop Claude Code", w.Code)
	}
	select {
	case driver := <-finished:
		if driver != "claude-code" {
			t.Fatal("stopping Claude Code stopped a different worker", driver)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Claude Code did not stop")
	}
	for _, job := range m.runList() {
		if job.agentDriver != "claude-code" && job.ctx.Err() != nil {
			t.Fatal("another worker was canceled", job.agentDriver)
		}
	}
	m.Shutdown()
	for range 3 {
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("worker did not stop")
		}
	}
}
