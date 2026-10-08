package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// The on-device model has a 4096-token window. OpenCode's agent prompt and
// tool list fill it on their own, so Chat talks to the bridge directly.
// Bytes overestimate tokens for English and roughly match them for CJK text.
const (
	appleIntelligenceInputBudgetBytes = 8400
	appleIntelligenceMaxOutputTokens  = 800
)

const appleIntelligenceSystemPrompt = "You are Glowbom, a helpful assistant for shaping software ideas. Answer briefly and directly. You cannot see project files, run commands, or change a project."

var appleIntelligenceBaseURL = "http://127.0.0.1:" + appleIntelligencePort

var (
	errAppleIntelligenceTooLong     = errors.New("apple intelligence input exceeds the context window")
	errAppleIntelligenceUnavailable = errors.New("apple intelligence bridge is not responding")
)

type appleIntelligenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func isAppleIntelligenceModel(id string) bool {
	return strings.HasPrefix(id, appleIntelligenceProvider+"/")
}

// appleIntelligenceConversation keeps the newest turns that fit the budget.
// The final user message is always included, and history starts with a user turn.
func appleIntelligenceConversation(messages []chatMessage, budget int, context ...string) ([]appleIntelligenceMessage, error) {
	return appleIntelligenceConversationWithSystem(messages, budget, appleIntelligenceSystemPrompt, context...)
}

func appleIntelligenceConversationWithSystem(messages []chatMessage, budget int, system string, context ...string) ([]appleIntelligenceMessage, error) {
	final := strings.TrimSpace(messages[len(messages)-1].Text)
	if final == "" {
		return nil, appleIntelligenceUserError("Type a message for Apple Intelligence.")
	}
	if len(context) > 0 && context[0] != "" {
		system += "\n" + context[0]
	}
	used := len(system) + len(final)
	if used > budget {
		return nil, appleIntelligenceUserError("This message is too long for Apple Intelligence. Shorten it or choose another model.")
	}
	history := []appleIntelligenceMessage{}
	for i := len(messages) - 2; i >= 0; i-- {
		text := strings.TrimSpace(messages[i].Text)
		if text == "" {
			continue
		}
		if used+len(text) > budget {
			break
		}
		used += len(text)
		history = append(history, appleIntelligenceMessage{Role: messages[i].Role, Content: text})
	}
	for len(history) > 0 && history[len(history)-1].Role != "user" {
		history = history[:len(history)-1]
	}
	conversation := []appleIntelligenceMessage{{Role: "system", Content: system}}
	for i := len(history) - 1; i >= 0; i-- {
		conversation = append(conversation, history[i])
	}
	return append(conversation, appleIntelligenceMessage{Role: "user", Content: final}), nil
}

func appleIntelligenceResponseError(status int, data []byte) error {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &envelope)
	message := strings.TrimSpace(envelope.Error.Message)
	lower := strings.ToLower(message)
	if envelope.Error.Code == "context_length_exceeded" || strings.Contains(lower, "context window") {
		return errAppleIntelligenceTooLong
	}
	log.Printf("[APPLE-INTELLIGENCE] bridge returned status=%d type=%q code=%q", status, envelope.Error.Type, envelope.Error.Code)
	if status == http.StatusServiceUnavailable || envelope.Error.Type == "server_error" {
		return appleIntelligenceUserError("Apple Intelligence is not available right now. Make sure it is turned on in System Settings and the model has finished downloading.")
	}
	if message == "" {
		return appleIntelligenceUserError("Apple Intelligence could not answer this message. Try rephrasing it or choose another model.")
	}
	return appleIntelligenceUserError("Apple Intelligence could not answer: " + sanitizeProviderError(errors.New(message)))
}

func streamAppleIntelligenceReply(ctx context.Context, messages []appleIntelligenceMessage, onText func(string)) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      appleIntelligenceModel,
		"stream":     true,
		"max_tokens": appleIntelligenceMaxOutputTokens,
		"messages":   messages,
	})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, appleIntelligenceBaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errAppleIntelligenceUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return "", appleIntelligenceResponseError(response.StatusCode, data)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var text strings.Builder
	finished := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			finished = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return text.String(), appleIntelligenceResponseError(0, []byte(payload))
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				onText(text.String())
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finished = true
			}
		}
	}
	if ctx.Err() != nil {
		return text.String(), ctx.Err()
	}
	if !finished {
		return text.String(), errAppleIntelligenceUnavailable
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", appleIntelligenceUserError("Apple Intelligence returned no text. Try rephrasing your message.")
	}
	return text.String(), nil
}

// appleIntelligenceReply restarts a crashed bridge and retries once before any
// text has streamed, so one failed request does not leave the model unusable.
func appleIntelligenceReply(ctx context.Context, messages []appleIntelligenceMessage, onStatus, onText func(string)) (string, error) {
	for attempt := 0; ; attempt++ {
		onStatus("Starting Apple Intelligence")
		if err := startAppleIntelligenceBridge(ctx); err != nil {
			return "", err
		}
		onStatus("Waiting for model")
		streamed := false
		text, err := streamAppleIntelligenceReply(ctx, messages, func(text string) {
			if !streamed {
				streamed = true
				onStatus("Writing response")
			}
			onText(text)
		})
		if err == nil || ctx.Err() != nil {
			return text, err
		}
		if !streamed && attempt == 0 {
			switch {
			case errors.Is(err, errAppleIntelligenceTooLong) && len(messages) > 2:
				messages = []appleIntelligenceMessage{messages[0], messages[len(messages)-1]}
				continue
			case errors.Is(err, errAppleIntelligenceUnavailable):
				log.Printf("[APPLE-INTELLIGENCE] bridge stopped during a request; restarting")
				stopAppleIntelligenceBridge()
				continue
			}
		}
		switch {
		case errors.Is(err, errAppleIntelligenceTooLong):
			return text, appleIntelligenceUserError("This message is too long for Apple Intelligence. Shorten it, start a new chat, or choose another model.")
		case errors.Is(err, errAppleIntelligenceUnavailable):
			return text, appleIntelligenceUserError("Apple Intelligence stopped responding. Send the message again, or turn Apple Intelligence off and on in Settings.")
		}
		return text, err
	}
}

func (s *chatService) appleIntelligenceChat(w http.ResponseWriter, r *http.Request, req chatRequest) {
	if req.Mode != "chat" {
		http.Error(w, "Apple Intelligence works in Chat only. Choose another model to draw a prototype or translate code.", http.StatusBadRequest)
		return
	}
	data := companionChatRequestData(r.Context())
	if len(req.AttachmentPaths) > 0 || len(data.images) > 0 {
		http.Error(w, "Apple Intelligence cannot read attachments. Remove them or choose a vision model.", http.StatusBadRequest)
		return
	}
	if !appleIntelligenceEnabled() {
		http.Error(w, "Turn on Apple Intelligence in Settings first.", http.StatusBadRequest)
		return
	}
	system := appleIntelligenceSystemPrompt
	guidance := chatAgentGuidance(req.AgentState, req.ProjectPath != "" || data.localProject != nil)
	if data.prototype {
		system, guidance = companionPrototypeSystem, ""
	}
	if reference := companionChatReference(data); reference != "" {
		guidance += "\n" + reference
	}
	messages, err := appleIntelligenceConversationWithSystem(req.Messages, appleIntelligenceInputBudgetBytes, system, guidance)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	emit := func(event map[string]any) {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	emit(map[string]any{"status": "Connecting", "model": req.Model})
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	text, err := appleIntelligenceReply(ctx, messages,
		func(status string) { emit(map[string]any{"status": status}) },
		func(text string) { emit(map[string]any{"text": text}) })
	if err != nil {
		message := chatFailureMessage(err, req.Model)
		var problem appleIntelligenceProblem
		if errors.As(err, &problem) {
			message = problem.message
		}
		emit(map[string]any{"done": true, "success": false, "error": message, "code": "apple_intelligence"})
		return
	}
	emit(map[string]any{"done": true, "success": true, "text": text, "projectPath": ""})
}
