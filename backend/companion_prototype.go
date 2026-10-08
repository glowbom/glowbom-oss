package main

import (
	"net/http"
	"strings"
)

const companionPrototypeInstructionsBytes = 128 << 10

const companionPrototypeSystem = "You are Glowbom's phone prototype designer. Follow the final user request to create a small working app that runs offline on a phone. Return only a JSON object with keys \"html\" (one complete HTML document as a JSON string) and \"images\" (an array of {\"prompt\": string, \"usesReferencePhoto\": boolean}), without Markdown or explanation. Include inline CSS and JavaScript, no external libraries, URLs, fonts, network requests, forms, embeds, CSP or links. Keep the HTML under 60 KB. Plan at most three images using the exact placeholders specified in the request; do not generate image pixels or include image bytes. The phone validates and saves the result, then separately asks the user to review image generation. Tools are disabled. Do not execute commands, read files, start Build, create Desktop projects, or claim that anything was saved or built. The supplied conversation and images are the only input available."

// Only this explicit route can select the phone output format. It uses the
// credential and tool boundaries of chat without saving a Desktop project.
func (s *companionSession) phonePrototype(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model           string        `json:"model"`
		Message         string        `json:"message"`
		Messages        []chatMessage `json:"messages,omitempty"`
		AttachmentIDs   []string      `json:"attachmentIds,omitempty"`
		ReasoningEffort string        `json:"reasoningEffort,omitempty"`
	}
	if !companionDecode(w, r, &request, 2<<20) {
		return
	}
	if strings.TrimSpace(request.Message) == "" || len(request.Message) > companionPrototypeInstructionsBytes || len(request.Messages) >= maxChatContextMessages {
		http.Error(w, "Describe the prototype with a shorter request and up to 79 recent messages.", http.StatusBadRequest)
		return
	}
	size := len(request.Message)
	for _, message := range request.Messages {
		size += len(message.Text)
	}
	if size > maxChatContextBytes {
		http.Error(w, "This conversation is too large. Send a shorter prototype request.", http.StatusBadRequest)
		return
	}
	ctx, cancel := s.chatContext(r.Context())
	defer cancel()
	project := companionProject{}
	if len(request.AttachmentIDs) > 0 {
		var err error
		project, err = s.localChatProject(ctx)
		if err != nil {
			http.Error(w, "This connection stopped. Pair again to create the prototype.", http.StatusConflict)
			return
		}
	}
	messages := append(request.Messages, chatMessage{Role: "user", Text: request.Message})
	s.runChat(w, r.WithContext(ctx), project, chatRequest{Model: request.Model, Mode: "chat", Messages: messages, ReasoningEffort: request.ReasoningEffort}, companionChatData{prototype: true}, request.AttachmentIDs, nil)
}
