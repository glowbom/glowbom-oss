package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const acpSetupTimeout = 15 * time.Second
const acpMaxMessageBytes = 32 << 20

var errACPStopped = errors.New("The ACP agent stopped before completing the request. Check its command and sign-in, then try again.")
var errACPProtocol = errors.New("The agent returned an invalid ACP message. Check that its command starts ACP over stdio.")
var errACPAuthRequired = errors.New("Sign in to this agent in Terminal, then check the ACP connection again in Settings.")

type acpMessage struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id,omitempty"`
}

type acpProbeResult struct {
	Name         string           `json:"name"`
	Version      string           `json:"version"`
	Model        string           `json:"model,omitempty"`
	Models       []acpModelOption `json:"models,omitempty"`
	Images       bool             `json:"images"`
	LoadSession  bool             `json:"loadSession"`
	AuthRequired bool             `json:"authRequired"`
}

type acpRemoteError struct {
	Code int `json:"code"`
}

func (e *acpRemoteError) Error() string {
	// Agent messages and error data can contain provider credentials or prompts.
	switch e.Code {
	case -32000:
		return errACPAuthRequired.Error()
	case -32002:
		return "The ACP agent could not find the saved session or requested resource. Start a new build session."
	case -32601:
		return "This agent does not support a required ACP method. Check its ACP version and launch arguments."
	case -32602:
		return "The ACP agent rejected the request settings. Check its ACP compatibility."
	default:
		return fmt.Sprintf("The ACP agent could not complete the request (code %d). Check the agent in Terminal and try again.", e.Code)
	}
}

type acpRPCFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpRemoteError `json:"error,omitempty"`
}

type acpReadResult struct {
	frame acpRPCFrame
	err   error
}

type acpWrite struct {
	data []byte
	done chan error
}

type acpPermissionReply struct {
	id     json.RawMessage
	result any
}

type acpPendingPermission struct {
	id     json.RawMessage
	cancel context.CancelFunc
}

type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

// Each build owns one subprocess. Nothing is launched by listing connections.
type acpProcess struct {
	cmd            *exec.Cmd
	input          io.WriteCloser
	ctx            context.Context
	cancel         context.CancelFunc
	waited         chan struct{}
	read           chan acpReadResult
	writes         chan acpWrite
	replies        chan acpPermissionReply
	pending        map[string]acpPendingPermission
	nextID         int
	session        string
	model          string
	modelSelection acpModelSelection
	forward        bool
	emit           func(acpMessage) error
	request        func(context.Context, acpMessage) (any, error)
	callbacks      sync.WaitGroup
}

func acpRuntimeEnvironment(environment []string) []string {
	// Agents use their own saved login. Do not inherit backend/provider secrets,
	// shell startup hooks, or runtime injection variables from the host process.
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"TMPDIR": true, "TMP": true, "TEMP": true, "SYSTEMROOT": true,
		"WINDIR": true, "APPDATA": true, "LOCALAPPDATA": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true,
		"COMSPEC": true, "PATHEXT": true, "XDG_CONFIG_HOME": true,
		"XDG_DATA_HOME": true, "XDG_CACHE_HOME": true,
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

func startACPProcess(profile acpProfile, directory string) (*acpProcess, error) {
	if strings.TrimSpace(profile.Command) == "" {
		return nil, errors.New("Set an executable and its ACP launch arguments in Settings.")
	}
	if !filepath.IsAbs(directory) {
		return nil, errors.New("Choose an absolute project folder for the ACP agent.")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &acpProcess{
		ctx: ctx, cancel: cancel, waited: make(chan struct{}),
		read: make(chan acpReadResult, 2), writes: make(chan acpWrite),
		replies: make(chan acpPermissionReply, 32), pending: make(map[string]acpPendingPermission),
	}
	p.cmd = exec.CommandContext(ctx, profile.Command, profile.Args...)
	p.cmd.Dir = directory
	p.cmd.Env = acpRuntimeEnvironment(os.Environ())
	p.cmd.Stderr = io.Discard
	p.cmd.WaitDelay = time.Second
	configureCursorCancellation(p.cmd)
	input, err := p.cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errACPStopped
	}
	p.input = input
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = input.Close()
		return nil, errACPStopped
	}
	if err := p.cmd.Start(); err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		return nil, errors.New("Could not start the ACP agent. Check its executable path and launch arguments in Settings.")
	}
	go p.readOutput(output)
	go p.writeInput()
	return p, nil
}

func (p *acpProcess) close() {
	for _, permission := range p.pending {
		permission.cancel()
	}
	// EOF lets agents flush their session before the bounded forced shutdown.
	_ = p.input.Close()
	select {
	case <-p.waited:
	case <-time.After(150 * time.Millisecond):
	}
	p.cancel()
	callbacksDone := make(chan struct{})
	go func() {
		p.callbacks.Wait()
		close(callbacksDone)
	}()
	select {
	case <-callbacksDone:
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case <-p.waited:
	case <-time.After(2 * time.Second):
	}
}

func (p *acpProcess) readOutput(output io.Reader) {
	defer close(p.waited)
	defer func() {
		p.cancel()
		_ = p.cmd.Wait()
	}()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), acpMaxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame acpRPCFrame
		if err := json.Unmarshal(line, &frame); err != nil || !validACPFrame(frame) {
			p.deliver(acpReadResult{err: errACPProtocol})
			return
		}
		if !p.deliver(acpReadResult{frame: frame}) {
			return
		}
	}
	if scanner.Err() != nil {
		p.deliver(acpReadResult{err: errACPProtocol})
	} else {
		p.deliver(acpReadResult{err: errACPStopped})
	}
}

func validACPFrame(frame acpRPCFrame) bool {
	if frame.JSONRPC != "2.0" {
		return false
	}
	if len(frame.ID) != 0 {
		var id string
		if json.Unmarshal(frame.ID, &id) != nil {
			if _, err := strconv.ParseInt(string(frame.ID), 10, 64); err != nil {
				return false
			}
		}
		if len(frame.ID) > 1024 || bytes.Equal(frame.ID, []byte("null")) {
			return false
		}
	}
	if frame.Method != "" {
		return len(frame.Result) == 0 && frame.Error == nil
	}
	return len(frame.ID) != 0 && (len(frame.Result) > 0) != (frame.Error != nil)
}

func (p *acpProcess) deliver(result acpReadResult) bool {
	select {
	case p.read <- result:
		return true
	case <-p.ctx.Done():
		return false
	}
}

func (p *acpProcess) writeInput() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case write := <-p.writes:
			_, err := p.input.Write(write.data)
			if err != nil {
				err = errACPStopped
			}
			write.done <- err
		}
	}
}

func (p *acpProcess) send(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > acpMaxMessageBytes {
		return errors.New("The ACP request is too large or invalid. Reduce its attachments and try again.")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	write := acpWrite{data: append(data, '\n'), done: make(chan error, 1)}
	select {
	case p.writes <- write:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return errACPStopped
	}
	select {
	case err := <-write.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *acpProcess) call(ctx context.Context, method string, params any, out any) error {
	p.nextID++
	id := strconv.Itoa(p.nextID)
	if err := p.send(ctx, map[string]any{"jsonrpc": "2.0", "id": p.nextID, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case reply := <-p.replies:
			key := string(reply.id)
			if pending, ok := p.pending[key]; ok {
				pending.cancel()
				delete(p.pending, key)
				if err := p.send(ctx, map[string]any{"jsonrpc": "2.0", "id": reply.id, "result": reply.result}); err != nil {
					return err
				}
			}
		case received := <-p.read:
			if received.err != nil {
				return received.err
			}
			frame := received.frame
			if frame.Method != "" {
				if err := p.handleMessage(ctx, frame); err != nil {
					return err
				}
				continue
			}
			if string(frame.ID) != id {
				return errACPProtocol
			}
			if frame.Error != nil {
				return frame.Error
			}
			if len(p.pending) != 0 || bytes.Equal(frame.Result, []byte("null")) || json.Unmarshal(frame.Result, out) != nil {
				return errACPProtocol
			}
			return nil
		}
	}
}

func (p *acpProcess) handleMessage(ctx context.Context, frame acpRPCFrame) error {
	message := acpMessage{Method: frame.Method, Params: frame.Params, ID: frame.ID}
	if len(frame.ID) == 0 {
		if frame.Method != "session/update" {
			return nil
		}
		if p.session == "" {
			// A new session is not owned until session/new returns its ID.
			return nil
		}
		if !p.ownsSession(frame.Params) {
			return errACPProtocol
		}
		if p.forward && p.emit != nil {
			return p.emit(message)
		}
		return nil
	}
	if frame.Method != "session/request_permission" {
		return p.send(ctx, map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{"code": -32601, "message": "Method not supported"}})
	}
	if !p.forward || !p.ownsSession(frame.Params) || p.request == nil || len(frame.Params) > 128<<10 {
		return p.send(ctx, map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": acpCancelledPermission()})
	}
	if _, valid := acpPermissionOptions(frame.Params); !valid {
		return p.send(ctx, map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": acpCancelledPermission()})
	}
	key := string(frame.ID)
	if _, duplicate := p.pending[key]; duplicate || len(p.pending) >= 32 {
		return errACPProtocol
	}
	permissionContext, cancel := context.WithCancel(ctx)
	p.pending[key] = acpPendingPermission{id: frame.ID, cancel: cancel}
	p.callbacks.Add(1)
	go func() {
		defer p.callbacks.Done()
		result, err := p.request(permissionContext, message)
		if err != nil || permissionContext.Err() != nil || !validACPPermissionResult(message.Params, result) {
			result = acpCancelledPermission()
		}
		select {
		case p.replies <- acpPermissionReply{id: frame.ID, result: result}:
		case <-p.ctx.Done():
		}
	}()
	return nil
}

func (p *acpProcess) ownsSession(params json.RawMessage) bool {
	var value struct {
		SessionID string `json:"sessionId"`
	}
	return p.session != "" && json.Unmarshal(params, &value) == nil && value.SessionID == p.session
}

func acpCancelledPermission() any {
	return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
}

func acpPermissionOptions(params json.RawMessage) ([]acpPermissionOption, bool) {
	var request struct {
		Options []acpPermissionOption `json:"options"`
	}
	if json.Unmarshal(params, &request) != nil || len(request.Options) == 0 || len(request.Options) > 32 {
		return nil, false
	}
	seen := make(map[string]bool, len(request.Options))
	for _, option := range request.Options {
		if option.OptionID == "" || len(option.OptionID) > 256 || strings.ContainsAny(option.OptionID, "\r\n\x00") || seen[option.OptionID] {
			return nil, false
		}
		seen[option.OptionID] = true
	}
	return request.Options, true
}

func validACPPermissionResult(params json.RawMessage, result any) bool {
	encoded, err := json.Marshal(result)
	if err != nil {
		return false
	}
	var reply struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if json.Unmarshal(encoded, &reply) != nil {
		return false
	}
	if reply.Outcome.Outcome == "cancelled" {
		return true
	}
	options, valid := acpPermissionOptions(params)
	if reply.Outcome.Outcome != "selected" || reply.Outcome.OptionID == "" || !valid {
		return false
	}
	for _, option := range options {
		if option.OptionID == reply.Outcome.OptionID && (option.Kind == "allow_once" || option.Kind == "reject_once") {
			return true
		}
	}
	return false
}

func (p *acpProcess) initialize(ctx context.Context) (acpProbeResult, error) {
	setupContext, cancel := context.WithTimeout(ctx, acpSetupTimeout)
	defer cancel()
	var response struct {
		ProtocolVersion int `json:"protocolVersion"`
		AuthMethods     []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"authMethods"`
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
		AgentInfo struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	err := p.call(setupContext, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false,
		},
		"clientInfo": map[string]string{"name": "glowbom", "title": "Glowbom", "version": "0.1.0"},
	}, &response)
	if err != nil {
		return acpProbeResult{}, acpSetupError(ctx, err)
	}
	if response.ProtocolVersion != 1 {
		return acpProbeResult{}, errors.New("This agent uses an unsupported ACP version. Glowbom currently supports ACP version 1.")
	}
	name := response.AgentInfo.Title
	if name == "" {
		name = response.AgentInfo.Name
	}
	result := acpProbeResult{Name: name, Version: response.AgentInfo.Version, Images: response.AgentCapabilities.PromptCapabilities.Image, LoadSession: response.AgentCapabilities.LoadSession}
	for _, method := range response.AuthMethods {
		// Grok Build advertises this noninteractive cached-login method. Never
		// select another method or launch terminal/browser authentication here.
		if method.ID != "cached_token" || (method.Type != "" && method.Type != "agent") {
			continue
		}
		var authenticated struct{}
		err := p.call(setupContext, "authenticate", map[string]any{
			"methodId": "cached_token", "_meta": map[string]bool{"headless": true},
		}, &authenticated)
		if err != nil {
			return result, acpSetupError(ctx, err)
		}
		break
	}
	return result, nil
}

func acpSetupError(parent context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("The ACP agent did not finish connecting within 15 seconds. Check its command and sign-in in Terminal, then try again.")
	}
	return err
}

type acpSessionResponse struct {
	SessionID     string          `json:"sessionId"`
	Models        json.RawMessage `json:"models"`
	ConfigOptions json.RawMessage `json:"configOptions"`
}

// Model details are optional. Bad metadata must not reject a valid session.
func acpReportedSessionModel(response acpSessionResponse) string {
	modelString := func(raw json.RawMessage) string {
		var value string
		if json.Unmarshal(raw, &value) != nil || len(value) > 256 || strings.IndexFunc(value, unicode.IsControl) >= 0 || strings.ContainsAny(value, "\u2028\u2029") {
			return ""
		}
		return strings.TrimSpace(value)
	}
	var models struct {
		CurrentModelID json.RawMessage `json:"currentModelId"`
	}
	if json.Unmarshal(response.Models, &models) == nil {
		if model := modelString(models.CurrentModelID); model != "" {
			return model
		}
	}
	var options []json.RawMessage
	if json.Unmarshal(response.ConfigOptions, &options) != nil {
		return ""
	}
	fallback := ""
	for _, raw := range options {
		var option struct {
			ID           string          `json:"id"`
			Category     string          `json:"category"`
			CurrentValue json.RawMessage `json:"currentValue"`
		}
		if json.Unmarshal(raw, &option) != nil {
			continue
		}
		model := modelString(option.CurrentValue)
		if option.ID == "model" && model != "" {
			return model
		}
		// Some agents group their provider selector under the model category.
		if fallback == "" && option.Category == "model" && option.ID != "provider" {
			fallback = model
		}
	}
	return fallback
}

func (p *acpProcess) setupSession(ctx context.Context, directory, sessionID string, loadSession bool) error {
	setupContext, cancel := context.WithTimeout(ctx, acpSetupTimeout)
	defer cancel()
	params := map[string]any{"cwd": directory, "mcpServers": []any{}}
	if sessionID != "" && loadSession {
		p.session = sessionID
		params["sessionId"] = sessionID
		var response acpSessionResponse
		if err := p.call(setupContext, "session/load", params, &response); err != nil {
			return acpSetupError(ctx, err)
		}
		p.model = acpReportedSessionModel(response)
		p.modelSelection = acpDiscoverSessionModels(response)
		return nil
	}
	var response acpSessionResponse
	if err := p.call(setupContext, "session/new", params, &response); err != nil {
		return acpSetupError(ctx, err)
	}
	if strings.TrimSpace(response.SessionID) == "" || len(response.SessionID) > 512 || strings.ContainsAny(response.SessionID, "\r\n\x00") {
		return errACPProtocol
	}
	p.session = response.SessionID
	p.model = acpReportedSessionModel(response)
	p.modelSelection = acpDiscoverSessionModels(response)
	return nil
}

func probeACP(ctx context.Context, profile acpProfile) (acpProbeResult, error) {
	if err := ctx.Err(); err != nil {
		return acpProbeResult{}, err
	}
	directory, err := os.MkdirTemp("", "glowbom-acp-probe-")
	if err != nil {
		return acpProbeResult{}, errors.New("Could not prepare a temporary folder for the ACP connection check.")
	}
	defer os.RemoveAll(directory)
	p, err := startACPProcess(profile, directory)
	if err != nil {
		return acpProbeResult{}, err
	}
	defer p.close()
	result, err := p.initialize(ctx)
	if err == nil {
		err = p.setupSession(ctx, directory, "", false)
		result.Model = p.model
		result.Models = p.modelSelection.options
	}
	var rpcError *acpRemoteError
	if errors.As(err, &rpcError) && rpcError.Code == -32000 {
		result.AuthRequired = true
		return result, nil
	}
	return result, err
}

func runACPTurn(ctx context.Context, profile acpProfile, directory string, sessionID string, prompt []map[string]any, emit func(acpMessage) error, request func(context.Context, acpMessage) (any, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := startACPProcess(profile, directory)
	if err != nil {
		return "", err
	}
	defer p.close()
	capabilities, err := p.initialize(ctx)
	if err != nil {
		return "", err
	}
	for _, content := range prompt {
		if content["type"] == "image" && !capabilities.Images {
			return "", errors.New("This ACP agent does not support image attachments. Choose an image-capable agent or remove the attachments.")
		}
	}
	if err := p.setupSession(ctx, directory, sessionID, capabilities.LoadSession); err != nil {
		return "", err
	}
	if err := p.selectModel(ctx, profile.Model); err != nil {
		return p.session, err
	}
	p.emit, p.request, p.forward = emit, request, true
	if emit != nil {
		value := map[string]any{"sessionId": p.session, "images": capabilities.Images, "restored": sessionID != "" && capabilities.LoadSession}
		if p.model != "" {
			value["model"] = p.model
		}
		params, _ := json.Marshal(value)
		if err := emit(acpMessage{Method: "glowbom/session", Params: params}); err != nil {
			return p.session, err
		}
	}
	var response struct {
		StopReason string `json:"stopReason"`
	}
	if prompt == nil {
		prompt = []map[string]any{}
	}
	err = p.call(ctx, "session/prompt", map[string]any{"sessionId": p.session, "prompt": prompt}, &response)
	if ctx.Err() != nil {
		p.cancelTurn()
		return p.session, ctx.Err()
	}
	if err != nil {
		p.cancelTurn()
		return p.session, err
	}
	switch response.StopReason {
	case "end_turn":
		return p.session, nil
	case "cancelled":
		return p.session, context.Canceled
	case "max_tokens", "max_turn_requests":
		return p.session, errors.New("The ACP agent reached its turn limit before finishing. Review its progress and send a follow-up request.")
	case "refusal":
		return p.session, errors.New("The ACP agent declined this request. Review its response and adjust the instructions.")
	default:
		return p.session, errors.New("The ACP agent did not report a recognized completion reason. Review its progress before retrying.")
	}
}

func (p *acpProcess) cancelTurn() {
	// Give cooperative agents a bounded chance to stop before close kills the
	// process group, including child tools that inherited its stdio pipes.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	for key, permission := range p.pending {
		permission.cancel()
		delete(p.pending, key)
		_ = p.send(ctx, map[string]any{"jsonrpc": "2.0", "id": permission.id, "result": acpCancelledPermission()})
	}
	if p.session != "" {
		_ = p.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": p.session}})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case received := <-p.read:
			if received.err != nil || received.frame.Method == "" {
				return
			}
			if received.frame.Method == "session/request_permission" && len(received.frame.ID) != 0 {
				_ = p.send(ctx, map[string]any{"jsonrpc": "2.0", "id": received.frame.ID, "result": acpCancelledPermission()})
			}
		}
	}
}
