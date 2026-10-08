package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppleIntelligenceConversationKeepsNewestTurnsThatFit(t *testing.T) {
	messages := []chatMessage{
		{Role: "user", Text: strings.Repeat("old ", 400)},
		{Role: "assistant", Text: "older answer"},
		{Role: "user", Text: "recent question"},
		{Role: "assistant", Text: "recent answer"},
		{Role: "user", Text: "hello"},
	}
	budget := len(appleIntelligenceSystemPrompt) + len("hello") + len("recent answer") + len("recent question") + len("older answer")
	conversation, err := appleIntelligenceConversation(messages, budget)
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, message := range conversation {
		roles = append(roles, message.Role+":"+message.Content)
	}
	got := strings.Join(roles, "|")
	want := "system:" + appleIntelligenceSystemPrompt + "|user:recent question|assistant:recent answer|user:hello"
	if got != want {
		t.Fatalf("conversation = %q, want %q", got, want)
	}
}

func TestAppleIntelligenceConversationRejectsOversizedMessage(t *testing.T) {
	_, err := appleIntelligenceConversation([]chatMessage{{Role: "user", Text: strings.Repeat("a", 9000)}}, appleIntelligenceInputBudgetBytes)
	var problem appleIntelligenceProblem
	if !errors.As(err, &problem) || !strings.Contains(problem.message, "too long") {
		t.Fatalf("oversized message was not explained: %v", err)
	}
}

func TestAppleIntelligenceResponseErrorDetectsContextOverflow(t *testing.T) {
	body := []byte(`{"error":{"message":"Input exceeds the model's context window. Shorten the conversation history.","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	if err := appleIntelligenceResponseError(http.StatusBadRequest, body); !errors.Is(err, errAppleIntelligenceTooLong) {
		t.Fatalf("overflow was not detected: %v", err)
	}
}

func withAppleIntelligenceServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previous := appleIntelligenceBaseURL
	appleIntelligenceBaseURL = server.URL
	t.Cleanup(func() { appleIntelligenceBaseURL = previous })
}

func TestStreamAppleIntelligenceReplyAccumulatesDeltas(t *testing.T) {
	var sent struct {
		Stream   bool                       `json:"stream"`
		Messages []appleIntelligenceMessage `json:"messages"`
	}
	withAppleIntelligenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range []string{"Hel", "lo"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", part)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	updates := []string{}
	text, err := streamAppleIntelligenceReply(context.Background(), []appleIntelligenceMessage{{Role: "user", Content: "hi"}}, func(text string) { updates = append(updates, text) })
	if err != nil || text != "Hello" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if strings.Join(updates, ",") != "Hel,Hello" {
		t.Fatalf("updates = %v", updates)
	}
	if !sent.Stream || len(sent.Messages) != 1 {
		t.Fatalf("unexpected request: %+v", sent)
	}
}

func TestStreamAppleIntelligenceReplyReportsDroppedConnection(t *testing.T) {
	withAppleIntelligenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n")
	})
	if _, err := streamAppleIntelligenceReply(context.Background(), []appleIntelligenceMessage{{Role: "user", Content: "hi"}}, func(string) {}); !errors.Is(err, errAppleIntelligenceUnavailable) {
		t.Fatalf("dropped stream was not reported: %v", err)
	}
}

func TestAppleIntelligenceChatRejectsBuildOnlyUses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := &chatService{}
	cases := []struct {
		name string
		body chatRequest
		want string
	}{
		{"prototype", chatRequest{Model: "apple-intelligence/apple-foundationmodel", Mode: "prototype", Messages: []chatMessage{{Role: "user", Text: "hi"}}}, "Chat only"},
		{"disabled", chatRequest{Model: "apple-intelligence/apple-foundationmodel", Mode: "chat", Messages: []chatMessage{{Role: "user", Text: "hi"}}}, "Turn on Apple Intelligence"},
	}
	for _, test := range cases {
		data, _ := json.Marshal(test.body)
		recorder := httptest.NewRecorder()
		service.streamHandler(recorder, httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(data)))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.want) {
			t.Fatalf("%s: %d %q", test.name, recorder.Code, recorder.Body.String())
		}
	}
}
