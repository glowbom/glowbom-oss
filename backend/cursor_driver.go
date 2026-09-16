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

// Cursor owns account credentials. Never import its login tokens into Glowbom.
func cursorExecutable() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("GLOWBOM_CURSOR_BIN")); configured != "" {
		if path, err := exec.LookPath(configured); err == nil {
			return path, nil
		}
		return "", errors.New("GLOWBOM_CURSOR_BIN does not point to an executable Cursor CLI")
	}
	if path, err := exec.LookPath("cursor-agent"); err == nil {
		return path, nil
	}
	// The installer also puts cursor-agent here when GUI PATH omits ~/.local/bin.
	if home, err := os.UserHomeDir(); err == nil {
		if path, err := exec.LookPath(filepath.Join(home, ".local", "bin", "cursor-agent")); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Cursor CLI is missing. Install it from cursor.com/docs/cli/installation, then run cursor-agent login. If installed as agent, set GLOWBOM_CURSOR_BIN to its full path")
}

func cursorHealthHandler(w http.ResponseWriter, r *http.Request) {
	path, err := cursorExecutable()
	if err != nil {
		writeJSON(w, map[string]interface{}{"healthy": false, "hint": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "status", "--format", "json")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	cmd.WaitDelay = time.Second
	configureCursorCancellation(cmd)
	if err := cmd.Run(); err != nil {
		writeJSON(w, map[string]interface{}{"healthy": false, "hint": "Cursor login check failed. Run cursor-agent login and cursor-agent status, then refresh."})
		return
	}
	var status struct {
		IsAuthenticated bool `json:"isAuthenticated"`
	}
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		writeJSON(w, map[string]interface{}{"healthy": false, "hint": "Could not read Cursor login status. Update Cursor CLI and refresh."})
		return
	}
	if !status.IsAuthenticated {
		writeJSON(w, map[string]interface{}{"healthy": false, "hint": "Cursor CLI is installed. Run cursor-agent login, finish signing in, then refresh."})
		return
	}
	writeJSON(w, map[string]interface{}{"healthy": true, "server": "Cursor CLI"})
}

var cursorSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func cursorArguments(model, session string) []string {
	args := []string{"--print", "--force", "--output-format", "stream-json"}
	if model = strings.TrimSpace(model); model != "" {
		args = append(args, "--model", model)
	}
	if strings.HasPrefix(session, "cursor-") {
		id := strings.TrimPrefix(session, "cursor-")
		if cursorSessionID.MatchString(id) {
			args = append(args, "--resume", id)
		}
	}
	return args
}

type cursorEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// A zero exit without a successful terminal event is an interrupted run, not success.
func streamCursor(ctx context.Context, binary, project string, args []string, prompt string, emit func(map[string]interface{})) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = project
	cmd.Stdin = strings.NewReader(prompt)
	// CLI diagnostics can contain credentials. Only normalized failures reach the UI.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	configureCursorCancellation(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", errors.New("Could not open Cursor output")
	}
	if err := cmd.Start(); err != nil {
		return "", errors.New("Could not start Cursor CLI")
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var result string
	var terminal, success, announced, hadText bool
	var streamErr error
	for scanner.Scan() {
		var event cursorEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			streamErr = errors.New("Cursor returned invalid streaming output. Update Cursor CLI and retry")
			cancel()
			break
		}
		switch event.Type {
		case "system":
			if event.Subtype == "init" && !announced && cursorSessionID.MatchString(event.SessionID) {
				emit(map[string]interface{}{"output": "Session created: cursor-" + event.SessionID})
				announced = true
			}
		case "assistant":
			for _, part := range event.Message.Content {
				if part.Type == "text" && part.Text != "" {
					hadText = true
					emit(map[string]interface{}{"output": part.Text})
				}
			}
		case "tool_call":
			if event.Subtype == "started" {
				emit(map[string]interface{}{"output": "Cursor is working on the project..."})
			}
		case "result":
			terminal, success, result = true, event.Subtype == "success" && !event.IsError, event.Result
		case "error":
			streamErr = errors.New("Cursor reported an error. Check cursor-agent status and your Cursor usage, then retry")
			cancel()
		}
	}
	if scanner.Err() != nil {
		streamErr = errors.New("Could not read Cursor output")
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil && streamErr == nil {
		return "", ctx.Err()
	}
	if streamErr != nil {
		return "", streamErr
	}
	if waitErr != nil {
		return "", errors.New("Cursor failed. Check cursor-agent status, your model and usage limits, and workspace trust in Cursor CLI")
	}
	if !terminal || !success {
		return "", errors.New("Cursor ended without a successful result")
	}
	if !hadText && result != "" {
		emit(map[string]interface{}{"output": result})
	}
	return result, nil
}

func runCursorRefine(w http.ResponseWriter, r *http.Request, req OpenCodeAgentRequest, instructions string) (string, string) {
	binary, err := cursorExecutable()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return "failed", err.Error()
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
	emit := func(event map[string]interface{}) { sendSSEData(w, flusher, event) }
	emit(map[string]interface{}{"output": "Starting Cursor. File edits and commands run under your local Cursor configuration."})
	prompt := buildRefinePrompt(req.ProjectPath, instructions)
	prompt += "\n\nCursor run: automatic Glowbom media generation is unavailable in this run. Use existing assets and report any media still needed. Do not create image placeholders (glowbomimages, glowbyimages, glowbomimage, or glowbyimage), glowbyvideo, or glowbyaudio placeholders. Do not stage, commit, or run other Git commands unless the user explicitly requested them."
	summary, runErr := streamCursor(r.Context(), binary, req.ProjectPath, cursorArguments(req.Model, req.SessionID), prompt, emit)
	changed, snapshotErr := detectChangedFilesFromSnapshot(req.ProjectPath, before)
	if snapshotErr != nil {
		emit(map[string]interface{}{"output": "Could not determine all changed files. Review the project folder."})
	}
	if runErr != nil {
		emit(map[string]interface{}{"done": true, "success": false, "error": runErr.Error(), "changedFiles": changed})
		return "failed", runErr.Error()
	}
	emit(map[string]interface{}{"done": true, "success": true, "changedFiles": changed})
	return "completed", summary
}
