package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type codexRuntimeTestRecord struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params map[string]any  `json:"params"`
	Error  json.RawMessage `json:"error"`
	Result json.RawMessage `json:"result"`
}

func mockCodexRuntime(t *testing.T, scenario string) string {
	t.Helper()
	audit := filepath.Join(t.TempDir(), "protocol.jsonl")
	t.Setenv("GLOWBOM_CODEX_BIN", os.Args[0])
	previousRuntime, previousCommand := glowbomCodex, codexRuntimeCommand
	runtime := &codexRuntime{}
	glowbomCodex = runtime
	codexRuntimeCommand = func(ctx context.Context, binary string, args ...string) *exec.Cmd {
		helperArgs := append([]string{"-test.run=^TestCodexRuntimeHelperProcess$", "--", "--glowbom-runtime-scenario=" + scenario, "--glowbom-runtime-audit=" + audit}, args...)
		return exec.CommandContext(ctx, binary, helperArgs...)
	}
	t.Cleanup(func() {
		runtime.close()
		glowbomCodex, codexRuntimeCommand = previousRuntime, previousCommand
	})
	return audit
}

func codexRuntimeRecords(t *testing.T, path string) []codexRuntimeTestRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []codexRuntimeTestRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record codexRuntimeTestRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid protocol audit: %v", err)
		}
		records = append(records, record)
	}
	return records
}

func codexRuntimeTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func codexRuntimeTestOptions(t *testing.T, text string) codexRunOptions {
	t.Helper()
	return codexRunOptions{Directory: t.TempDir(), Model: "test-model", Input: []map[string]any{{"type": "text", "text": text}}}
}

// The helper acts as a separate stdio server, including unsolicited requests.
func TestCodexRuntimeHelperProcess(t *testing.T) {
	scenario, auditPath := "", ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--glowbom-runtime-scenario=") {
			scenario = strings.TrimPrefix(arg, "--glowbom-runtime-scenario=")
		}
		if strings.HasPrefix(arg, "--glowbom-runtime-audit=") {
			auditPath = strings.TrimPrefix(arg, "--glowbom-runtime-audit=")
		}
	}
	if scenario == "" {
		return
	}
	audit, err := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(11)
	}
	log := func(value any) {
		data, _ := json.Marshal(value)
		if _, err := audit.Write(append(data, '\n')); err != nil {
			os.Exit(12)
		}
	}
	var inheritedSecrets []string
	for _, name := range []string{"GLOWBOM_SERVER_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"} {
		if os.Getenv(name) != "" {
			inheritedSecrets = append(inheritedSecrets, name)
		}
	}
	log(map[string]any{"method": "test/launch", "params": map[string]any{"args": os.Args, "pid": os.Getpid(), "inheritedSecrets": inheritedSecrets}})
	send := func(value any) {
		if json.NewEncoder(os.Stdout).Encode(value) != nil {
			os.Exit(13)
		}
	}
	event := func(method, thread string, extra map[string]any) {
		params := map[string]any{"threadId": thread, "turnId": "turn-" + thread}
		for key, value := range extra {
			params[key] = value
		}
		send(map[string]any{"method": method, "params": params})
	}
	complete := func(thread, status string) {
		event("turn/completed", thread, map[string]any{"turn": map[string]any{"id": "turn-" + thread, "status": status, "error": map[string]string{"message": "fixture turn failed"}}})
	}
	threads := 0
	initialized := false
	var parallel []string
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request codexRuntimeTestRecord
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(14)
		}
		log(request)
		if request.Method == "" {
			continue
		}
		if request.Method == "initialized" {
			initialized = true
			continue
		}
		if request.Method != "initialize" && !initialized {
			os.Exit(15)
		}
		var result any = map[string]any{}
		switch request.Method {
		case "initialize":
			result = map[string]string{"userAgent": "fake-codex"}
		case "account/read":
			result = map[string]any{"account": map[string]string{"type": "chatgpt"}, "requiresOpenaiAuth": true}
			if scenario == "logged-out" {
				result = map[string]any{"account": nil, "requiresOpenaiAuth": true}
			}
		case "account/login/start":
			result = map[string]string{"authUrl": "https://auth.openai.com/authorize?state=fixture", "loginId": "login-fixture"}
		case "account/login/cancel":
			result = map[string]string{"status": "canceled"}
		case "test/login-complete":
			send(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "login-fixture", "success": true}})
		case "test/crash":
			os.Exit(19)
		case "model/list":
			if scenario == "current-models" {
				result = map[string]any{"data": []any{
					map[string]any{"model": "gpt-5.6-sol"},
					map[string]any{"model": "gpt-6-luna"},
					map[string]any{"model": "gpt-6-astra"},
					map[string]any{"model": "gpt-6.1-sol", "isDefault": true},
					map[string]any{"model": "gpt-6-sol"},
				}}
				break
			}
			if request.Params["cursor"] == nil {
				result = map[string]any{"data": []any{
					map[string]any{"id": "catalog-id", "model": "vision-model", "displayName": "Vision", "inputModalities": []string{"text", "image"},
						"isDefault": true, "defaultReasoningEffort": "medium", "supportedReasoningEfforts": []any{
							map[string]string{"reasoningEffort": "low"}, map[string]string{"reasoningEffort": "medium"},
							map[string]string{"reasoningEffort": "ultra"}, map[string]string{"reasoningEffort": "medium"},
							map[string]string{"reasoningEffort": ""},
						}},
					map[string]any{"model": "hidden-model", "hidden": true},
				}, "nextCursor": "page-two"}
			} else {
				result = map[string]any{"data": []any{
					map[string]any{"model": "vision-model"},
					map[string]any{"id": "text-model", "inputModalities": []string{"text"}},
				}, "nextCursor": nil}
			}
		case "config/read":
			result = map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"inherited-server": map[string]any{}, "server.with.dots": map[string]any{}}}}
		case "thread/start", "thread/resume":
			threads++
			thread := fmt.Sprintf("thread-%d", threads)
			if resumed, ok := request.Params["threadId"].(string); ok {
				thread = resumed
			}
			result = map[string]any{"thread": map[string]string{"id": thread}}
		case "mcpServerStatus/list":
			result = map[string]any{"data": []any{}, "nextCursor": nil}
		case "turn/start":
			thread := request.Params["threadId"].(string)
			input := request.Params["input"].([]any)
			text := input[0].(map[string]any)["text"].(string)
			send(map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]string{"id": "turn-" + thread}}})
			switch text {
			case "crash":
				os.Exit(19)
			case "wait":
				event("item/agentMessage/delta", thread, map[string]any{"delta": "started"})
			case "tool":
				send(map[string]any{"id": "approval-fixture", "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": thread, "turnId": "turn-" + thread}})
			case "failed":
				complete(thread, "failed")
			case "terminal-error":
				event("error", thread, map[string]any{"willRetry": false, "error": map[string]string{"message": "fixture terminal error"}})
			case "parallel":
				parallel = append(parallel, thread)
				if len(parallel) == 2 {
					for _, target := range []string{parallel[1], parallel[0]} {
						event("item/agentMessage/delta", target, map[string]any{"delta": target})
					}
					for _, target := range parallel {
						complete(target, "completed")
					}
					parallel = nil
				}
			default:
				event("item/agentMessage/delta", thread, map[string]any{"delta": thread})
				complete(thread, "completed")
			}
			continue
		case "turn/interrupt":
		default:
			send(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "unknown fixture method"}})
			continue
		}
		send(map[string]any{"id": request.ID, "result": result})
	}
	os.Exit(0)
}

func TestCodexRuntimeHandshakeAndModelCatalog(t *testing.T) {
	audit := mockCodexRuntime(t, "ready")
	models, err := codexChatModels(codexRuntimeTestContext(t))
	if err != nil || len(models) != 2 {
		t.Fatalf("catalog returned %v, %v", models, err)
	}
	if models[0].ID != "codex/vision-model" || models[0].Name != "Vision" || !models[0].Images || !models[0].Build || models[0].Provider != "Codex" || models[1].ID != "codex/text-model" || models[1].Images {
		t.Fatalf("incorrect catalog routing or capabilities: %+v", models)
	}
	if !models[0].IsDefault || models[0].DefaultReasoningEffort != "medium" || strings.Join(models[0].ReasoningEfforts, ",") != "low,medium,ultra" || len(models[1].ReasoningEfforts) != 0 {
		t.Fatalf("lost or invented reasoning capabilities: %+v", models)
	}
	records := codexRuntimeRecords(t, audit)
	if len(records) < 6 || records[1].Method != "initialize" || records[2].Method != "initialized" {
		t.Fatalf("missing app-server handshake: %+v", records)
	}
	pages := 0
	for _, record := range records {
		if record.Method == "model/list" {
			pages++
			if record.Params["includeHidden"] != false || (pages == 2 && record.Params["cursor"] != "page-two") {
				t.Fatalf("incorrect catalog pagination: %+v", record.Params)
			}
		}
	}
	if pages != 2 {
		t.Fatalf("expected two catalog pages, got %d", pages)
	}
}

func TestCodexRuntimeCurrentModelsComeFirst(t *testing.T) {
	mockCodexRuntime(t, "current-models")
	models, err := codexChatModels(codexRuntimeTestContext(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex/gpt-6.1-sol", "codex/gpt-6-astra", "codex/gpt-6-sol", "codex/gpt-6-luna", "codex/gpt-5.6-sol"}
	if len(models) != len(want) {
		t.Fatalf("wrong catalog size: %v", models)
	}
	for i, model := range models {
		if model.ID != want[i] {
			t.Fatalf("unexpected model at %d: %s", i, model.ID)
		}
	}
}

func TestCodexRuntimeEffortForChatBuildAndResume(t *testing.T) {
	for _, mode := range []string{"chat", "build", "resume"} {
		t.Run(mode, func(t *testing.T) {
			audit := mockCodexRuntime(t, "ready")
			options := codexRuntimeTestOptions(t, "success")
			options.Model, options.ReasoningEffort = "vision-model", "ultra"
			options.ChatOnly = mode == "chat"
			if mode == "resume" {
				options.ThreadID = "saved-thread"
			}
			if _, err := runCodexTurn(codexRuntimeTestContext(t), options, nil, nil); err != nil {
				t.Fatal(err)
			}
			started := false
			for _, record := range codexRuntimeRecords(t, audit) {
				if record.Method == "thread/start" || record.Method == "thread/resume" {
					config := record.Params["config"].(map[string]any)
					if config["model_reasoning_effort"] != "ultra" {
						t.Fatalf("thread inherited stale effort: %+v", config)
					}
				}
				if record.Method == "turn/start" {
					started = true
					if record.Params["effort"] != "ultra" || record.Params["model"] != "vision-model" {
						t.Fatalf("turn lost selected model or effort: %+v", record.Params)
					}
					if mode == "chat" && record.Params["summary"] != "auto" {
						t.Fatalf("chat did not request public reasoning summaries: %+v", record.Params)
					}
					if mode != "chat" && record.Params["summary"] != nil {
						t.Fatal("chat summary preference changed a build turn")
					}
				}
			}
			if !started {
				t.Fatal("no turn was started")
			}
		})
	}
}

func TestCodexRuntimeRejectsUnsupportedEffortBeforeTurn(t *testing.T) {
	for _, effort := range []string{"max", "garbage"} {
		t.Run(effort, func(t *testing.T) {
			audit := mockCodexRuntime(t, "ready")
			options := codexRuntimeTestOptions(t, "success")
			options.Model, options.ReasoningEffort = "vision-model", effort
			if _, err := runCodexTurn(codexRuntimeTestContext(t), options, nil, nil); err == nil || !strings.Contains(err.Error(), "effort is not available") {
				t.Fatalf("invalid effort was not rejected: %v", err)
			}
			for _, record := range codexRuntimeRecords(t, audit) {
				if record.Method == "turn/start" || record.Method == "thread/start" {
					t.Fatal("unsupported effort started a task")
				}
			}
		})
	}
}

func TestCodexRuntimeLoggedOutHasNoModels(t *testing.T) {
	audit := mockCodexRuntime(t, "logged-out")
	models, err := codexChatModels(codexRuntimeTestContext(t))
	if err != nil || len(models) != 0 {
		t.Fatalf("logged-out catalog returned %v, %v", models, err)
	}
	for _, record := range codexRuntimeRecords(t, audit) {
		if record.Method == "model/list" {
			t.Fatal("queried models before connecting an account")
		}
	}
}

func TestCodexRuntimeConcurrentThreadsStaySeparate(t *testing.T) {
	mockCodexRuntime(t, "ready")
	ctx := codexRuntimeTestContext(t)
	type outcome struct {
		thread, text string
		err          error
	}
	results := make(chan outcome, 2)
	for range 2 {
		options := codexRuntimeTestOptions(t, "parallel")
		go func() {
			text := ""
			thread, err := runCodexTurn(ctx, options, func(message codexRPCMessage) error {
				if message.Method == "item/agentMessage/delta" {
					var params struct {
						Delta string `json:"delta"`
					}
					_ = json.Unmarshal(message.Params, &params)
					text += params.Delta
				}
				return nil
			}, nil)
			results <- outcome{thread, text, err}
		}()
	}
	seen := map[string]bool{}
	for range 2 {
		result := <-results
		if result.err != nil || result.thread == "" || result.text != result.thread || seen[result.thread] {
			t.Fatalf("crossed or lost thread event: %+v", result)
		}
		seen[result.thread] = true
	}
}

func TestCodexRuntimeResumeOverridesProjectAndPolicy(t *testing.T) {
	audit := mockCodexRuntime(t, "ready")
	options := codexRuntimeTestOptions(t, "success")
	options.ThreadID, options.Instructions = "saved-thread", "Use the currently selected project."
	thread, err := runCodexTurn(codexRuntimeTestContext(t), options, nil, nil)
	if err != nil || thread != options.ThreadID {
		t.Fatalf("resume failed: thread=%q err=%v", thread, err)
	}
	resumed, started := false, false
	for _, record := range codexRuntimeRecords(t, audit) {
		switch record.Method {
		case "config/read":
			if record.Params["cwd"] != options.Directory {
				t.Fatal("read inherited config from a stale project")
			}
		case "thread/start":
			t.Fatal("saved build was replaced by a new thread")
		case "thread/resume":
			resumed = true
			if record.Params["threadId"] != options.ThreadID || record.Params["cwd"] != options.Directory || record.Params["model"] != options.Model || record.Params["developerInstructions"] != options.Instructions || record.Params["approvalPolicy"] != "on-request" || record.Params["sandbox"] != "workspace-write" {
				t.Fatalf("resume inherited stale project or policy: %+v", record.Params)
			}
		case "turn/start":
			started = true
			policy := record.Params["sandboxPolicy"].(map[string]any)
			roots := policy["writableRoots"].([]any)
			if record.Params["threadId"] != options.ThreadID || record.Params["cwd"] != options.Directory || record.Params["model"] != options.Model || len(roots) != 1 || roots[0] != options.Directory || record.Params["approvalPolicy"] != "on-request" {
				t.Fatalf("turn inherited stale project or policy: %+v", record.Params)
			}
		}
	}
	if !resumed || !started {
		t.Fatal("resume did not start a turn")
	}
}

func TestCodexRuntimeCancellationInterruptsAndRestartRecovers(t *testing.T) {
	audit := mockCodexRuntime(t, "ready")
	ctx, cancel := context.WithCancel(codexRuntimeTestContext(t))
	defer cancel()
	options := codexRuntimeTestOptions(t, "wait")
	started, finished := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		_, err := runCodexTurn(ctx, options, func(message codexRPCMessage) error {
			if message.Method == "item/agentMessage/delta" {
				started <- struct{}{}
			}
			return nil
		}, nil)
		finished <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("turn did not start")
	}
	if err := glowbomCodex.restart(ctx); !errors.Is(err, errCodexBusy) {
		t.Fatalf("restart did not protect active turn: %v", err)
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled turn returned %v", err)
	}
	interrupted := false
	for _, record := range codexRuntimeRecords(t, audit) {
		if record.Method == "turn/interrupt" {
			interrupted = record.Params["threadId"] == "thread-1" && record.Params["turnId"] == "turn-thread-1"
		}
	}
	if !interrupted {
		t.Fatal("cancellation did not interrupt the correct turn")
	}
	if err := glowbomCodex.restart(codexRuntimeTestContext(t)); err != nil {
		t.Fatal(err)
	}
	options.Input[0]["text"] = "success"
	if _, err := runCodexTurn(codexRuntimeTestContext(t), options, nil, nil); err != nil {
		t.Fatalf("turn failed after restart: %v", err)
	}
}

func TestCodexRuntimeFailuresDoNotReportSuccess(t *testing.T) {
	for _, scenario := range []string{"failed", "terminal-error", "crash"} {
		t.Run(scenario, func(t *testing.T) {
			mockCodexRuntime(t, "ready")
			options := codexRuntimeTestOptions(t, scenario)
			if _, err := runCodexTurn(codexRuntimeTestContext(t), options, nil, nil); err == nil {
				t.Fatal("failed turn reported success")
			}
			if err := glowbomCodex.restart(codexRuntimeTestContext(t)); err != nil {
				t.Fatal(err)
			}
			options.Input[0]["text"] = "success"
			if _, err := runCodexTurn(codexRuntimeTestContext(t), options, nil, nil); err != nil {
				t.Fatalf("failed to recover: %v", err)
			}
		})
	}
}

func TestCodexRuntimeProcessExitCancelsPendingRequest(t *testing.T) {
	mockCodexRuntime(t, "ready")
	ctx, cancel := context.WithCancel(codexRuntimeTestContext(t))
	defer cancel()
	options := codexRuntimeTestOptions(t, "tool")
	requested, finished := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		_, err := runCodexTurn(ctx, options, nil, func(message codexRPCMessage) (any, error) {
			if message.Context == nil {
				return nil, errors.New("permission request omitted its process lifetime context")
			}
			requested <- struct{}{}
			<-message.Context.Done()
			return nil, message.Context.Err()
		})
		finished <- err
	}()
	select {
	case <-requested:
	case err := <-finished:
		t.Fatalf("turn finished before requesting approval: %v", err)
	case <-ctx.Done():
		t.Fatal("turn never requested approval")
	}
	p, err := glowbomCodex.process(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.send(map[string]any{"method": "test/crash"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("crashed process reported success")
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-finished
		t.Fatal("process exit left its approval waiting")
	}
	glowbomCodex.mu.Lock()
	active := glowbomCodex.active
	glowbomCodex.mu.Unlock()
	if active != 0 {
		t.Fatalf("process exit left %d active turns", active)
	}
	if err := glowbomCodex.restart(codexRuntimeTestContext(t)); err != nil {
		t.Fatalf("pending approval prevented recovery: %v", err)
	}
}

func TestCodexRuntimeCompletedLoginRefreshesIdleChat(t *testing.T) {
	mockCodexRuntime(t, "ready")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	ctx := codexRuntimeTestContext(t)
	oldChat, err := glowbomCodex.process(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/codex/login", nil)
	r.Header.Set("X-Glowbom-Token", "fixture-token")
	w := httptest.NewRecorder()
	codexRuntimeHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	waiting := readCodexStatus(ctx)
	if waiting.Connected || !waiting.LoginPending {
		t.Fatalf("status treated the old account as the new login: %+v", waiting)
	}
	glowbomCodex.mu.Lock()
	pending, chat := glowbomCodex.loginID != "", glowbomCodex.processes[1]
	glowbomCodex.mu.Unlock()
	if !pending || chat != oldChat {
		t.Fatal("existing connected account was mistaken for a completed new login")
	}
	p, err := glowbomCodex.process(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.call(ctx, "test/login-complete", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	status := readCodexStatus(ctx)
	if !status.Connected {
		t.Fatalf("completed login did not connect: %+v", status)
	}
	glowbomCodex.mu.Lock()
	pending, chat = glowbomCodex.loginID != "", glowbomCodex.processes[1]
	glowbomCodex.mu.Unlock()
	if pending || chat != nil {
		t.Fatal("completed login retained the previous chat account")
	}
	select {
	case <-oldChat.done:
	default:
		t.Fatal("old chat process was not retired")
	}
	newChat, err := glowbomCodex.process(ctx, true)
	if err != nil || newChat == oldChat {
		t.Fatalf("chat did not recreate after login: %v", err)
	}
}

func TestCodexRuntimeChatIsolationAndToolDenial(t *testing.T) {
	audit := mockCodexRuntime(t, "ready")
	for _, name := range []string{"GLOWBOM_SERVER_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"} {
		t.Setenv(name, "must-not-reach-codex")
	}
	options := codexRuntimeTestOptions(t, "success")
	if _, err := runCodexTurn(codexRuntimeTestContext(t), options, nil, nil); err != nil {
		t.Fatal(err)
	}
	options.ChatOnly, options.ThreadID = true, "build-thread-must-not-resume"
	options.Input[0]["text"] = "tool"
	_, err := runCodexTurn(codexRuntimeTestContext(t), options, nil, func(codexRPCMessage) (any, error) {
		t.Fatal("chat requested a user tool approval")
		return map[string]string{"decision": "accept"}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "requested a tool during chat") {
		t.Fatalf("chat accepted a tool request: %v", err)
	}
	launches, chatThreads, denials := 0, 0, 0
	for _, record := range codexRuntimeRecords(t, audit) {
		switch record.Method {
		case "test/launch":
			launches++
			if inherited, ok := record.Params["inheritedSecrets"].([]any); ok && len(inherited) != 0 {
				t.Fatalf("Codex inherited backend credentials or API routing: %v", inherited)
			}
			args, _ := json.Marshal(record.Params["args"])
			if !strings.Contains(string(args), `model_provider=\"openai\"`) {
				t.Fatal("Codex did not select the native OpenAI provider")
			}
			if launches == 2 {
				for _, flag := range []string{"features.code_mode_host=false", "features.shell_tool=false", "features.unified_exec=false", "features.apps=false", "features.hooks=false", "features.image_generation=false"} {
					if !strings.Contains(string(args), flag) {
						t.Fatalf("chat process missing tool isolation flag %s", flag)
					}
				}
			} else {
				for _, flag := range []string{"features.code_mode_host=true", "features.shell_tool=true", "features.unified_exec=true"} {
					if !strings.Contains(string(args), flag) {
						t.Fatalf("build process missing built-in tool flag %s", flag)
					}
				}
			}
		case "thread/start":
			config := record.Params["config"].(map[string]any)
			servers := config["mcp_servers"].(map[string]any)
			for _, name := range []string{"inherited-server", "server.with.dots"} {
				if servers[name].(map[string]any)["enabled"] != false {
					t.Fatal("inherited MCP server was enabled")
				}
			}
			if record.Params["ephemeral"] == true {
				chatThreads++
				if record.Params["approvalPolicy"] != "never" || record.Params["sandbox"] != "read-only" {
					t.Fatalf("unsafe chat thread: %+v", record.Params)
				}
			}
		case "thread/resume":
			t.Fatal("chat resumed a build conversation")
		case "turn/start":
			policy := record.Params["sandboxPolicy"].(map[string]any)
			if policy["networkAccess"] != false {
				t.Fatal("turn enabled network")
			}
			if record.Params["approvalPolicy"] == "never" && policy["type"] != "readOnly" {
				t.Fatal("chat turn was not read-only")
			}
			if record.Params["approvalPolicy"] == "on-request" {
				roots, ok := policy["writableRoots"].([]any)
				if !ok || len(roots) != 1 || roots[0] != options.Directory || policy["excludeSlashTmp"] != true || policy["excludeTmpdirEnvVar"] != true {
					t.Fatalf("build sandbox escaped the selected project: %+v", policy)
				}
			}
		case "":
			if string(record.ID) == `"approval-fixture"` {
				denials++
				if len(record.Error) == 0 || string(record.Error) == "null" || (len(record.Result) > 0 && string(record.Result) != "null") {
					t.Fatal("chat approved a tool request")
				}
			}
		}
	}
	if launches != 2 || chatThreads != 1 || denials != 1 {
		t.Fatalf("isolation missing: processes=%d chatThreads=%d denials=%d", launches, chatThreads, denials)
	}
}

func TestCodexRuntimeHTTPAuthenticationAndMethods(t *testing.T) {
	audit := mockCodexRuntime(t, "ready")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	for _, tc := range []struct {
		path, method, token string
		status              int
	}{
		{"/codex/status", http.MethodGet, "", http.StatusUnauthorized},
		{"/codex/restart", http.MethodPost, "wrong", http.StatusUnauthorized},
		{"/codex/status", http.MethodPost, "fixture-token", http.StatusMethodNotAllowed},
		{"/codex/login", http.MethodGet, "fixture-token", http.StatusMethodNotAllowed},
		{"/codex/restart", http.MethodGet, "fixture-token", http.StatusMethodNotAllowed},
		{"/codex/login/cancel", http.MethodGet, "fixture-token", http.StatusMethodNotAllowed},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("X-Glowbom-Token", tc.token)
		w := httptest.NewRecorder()
		codexRuntimeHandler(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: got %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	if _, err := os.Stat(audit); !os.IsNotExist(err) {
		t.Fatal("rejected HTTP request started Codex")
	}
	for _, tc := range []struct {
		path, method, body string
		status             int
	}{
		{"/codex/status", http.MethodGet, "", http.StatusOK},
		{"/codex/login", http.MethodPost, "", http.StatusOK},
		{"/codex/login/cancel", http.MethodPost, `{}`, http.StatusBadRequest},
		{"/codex/login/cancel", http.MethodPost, `{"loginId":"login-fixture"}`, http.StatusOK},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("X-Glowbom-Token", "fixture-token")
		w := httptest.NewRecorder()
		codexRuntimeHandler(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("account response allows caching")
		}
		if tc.path == "/codex/login" && !strings.Contains(w.Body.String(), `"authorizationURL":"https://auth.openai.com/`) {
			t.Fatal("missing sign-in URL")
		}
	}
}
