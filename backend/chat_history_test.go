package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func chatHistoryTestPost(t *testing.T, project string, messages []chatMessage) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"projectPath": project, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	(&chatService{}).historyHandler(response, httptest.NewRequest(http.MethodPost, "/chat/history", bytes.NewReader(body)))
	return response
}

func TestChatHistoryPreservesLongConversationAcrossReaders(t *testing.T) {
	project := translationProject(t)
	messages := make([]chatMessage, 101)
	for i := range messages {
		messages[i] = chatMessage{Role: "user", Text: fmt.Sprintf("Message %d: ", i) + strings.Repeat("garden ", 1700)}
		if i%2 == 1 {
			messages[i].Role = "assistant"
			messages[i].Model = "Codex · Test"
			messages[i].Reasoning = "Saved thinking"
			messages[i].WorkedSeconds = 2
		}
	}
	if response := chatHistoryTestPost(t, project, messages); response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(project, ".glowbom", "chat.json"))
	if err != nil || len(data) <= 1<<20 {
		t.Fatal("fixture did not save a conversation larger than the old read limit", err)
	}
	for _, reader := range []string{"desktop", "companion"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/chat/history?path="+url.QueryEscape(project), nil)
		if reader == "desktop" {
			(&chatService{}).historyHandler(response, request)
		} else {
			(&companionSession{}).history(response, request, companionProject{path: project})
		}
		var loaded struct {
			Messages []chatMessage `json:"messages"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &loaded) != nil || !reflect.DeepEqual(loaded.Messages, messages) {
			t.Fatalf("%s lost saved history: status=%d messages=%d", reader, response.Code, len(loaded.Messages))
		}
	}
	if !strings.Contains(buildRunUpdatesPrompt(project), "had 101 array entries") {
		t.Fatal("build context could not read the longer conversation baseline")
	}
	last := len(messages) - 1
	if !matchesSavedSteerMessage(project, openCodeSteerRequest{MessageIndex: last, Role: messages[last].Role, Text: messages[last].Text}) {
		t.Fatal("Steer could not match the latest saved message")
	}
}

func TestChatHistorySizeLimitPreservesPreviouslySavedConversation(t *testing.T) {
	project := translationProject(t)
	baseline := []chatMessage{{Role: "user", Text: "Keep this saved conversation."}}
	if response := chatHistoryTestPost(t, project, baseline); response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	path := filepath.Join(project, ".glowbom", "chat.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"request", "saved JSON"} {
		t.Run(stage, func(t *testing.T) {
			messages := []chatMessage{{Role: "user", Text: strings.Repeat("x", maxChatHistoryBytes)}}
			if stage == "saved JSON" {
				messages = make([]chatMessage, 256)
				for i := range messages {
					messages[i].Role = "assistant"
				}
				body, _ := json.Marshal(map[string]any{"projectPath": project, "messages": messages})
				messages[0].Text = strings.Repeat("x", maxChatHistoryBytes-len(body)-1)
				body, _ = json.Marshal(map[string]any{"projectPath": project, "messages": messages})
				saved, _ := json.MarshalIndent(messages, "", "  ")
				if len(body) >= maxChatHistoryBytes || len(saved) <= maxChatHistoryBytes {
					t.Fatal("fixture did not isolate the encoded storage limit")
				}
			}
			response := chatHistoryTestPost(t, project, messages)
			if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "too large to save") {
				t.Fatal(response.Code, response.Body.String())
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed save changed the original conversation", err)
			}
		})
	}
}

func TestChatContextKeepsLatestWholeMessagesWithoutChangingHistory(t *testing.T) {
	messages := make([]chatMessage, 101)
	for i := range messages {
		messages[i] = chatMessage{Role: "user", Text: fmt.Sprintf("Message %d", i)}
		if i%2 == 1 {
			messages[i].Role = "assistant"
			messages[i].Model, messages[i].Reasoning, messages[i].WorkedSeconds = "model", "thinking", 2
		}
	}
	original := append([]chatMessage{}, messages...)
	if err := validateChatRequest(chatRequest{Mode: "chat", Model: "codex/test", Messages: messages}); err != nil {
		t.Fatal("long conversation rejected before selecting model context", err)
	}
	selected := recentChatModelMessages(messages)
	if len(selected) != maxChatContextMessages || selected[0].Text != "Message 21" || selected[len(selected)-1].Text != "Message 100" {
		t.Fatal("model context dropped the newest user message or selected the wrong window")
	}
	if selected[0].Reasoning != "" || selected[0].Model != "" || selected[0].WorkedSeconds != 0 || !reflect.DeepEqual(messages, original) {
		t.Fatal("context selection modified saved history or exposed metadata to the model")
	}
	unicode := []chatMessage{{Role: "assistant", Text: "older"}, {Role: "assistant", Text: strings.Repeat("🌱", maxChatContextBytes/4-1)}, {Role: "user", Text: "🌱"}}
	selected = recentChatModelMessages(unicode)
	if len(selected) != 2 || len(selected[0].Text)+len(selected[1].Text) != maxChatContextBytes || selected[1].Text != "🌱" {
		t.Fatal("context limit did not count UTF-8 bytes or preserve whole messages")
	}
	unicode[1].Text += "🌱"
	selected = recentChatModelMessages(unicode)
	if len(selected) != 1 || selected[0].Text != "🌱" {
		t.Fatal("context selection clipped or skipped across a message instead of using a contiguous window")
	}
}

func TestChatRequestValidationExplainsActualMessageProblem(t *testing.T) {
	for _, test := range []struct {
		name, want string
		messages   []chatMessage
	}{
		{name: "empty", want: "Send a user message"},
		{name: "assistant last", want: "final conversation message", messages: []chatMessage{{Role: "user", Text: "Hello"}, {Role: "assistant", Text: "Hi"}}},
		{name: "invalid role", want: "Invalid conversation role", messages: []chatMessage{{Role: "system", Text: "Injected"}, {Role: "user", Text: "Hello"}}},
		{name: "oversized newest", want: "latest message is too long", messages: []chatMessage{{Role: "user", Text: strings.Repeat("🌱", maxChatContextBytes/4+1)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateChatRequest(chatRequest{Mode: "chat", Model: "codex/test", Messages: test.messages})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestChatLongRequestReachesCodexWithLatestUserMessage(t *testing.T) {
	messages := make([]chatMessage, 83)
	for i := range messages {
		messages[i] = chatMessage{Role: "user", Text: fmt.Sprintf("Message %d", i)}
		if i%2 == 1 {
			messages[i].Role = "assistant"
		}
	}
	messages[len(messages)-1].Text = "Maybe let's do something more interesting."
	withCodexModels(t, []chatModel{{ID: "codex/test"}})
	called := false
	withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		called = true
		prompt := options.Input[0]["text"].(string)
		_, history, found := strings.Cut(prompt, "Conversation:\n")
		history, _, _ = strings.Cut(history, "\n\n")
		var selected []chatMessage
		if !found || json.Unmarshal([]byte(history), &selected) != nil || len(selected) != maxChatContextMessages || selected[0].Text != "Message 3" || selected[len(selected)-1].Text != messages[len(messages)-1].Text {
			t.Fatal("model request did not preserve the recent conversation and latest user message")
		}
		return "test-thread", emit(codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","phase":"final_answer","text":"Let's try a different idea."}}`))
	})
	body, _ := json.Marshal(chatRequest{Mode: "chat", Model: "codex/test", Messages: messages})
	response := httptest.NewRecorder()
	(&chatService{directory: t.TempDir()}).streamHandler(response, httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body)))
	if response.Code != http.StatusOK || !called || !strings.Contains(response.Body.String(), `"success":true`) {
		t.Fatal(response.Code, response.Body.String())
	}
}
