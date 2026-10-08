package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

// Keep the transport replaceable in focused tests without starting a signed-in CLI.
var codexRunTurn = runCodexTurn
var codexLoadChatModels = codexChatModels

const codexSessionPrefix = "codex:"

func codexModelID(model string) string {
	return strings.TrimPrefix(strings.TrimSpace(model), "codex/")
}

type codexTextStream struct {
	order  []string
	parts  map[string]string
	phases map[string]string
}

// Keep progress in the stream, but use the terminal answer for saved results.
// Older servers omit phase, so use their last message as the result.
func (s *codexTextStream) result() string {
	legacy := ""
	for index := len(s.order) - 1; index >= 0; index-- {
		id := s.order[index]
		text := strings.TrimSpace(s.parts[id])
		if text == "" {
			continue
		}
		if s.phases[id] == "final_answer" {
			return text
		}
		if s.phases[id] == "" && legacy == "" {
			legacy = text
		}
	}
	return legacy
}

func (s *codexTextStream) accept(message codexRPCMessage) (string, string, error) {
	var value struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
		Item   struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Text  string `json:"text"`
			Phase string `json:"phase"`
		} `json:"item"`
	}
	if json.Unmarshal(message.Params, &value) != nil {
		return "", "", errors.New("Codex returned an invalid progress event.")
	}
	if (message.Method == "item/started" || message.Method == "item/completed") && value.Item.Type == "agentMessage" && value.Item.ID != "" && value.Item.Phase != "" {
		if s.phases == nil {
			s.phases = map[string]string{}
		}
		s.phases[value.Item.ID] = value.Item.Phase
	}
	id, text, delta := "", "", ""
	switch message.Method {
	case "item/agentMessage/delta":
		id, delta = value.ItemID, value.Delta
	case "item/completed":
		if value.Item.Type == "agentMessage" {
			id, text = value.Item.ID, value.Item.Text
		}
	}
	if id == "" {
		return "", "", nil
	}
	if s.parts == nil {
		s.parts = map[string]string{}
	}
	if _, exists := s.parts[id]; !exists {
		s.order = append(s.order, id)
	}
	if message.Method == "item/agentMessage/delta" {
		if len(s.parts[id])+len(delta) > maxChatResultBytes {
			return "", "", errors.New("Codex returned too much text. Start a smaller request.")
		}
		s.parts[id] += delta
	} else {
		if len(text) > maxChatResultBytes {
			return "", "", errors.New("Codex returned too much text. Start a smaller request.")
		}
		if strings.HasPrefix(text, s.parts[id]) {
			delta = strings.TrimPrefix(text, s.parts[id])
		}
		s.parts[id] = text
	}
	var joined strings.Builder
	for index, key := range s.order {
		if index > 0 {
			joined.WriteString("\n\n")
		}
		joined.WriteString(s.parts[key])
		if joined.Len() > maxChatResultBytes {
			return "", "", errors.New("Codex returned too much text. Start a smaller request.")
		}
	}
	return joined.String(), delta, nil
}

type codexSummaryItem struct {
	parts     []string
	completed bool
}

type codexSummaryStream struct {
	order []string
	items map[string]*codexSummaryItem
	bytes int
	last  string
}

// Read only public summaries. Raw reasoning and encrypted items are not UI text.
func (s *codexSummaryStream) accept(message codexRPCMessage) (string, error) {
	switch message.Method {
	case "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/completed":
	default:
		return "", nil
	}
	var value struct {
		ItemID       string `json:"itemId"`
		SummaryIndex int    `json:"summaryIndex"`
		Delta        string `json:"delta"`
		Item         struct {
			ID      string   `json:"id"`
			Type    string   `json:"type"`
			Summary []string `json:"summary"`
		} `json:"item"`
	}
	if json.Unmarshal(message.Params, &value) != nil {
		return "", errors.New("Codex returned an invalid summary event.")
	}
	completed := message.Method == "item/completed"
	id := value.ItemID
	if completed {
		if value.Item.Type != "reasoning" || value.Item.Summary == nil {
			return "", nil
		}
		id = value.Item.ID
	} else if value.SummaryIndex < 0 || value.SummaryIndex >= 128 {
		return "", nil
	}
	if id == "" || len(id) > 1024 {
		return "", nil
	}
	if s.items == nil {
		s.items = map[string]*codexSummaryItem{}
	}
	item := s.items[id]
	if item == nil {
		if len(s.order) >= 128 {
			return "", nil
		}
		item = &codexSummaryItem{}
		s.items[id] = item
		s.order = append(s.order, id)
	}
	if completed {
		// The completed item's summary replaces its streamed sections.
		for _, part := range item.parts {
			s.bytes -= len(part)
		}
		item.parts = nil
		for _, part := range value.Item.Summary[:min(len(value.Item.Summary), 128)] {
			text := boundedChatReasoning(part, maxChatReasoningBytes-s.bytes)
			item.parts = append(item.parts, text)
			s.bytes += len(text)
		}
		item.completed = true
	} else {
		if item.completed {
			return "", nil
		}
		for len(item.parts) <= value.SummaryIndex {
			item.parts = append(item.parts, "")
		}
		if message.Method == "item/reasoning/summaryTextDelta" {
			text := boundedChatReasoning(value.Delta, maxChatReasoningBytes-s.bytes)
			item.parts[value.SummaryIndex] += text
			s.bytes += len(text)
		}
	}
	var joined strings.Builder
	for _, key := range s.order {
		for _, part := range s.items[key].parts {
			if strings.TrimSpace(part) == "" {
				continue
			}
			if joined.Len() > 0 {
				joined.WriteString("\n\n")
			}
			joined.WriteString(part)
		}
	}
	summary := boundedChatReasoning(joined.String(), maxChatReasoningBytes)
	if summary == s.last {
		return "", nil
	}
	s.last = summary
	return summary, nil
}

func completeCodexChat(ctx context.Context, directory, model, system string, parts []map[string]any, onText func(string), option chatCompletionOptions) (string, error) {
	input := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part["type"] {
		case "text":
			input = append(input, map[string]any{"type": "text", "text": part["text"]})
		case "file":
			input = append(input, map[string]any{"type": "image", "url": part["url"]})
		default:
			return "", errors.New("Codex does not support this conversation attachment.")
		}
	}
	stream, summaries, result := codexTextStream{}, codexSummaryStream{}, ""
	_, err := codexRunTurn(ctx, codexRunOptions{Directory: directory, Model: codexModelID(model), ReasoningEffort: option.ReasoningEffort, Instructions: system, Input: input, ChatOnly: true}, func(message codexRPCMessage) error {
		summary, err := summaries.accept(message)
		if err != nil {
			return err
		}
		if summary != "" && option.OnProgress != nil {
			option.OnProgress(chatProgress{Reasoning: summary})
		}
		text, _, err := stream.accept(message)
		if err != nil {
			return err
		}
		if text != "" {
			result = text
			if option.OnProgress != nil {
				option.OnProgress(chatProgress{Status: "Writing reply"})
			}
			if onText != nil {
				onText(text)
			}
		}
		return nil
	}, func(codexRPCMessage) (any, error) {
		return nil, errors.New("Tools and approvals are disabled in Glowbom chat.")
	})
	if err == nil {
		result = stream.result()
	}
	if err == nil && strings.TrimSpace(result) == "" {
		err = errors.New("Codex returned no text. Retry your message or choose another connected model.")
	}
	return result, err
}

type codexPendingInput struct {
	project   string
	session   string
	kind      string
	questions []string
	decisions map[string]any
	wireID    string
	ctx       context.Context
	response  chan any
}

var codexPendingInputs = struct {
	sync.Mutex
	items map[string]*codexPendingInput
}{items: map[string]*codexPendingInput{}}

func codexInputProject(path string) (string, error) {
	if path == "" {
		return "", errors.New("The selected project is required for this Codex response.")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func codexExecPolicyPrefix(raw json.RawMessage) []string {
	var decision map[string]json.RawMessage
	if json.Unmarshal(raw, &decision) != nil || len(decision) != 1 {
		return nil
	}
	var amendment map[string]json.RawMessage
	if json.Unmarshal(decision["acceptWithExecpolicyAmendment"], &amendment) != nil || len(amendment) != 1 {
		return nil
	}
	return companionCommandPrefix(amendment["execpolicy_amendment"])
}

func codexApprovalResponses(offered []json.RawMessage) ([]string, map[string]any) {
	// An older server can still accept or decline one action. Remembered
	// approvals require an explicitly advertised provider choice.
	if offered == nil {
		offered = []json.RawMessage{
			json.RawMessage(`"accept"`),
			json.RawMessage(`"decline"`), json.RawMessage(`"cancel"`),
		}
	}
	responses := []string{}
	decisions := map[string]any{}
	ambiguousPrefix := false
	for _, raw := range offered {
		var decision string
		if json.Unmarshal(raw, &decision) != nil {
			if codexExecPolicyPrefix(raw) == nil || ambiguousPrefix {
				continue
			}
			if previous, exists := decisions["execpolicy"]; exists {
				if string(previous.(json.RawMessage)) != string(raw) {
					delete(decisions, "execpolicy")
					ambiguousPrefix = true
				}
				continue
			}
			responses = append(responses, "execpolicy")
			decisions["execpolicy"] = append(json.RawMessage(nil), raw...)
			continue
		}
		var response string
		switch decision {
		case "accept":
			response = "once"
		case "acceptForSession":
			response = "session"
		case "decline":
			response = "reject"
		case "cancel":
			response = "cancel"
		default:
			continue
		}
		if _, exists := decisions[response]; exists {
			continue
		}
		responses = append(responses, response)
		decisions[response] = decision
	}
	if ambiguousPrefix {
		clean := responses[:0]
		for _, response := range responses {
			if response != "execpolicy" {
				clean = append(clean, response)
			}
		}
		responses = clean
	}
	return responses, decisions
}

func waitForCodexInput(ctx context.Context, project string, message codexRPCMessage, emit func(map[string]interface{}), fileChanges ...map[string]string) (any, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if message.Context != nil {
		if err := message.Context.Err(); err != nil {
			return nil, err
		}
		stop := context.AfterFunc(message.Context, cancel)
		defer stop()
	}
	var request struct {
		ThreadID               string            `json:"threadId"`
		ItemID                 string            `json:"itemId"`
		Command                string            `json:"command"`
		Cwd                    string            `json:"cwd"`
		Reason                 string            `json:"reason"`
		GrantRoot              string            `json:"grantRoot"`
		AvailableDecisions     []json.RawMessage `json:"availableDecisions"`
		NetworkApprovalContext *struct {
			Host     string `json:"host"`
			Protocol string `json:"protocol"`
		} `json:"networkApprovalContext"`
		Questions []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			IsSecret bool   `json:"isSecret"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(message.Params, &request) != nil || request.ThreadID == "" {
		return nil, errors.New("Codex returned an invalid input request.")
	}
	root, err := codexInputProject(project)
	if err != nil {
		return nil, err
	}
	id := "codex-" + randomUUIDString()
	pending := &codexPendingInput{project: root, session: codexSessionPrefix + request.ThreadID, kind: "permission", ctx: ctx, response: make(chan any, 1)}
	public := map[string]any{"id": id, "sessionID": pending.session}
	switch message.Method {
	case "item/commandExecution/requestApproval":
		public["availableResponses"], pending.decisions = codexApprovalResponses(request.AvailableDecisions)
		if amendment, offered := pending.decisions["execpolicy"]; offered {
			prefix := codexExecPolicyPrefix(amendment.(json.RawMessage))
			for index := range prefix {
				prefix[index] = companionPublicRunText(prefix[index], 512)
			}
			public["execPolicyAmendment"] = prefix
		}
		if len(pending.decisions) == 0 {
			return nil, errors.New("Codex did not offer an approval choice supported by Glowbom.")
		}
		public["title"], public["type"] = "Allow Codex to run this command?", "command"
		details := request.Reason
		if request.Cwd != "" {
			details += "\nFolder: " + request.Cwd
		}
		if request.NetworkApprovalContext != nil {
			details += "\nNetwork: " + request.NetworkApprovalContext.Protocol + "://" + request.NetworkApprovalContext.Host
		}
		// Do not display a truncated command as the action the owner approves.
		if len(request.Command) > 2000 || len(details) > 2000 {
			return nil, errors.New("This Codex approval is too large to review in Glowbom.")
		}
		public["message"] = companionPublicRunText(details, 2000)
		public["pattern"] = companionPublicRunText(request.Command, 4000)
	case "item/fileChange/requestApproval":
		if request.GrantRoot != "" {
			return nil, errors.New("Persistent Codex folder grants are not supported. Request approval for individual file changes.")
		}
		public["availableResponses"], pending.decisions = codexApprovalResponses(request.AvailableDecisions)
		// Command-prefix grants do not apply to file approval requests.
		delete(pending.decisions, "execpolicy")
		responses := public["availableResponses"].([]string)
		clean := responses[:0]
		for _, response := range responses {
			if response != "execpolicy" {
				clean = append(clean, response)
			}
		}
		public["availableResponses"] = clean
		if len(pending.decisions) == 0 {
			return nil, errors.New("Codex did not offer a supported file approval choice.")
		}
		public["title"], public["type"] = "Allow these Codex file changes?", "edit"
		public["message"] = companionPublicRunText(request.Reason, 2000)
		if len(fileChanges) > 0 {
			paths := fileChanges[0][request.ItemID]
			if len(paths) > 2000 {
				return nil, errors.New("This Codex file approval is too large to review in Glowbom.")
			}
			public["pattern"] = companionPublicRunText(paths, 2000)
		}
	case "item/tool/requestUserInput":
		if len(request.Questions) == 0 || len(request.Questions) > 16 {
			return nil, errors.New("Codex returned unsupported questions.")
		}
		pending.kind = "question"
		questions := []map[string]any{}
		seen := map[string]bool{}
		for _, question := range request.Questions {
			if question.IsSecret || question.ID == "" || len(question.ID) > 160 || seen[question.ID] || len(question.Options) > 32 {
				return nil, errors.New("This Codex question cannot be answered through Glowbom.")
			}
			seen[question.ID] = true
			pending.questions = append(pending.questions, question.ID)
			options := []map[string]string{}
			for _, option := range question.Options {
				options = append(options, map[string]string{"id": option.Label, "label": option.Label, "description": companionPublicRunText(option.Description, 2000)})
			}
			questions = append(questions, map[string]any{"id": question.ID, "prompt": companionPublicRunText(question.Question, 4000), "options": options})
		}
		public["prompt"], public["questions"] = questions[0]["prompt"], questions
	default:
		return nil, errors.New("Glowbom does not support this Codex tool request.")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	permissionChanged := buildPermissionChanged(ctx)
	requestID := id
	if len(message.ID) > 0 && string(message.ID) != "null" {
		requestID = string(message.ID)
	}
	pending.wireID = requestID
	if pending.kind == "permission" {
		once, offered := pending.decisions["once"]
		if buildPermissionAllAvailable(ctx, "codex", root, pending.session, offered) {
			public["availableResponses"] = append(public["availableResponses"].([]string), "all")
		}
		if offered && buildPermissionAllEnabled(ctx, "codex", root, pending.session) && buildPermissionClaim(ctx, "codex", root, pending.session, requestID) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return map[string]any{"decision": once}, nil
		}
	}
	codexPendingInputs.Lock()
	codexPendingInputs.items[id] = pending
	codexPendingInputs.Unlock()
	defer func() {
		codexPendingInputs.Lock()
		delete(codexPendingInputs.items, id)
		codexPendingInputs.Unlock()
	}()
	emit(map[string]interface{}{pending.kind: public})
	for {
		select {
		case response := <-pending.response:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return response, nil
		case <-permissionChanged:
			permissionChanged = buildPermissionChanged(ctx)
			once, offered := pending.decisions["once"]
			if pending.kind == "permission" && offered && buildPermissionAllEnabled(ctx, "codex", root, pending.session) && buildPermissionClaim(ctx, "codex", root, pending.session, requestID) {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return map[string]any{"decision": once}, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func respondToCodexInput(project, session, id, kind string, response string, answers QuestionAnswers, byID AnswerByQuestionID) error {
	root, err := codexInputProject(project)
	if err != nil {
		return err
	}
	codexPendingInputs.Lock()
	defer codexPendingInputs.Unlock()
	pending := codexPendingInputs.items[id]
	if pending == nil || pending.project != root || pending.session != session || pending.kind != kind || pending.ctx.Err() != nil {
		return errors.New("This Codex request is no longer waiting in the selected project.")
	}
	var result any
	if kind == "permission" {
		decision, offered := pending.decisions[response]
		if !offered {
			return errors.New("Choose one of the approval options offered for this Codex request.")
		}
		if value, ok := decision.(string); ok {
			result = map[string]string{"decision": value}
		} else {
			// Return the exact offered amendment, never a client-supplied prefix.
			result = map[string]any{"decision": decision}
		}
	} else {
		if len(byID) > len(pending.questions) || len(answers) > 0 && len(answers) != len(pending.questions) {
			return errors.New("Answer each displayed Codex question.")
		}
		values := map[string]any{}
		for index, question := range pending.questions {
			selected := byID[question]
			if len(selected) == 0 && len(answers) > index {
				selected = answers[index]
			}
			if len(selected) == 0 && len(pending.questions) == 1 && strings.TrimSpace(response) != "" {
				selected = []string{response}
			}
			if len(selected) == 0 || len(selected) > 32 {
				return errors.New("Answer each displayed Codex question.")
			}
			for _, answer := range selected {
				if strings.TrimSpace(answer) == "" || len(answer) > 4000 {
					return errors.New("Keep each Codex answer under 4000 characters.")
				}
			}
			values[question] = map[string]any{"answers": append([]string(nil), selected...)}
		}
		for question := range byID {
			if _, ok := values[question]; !ok {
				return errors.New("This answer does not match a displayed Codex question.")
			}
		}
		result = map[string]any{"answers": values}
	}
	if kind == "permission" {
		buildPermissionReserve(pending.ctx, "codex", root, session, pending.wireID)
	}
	select {
	case pending.response <- result:
		delete(codexPendingInputs.items, id)
		return nil
	default:
		return errors.New("This Codex request has already been answered.")
	}
}

func runCodexRefine(w http.ResponseWriter, r *http.Request, req OpenCodeAgentRequest, instructions string, onComplete func(string, string, []string)) (string, string) {
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
	prompt := buildRefinePrompt(req.ProjectPath, instructions)
	prompt += "\n\nCodex run: automatic Glowbom media generation is unavailable in this run. Use existing assets and report any media still needed. Do not create image, video, or audio placeholders. Do not stage, commit, or run other Git commands unless the user explicitly requested them."
	threadID := ""
	if strings.HasPrefix(req.SessionID, codexSessionPrefix) {
		threadID = strings.TrimPrefix(req.SessionID, codexSessionPrefix)
	}
	emit(map[string]interface{}{"output": "Starting Codex..."})
	stream := codexTextStream{}
	fileChanges := map[string]string{}
	_, runErr := codexRunTurn(r.Context(), codexRunOptions{Directory: req.ProjectPath, Model: codexModelID(req.Model), ReasoningEffort: req.ReasoningEffort, ThreadID: threadID, Instructions: "You are building the selected Glowbom project. Follow the project instructions and request approval when required.", Input: []map[string]any{{"type": "text", "text": prompt}}}, func(message codexRPCMessage) error {
		if message.Method == "thread/started" {
			var value struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if json.Unmarshal(message.Params, &value) == nil && value.Thread.ID != "" {
				emit(map[string]interface{}{"output": "Session created: " + codexSessionPrefix + value.Thread.ID})
			}
		}
		_, delta, err := stream.accept(message)
		if err != nil {
			return err
		}
		if delta != "" {
			emit(map[string]interface{}{"outputChunk": delta})
		}
		if message.Method == "item/started" {
			var value struct {
				Item struct {
					ID      string `json:"id"`
					Type    string `json:"type"`
					Changes []struct {
						Path string `json:"path"`
					} `json:"changes"`
				} `json:"item"`
			}
			_ = json.Unmarshal(message.Params, &value)
			switch value.Item.Type {
			case "commandExecution":
				emit(map[string]interface{}{"output": "Codex is running a project command..."})
			case "fileChange":
				paths := []string{}
				for _, change := range value.Item.Changes {
					paths = append(paths, change.Path)
				}
				fileChanges[value.Item.ID] = strings.Join(paths, "\n")
				emit(map[string]interface{}{"output": "Codex is updating project files..."})
			}
		}
		return nil
	}, func(message codexRPCMessage) (any, error) {
		return waitForCodexInput(r.Context(), req.ProjectPath, message, emit, fileChanges)
	})
	changed, snapshotErr := detectChangedFilesFromSnapshot(req.ProjectPath, before)
	if snapshotErr != nil {
		emit(map[string]interface{}{"output": "Could not determine all changed files. Review the project folder."})
	}
	status := "completed"
	summary := stream.result()
	if runErr != nil {
		status, summary = "failed", sanitizeProviderError(runErr)
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

func codexInputResponseError(w http.ResponseWriter, err error) {
	http.Error(w, fmt.Sprintf("Could not answer Codex: %s", err), http.StatusConflict)
}
