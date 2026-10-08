package main

import (
	"context"
	"encoding/json"
	"fmt"
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

func buildAllDriverContext(t *testing.T, driver, project, mode string) (context.Context, *buildPermissionPolicy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	policy := newBuildPermissionPolicy(ctx, driver, project, mode)
	if policy == nil {
		t.Fatal("could not create fixture build policy")
	}
	t.Cleanup(policy.close)
	return withBuildPermissionPolicy(ctx, policy), policy
}

func buildAllHasChoice(permission map[string]any, want string) bool {
	for _, response := range permission["availableResponses"].([]string) {
		if response == want {
			return true
		}
	}
	return false
}

func TestBuildAllDriverCodexUsesOnlyNativeOnceAndExactBuildSession(t *testing.T) {
	project := t.TempDir()
	ctx, _ := buildAllDriverContext(t, "codex", project, "all")
	message := codexTestMessage("item/commandExecution/requestApproval", `{"threadId":"build-thread","command":"fixture-command","availableDecisions":["accept","acceptForSession","decline"]}`)
	message.ID = json.RawMessage(`1`)
	result, err := waitForCodexInput(ctx, project, message, func(map[string]interface{}) { t.Fatal("the current build asked for its supported one-time choice") })
	if err != nil || !reflect.DeepEqual(result, map[string]any{"decision": "accept"}) {
		t.Fatalf("native once = %#v, %v", result, err)
	}
	for _, test := range []struct {
		name, params string
		ctx          context.Context
	}{
		{"another session", `{"threadId":"other-thread","availableDecisions":["accept","decline"]}`, ctx},
		{"persistent only", `{"threadId":"build-thread","availableDecisions":["acceptForSession","decline"]}`, ctx},
		{"unmanaged", `{"threadId":"build-thread","availableDecisions":["accept","decline"]}`, context.Background()},
	} {
		t.Run(test.name, func(t *testing.T) {
			shown := false
			msg := codexTestMessage("item/commandExecution/requestApproval", test.params)
			msg.ID = json.RawMessage(`2`)
			_, err := waitForCodexInput(test.ctx, project, msg, func(event map[string]interface{}) {
				permission := event["permission"].(map[string]any)
				shown = true
				if buildAllHasChoice(permission, "all") {
					t.Error("offered All outside its once-capable current build session")
				}
				if err := respondToCodexInput(project, permission["sessionID"].(string), permission["id"].(string), "permission", "reject", nil, nil); err != nil {
					t.Error(err)
				}
			})
			if err != nil || !shown {
				t.Fatalf("request was not left manual: %v", err)
			}
		})
	}
	// Repeating a provider request ID cannot produce another automatic approval.
	shown := false
	_, err = waitForCodexInput(ctx, project, message, func(event map[string]interface{}) {
		shown = true
		p := event["permission"].(map[string]any)
		_ = respondToCodexInput(project, p["sessionID"].(string), p["id"].(string), "permission", "reject", nil, nil)
	})
	if err != nil || !shown {
		t.Fatal("replayed request was approved automatically", err)
	}
}

func TestBuildAllDriverCodexQuestionsAndCanceledProviderRequestStayManual(t *testing.T) {
	project := t.TempDir()
	ctx, _ := buildAllDriverContext(t, "codex", project, "all")
	message := codexTestMessage("item/tool/requestUserInput", `{"threadId":"build-thread","questions":[{"id":"choice","question":"Which page?","options":[{"label":"Home"}]}]}`)
	shown := false
	result, err := waitForCodexInput(ctx, project, message, func(event map[string]interface{}) {
		shown = true
		q := event["question"].(map[string]any)
		if err := respondToCodexInput(project, q["sessionID"].(string), q["id"].(string), "question", "", nil, AnswerByQuestionID{"choice": {"Home"}}); err != nil {
			t.Error(err)
		}
	})
	if err != nil || result == nil || !shown {
		t.Fatal("build-wide permission answered an agent question", err)
	}
	providerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	message = codexTestMessage("item/commandExecution/requestApproval", `{"threadId":"build-thread","availableDecisions":["accept","decline"]}`)
	message.Context = providerCtx
	_, err = waitForCodexInput(ctx, project, message, func(map[string]interface{}) { t.Fatal("canceled provider request was displayed") })
	if err != context.Canceled {
		t.Fatal("canceled provider request was approved", err)
	}
}

func TestBuildAllDriverCodexBufferedApprovalCannotOutliveCancellation(t *testing.T) {
	for index := 0; index < 20; index++ {
		project := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		policy := newBuildPermissionPolicy(ctx, "codex", project, "ask")
		ctx = withBuildPermissionPolicy(ctx, policy)
		message := codexTestMessage("item/commandExecution/requestApproval", `{"threadId":"thread","availableDecisions":["accept","decline"]}`)
		result, err := waitForCodexInput(ctx, project, message, func(event map[string]interface{}) {
			p := event["permission"].(map[string]any)
			if err := respondToCodexInput(project, p["sessionID"].(string), p["id"].(string), "permission", "once", nil, nil); err != nil {
				t.Error(err)
			}
			cancel()
		})
		policy.close()
		if err != context.Canceled || result != nil {
			t.Fatal("a buffered approval survived cancellation", result, err)
		}
	}
}

func TestBuildAllDriverCodexCancellationPreventsFinalRPCApproval(t *testing.T) {
	audit := mockCodexRuntime(t, "ready")
	ctx, cancel := context.WithCancel(codexRuntimeTestContext(t))
	defer cancel()
	options := codexRuntimeTestOptions(t, "tool")
	requested := false
	_, err := runCodexTurn(ctx, options, nil, func(codexRPCMessage) (any, error) {
		requested = true
		cancel()
		// Simulate an approval callback finishing just as the parent build stops.
		return map[string]string{"decision": "accept"}, nil
	})
	if !requested || err != context.Canceled {
		t.Fatal("canceled callback still continued the runtime", requested, err)
	}
	for _, record := range codexRuntimeRecords(t, audit) {
		if string(record.ID) == `"approval-fixture"` && len(record.Result) > 0 {
			t.Fatal("runtime wrote an approval after its build was canceled")
		}
	}
}

func TestBuildAllDriverACPUsesOfferedOnceAcrossToolsButNotPersistentChoices(t *testing.T) {
	project, profile := acpBuildFixture(t)
	ctx, _ := buildAllDriverContext(t, "acp", project, "all")
	approvals := newACPBuildApprovals(ctx)
	defer approvals.close()
	for index, title := range []string{"read_files: first.html", "run_command: fixture-command"} {
		message := acpApprovalMessage("build-session", title, "execute", fmt.Sprintf("once-%d", index))
		message.ID = json.RawMessage(fmt.Sprintf("%d", index))
		result, err := waitForACPPermission(ctx, profile, project, message, func(map[string]interface{}) { t.Fatal("All asked again for a different tool in this build") }, approvals)
		encoded, _ := json.Marshal(result)
		if err != nil || string(encoded) != fmt.Sprintf(`{"outcome":{"optionId":"once-%d","outcome":"selected"}}`, index) {
			t.Fatalf("native once choice = %s, %v", encoded, err)
		}
	}
	for _, raw := range []string{
		`{"sessionId":"build-session","toolCall":{"title":"run_command"},"options":[{"kind":"allow_always","optionId":"persistent"},{"kind":"reject_once","optionId":"reject"}]}`,
		`{"sessionId":"other-session","toolCall":{"title":"run_command"},"options":[{"kind":"allow_once","optionId":"once"},{"kind":"reject_once","optionId":"reject"}]}`,
	} {
		shown := false
		_, err := waitForACPPermission(ctx, profile, project, acpMessage{Method: "session/request_permission", Params: json.RawMessage(raw)}, func(event map[string]interface{}) {
			shown = true
			p := event["permission"].(map[string]any)
			if buildAllHasChoice(p, "all") {
				t.Error("offered All for a persistent-only or foreign-session request")
			}
			if err := respondToACPPermission(project, p["sessionID"].(string), p["id"].(string), "reject"); err != nil {
				t.Error(err)
			}
		}, approvals)
		if err != nil || !shown {
			t.Fatal("ACP request did not remain manual", err)
		}
	}
	nextCtx, _ := buildAllDriverContext(t, "acp", project, "ask")
	nextApprovals := newACPBuildApprovals(nextCtx)
	defer nextApprovals.close()
	shown := acpCheckApproval(t, nextCtx, profile, project, acpApprovalMessage("build-session", "read_files", "read", "next-build"), nextApprovals, "once", "next-build")
	if !buildAllHasChoice(shown, "all") {
		t.Fatal("managed next build did not offer its build-wide option")
	}
}

func TestBuildAllDriverCodexWaitsForCurrentApprovalTransaction(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			project := t.TempDir()
			ctx, _ := buildAllDriverContext(t, "codex", project, "ask")
			change, err := beginBuildPermissionAll(ctx, "codex", project, "codex:build-thread", "current")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			shown := make(chan struct{}, 1)
			go func() {
				message := codexTestMessage("item/commandExecution/requestApproval", `{"threadId":"build-thread","availableDecisions":["accept","decline"]}`)
				message.ID = json.RawMessage(`7`)
				_, err := waitForCodexInput(ctx, project, message, func(event map[string]interface{}) {
					shown <- struct{}{}
					p := event["permission"].(map[string]any)
					_ = respondToCodexInput(project, p["sessionID"].(string), p["id"].(string), "permission", "reject", nil, nil)
				})
				done <- err
			}()
			select {
			case <-done:
				t.Fatal("a later request ran before the current approval was acknowledged")
			case <-shown:
				t.Fatal("a later request raced the approval transition")
			case <-time.After(30 * time.Millisecond):
			}
			change.finish(success)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if wasShown := len(shown) > 0; wasShown == success {
				t.Fatal("later request did not follow the acknowledged transition outcome")
			}
		})
	}
}

func TestBuildAllDriverOpenCodeUncertainApprovalNeverRetries(t *testing.T) {
	for _, protocol := range []string{"v1", "v2"} {
		for _, failure := range []string{"refused", "disconnected", "redirect"} {
			t.Run(protocol+"/"+failure, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["decision"] != "once" && body["reply"] != "once" {
						t.Error("automatic approval used a persistent provider grant")
					}
					switch failure {
					case "disconnected":
						conn, _, _ := w.(http.Hijacker).Hijack()
						conn.Close()
					case "redirect":
						http.Redirect(w, r, "/unexpected-redirect", http.StatusTemporaryRedirect)
					default:
						http.Error(w, "fixture refusal", http.StatusInternalServerError)
					}
				}))
				defer server.Close()
				setOpenCodeProtocol(server.URL, protocol)
				project := t.TempDir()
				ctx, _ := buildAllDriverContext(t, "opencode", project, "all")
				requests := &openCodeBuildPermissionRequests{ctx: ctx, driver: NewOpenCodeDriver(server.URL), project: project, session: "build-session", pending: map[string]bool{}, attempted: map[string]bool{}}
				if handled, err := requests.tryOnce("permission-1", "build-session"); handled || err == nil {
					t.Fatal("uncertain response was reported as approved", handled, err)
				}
				if handled, err := requests.tryOnce("permission-1", "build-session"); handled || err != nil {
					t.Fatal("an uncertain approval was not retained as manual", handled, err)
				}
				if calls.Load() != 1 {
					t.Fatalf("automatic replies = %d, want exactly one", calls.Load())
				}
			})
		}
	}
}

func TestBuildAllDriverOpenCodeDoesNotReplayEarlierExplicitDecision(t *testing.T) {
	for _, decision := range []string{"once", "always", "reject"} {
		t.Run(decision, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, "true")
			}))
			defer server.Close()
			setOpenCodeProtocol(server.URL, "v1")
			project := t.TempDir()
			ctx, _ := buildAllDriverContext(t, "opencode", project, "ask")
			driver := NewOpenCodeDriver(server.URL)
			requests := &openCodeBuildPermissionRequests{ctx: ctx, driver: driver, project: project, session: "build-session", pending: map[string]bool{}, attempted: map[string]bool{}}
			if handled, err := requests.tryOnce("permission-a", "build-session"); handled || err != nil {
				t.Fatal("Ask bypassed manual review", handled, err)
			}
			if err := driver.respondToPermission(ctx, "build-session", "permission-a", decision, project); err != nil {
				t.Fatal(err)
			}
			if handled, err := requests.tryOnce("permission-b", "build-session"); handled || err != nil {
				t.Fatal("Ask bypassed the later request", handled, err)
			}
			change, err := beginBuildPermissionAll(ctx, "opencode", project, "build-session", "permission-b")
			if err != nil {
				t.Fatal(err)
			}
			err = driver.respondToPermission(ctx, "build-session", "permission-b", "once", project)
			change.finish(err == nil)
			if err != nil {
				t.Fatal(err)
			}
			for id := range requests.pending {
				if handled, err := requests.tryOnce(id, "build-session"); !handled || err != nil {
					t.Fatal("an explicit decision remained pending", id, handled, err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("answered request replayed after All: HTTP replies = %d", calls.Load())
			}
			if handled, err := requests.tryOnce("permission-c", "build-session"); !handled || err != nil {
				t.Fatal("new build permission did not use once", handled, err)
			}
			if calls.Load() != 3 {
				t.Fatal("new request did not receive exactly one reply", calls.Load())
			}
		})
	}
}

func TestBuildAllDriverRejectsInvalidPermissionModeBeforeSideEffects(t *testing.T) {
	for _, handler := range []http.HandlerFunc{openCodeRefineHandler, openCodeVerifyHandler} {
		for _, mode := range []string{"always", "ALL", " all ", "bypassPermissions"} {
			body, _ := json.Marshal(OpenCodeAgentRequest{PermissionMode: mode, ElevenLabsUseSavedKey: true, ImageUseSavedKey: true})
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", strings.NewReader(string(body))))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "permissionMode") {
				t.Fatalf("invalid mode reached credentials/project/provider validation: %d %s", w.Code, w.Body.String())
			}
		}
	}
}

func TestBuildAllDriverPreviouslyWaitingPermissionRechecksCommittedMode(t *testing.T) {
	for _, driver := range []string{"codex", "acp"} {
		t.Run(driver, func(t *testing.T) {
			project, profile := acpBuildFixture(t)
			ctx, _ := buildAllDriverContext(t, driver, project, "ask")
			shown := make(chan map[string]any, 1)
			done := make(chan error, 1)
			go func() {
				emit := func(event map[string]interface{}) { shown <- event["permission"].(map[string]any) }
				if driver == "codex" {
					message := codexTestMessage("item/commandExecution/requestApproval", `{"threadId":"thread","availableDecisions":["accept","decline"]}`)
					message.ID = json.RawMessage(`8`)
					_, err := waitForCodexInput(ctx, project, message, emit)
					done <- err
				} else {
					approvals := newACPBuildApprovals(ctx)
					defer approvals.close()
					message := acpApprovalMessage("session", "run_command", "execute", "native-once")
					message.ID = json.RawMessage(`8`)
					_, err := waitForACPPermission(ctx, profile, project, message, emit, approvals)
					done <- err
				}
			}()
			pending := <-shown
			if !buildAllHasChoice(pending, "all") {
				t.Fatal("managed pending request omitted All")
			}
			change, err := beginBuildPermissionAll(ctx, driver, project, pending["sessionID"].(string), "other-pending")
			if err != nil {
				t.Fatal(err)
			}
			change.finish(true)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("already-emitted permission did not recheck the committed build choice")
			}
		})
	}
}

func TestBuildAllDriverManualOnceReservesOriginalWireRequest(t *testing.T) {
	for _, driver := range []string{"codex", "acp"} {
		t.Run(driver, func(t *testing.T) {
			project, profile := acpBuildFixture(t)
			ctx, _ := buildAllDriverContext(t, driver, project, "ask")
			approvals := newACPBuildApprovals(ctx)
			defer approvals.close()
			message := codexTestMessage("item/commandExecution/requestApproval", `{"threadId":"thread","availableDecisions":["accept","decline"]}`)
			message.ID = json.RawMessage(`42`)
			acp := acpApprovalMessage("session", "run_command", "execute", "native-once")
			acp.ID = message.ID
			handle := func(emit func(map[string]interface{})) error {
				if driver == "codex" {
					_, err := waitForCodexInput(ctx, project, message, emit)
					return err
				}
				_, err := waitForACPPermission(ctx, profile, project, acp, emit, approvals)
				return err
			}
			err := handle(func(event map[string]interface{}) {
				p := event["permission"].(map[string]any)
				change, err := beginBuildPermissionAll(ctx, driver, project, p["sessionID"].(string), p["id"].(string))
				if err != nil {
					t.Error(err)
					return
				}
				if driver == "codex" {
					err = respondToCodexInput(project, p["sessionID"].(string), p["id"].(string), "permission", "once", nil, nil)
				} else {
					err = respondToACPPermission(project, p["sessionID"].(string), p["id"].(string), "once")
				}
				change.finish(err == nil)
				if err != nil {
					t.Error(err)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			shown := false
			err = handle(func(event map[string]interface{}) {
				shown = true
				p := event["permission"].(map[string]any)
				if driver == "codex" {
					_ = respondToCodexInput(project, p["sessionID"].(string), p["id"].(string), "permission", "reject", nil, nil)
				} else {
					_ = respondToACPPermission(project, p["sessionID"].(string), p["id"].(string), "reject")
				}
			})
			if err != nil || !shown {
				t.Fatal("manual All's original RPC request received another automatic once", err)
			}
		})
	}
}

type buildAllSSEWriter struct {
	*httptest.ResponseRecorder
	onEvent func(map[string]any)
}

func (w *buildAllSSEWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	for _, line := range strings.Split(string(data), "\n") {
		var event map[string]any
		if strings.HasPrefix(line, "data: ") && json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil {
			w.onEvent(event)
		}
	}
	return n, err
}

func TestBuildAllDriverOpenCodeReconcilesWaitingRequestsAndRetainsRefusal(t *testing.T) {
	for _, scenario := range []string{"auto-success", "auto-refusal", "manual-refusal"} {
		t.Run(scenario, func(t *testing.T) {
			refuseEarlier := scenario != "auto-success"
			project := t.TempDir()
			ctx, _ := buildAllDriverContext(t, "opencode", project, "ask")
			completed := make(chan struct{})
			var onceA, onceB, rejects, foreign atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/event" {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range []string{
						`{"type":"permission.asked","properties":{"id":"foreign","sessionID":"other","title":"Other session"}}`,
						`{"type":"question.asked","properties":{"id":"question-1","sessionID":"build-session","questions":[{"question":"Which page?","options":[{"label":"Home"}]}]}}`,
						`{"type":"permission.asked","properties":{"id":"permission-a","sessionID":"build-session","title":"First command"}}`,
						`{"type":"permission.asked","properties":{"id":"permission-b","sessionID":"build-session","title":"Second command"}}`,
					} {
						fmt.Fprintf(w, "data: %s\n\n", event)
						w.(http.Flusher).Flush()
					}
					select {
					case <-completed:
					case <-r.Context().Done():
						return
					}
					fmt.Fprint(w, "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"build-session\"}}\n\n")
					return
				}
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				switch r.URL.Path {
				case "/permission/permission-a/reply":
					if body["reply"] == "reject" {
						rejects.Add(1)
						close(completed)
					} else {
						onceA.Add(1)
						if refuseEarlier {
							http.Error(w, "fixture refusal", 500)
							return
						}
						close(completed)
					}
				case "/permission/permission-b/reply":
					onceB.Add(1)
				default:
					foreign.Add(1)
					t.Errorf("unexpected permission/question/abort endpoint: %s", r.URL.Path)
				}
				fmt.Fprint(w, "true")
			}))
			defer server.Close()
			setOpenCodeProtocol(server.URL, "v1")
			driver := NewOpenCodeDriver(server.URL)
			manualA, questions := 0, 0
			response := &buildAllSSEWriter{ResponseRecorder: httptest.NewRecorder(), onEvent: func(event map[string]any) {
				if event["question"] != nil {
					questions++
				}
				p, ok := event["permission"].(map[string]any)
				if !ok {
					return
				}
				id := p["id"].(string)
				if id == "permission-a" {
					manualA++
					if manualA == 1 && scenario == "manual-refusal" {
						if err := driver.respondToPermission(ctx, "build-session", id, "once", project); err == nil {
							t.Error("manual fixture refusal was reported as confirmed")
						}
					}
					if manualA == 2 {
						if err := driver.respondToPermission(ctx, "build-session", id, "reject", project); err != nil {
							t.Error(err)
						}
					}
				} else if id == "permission-b" {
					change, err := beginBuildPermissionAll(ctx, "opencode", project, "build-session", id)
					if err != nil {
						t.Error(err)
						return
					}
					err = driver.respondToPermission(ctx, "build-session", id, "once", project)
					change.finish(err == nil)
					if err != nil {
						t.Error(err)
					}
				} else {
					t.Error("displayed another session's permission")
				}
			}}
			closed := make(chan struct{})
			close(closed)
			success, _, _, errText := driver.streamEventsAndWaitForCompletion(ctx, response, response, project, "build-session", closed)
			if !success || errText != "" || onceA.Load() != 1 || onceB.Load() != 1 || foreign.Load() != 0 || questions != 1 {
				t.Fatalf("bad stream outcome: %t %q A=%d B=%d other=%d questions=%d", success, errText, onceA.Load(), onceB.Load(), foreign.Load(), questions)
			}
			if refuseEarlier && (manualA != 2 || rejects.Load() != 1) {
				t.Fatal("earlier uncertain approval was not re-exposed for manual refusal")
			}
		})
	}
}

func TestClaudeCodeAllArgumentsApplyToOnlyTheCurrentPrintProcess(t *testing.T) {
	for _, model := range []string{"sonnet", "haiku"} {
		args := claudeCodeArguments(model, claudeCodeSessionPrefix+testClaudeCodeSession, "all")
		if args[5] != "bypassPermissions" || !strings.Contains(strings.Join(args, " "), "--resume "+testClaudeCodeSession) || strings.Contains(strings.Join(args, " "), "--bg") {
			t.Fatalf("wrong process scope: %v", args)
		}
		for _, mode := range []string{"", "ask", "unknown"} {
			if args := claudeCodeArguments(model, claudeCodeSessionPrefix+testClaudeCodeSession, mode); args[5] == "bypassPermissions" {
				t.Fatal("another process inherited bypass permissions", args)
			}
		}
	}
}

func TestClaudeCodeAllRefinePassesOptInWithoutWritingSavedRules(t *testing.T) {
	project := t.TempDir()
	bin := fakeClaudeCode(t, `printf '%s\n' "$@" > argv.txt
cat > /dev/null
printf '%s\n' '{"type":"result","subtype":"success","result":"Fixture complete"}'
`)
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", bin)
	for _, mode := range []string{"all", "ask"} {
		w := httptest.NewRecorder()
		status, _ := runClaudeCodeRefine(w, httptest.NewRequest(http.MethodPost, "/opencode/refine", nil), OpenCodeAgentRequest{ProjectPath: project, Model: "claude-code/haiku", PermissionMode: mode}, "Fixture only")
		if status != "completed" {
			t.Fatal(status, w.Body.String())
		}
		args, _ := os.ReadFile(filepath.Join(project, "argv.txt"))
		if bypassed := strings.Contains(string(args), "bypassPermissions"); bypassed != (mode == "all") {
			t.Fatal("CLI mode did not follow this request", string(args))
		}
		if _, err := os.Stat(filepath.Join(project, ".claude")); !os.IsNotExist(err) {
			t.Fatal("run wrote saved permission configuration")
		}
	}
}
