package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A steer belongs to one active build, not merely to an OpenCode session. A
// session can be reused by a later build in the same project.
type openCodeSteerRun struct {
	mu          sync.Mutex
	projectPath string
	runID       string
	sessionID   string
	serverURL   string
	providerID  string
	modelID     string
	accepting   bool
	sent        map[string]*openCodeSteerDelivery
	nudgeID     string
}

type openCodeSteerDelivery struct {
	reference string
	messageID string
	persisted bool
	nudged    bool
}

var openCodeSteerRuns = struct {
	sync.Mutex
	byProject map[string]map[string]*openCodeSteerRun
}{byProject: make(map[string]map[string]*openCodeSteerRun)}

func registerOpenCodeSteerRun(projectPath, runID, sessionID, providerID, modelID, serverURL string) (*openCodeSteerRun, error) {
	root, err := chatProjectRoot(projectPath)
	if err != nil {
		return nil, err
	}
	if runID == "" || sessionID == "" || serverURL == "" {
		return nil, fmt.Errorf("build session is not ready for steering")
	}
	run := &openCodeSteerRun{
		projectPath: root,
		runID:       runID,
		sessionID:   sessionID,
		serverURL:   serverURL,
		providerID:  providerID,
		modelID:     modelID,
		accepting:   true,
		sent:        make(map[string]*openCodeSteerDelivery),
	}
	openCodeSteerRuns.Lock()
	defer openCodeSteerRuns.Unlock()
	if openCodeSteerRuns.byProject[root] == nil {
		openCodeSteerRuns.byProject[root] = make(map[string]*openCodeSteerRun)
	}
	openCodeSteerRuns.byProject[root][runID] = run
	return run, nil
}

func (run *openCodeSteerRun) close() {
	if run == nil {
		return
	}
	// A prompt submission holds this lock. Closing waits for that submission so
	// the build cannot enter its post-pass while a steer is still being sent.
	run.mu.Lock()
	run.accepting = false
	run.mu.Unlock()
	openCodeSteerRuns.Lock()
	if runs := openCodeSteerRuns.byProject[run.projectPath]; runs != nil {
		if runs[run.runID] == run {
			delete(runs, run.runID)
		}
		if len(runs) == 0 {
			delete(openCodeSteerRuns.byProject, run.projectPath)
		}
	}
	openCodeSteerRuns.Unlock()
}

func activeOpenCodeSteerRun(projectPath, runID string) *openCodeSteerRun {
	openCodeSteerRuns.Lock()
	defer openCodeSteerRuns.Unlock()
	return openCodeSteerRuns.byProject[projectPath][runID]
}

func activeOpenCodeSteerRunBySession(runID, sessionID string) *openCodeSteerRun {
	openCodeSteerRuns.Lock()
	defer openCodeSteerRuns.Unlock()
	for _, runs := range openCodeSteerRuns.byProject {
		if run := runs[runID]; run != nil && run.sessionID == sessionID {
			return run
		}
	}
	return nil
}

type openCodeSteerRequest struct {
	ProjectPath  string `json:"projectPath"`
	RunID        string `json:"runId"`
	MessageIndex int    `json:"messageIndex"`
	Role         string `json:"role"`
	Text         string `json:"text"`
}

func openCodeSteerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req openCodeSteerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "Invalid steer request", http.StatusBadRequest)
		return
	}
	if req.RunID == "" || req.MessageIndex < 0 || (req.Role != "user" && req.Role != "assistant") || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "Choose a saved chat message from the active build.", http.StatusBadRequest)
		return
	}
	root, err := chatProjectRoot(req.ProjectPath)
	if err != nil {
		http.Error(w, "Open a Glowbom project first.", http.StatusBadRequest)
		return
	}
	run := activeOpenCodeSteerRun(root, req.RunID)
	if run == nil {
		http.Error(w, "This build is no longer accepting steers.", http.StatusConflict)
		return
	}
	if !matchesSavedSteerMessage(root, req) {
		http.Error(w, "This chat message has changed. Choose it again.", http.StatusConflict)
		return
	}

	key := steerMessageKey(req)
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.accepting {
		http.Error(w, "This build is no longer accepting steers.", http.StatusConflict)
		return
	}
	if previous := run.sent[key]; previous != nil {
		if !previous.persisted {
			checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			previous.messageID = run.waitForMessage(checkCtx, previous.reference, time.Second)
			previous.persisted = previous.messageID != ""
			cancel()
		}
		status := "queued"
		if previous.persisted {
			status = "sent"
		}
		writeJSON(w, map[string]any{"status": status, "runId": run.runID, "messageIndex": req.MessageIndex})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	reference := steerReference(run.runID, key)
	delivery := &openCodeSteerDelivery{reference: reference}
	run.sent[key] = delivery
	if err := run.send(ctx, req, reference); err != nil {
		var providerError *chatProviderError
		if errors.As(err, &providerError) || errors.Is(err, errChatServerAuth) {
			delete(run.sent, key)
			http.Error(w, "OpenCode rejected this steer. Check the build connection and try again.", http.StatusBadGateway)
			return
		}
		// A transport error can follow an accepted async request. Keep the same
		// reference and reconcile it rather than risk sending the steer twice.
		delivery.messageID = run.waitForMessage(ctx, reference, time.Second)
		delivery.persisted = delivery.messageID != ""
		status := "queued"
		if delivery.persisted {
			status = "sent"
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "runId": run.runID, "messageIndex": req.MessageIndex})
		return
	}
	// OpenCode returns 204 before its forked prompt has saved the user message.
	// A queued response is intentionally weaker than a confirmed stored steer.
	delivery.messageID = run.waitForMessage(ctx, reference, 4*time.Second)
	delivery.persisted = delivery.messageID != ""
	status := "queued"
	if delivery.persisted {
		status = "sent"
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "runId": run.runID, "messageIndex": req.MessageIndex})
}

func matchesSavedSteerMessage(projectPath string, req openCodeSteerRequest) bool {
	saved, err := readChatProjectFile(projectPath, ".glowbom/chat.json", maxChatHistoryBytes)
	if err != nil || saved == "" {
		return false
	}
	var messages []chatMessage
	if json.Unmarshal([]byte(saved), &messages) != nil || req.MessageIndex >= len(messages) {
		return false
	}
	message := messages[req.MessageIndex]
	return message.Role == req.Role && message.Text == req.Text
}

func steerMessageKey(req openCodeSteerRequest) string {
	sum := sha256.Sum256([]byte(req.Role + "\x00" + req.Text))
	return strconv.Itoa(req.MessageIndex) + ":" + hex.EncodeToString(sum[:])
}

func steerReference(runID, key string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + key))
	return hex.EncodeToString(sum[:16])
}

func (run *openCodeSteerRun) service() *chatService {
	return &chatService{directory: run.projectPath, serverURL: run.serverURL, client: openCodeHTTPClient(&http.Client{Timeout: 12 * time.Second})}
}

func (run *openCodeSteerRun) send(ctx context.Context, req openCodeSteerRequest, reference string) error {
	payload := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": steerPromptText(req, reference)}},
	}
	if run.providerID != "" && run.modelID != "" {
		payload["model"] = map[string]string{"providerID": run.providerID, "modelID": run.modelID}
	}
	response, err := run.service().request(ctx, http.MethodPost, "/session/"+url.PathEscape(run.sessionID)+"/prompt_async", payload)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}

func (run *openCodeSteerRun) waitForMessage(ctx context.Context, reference string, limit time.Duration) string {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		stored, err := run.sessionMessages(checkCtx)
		cancel()
		if err == nil {
			if id := findSteerUserMessageID(stored, reference); id != "" {
				return id
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-deadline.C:
			return ""
		case <-tick.C:
		}
	}
}

type openCodeSteerSessionMessage struct {
	Info struct {
		ID       string `json:"id"`
		Role     string `json:"role"`
		ParentID string `json:"parentID"`
		Finish   string `json:"finish"`
		Error    any    `json:"error"`
		Time     struct {
			Created float64 `json:"created"`
		} `json:"time"`
	} `json:"info"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

func (run *openCodeSteerRun) sessionMessages(ctx context.Context) ([]openCodeSteerSessionMessage, error) {
	var messages []openCodeSteerSessionMessage
	path := "/session/" + url.PathEscape(run.sessionID) + "/message"
	err := run.service().json(ctx, http.MethodGet, path, nil, &messages)
	return messages, err
}

func findSteerUserMessageID(messages []openCodeSteerSessionMessage, reference string) string {
	marker := "[Glowbom steer " + reference + "]"
	for _, message := range messages {
		if message.Info.Role != "user" {
			continue
		}
		for _, part := range message.Parts {
			if part.Type == "text" && strings.HasPrefix(part.Text, marker) {
				return message.Info.ID
			}
		}
	}
	return ""
}

type steerMessageOrder struct {
	created float64
	id      string
}

func (run *openCodeSteerRun) consumed(messages []openCodeSteerSessionMessage) (bool, error) {
	createdByUserID := make(map[string]steerMessageOrder)
	for _, message := range messages {
		if message.Info.Role == "user" {
			createdByUserID[message.Info.ID] = steerMessageOrder{created: message.Info.Time.Created, id: message.Info.ID}
		}
	}
	latestSteer := steerMessageOrder{}
	for _, delivery := range run.sent {
		if delivery.messageID == "" {
			delivery.messageID = findSteerUserMessageID(messages, delivery.reference)
		}
		order, found := createdByUserID[delivery.messageID]
		if !found {
			return false, fmt.Errorf("OpenCode did not store a steered chat message before the build ended")
		}
		delivery.persisted = true
		if order.created > latestSteer.created || (order.created == latestSteer.created && order.id > latestSteer.id) {
			latestSteer = order
		}
	}
	for _, message := range messages {
		info := message.Info
		parent, found := createdByUserID[info.ParentID]
		if info.Role == "assistant" && info.Finish != "" && info.Finish != "tool-calls" && info.Finish != "unknown" && info.Error == nil && found &&
			(parent.created > latestSteer.created || (parent.created == latestSteer.created && parent.id >= latestSteer.id)) {
			return true, nil
		}
	}
	return false, nil
}

func (run *openCodeSteerRun) sessionBusy(ctx context.Context) bool {
	var status map[string]struct {
		Type string `json:"type"`
	}
	if run.service().json(ctx, http.MethodGet, "/session/status", nil, &status) != nil {
		return false
	}
	return status[run.sessionID].Type == "busy" || status[run.sessionID].Type == "retry"
}

// At session.idle, a stored user message alone is insufficient. The runner
// may have taken its final history snapshot before an async steer arrived.
// A finished assistant turn parented by that steer or a later user message is
// evidence that the runner reread the steer. Otherwise we request one more
// turn in the same session before reporting the build complete.
func (run *openCodeSteerRun) continueAtIdle(ctx context.Context) (continueWork bool, resultErr error) {
	run.mu.Lock()
	defer func() {
		// Close the acceptance window in the same lock as the final history
		// check. A click must not slip in between idle and build post-pass.
		if !continueWork {
			run.accepting = false
		}
		run.mu.Unlock()
	}()
	if len(run.sent) == 0 {
		return false, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	messages, err := run.sessionMessages(checkCtx)
	if err != nil {
		return false, fmt.Errorf("Could not confirm whether the agent saw a steered message")
	}
	for _, delivery := range run.sent {
		if delivery.messageID != "" || findSteerUserMessageID(messages, delivery.reference) != "" {
			continue
		}
		// prompt_async may acknowledge its fork before it has saved the user
		// message. Give that fork a brief bounded chance to persist at idle.
		delivery.messageID = run.waitForMessage(checkCtx, delivery.reference, 2*time.Second)
		if delivery.messageID == "" {
			return false, fmt.Errorf("OpenCode did not store a steered chat message before the build ended")
		}
		messages, err = run.sessionMessages(checkCtx)
		if err != nil {
			return false, fmt.Errorf("Could not confirm whether the agent saw a steered message")
		}
	}
	if done, err := run.consumed(messages); done || err != nil {
		return false, err
	}
	for _, delivery := range run.sent {
		if delivery.nudged {
			// A late idle event from the first runner can arrive after the nudge
			// was stored but before its new runner starts. Briefly wait for that
			// runner or its completed assistant message.
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(150 * time.Millisecond)
			defer tick.Stop()
			for {
				if run.sessionBusy(checkCtx) {
					return true, nil
				}
				if current, readErr := run.sessionMessages(checkCtx); readErr == nil {
					if done, verifyErr := run.consumed(current); done || verifyErr != nil {
						return false, verifyErr
					}
				}
				select {
				case <-checkCtx.Done():
					return false, fmt.Errorf("Could not confirm a response to the steered message")
				case <-deadline.C:
					return false, fmt.Errorf("OpenCode stored the steered message but did not finish a response to it")
				case <-tick.C:
				}
			}
		}
	}
	nudgeReference := "nudge" + strings.ReplaceAll(randomUUIDString(), "-", "")
	payload := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "[Glowbom steer " + nudgeReference + "]\nBefore finishing this build, address the priority chat steer already in this session. Make any appropriate project changes, then briefly explain your decision. Do not treat the same saved chat entry as a second correction."}},
	}
	if run.providerID != "" && run.modelID != "" {
		payload["model"] = map[string]string{"providerID": run.providerID, "modelID": run.modelID}
	}
	response, err := run.service().request(checkCtx, http.MethodPost, "/session/"+url.PathEscape(run.sessionID)+"/prompt_async", payload)
	if err != nil {
		return false, fmt.Errorf("Could not resume the agent to address the steered message")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	run.nudgeID = run.waitForMessage(checkCtx, nudgeReference, 4*time.Second)
	if run.nudgeID == "" {
		return false, fmt.Errorf("OpenCode did not store the follow-up for the steered message")
	}
	for _, delivery := range run.sent {
		delivery.nudged = true
	}
	return true, nil
}

func steerPromptText(req openCodeSteerRequest, reference string) string {
	marker := "[Glowbom steer " + reference + "]\n"
	if req.Role == "assistant" {
		return marker + "Glowbom steer: The person selected this earlier assistant reply from the project chat as priority context for the current build. These are the assistant's earlier words, not a new user instruction. Use your judgment about what action, if any, they call for. This same reply is saved in .glowbom/chat.json; do not count it twice.\n\nEarlier assistant reply:\n" + req.Text
	}
	return marker + "Glowbom steer: The person selected this user message from the project chat as a priority correction for the current build. Use your judgment about how to apply it and mention your decision in a GLOWBOM_STATUS: line. This same message is saved in .glowbom/chat.json; do not count it twice.\n\nSelected user message:\n" + req.Text
}
