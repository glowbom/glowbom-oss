package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

const acpSessionPrefix = "acp:"

var acpRunTurn = runACPTurn

// Catalog reads never start user-configured executables.
func listACPModels() []chatModel {
	profiles, err := loadACPProfiles()
	if err != nil {
		return nil
	}
	models := make([]chatModel, 0, len(profiles))
	for _, profile := range profiles {
		models = append(models, chatModel{ID: "acp/" + profile.ID, Name: profile.Name, Provider: "ACP", Build: true})
	}
	return models
}

func acpModelProfile(model string) (acpProfile, error) {
	if !strings.HasPrefix(model, "acp/") {
		return acpProfile{}, errors.New("Choose a saved ACP connection in Settings.")
	}
	return resolveACPProfile(strings.TrimPrefix(model, "acp/"))
}

// Sessions belong to the exact connection configuration and canonical project.
// Replacing an agent in a saved slot cannot resume the previous agent's session.
func acpSessionNamespace(profile acpProfile, project string) string {
	sum := sha256.Sum256([]byte(acpProfileFingerprint(profile) + "\x00" + project))
	return acpSessionPrefix + profile.ID + ":" + hex.EncodeToString(sum[:]) + ":"
}

func acpSavedSession(profile acpProfile, project, session string) string {
	return acpSessionNamespace(profile, project) + base64.RawURLEncoding.EncodeToString([]byte(session))
}

func acpResumeSession(profile acpProfile, project, saved string) string {
	namespace := acpSessionNamespace(profile, project)
	if !strings.HasPrefix(saved, namespace) || !validSavedACPSession(saved) {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(saved, namespace))
	if err != nil || len(data) == 0 || len(data) > 512 || strings.ContainsAny(string(data), "\r\n\x00") {
		return ""
	}
	return string(data)
}

func validSavedACPSession(value string) bool {
	if strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	parts := strings.Split(value, ":")
	if len(parts) != 4 || parts[0] != "acp" || (parts[1] != "acp-1" && parts[1] != "acp-2" && parts[1] != "acp-3") || len(parts[2]) != 64 || len(value) > 2048 {
		return false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return false
	}
	session, err := base64.RawURLEncoding.DecodeString(parts[3])
	return err == nil && len(session) > 0 && len(session) <= 512 && !strings.ContainsAny(string(session), "\r\n\x00")
}

func acpPrompt(project, instructions string, attachments []stagedInstructionAttachment) ([]map[string]any, error) {
	prompt := buildRefinePrompt(project, instructions)
	prompt += "\n\nUse existing assets and report any media still needed. Automatic Glowbom media generation is unavailable for this run. Do not stage, commit, or run other Git commands unless the user explicitly requested them."
	parts := []map[string]any{{"type": "text", "text": prompt}}
	if len(attachments) == 0 {
		return parts, nil
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, errors.New("Could not read the selected project's attachments.")
	}
	defer root.Close()
	var total int
	for _, attachment := range attachments {
		file, err := root.Open(attachment.RelativePath)
		if err != nil {
			return nil, errors.New("Could not read a build attachment. Attach it again.")
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			file.Close()
			return nil, errors.New("Build attachments must be regular files.")
		}
		header := make([]byte, 512)
		n, readErr := file.Read(header)
		if readErr != nil && readErr != io.EOF {
			file.Close()
			return nil, errors.New("Could not read a build attachment.")
		}
		mime := http.DetectContentType(header[:n])
		if !strings.HasPrefix(mime, "image/") {
			file.Close()
			continue
		}
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
			file.Close()
			return nil, errors.New("ACP image attachments must be PNG, JPEG, or WebP.")
		}
		data, err := io.ReadAll(io.LimitReader(file, (10<<20)+1))
		file.Close()
		data = append(header[:n:n], data...)
		total += len(data)
		if err != nil || total > 10<<20 {
			return nil, errors.New("Choose ACP images totaling no more than 10 MB.")
		}
		parts = append(parts, map[string]any{"type": "image", "mimeType": mime, "data": base64.StdEncoding.EncodeToString(data)})
	}
	return parts, nil
}

type acpPendingApproval struct {
	project  string
	session  string
	ctx      context.Context
	choices  map[string]string
	wireID   string
	response chan string
}

var acpPendingPermissions = struct {
	sync.Mutex
	items map[string]*acpPendingApproval
}{items: map[string]*acpPendingApproval{}}

func waitForACPPermission(ctx context.Context, profile acpProfile, project string, message acpMessage, emit func(map[string]interface{}), approvals *acpBuildApprovals) (any, error) {
	if message.Method != "session/request_permission" {
		return nil, errors.New("This ACP request is not supported by Glowbom.")
	}
	var request struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			Name      *string         `json:"name"`
			Title     string          `json:"title"`
			Kind      string          `json:"kind"`
			RawInput  json.RawMessage `json:"rawInput"`
			Locations json.RawMessage `json:"locations"`
			Content   json.RawMessage `json:"content"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if json.Unmarshal(message.Params, &request) != nil || request.SessionID == "" || len(request.SessionID) > 512 || len(request.Options) > 32 || len(request.ToolCall.Title) > 240 {
		return nil, errors.New("The ACP agent sent an unsupported permission request.")
	}
	tool := acpBuildPermissionTool(request.ToolCall.Name, request.ToolCall.Title)
	scope := acpBuildPermissionScope{session: acpSavedSession(profile, project, request.SessionID), kind: request.ToolCall.Kind, tool: tool}
	choices := map[string]string{"cancel": ""}
	responses := []string{}
	seenOptions := map[string]bool{}
	for _, option := range request.Options {
		if option.OptionID == "" || len(option.OptionID) > 256 || strings.ContainsAny(option.OptionID, "\r\n\x00") || seenOptions[option.OptionID] {
			return nil, errors.New("The ACP agent sent an invalid permission choice.")
		}
		seenOptions[option.OptionID] = true
		response := ""
		switch option.Kind {
		case "allow_once":
			response = "once"
		case "reject_once":
			response = "reject"
		}
		// ACP always choices may persist beyond this session. Never translate
		// them into Glowbom's session-only grants.
		if response != "" {
			if _, exists := choices[response]; !exists {
				choices[response] = option.OptionID
				responses = append(responses, response)
				if response == "once" && tool != "" && approvals.active() {
					choices["build"] = option.OptionID
					responses = append(responses, "build")
				}
			}
		}
	}
	responses = append(responses, "cancel")
	details := []string{}
	for _, value := range []json.RawMessage{request.ToolCall.RawInput, request.ToolCall.Locations, request.ToolCall.Content} {
		if len(value) > 0 && string(value) != "null" && string(value) != "[]" && string(value) != "{}" {
			details = append(details, string(value))
		}
	}
	pattern := strings.Join(details, "\n")
	if len(pattern) > 2000 {
		return nil, errors.New("This ACP action is too large to review in Glowbom. Request a smaller action.")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	id := "acp-" + randomUUIDString()
	permissionChanged := buildPermissionChanged(ctx)
	once, offered := choices["once"]
	if buildPermissionAllAvailable(ctx, "acp", project, scope.session, offered) {
		responses = append(responses, "all")
	}
	requestID := id
	if len(message.ID) > 0 && string(message.ID) != "null" {
		requestID = string(message.ID)
	}
	if offered && buildPermissionAllEnabled(ctx, "acp", project, scope.session) && buildPermissionClaim(ctx, "acp", project, scope.session, requestID) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": once}}, nil
	}
	if once, offered := choices["build"]; offered && approvals.permits(scope) {
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": once}}, nil
	}
	pending := &acpPendingApproval{project: project, session: acpSavedSession(profile, project, request.SessionID), ctx: ctx, choices: choices, wireID: requestID, response: make(chan string, 1)}
	acpPendingPermissions.Lock()
	acpPendingPermissions.items[id] = pending
	acpPendingPermissions.Unlock()
	defer func() {
		acpPendingPermissions.Lock()
		delete(acpPendingPermissions.items, id)
		acpPendingPermissions.Unlock()
	}()
	permission := map[string]any{"id": id, "sessionID": pending.session, "title": "Allow " + profile.Name + "?", "type": request.ToolCall.Kind, "message": request.ToolCall.Title, "pattern": pattern, "availableResponses": responses}
	if _, offered := choices["build"]; offered {
		permission["buildApprovalTool"] = tool
	}
	emit(map[string]interface{}{"permission": permission})
	for {
		select {
		case <-permissionChanged:
			permissionChanged = buildPermissionChanged(ctx)
			if offered && buildPermissionAllEnabled(ctx, "acp", project, scope.session) && buildPermissionClaim(ctx, "acp", project, scope.session, requestID) {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": once}}, nil
			}
		case response := <-pending.response:
			if response == "cancel" || ctx.Err() != nil {
				return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
			}
			if response == "build" {
				if !approvals.remember(scope) {
					return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
				}
				emit(map[string]interface{}{"output": "Allowed " + tool + " for this build."})
			}
			return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": choices[response]}}, nil
		case <-ctx.Done():
			return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
		}
	}
}

func respondToACPPermission(project, session, id, response string) error {
	root, err := codexInputProject(project)
	if err != nil {
		return err
	}
	acpPendingPermissions.Lock()
	defer acpPendingPermissions.Unlock()
	pending := acpPendingPermissions.items[id]
	if pending == nil || pending.project != root || pending.session != session || pending.ctx.Err() != nil {
		return errors.New("This ACP permission is no longer waiting in the selected project.")
	}
	if _, offered := pending.choices[response]; !offered {
		return errors.New("Choose one of the permission options offered by this ACP agent.")
	}
	buildPermissionReserve(pending.ctx, "acp", root, session, pending.wireID)
	select {
	case pending.response <- response:
		delete(acpPendingPermissions.items, id)
		return nil
	default:
		return errors.New("This ACP permission has already been answered.")
	}
}

func runACPRefine(w http.ResponseWriter, r *http.Request, req OpenCodeAgentRequest, profile acpProfile, instructions string, attachments []stagedInstructionAttachment, onComplete func(string, string, []string)) (string, string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return "failed", "Streaming not supported"
	}
	project, err := codexInputProject(req.ProjectPath)
	if err != nil {
		http.Error(w, "Could not open the selected project", http.StatusBadRequest)
		return "failed", "Could not open the selected project"
	}
	before, err := captureProjectFileSnapshot(project)
	if err != nil {
		http.Error(w, "Could not inspect project files", http.StatusBadRequest)
		return "failed", "Could not inspect project files"
	}
	parts, err := acpPrompt(project, instructions, attachments)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "failed", err.Error()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	var outputMu, permissionMu sync.Mutex
	approvals := newACPBuildApprovals(r.Context())
	defer approvals.close()
	emit := func(event map[string]interface{}) {
		outputMu.Lock()
		defer outputMu.Unlock()
		if runID := w.Header().Get("X-Glowbom-Run-ID"); runID != "" {
			event["runId"] = runID
		}
		sendSSEData(w, flusher, event)
	}
	emit(map[string]interface{}{"agentName": profile.Name, "output": "Starting " + profile.Name + "..."})
	var result strings.Builder
	_, runErr := acpRunTurn(r.Context(), profile, project, acpResumeSession(profile, project, req.SessionID), parts, func(message acpMessage) error {
		if message.Method == "glowbom/session" {
			var session struct {
				SessionID string `json:"sessionId"`
				Model     string `json:"model"`
			}
			if json.Unmarshal(message.Params, &session) != nil || session.SessionID == "" || len(session.SessionID) > 512 || strings.ContainsAny(session.SessionID, "\r\n\x00") {
				return errors.New("The ACP agent returned an invalid session.")
			}
			saved := acpSavedSession(profile, project, session.SessionID)
			emit(map[string]interface{}{"sessionID": saved, "output": "Session created: " + saved})
			if validACPText(session.Model, 256, true) {
				emit(map[string]interface{}{"output": "Using model: " + session.Model})
			}
			return nil
		}
		if message.Method != "session/update" {
			return nil
		}
		var value struct {
			Update struct {
				SessionUpdate string          `json:"sessionUpdate"`
				Title         string          `json:"title"`
				Status        string          `json:"status"`
				Content       json.RawMessage `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(message.Params, &value) != nil {
			return errors.New("The ACP agent returned an invalid progress event.")
		}
		switch value.Update.SessionUpdate {
		case "agent_message_chunk":
			var content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(value.Update.Content, &content) != nil || content.Type != "text" {
				return nil
			}
			if result.Len()+len(content.Text) > maxChatResultBytes {
				return errors.New("The ACP agent returned too much text. Start a smaller request.")
			}
			result.WriteString(content.Text)
			emit(map[string]interface{}{"outputChunk": content.Text})
		case "tool_call", "tool_call_update":
			if value.Update.Title != "" {
				emit(map[string]interface{}{"output": companionPublicRunText(value.Update.Title, 1000)})
			}
		}
		return nil
	}, func(ctx context.Context, message acpMessage) (any, error) {
		permissionMu.Lock()
		defer permissionMu.Unlock()
		return waitForACPPermission(ctx, profile, project, message, emit, approvals)
	})
	changed, snapshotErr := detectChangedFilesFromSnapshot(project, before)
	if snapshotErr != nil {
		emit(map[string]interface{}{"output": "Could not determine all changed files. Review the project folder."})
	}
	status, summary := "completed", strings.TrimSpace(result.String())
	if runErr == nil && summary == "" && len(changed) == 0 {
		runErr = errors.New("The ACP agent ended without a response or file changes. Check the agent's model, login, and available credits, then retry.")
	}
	if runErr != nil {
		status, summary = "failed", sanitizeProviderError(runErr)
		if r.Context().Err() != nil {
			status, summary = "canceled", "Build stopped."
		}
	}
	if onComplete != nil {
		onComplete(status, summary, changed)
	}
	if runErr != nil {
		emit(map[string]interface{}{"done": true, "success": false, "error": summary, "changedFiles": changed})
	} else {
		emit(map[string]interface{}{"done": true, "success": true, "resultText": summary, "changedFiles": changed})
	}
	return status, summary
}
