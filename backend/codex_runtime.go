package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Codex owns its credentials and runs locally over private stdio pipes.
// Chat and Build use separate processes so chat cannot inherit build tools.
var glowbomCodex = &codexRuntime{}
var codexRuntimeCommand = exec.CommandContext

var errCodexStopped = errors.New("The Codex server stopped. Retry your message or restart Codex in Tools.")
var errCodexBusy = errors.New("Wait for your Codex tasks to finish, or stop them before restarting Codex.")

type codexRPCMessage struct {
	Context context.Context `json:"-"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type codexRuntime struct {
	mu        sync.Mutex
	processes [2]*codexProcess
	active    int
	loginID   string
}

type codexProcess struct {
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	input     io.WriteCloser
	workdir   string
	version   string
	done      chan struct{}
	closeOnce sync.Once
	writeMu   sync.Mutex
	mu        sync.Mutex
	nextID    int
	pending   map[string]chan codexRPCMessage
	streams   map[string]chan codexRPCMessage
	logins    map[string]bool
}

type codexRunOptions struct {
	Directory, Model, ThreadID, Instructions string
	ReasoningEffort                          string
	Input                                    []map[string]any
	ChatOnly                                 bool
}

func (runtime *codexRuntime) process(ctx context.Context, chatOnly bool) (*codexProcess, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	index := 0
	if chatOnly {
		index = 1
	}
	if current := runtime.processes[index]; current != nil {
		select {
		case <-current.done:
			runtime.processes[index] = nil
		default:
			return current, nil
		}
	}
	process, err := startCodexProcess(ctx, chatOnly)
	if err == nil {
		runtime.processes[index] = process
	}
	return process, err
}

func (runtime *codexRuntime) close() {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.loginID = ""
	for index, process := range runtime.processes {
		if process != nil {
			process.close()
			runtime.processes[index] = nil
		}
	}
}

func (runtime *codexRuntime) restart(ctx context.Context) error {
	runtime.mu.Lock()
	if runtime.active != 0 {
		runtime.mu.Unlock()
		return errCodexBusy
	}
	runtime.loginID = ""
	for index, process := range runtime.processes {
		if process != nil {
			process.close()
			runtime.processes[index] = nil
		}
	}
	runtime.mu.Unlock()
	_, err := runtime.process(ctx, false)
	return err
}

func startCodexProcess(ctx context.Context, chatOnly bool) (*codexProcess, error) {
	binary, err := codexExecutable()
	if err != nil {
		return nil, errors.New("Install Codex, then check again in Tools. You can also set GLOWBOM_CODEX_BIN to its executable.")
	}
	directory, err := os.MkdirTemp("", "glowbom-codex-")
	if err != nil {
		return nil, errors.New("Could not prepare the local Codex server.")
	}
	processContext, cancel := context.WithCancel(context.Background())
	p := &codexProcess{cancel: cancel, workdir: directory, version: codexExecutableVersion(binary), done: make(chan struct{}), pending: map[string]chan codexRPCMessage{}, streams: map[string]chan codexRPCMessage{}, logins: map[string]bool{}}
	args := []string{"app-server", "--listen", "stdio://"}
	config := []string{
		`model_provider="openai"`,
		`features.apps=false`, `features.plugins=false`, `features.remote_plugin=false`, `features.hooks=false`,
		`features.multi_agent=false`, `features.code_mode=false`,
		`features.browser_use=false`, `features.in_app_browser=false`, `features.computer_use=false`,
		`features.image_generation=false`, `features.tool_suggest=false`, `notify=[]`,
	}
	if chatOnly {
		config = append(config, `features.code_mode_host=false`, `features.shell_tool=false`, `features.unified_exec=false`, `tools.view_image=false`, `web_search="disabled"`)
	} else {
		// Current Codex runtimes use this host for their built-in file tools.
		config = append(config, `features.code_mode_host=true`, `features.shell_tool=true`, `features.unified_exec=true`)
	}
	for _, value := range config {
		args = append(args, "-c", value)
	}
	p.cmd = codexRuntimeCommand(processContext, binary, args...)
	p.cmd.Dir = directory
	p.cmd.Env = codexRuntimeEnvironment(os.Environ())
	p.cmd.Stderr = io.Discard
	p.cmd.WaitDelay = time.Second
	configureCursorCancellation(p.cmd)
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		p.close()
		_ = os.RemoveAll(directory)
		return nil, errCodexStopped
	}
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		p.close()
		_ = os.RemoveAll(directory)
		return nil, errCodexStopped
	}
	if err := p.cmd.Start(); err != nil {
		p.close()
		_ = os.RemoveAll(directory)
		return nil, errCodexStopped
	}
	go func() {
		p.read(output)
		p.close()
		_ = p.cmd.Wait()
		_ = os.RemoveAll(directory)
	}()
	initializeContext, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	if err := p.call(initializeContext, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "glowbom", "title": "Glowbom", "version": "0.1.0"},
	}, nil); err != nil {
		p.close()
		return nil, err
	}
	if err := p.send(map[string]any{"method": "initialized"}); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func codexRuntimeEnvironment(environment []string) []string {
	// Codex reads its own saved login. Never give model tools Glowbom's bearer
	// token or unrelated provider credentials from the backend environment.
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"TMPDIR": true, "TMP": true, "TEMP": true, "SYSTEMROOT": true,
		"WINDIR": true, "APPDATA": true, "LOCALAPPDATA": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true,
		"CODEX_HOME": true, "COMSPEC": true, "PATHEXT": true,
	}
	result := make([]string, 0, len(allowed))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if allowed[strings.ToUpper(name)] {
			result = append(result, entry)
		}
	}
	return result
}

func (p *codexProcess) close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.cancel()
		if p.input != nil {
			_ = p.input.Close()
		}
	})
}

func (p *codexProcess) send(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-p.done:
		return errCodexStopped
	default:
	}
	if err := json.NewEncoder(p.input).Encode(value); err != nil {
		p.close()
		return errCodexStopped
	}
	return nil
}

func (p *codexProcess) read(output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var message codexRPCMessage
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			return
		}
		p.mu.Lock()
		if message.Method == "account/login/completed" {
			var login struct {
				LoginID string `json:"loginId"`
				Success bool   `json:"success"`
			}
			if json.Unmarshal(message.Params, &login) == nil && login.LoginID != "" {
				p.logins[login.LoginID] = login.Success
			}
		}
		var destination chan codexRPCMessage
		if message.Method == "" {
			destination = p.pending[string(message.ID)]
		} else {
			var params struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(message.Params, &params)
			destination = p.streams[params.ThreadID]
		}
		p.mu.Unlock()
		if destination != nil {
			select {
			case destination <- message:
			case <-p.done:
				return
			default:
				// Never silently lose a permission request or terminal event.
				return
			}
		} else if message.Method != "" && len(message.ID) != 0 && string(message.ID) != "null" {
			_ = p.send(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": "Glowbom does not support this Codex request."}})
		}
	}
}

func (p *codexProcess) call(ctx context.Context, method string, params, result any) error {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	response := make(chan codexRPCMessage, 1)
	p.pending[strconv.Itoa(id)] = response
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, strconv.Itoa(id)); p.mu.Unlock() }()
	if err := p.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errCodexStopped
	case message := <-response:
		if len(message.Error) > 0 && string(message.Error) != "null" {
			return codexRPCError(message.Error)
		}
		if result != nil && json.Unmarshal(message.Result, result) != nil {
			return errors.New("Codex returned an unsupported response. Update Codex and try again.")
		}
		return nil
	}
}

func codexRPCError(raw json.RawMessage) error {
	var value struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &value) == nil && value.Message != "" {
		return errors.New(sanitizeProviderError(errors.New(value.Message)))
	}
	return errors.New("Codex could not complete this request. Check its connection in Tools.")
}

// Disabling every inherited MCP entry is necessary because config tables merge.
func (p *codexProcess) threadConfig(ctx context.Context, directory string) (map[string]any, error) {
	var settings struct {
		Config struct {
			MCPServers map[string]json.RawMessage `json:"mcp_servers"`
		} `json:"config"`
	}
	if err := p.call(ctx, "config/read", map[string]any{"cwd": directory, "includeLayers": false}, &settings); err != nil {
		return nil, err
	}
	servers := map[string]any{}
	for name := range settings.Config.MCPServers {
		servers[name] = map[string]bool{"enabled": false}
	}
	return map[string]any{"mcp_servers": servers}, nil
}

func runCodexTurn(ctx context.Context, options codexRunOptions, onMessage func(codexRPCMessage) error, onRequest func(codexRPCMessage) (any, error)) (threadID string, err error) {
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	runtime := glowbomCodex
	runtime.mu.Lock()
	runtime.active++
	runtime.mu.Unlock()
	defer func() { runtime.mu.Lock(); runtime.active--; runtime.mu.Unlock() }()
	p, err := runtime.process(ctx, options.ChatOnly)
	if err != nil {
		return "", err
	}
	go func() {
		select {
		case <-p.done:
			cancelRun()
		case <-ctx.Done():
		}
	}()
	setupContext, cancelSetup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSetup()
	if options.ReasoningEffort != "" {
		if err := p.validateReasoningEffort(setupContext, options.Model, options.ReasoningEffort); err != nil {
			return "", err
		}
	}
	config, err := p.threadConfig(setupContext, options.Directory)
	if err != nil {
		return "", err
	}
	if options.ReasoningEffort != "" {
		config["model_reasoning_effort"] = options.ReasoningEffort
	}
	sandbox, approval := "workspace-write", "on-request"
	if options.ChatOnly {
		sandbox, approval = "read-only", "never"
	}
	params := map[string]any{"cwd": options.Directory, "model": options.Model, "sandbox": sandbox, "approvalPolicy": approval, "config": config, "developerInstructions": options.Instructions}
	method := "thread/start"
	if options.ThreadID != "" && !options.ChatOnly {
		method = "thread/resume"
		params["threadId"] = options.ThreadID
	} else {
		params["ephemeral"] = options.ChatOnly
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err = p.call(setupContext, method, params, &thread); err != nil {
		return "", err
	}
	threadID = thread.Thread.ID
	if threadID == "" {
		return "", errors.New("Codex did not start a conversation.")
	}
	var mcp struct {
		Data []struct {
			Tools map[string]json.RawMessage `json:"tools"`
		} `json:"data"`
		NextCursor string `json:"nextCursor"`
	}
	if err := p.call(setupContext, "mcpServerStatus/list", map[string]any{"threadId": threadID, "detail": "toolsAndAuthOnly", "limit": 100}, &mcp); err != nil {
		return threadID, err
	}
	if mcp.NextCursor != "" {
		return threadID, errors.New("Could not verify the tools enabled for this Codex conversation.")
	}
	for _, server := range mcp.Data {
		if len(server.Tools) != 0 {
			return threadID, errors.New("Codex could not disable external tools for this conversation. Restart Codex and try again.")
		}
	}
	stream := make(chan codexRPCMessage, 512)
	p.mu.Lock()
	if p.streams[threadID] != nil {
		p.mu.Unlock()
		return threadID, errors.New("This Codex conversation already has an active task.")
	}
	p.streams[threadID] = stream
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.streams, threadID); p.mu.Unlock() }()
	if onMessage != nil {
		data, _ := json.Marshal(map[string]any{"thread": map[string]string{"id": threadID}})
		if err = onMessage(codexRPCMessage{Method: "thread/started", Params: data}); err != nil {
			return threadID, err
		}
	}
	turnParams := map[string]any{"threadId": threadID, "input": options.Input, "model": options.Model, "cwd": options.Directory, "approvalPolicy": approval}
	if options.ReasoningEffort != "" {
		turnParams["effort"] = options.ReasoningEffort
	}
	if options.ChatOnly {
		turnParams["summary"] = "auto"
		turnParams["sandboxPolicy"] = map[string]any{"type": "readOnly", "networkAccess": false}
	} else {
		turnParams["sandboxPolicy"] = map[string]any{"type": "workspaceWrite", "writableRoots": []string{options.Directory}, "networkAccess": false, "excludeSlashTmp": true, "excludeTmpdirEnvVar": true}
	}
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err = p.call(setupContext, "turn/start", turnParams, &turn); err != nil {
		// An interrupted start has an uncertain outcome. Retire this process so
		// a task cannot continue after its client has stopped waiting.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			p.close()
		}
		return threadID, err
	}
	if turn.Turn.ID == "" {
		p.close()
		return threadID, errors.New("Codex did not start the task.")
	}
	completed := false
	defer func() {
		if !completed {
			interruptContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if p.call(interruptContext, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turn.Turn.ID}, nil) != nil {
				p.close()
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			select {
			case <-p.done:
				return threadID, errCodexStopped
			default:
			}
			return threadID, ctx.Err()
		case <-p.done:
			return threadID, errCodexStopped
		case message := <-stream:
			var event struct {
				TurnID    string          `json:"turnId"`
				WillRetry bool            `json:"willRetry"`
				Error     json.RawMessage `json:"error"`
				Turn      struct {
					ID     string          `json:"id"`
					Status string          `json:"status"`
					Error  json.RawMessage `json:"error"`
				} `json:"turn"`
			}
			if json.Unmarshal(message.Params, &event) != nil {
				return threadID, errors.New("Codex returned an invalid task event.")
			}
			if event.TurnID != "" && event.TurnID != turn.Turn.ID {
				continue
			}
			if len(message.ID) > 0 && string(message.ID) != "null" {
				message.Context = ctx
				var result any
				requestErr := errors.New("This Codex request is not supported in Glowbom.")
				if !options.ChatOnly && onRequest != nil {
					result, requestErr = onRequest(message)
				}
				if err = ctx.Err(); err != nil {
					return threadID, err
				}
				response := map[string]any{"id": message.ID, "result": result}
				if requestErr != nil {
					delete(response, "result")
					response["error"] = map[string]any{"code": -32601, "message": "Glowbom declined an unsupported Codex request."}
				}
				if err = p.send(response); err != nil {
					return threadID, err
				}
				if options.ChatOnly {
					return threadID, errors.New("Codex requested a tool during chat. Use Build for tasks that need tools.")
				}
				continue
			}
			if onMessage != nil {
				if err = onMessage(message); err != nil {
					return threadID, err
				}
			}
			if message.Method == "turn/completed" && event.Turn.ID == turn.Turn.ID {
				completed = true
				switch event.Turn.Status {
				case "completed":
					return threadID, nil
				case "interrupted":
					return threadID, context.Canceled
				default:
					return threadID, codexRPCError(event.Turn.Error)
				}
			}
			if message.Method == "error" && !event.WillRetry {
				return threadID, codexRPCError(event.Error)
			}
		}
	}
}

func codexChatModels(ctx context.Context) ([]chatModel, error) {
	if _, err := codexExecutable(); err != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := glowbomCodex.process(ctx, false)
	if err != nil {
		return nil, err
	}
	connected, err := p.connected(ctx)
	if err != nil || !connected {
		return nil, err
	}
	return p.models(ctx)
}

func (p *codexProcess) models(ctx context.Context) ([]chatModel, error) {
	models := []chatModel{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 20; page++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var catalog struct {
			Data []struct {
				ID, Model, DisplayName    string
				Hidden                    bool
				IsDefault                 bool
				DefaultReasoningEffort    string
				InputModalities           []string
				SupportedReasoningEfforts []struct{ ReasoningEffort string }
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := p.call(ctx, "model/list", params, &catalog); err != nil {
			return nil, err
		}
		for _, model := range catalog.Data {
			id := model.Model
			if id == "" {
				id = model.ID
			}
			if model.Hidden || id == "" || seen[id] {
				continue
			}
			seen[id] = true
			name := model.DisplayName
			if name == "" {
				name = id
			}
			images := model.InputModalities == nil
			for _, modality := range model.InputModalities {
				images = images || modality == "image"
			}
			efforts := []string{}
			seenEfforts := map[string]bool{}
			for _, option := range model.SupportedReasoningEfforts {
				effort := strings.TrimSpace(option.ReasoningEffort)
				if effort != "" && !seenEfforts[effort] {
					efforts = append(efforts, effort)
					seenEfforts[effort] = true
				}
			}
			defaultEffort := model.DefaultReasoningEffort
			if !seenEfforts[defaultEffort] {
				defaultEffort = ""
			}
			models = append(models, chatModel{
				ID: "codex/" + id, Name: name, Provider: "Codex", Images: images, Build: true,
				ReasoningEfforts: efforts, DefaultReasoningEffort: defaultEffort, IsDefault: model.IsDefault,
			})
		}
		if catalog.NextCursor == "" || catalog.NextCursor == cursor {
			break
		}
		cursor = catalog.NextCursor
	}
	// Put current models first while keeping the server's order for other models.
	sort.SliceStable(models, func(i, j int) bool {
		return codexModelRank(models[i]) < codexModelRank(models[j])
	})
	return models, nil
}

func codexModelRank(model chatModel) int {
	switch model.ID {
	case "codex/gpt-6.1-sol":
		return 0
	case "codex/gpt-6-astra":
		return 1
	case "codex/gpt-6-sol":
		return 2
	case "codex/gpt-6-luna":
		return 3
	default:
		if model.IsDefault {
			return 4
		}
		return 5
	}
}

func (p *codexProcess) validateReasoningEffort(ctx context.Context, modelID, effort string) error {
	models, err := p.models(ctx)
	if err != nil {
		return err
	}
	for _, model := range models {
		if model.ID != "codex/"+modelID {
			continue
		}
		for _, supported := range model.ReasoningEfforts {
			if effort == supported {
				return nil
			}
		}
		return errors.New("This effort is not available for the selected Codex model. Refresh the model list and choose a supported effort.")
	}
	return errors.New("This Codex model is no longer available. Refresh the model list and choose another model.")
}

func (p *codexProcess) connected(ctx context.Context) (bool, error) {
	var account struct {
		Account            json.RawMessage `json:"account"`
		RequiresOpenAIAuth bool            `json:"requiresOpenaiAuth"`
	}
	if err := p.call(ctx, "account/read", map[string]bool{"refreshToken": false}, &account); err != nil {
		return false, err
	}
	return (len(account.Account) > 0 && string(account.Account) != "null") || !account.RequiresOpenAIAuth, nil
}

type codexStatus struct {
	Version      string `json:"version,omitempty"`
	Installed    bool   `json:"installed"`
	Running      bool   `json:"running"`
	Connected    bool   `json:"connected"`
	Busy         bool   `json:"busy"`
	Error        string `json:"error,omitempty"`
	LoginPending bool   `json:"loginPending,omitempty"`
}

func readCodexStatus(ctx context.Context) codexStatus {
	status := codexStatus{}
	_, err := codexExecutable()
	status.Installed = err == nil
	glowbomCodex.mu.Lock()
	status.Busy = glowbomCodex.active != 0
	glowbomCodex.mu.Unlock()
	if !status.Installed {
		return status
	}
	p, err := glowbomCodex.process(ctx, false)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Version = p.version
	status.Running = true
	status.Connected, err = p.connected(ctx)
	if err != nil {
		status.Error = err.Error()
	}
	glowbomCodex.mu.Lock()
	if glowbomCodex.loginID != "" {
		p.mu.Lock()
		success, finished := p.logins[glowbomCodex.loginID]
		p.mu.Unlock()
		status.LoginPending = !finished
		if !finished {
			status.Connected = false
		} else if !success {
			status.Error = "Codex sign-in did not finish. Try signing in again."
			glowbomCodex.loginID = ""
		} else if glowbomCodex.active == 0 {
			// A chat process may have loaded the previous account before the
			// browser sign-in finished. Recreate it on the next conversation.
			if chat := glowbomCodex.processes[1]; chat != nil {
				chat.close()
				glowbomCodex.processes[1] = nil
			}
			glowbomCodex.loginID = ""
		}
	}
	glowbomCodex.mu.Unlock()
	return status
}

func codexRuntimeHandler(w http.ResponseWriter, r *http.Request) {
	if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
		http.Error(w, "Local authentication required.", http.StatusUnauthorized)
		return
	}
	method := http.MethodPost
	if r.URL.Path == "/codex/status" {
		method = http.MethodGet
	}
	if r.Method != method {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	fail := func(err error, status int) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": sanitizeProviderError(err)})
	}
	switch r.URL.Path {
	case "/codex/status":
		_ = json.NewEncoder(w).Encode(readCodexStatus(ctx))
	case "/codex/restart":
		if err := glowbomCodex.restart(ctx); err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, errCodexBusy) {
				status = http.StatusConflict
			}
			fail(err, status)
			return
		}
		_ = json.NewEncoder(w).Encode(readCodexStatus(ctx))
	case "/codex/login":
		glowbomCodex.mu.Lock()
		busy := glowbomCodex.active != 0
		glowbomCodex.mu.Unlock()
		if busy {
			fail(errors.New("Finish or stop your Codex tasks before changing its account."), http.StatusConflict)
			return
		}
		p, err := glowbomCodex.process(ctx, false)
		if err != nil {
			fail(err, http.StatusServiceUnavailable)
			return
		}
		var login struct {
			AuthURL string `json:"authUrl"`
			LoginID string `json:"loginId"`
		}
		if err := p.call(ctx, "account/login/start", map[string]string{"type": "chatgpt"}, &login); err != nil {
			fail(err, http.StatusServiceUnavailable)
			return
		}
		parsed, err := url.Parse(login.AuthURL)
		if err != nil || parsed.Scheme != "https" || login.LoginID == "" {
			fail(fmt.Errorf("Codex did not return a sign-in link."), http.StatusBadGateway)
			return
		}
		glowbomCodex.mu.Lock()
		glowbomCodex.loginID = login.LoginID
		glowbomCodex.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"authorizationURL": login.AuthURL, "loginId": login.LoginID})
	case "/codex/login/cancel":
		var body struct {
			LoginID string `json:"loginId"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil || body.LoginID == "" {
			fail(errors.New("Choose the sign-in to cancel."), http.StatusBadRequest)
			return
		}
		p, err := glowbomCodex.process(ctx, false)
		if err == nil {
			err = p.call(ctx, "account/login/cancel", map[string]string{"loginId": body.LoginID}, nil)
		}
		if err != nil {
			fail(err, http.StatusServiceUnavailable)
			return
		}
		glowbomCodex.mu.Lock()
		if glowbomCodex.loginID == body.LoginID {
			glowbomCodex.loginID = ""
		}
		glowbomCodex.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	default:
		http.NotFound(w, r)
	}
}
