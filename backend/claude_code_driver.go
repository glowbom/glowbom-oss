package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const claudeCodeSessionPrefix = "claude-code:"

// Claude Code owns account credentials. Glowbom only reads its login status.
func claudeCodeExecutable() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("GLOWBOM_CLAUDE_CODE_BIN")); configured != "" {
		if path, err := exec.LookPath(configured); err == nil {
			return path, nil
		}
		return "", errors.New("GLOWBOM_CLAUDE_CODE_BIN does not point to an executable Claude Code CLI")
	}
	// Prefer the native installation over an older npm installation on PATH.
	if home, err := os.UserHomeDir(); err == nil {
		if path, err := exec.LookPath(filepath.Join(home, ".local", "bin", "claude")); err == nil {
			return path, nil
		}
	}
	if path, err := exec.LookPath("claude"); err == nil {
		return path, nil
	}
	return "", errors.New("Claude Code CLI is missing. Install it from code.claude.com/docs/en/setup, then run claude auth login")
}

func claudeCodeAuthenticated(ctx context.Context, binary string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "auth", "status", "--json")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	cmd.WaitDelay = time.Second
	configureCursorCancellation(cmd)
	if err := cmd.Run(); err != nil {
		return false, errors.New("Claude Code login check failed. Run claude auth login and claude auth status, then refresh.")
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		return false, errors.New("Could not read Claude Code login status. Update Claude Code and refresh.")
	}
	return status.LoggedIn, nil
}

func claudeCodeHealthHandler(w http.ResponseWriter, r *http.Request) {
	binary, err := claudeCodeExecutable()
	if err != nil {
		writeJSON(w, map[string]interface{}{"healthy": false, "hint": err.Error()})
		return
	}
	authenticated, err := claudeCodeAuthenticated(r.Context(), binary)
	if err != nil {
		writeJSON(w, map[string]interface{}{"healthy": false, "hint": err.Error()})
		return
	}
	if !authenticated {
		writeJSON(w, map[string]interface{}{"healthy": false, "hint": "Claude Code is installed. Run claude auth login, finish signing in, then refresh."})
		return
	}
	writeJSON(w, map[string]interface{}{"healthy": true, "server": "Claude Code CLI"})
}

func listClaudeCodeModels(ctx context.Context) []chatModel {
	binary, err := claudeCodeExecutable()
	if err != nil {
		return nil
	}
	if authenticated, err := claudeCodeAuthenticated(ctx, binary); err != nil || !authenticated {
		return nil
	}
	// The CLI has no model-list command. These are aliases, not verified account entitlements.
	return []chatModel{
		{ID: "claude-code/default", Name: "Default", Provider: "Claude Code", Build: true},
		{ID: "claude-code/sonnet", Name: "Sonnet", Provider: "Claude Code", Build: true},
		{ID: "claude-code/opus", Name: "Opus", Provider: "Claude Code", Build: true},
		{ID: "claude-code/haiku", Name: "Haiku", Provider: "Claude Code", Build: true},
	}
}

var claudeCodeSessionID = regexp.MustCompile(`^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$`)
var claudeCodeCLIModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func claudeCodeCLIModel(model string) string {
	model = strings.TrimPrefix(strings.TrimSpace(model), "claude-code/")
	if claudeCodeCLIModelID.MatchString(model) {
		return model
	}
	return ""
}

func claudeCodeArguments(model, session string, permissionModes ...string) []string {
	model = claudeCodeCLIModel(model)
	if model == "" {
		model = "default"
	}
	mode := "auto"
	// Haiku does not support Claude Code's action classifier. Local allow rules
	// still authorize commands, while acceptEdits permits ordinary project edits.
	if strings.Contains(strings.ToLower(model), "haiku") {
		mode = "acceptEdits"
	}
	// This flag belongs to this print process; it never writes a saved permission rule.
	if len(permissionModes) > 0 && permissionModes[0] == "all" {
		mode = "bypassPermissions"
	}
	args := []string{"--print", "--verbose", "--output-format", "stream-json", "--permission-mode", mode, "--permission-prompts", "none", "--model", model}
	if strings.HasPrefix(session, claudeCodeSessionPrefix) {
		id := strings.TrimPrefix(session, claudeCodeSessionPrefix)
		if claudeCodeSessionID.MatchString(id) {
			args = append(args, "--resume", id)
		}
	}
	return args
}

type claudeCodeEvent struct {
	Type              string            `json:"type"`
	Subtype           string            `json:"subtype"`
	SessionID         string            `json:"session_id"`
	Model             string            `json:"model"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Message           json.RawMessage   `json:"message"`
}

// Each run owns its process and resumes only its explicitly selected Claude session.
func streamClaudeCode(ctx context.Context, binary, project string, args []string, prompt string, emit func(map[string]interface{})) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = project
	cmd.Stdin = strings.NewReader(prompt)
	// CLI diagnostics and auth metadata may contain private account information.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	configureCursorCancellation(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", errors.New("Could not open Claude Code output")
	}
	if err := cmd.Start(); err != nil {
		return "", errors.New("Could not start Claude Code CLI")
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var result string
	var terminal, success, announced, hadText, permissionDenied bool
	var streamErr error
	for scanner.Scan() {
		var event claudeCodeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			streamErr = errors.New("Claude Code returned invalid streaming output. Update Claude Code and retry")
			cancel()
			break
		}
		switch event.Type {
		case "system":
			if event.Subtype == "init" && !announced {
				if claudeCodeSessionID.MatchString(event.SessionID) {
					emit(map[string]interface{}{"output": "Session created: " + claudeCodeSessionPrefix + event.SessionID})
					announced = true
				}
				if claudeCodeCLIModelID.MatchString(event.Model) {
					emit(map[string]interface{}{"output": "Claude Code model: " + event.Model})
				}
			}
			if event.Subtype == "permission_denied" {
				permissionDenied = true
				emit(map[string]interface{}{"output": "Claude Code blocked an action under its local permission rules."})
			}
		case "assistant":
			var message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if len(event.Message) > 0 && json.Unmarshal(event.Message, &message) != nil {
				streamErr = errors.New("Claude Code returned an invalid assistant message. Update Claude Code and retry")
				cancel()
				break
			}
			for _, part := range message.Content {
				switch part.Type {
				case "text":
					if part.Text != "" {
						hadText = true
						emit(map[string]interface{}{"output": part.Text})
					}
				case "tool_use":
					emit(map[string]interface{}{"output": "Claude Code is working on the project..."})
				}
			}
		case "result":
			terminal, success, result = true, event.Subtype == "success" && !event.IsError, event.Result
			permissionDenied = permissionDenied || len(event.PermissionDenials) > 0
		case "error":
			streamErr = errors.New("Claude Code reported an error. Check claude auth status, your model, and your usage limits, then retry")
			cancel()
		}
	}
	if scanner.Err() != nil {
		streamErr = errors.New("Could not read Claude Code output")
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil && streamErr == nil {
		return "", ctx.Err()
	}
	if streamErr != nil {
		return "", streamErr
	}
	if permissionDenied {
		return "", errors.New("Claude Code could not complete all actions because permission was denied. Review the project's permissions in Claude Code, or choose a model that supports auto mode, then retry")
	}
	if waitErr != nil {
		return "", errors.New("Claude Code failed. Check claude auth status, your model and usage limits, and update the CLI to version 2.1.259 or later")
	}
	if !terminal || !success {
		return "", errors.New("Claude Code ended without a successful result")
	}
	if !hadText && result != "" {
		emit(map[string]interface{}{"output": result})
	}
	return result, nil
}

func runClaudeCodeRefine(w http.ResponseWriter, r *http.Request, req OpenCodeAgentRequest, instructions string, onComplete ...func(string, string, []string)) (string, string) {
	binary, err := claudeCodeExecutable()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return "failed", err.Error()
	}
	if strings.TrimSpace(req.Model) != "" && claudeCodeCLIModel(req.Model) == "" {
		http.Error(w, "Invalid Claude Code model", http.StatusBadRequest)
		return "failed", "Invalid Claude Code model"
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return "failed", "Streaming not supported"
	}
	before, err := captureProjectFileSnapshot(req.ProjectPath)
	if err != nil {
		http.Error(w, "Could not inspect project files", http.StatusBadRequest)
		return "failed", "Could not inspect project files"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	emit := func(event map[string]interface{}) {
		if runID := w.Header().Get("X-Glowbom-Run-ID"); runID != "" {
			event["runId"] = runID
		}
		sendSSEData(w, flusher, event)
	}
	if req.PermissionMode == "all" {
		emit(map[string]interface{}{"output": "Starting Claude Code. Permission prompts are bypassed for this build. Managed restrictions may still apply."})
	} else {
		emit(map[string]interface{}{"output": "Starting Claude Code. Actions follow Claude Code permission checks and your local rules."})
	}
	if req.PermissionMode != "all" && strings.Contains(strings.ToLower(claudeCodeCLIModel(req.Model)), "haiku") {
		emit(map[string]interface{}{"output": "Haiku can edit project files. Build commands need an allow rule in your local Claude Code settings."})
	}
	prompt := buildRefinePrompt(req.ProjectPath, instructions)
	prompt += "\n\nClaude Code run: automatic Glowbom media generation is unavailable in this run. Use existing assets and report any media still needed. Do not create image placeholders (glowbomimages, glowbyimages, glowbomimage, or glowbyimage), glowbyvideo, or glowbyaudio placeholders. Do not stage, commit, or run other Git commands unless the user explicitly requested them. Other agents may work in this folder at the same time. Preserve unrelated changes and stay within this task. If permissions block an action, report what remains unfinished."
	summary, runErr := streamClaudeCode(r.Context(), binary, req.ProjectPath, claudeCodeArguments(req.Model, req.SessionID, req.PermissionMode), prompt, emit)
	changed, snapshotErr := detectChangedFilesFromSnapshot(req.ProjectPath, before)
	if snapshotErr != nil {
		emit(map[string]interface{}{"output": "Could not determine all changed files. Review the project folder."})
	}
	if runErr != nil {
		if len(onComplete) > 0 {
			onComplete[0]("failed", runErr.Error(), changed)
		}
		emit(map[string]interface{}{"done": true, "success": false, "error": runErr.Error(), "changedFiles": changed})
		return "failed", runErr.Error()
	}
	if len(onComplete) > 0 {
		onComplete[0]("completed", summary, changed)
	}
	emit(map[string]interface{}{"done": true, "success": true, "changedFiles": changed, "resultText": summary})
	return "completed", summary
}
