package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestChatReasoningStreamIsSeparateAndIsolated(t *testing.T) {
	longThought := strings.Repeat("☀", maxChatReasoningBytes)
	longPart, _ := json.Marshal(map[string]any{"type": "message.part.updated", "properties": map[string]any{"part": map[string]string{"id": "long_thought", "type": "reasoning", "text": longThought, "messageID": "assistant", "sessionID": "ses_progress"}}})
	events := make(chan string, 30)
	received := make(chan struct{})
	var receivedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			writeJSON(w, map[string]any{"id": "ses_progress", "permission": []map[string]string{{"permission": "*", "pattern": "*", "action": "deny"}}})
		case "/experimental/tool/ids":
			writeJSON(w, []string{"bash"})
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {}\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case event := <-events:
					fmt.Fprintf(w, "data: %s\n\n", event)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case "/session/ses_progress/message":
			for _, event := range []string{
				`{"type":"message.updated","properties":{"info":{"id":"user","role":"user","sessionID":"ses_progress"}}}`,
				`{"type":"message.updated","properties":{"info":{"id":"foreign","role":"assistant","sessionID":"another_session"}}}`,
				`{"type":"message.updated","properties":{"info":{"id":"assistant","role":"assistant","sessionID":"ses_progress"}}}`,
				`{"type":"message.updated","properties":{"info":{"id":"other_assistant","role":"assistant","sessionID":"ses_progress"}}}`,
				`{"type":"message.part.updated","properties":{"part":{"id":"user_thought","type":"reasoning","text":"private user","messageID":"user","sessionID":"ses_progress"}}}`,
				`{"type":"message.part.updated","properties":{"part":{"id":"foreign_thought","type":"reasoning","text":"private session","messageID":"foreign","sessionID":"another_session"}}}`,
				`{"type":"message.part.updated","properties":{"part":{"id":"unknown_thought","type":"reasoning","text":"unknown role","messageID":"unknown","sessionID":"ses_progress"}}}`,
				`{"type":"message.part.updated","properties":{"part":{"id":"thought","type":"reasoning","text":"Planning","messageID":"assistant","sessionID":"ses_progress"}}}`,
				`{"type":"message.part.delta","properties":{"partID":"thought","field":"text","delta":" private delta","messageID":"assistant","sessionID":"another_session"}}`,
				`{"type":"message.part.delta","properties":{"partID":"thought","field":"text","delta":" wrong owner","messageID":"other_assistant","sessionID":"ses_progress"}}`,
				`{"type":"message.part.delta","properties":{"partID":"thought","field":"text","delta":" the layout","messageID":"assistant","sessionID":"ses_progress"}}`,
				string(longPart),
				`{"type":"message.part.delta","properties":{"partID":"long_thought","field":"text","delta":"extra","messageID":"assistant","sessionID":"ses_progress"}}`,
				`{"type":"session.status","properties":{"sessionID":"ses_progress","status":{"type":"retry","attempt":1,"message":"Temporary error"}}}`,
				`{"type":"message.part.updated","properties":{"part":{"id":"answer","type":"text","text":"Code","messageID":"assistant","sessionID":"ses_progress"}}}`,
				`{"type":"message.part.delta","properties":{"partID":"answer","field":"text","delta":" ready","messageID":"assistant","sessionID":"ses_progress"}}`,
			} {
				events <- event
			}
			select {
			case <-received:
			case <-r.Context().Done():
				return
			}
			writeJSON(w, map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "reasoning", "text": "Planning the layout"}, {"type": "reasoning", "text": longThought}, {"type": "text", "text": "Code ready"}}})
		default:
			writeJSON(w, true)
		}
	}))
	defer server.Close()
	service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var reasoning, texts []string
	statuses := map[string]bool{}
	result, err := service.complete(ctx, "test/model", "Chat", nil, func(text string) {
		texts = append(texts, text)
		if text == "Code ready" {
			receivedOnce.Do(func() { close(received) })
		}
	}, chatCompletionOptions{OnProgress: func(p chatProgress) {
		statuses[p.Status] = true
		if p.Reasoning != "" {
			reasoning = append(reasoning, p.Reasoning)
		}
	}})
	if err != nil || result != "Code ready" {
		t.Fatal(result, err)
	}
	if len(reasoning) < 4 || !strings.HasPrefix(reasoning[len(reasoning)-1], "Planning the layout☀") {
		t.Fatal("missing cumulative reasoning")
	}
	for _, frame := range reasoning {
		if len(frame) > maxChatReasoningBytes || !utf8.ValidString(frame) {
			t.Fatal("reasoning stream exceeded its bound or split a character")
		}
		if strings.Contains(frame, "private") || strings.Contains(frame, "owner") || strings.Contains(frame, "unknown") || strings.Contains(frame, "Code") {
			t.Fatal("unrelated content reached reasoning", frame)
		}
	}
	for _, frame := range texts {
		if strings.Contains(frame, "Planning") {
			t.Fatal("reasoning reached output", frame)
		}
	}
	for _, status := range []string{"Waiting for model", "Thinking", "Writing response", "Retrying connection"} {
		if !statuses[status] {
			t.Fatal("missing status", status)
		}
	}
}

func TestChatGenerationSelectsOnlyAdvertisedLowVariant(t *testing.T) {
	t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
	rememberCursorModels(nil)
	for _, tc := range []struct {
		mode, variants, want string
		lowEffort            bool
	}{
		{"prototype", `{"low":{"reasoningEffort":"low"}}`, "low", false},
		{"translation", `{"low":{"thinkingConfig":{"thinkingLevel":"low"}}}`, "low", false},
		{"chat", `{"low":{"reasoningEffort":"low"}}`, "", false},
		{"prototype", `{"high":{"reasoningEffort":"high"}}`, "", false},
		{"translation", `{"low":{"disabled":true}}`, "", false},
		{"translation", `{"low":null}`, "", false},
		{"chat", `{"low":{"reasoningEffort":"low"}}`, "low", true},
		{"chat", `{}`, "", true},
		{"chat", `{"low":{"disabled":true}}`, "", true},
	} {
		t.Run(fmt.Sprintf("%s/%t/%s", tc.mode, tc.lowEffort, tc.variants), func(t *testing.T) {
			project := translationProject(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/provider":
					fmt.Fprintf(w, `{"connected":["test"],"all":[{"id":"test","models":{"model":{"name":"Model","variants":%s}}}]}`, tc.variants)
				case "/session":
					writeJSON(w, map[string]any{"id": "ses_variant", "permission": []map[string]string{{"permission": "*", "pattern": "*", "action": "deny"}}})
				case "/experimental/tool/ids":
					writeJSON(w, []string{"bash"})
				case "/event":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {}\n\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				case "/session/ses_variant/message":
					var body struct {
						Variant string `json:"variant"`
						Parts   []struct {
							Text string `json:"text"`
						} `json:"parts"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.Variant != tc.want {
						t.Errorf("variant = %q, want %q", body.Variant, tc.want)
					}
					if len(body.Parts) != 1 || strings.Contains(body.Parts[0].Text, "saved thinking") {
						t.Error("thinking added to model context")
					}
					output := "Code ready"
					if tc.mode == "prototype" {
						output = "<!doctype html><html><body>Updated</body></html>"
					}
					writeJSON(w, map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "reasoning", "text": "provider thoughts"}, {"type": "text", "text": output}}})
				default:
					writeJSON(w, true)
				}
			}))
			defer server.Close()
			service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
			body, _ := json.Marshal(chatRequest{ProjectPath: project, Model: "test/model", Mode: tc.mode, LowEffort: tc.lowEffort, Stack: &chatStack{ID: "php", Name: "PHP", Description: "PHP 8"}, Messages: []chatMessage{{Role: "assistant", Text: "Ready", Reasoning: "saved thinking"}, {Role: "user", Text: "Continue"}}})
			response := httptest.NewRecorder()
			service.streamHandler(response, httptest.NewRequest("POST", "/chat/stream", strings.NewReader(string(body))))
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"success":true`) || !strings.Contains(response.Body.String(), `"reasoning":"provider thoughts"`) {
				t.Fatal(response.Code, response.Body.String())
			}
			result := readTestChatResult(t, project)
			if strings.Contains(result.HTML, "provider thoughts") {
				t.Fatal("reasoning saved to HTML")
			}
			for _, translation := range result.Translations {
				if strings.Contains(translation.Code, "provider thoughts") {
					t.Fatal("reasoning saved to translation")
				}
			}
		})
	}
}

func TestChatThinkingHistoryAndBounds(t *testing.T) {
	project := translationProject(t)
	service := &chatService{}
	messages := []chatMessage{{Role: "user", Text: "Hello", Reasoning: "should be removed", WorkedSeconds: 100, BuildSeconds: 100}, {Role: "assistant", Text: "Hi", Reasoning: "provider thinking", WorkedSeconds: 62, BuildSeconds: 980}}
	post := func(messages []chatMessage) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"projectPath": project, "messages": messages})
		response := httptest.NewRecorder()
		service.historyHandler(response, httptest.NewRequest("POST", "/chat/history", strings.NewReader(string(body))))
		return response
	}
	if response := post(messages); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	get := httptest.NewRecorder()
	service.historyHandler(get, httptest.NewRequest("GET", "/chat/history?path="+url.QueryEscape(project), nil))
	if get.Code != 200 || !strings.Contains(get.Body.String(), "provider thinking") || !strings.Contains(get.Body.String(), `"workedSeconds":62`) || !strings.Contains(get.Body.String(), `"buildSeconds":980`) || strings.Contains(get.Body.String(), "should be removed") || strings.Contains(get.Body.String(), `"workedSeconds":100`) || strings.Contains(get.Body.String(), `"buildSeconds":100`) {
		t.Fatal(get.Body.String())
	}
	messages[1].WorkedSeconds = -1
	if post(messages).Code != 400 {
		t.Fatal("accepted negative work duration")
	}
	messages[1].WorkedSeconds = 62
	messages[1].BuildSeconds = -1
	if post(messages).Code != 400 {
		t.Fatal("accepted negative build duration")
	}
	messages[1].BuildSeconds = 980
	messages[1].Reasoning = strings.Repeat("x", maxChatReasoningBytes+1)
	if post(messages).Code != 400 {
		t.Fatal("accepted oversized history thinking")
	}
	messages = append(messages, chatMessage{Role: "user", Text: "Continue"})
	if validateChatRequest(chatRequest{Mode: "chat", Model: "test/model", Messages: messages}) == nil {
		t.Fatal("accepted oversized request thinking")
	}
	for _, text := range []string{strings.Repeat("x", maxChatReasoningBytes+20), strings.Repeat("☀", maxChatReasoningBytes)} {
		bounded := boundedChatReasoning(text, maxChatReasoningBytes)
		if len(bounded) > maxChatReasoningBytes || !utf8.ValidString(bounded) {
			t.Fatal("invalid reasoning limit")
		}
	}
}

func TestChatTimeoutIsExplainedWithoutConfusingContextLength(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, fmt.Errorf("request: %w", context.DeadlineExceeded), decodeChatProviderError([]byte(`{"message":"context deadline exceeded"}`), 0)} {
		if chatFailureCode(err) != "chat_timeout" || !strings.Contains(chatFailureMessage(err, "test/model"), "took too long") {
			t.Fatal(err)
		}
	}
	for _, err := range []error{context.Canceled, errors.New("context length exceeded"), decodeChatProviderError([]byte(`{"message":"context window exceeded"}`), 400)} {
		if chatFailureCode(err) == "chat_timeout" {
			t.Fatal("misclassified error", err)
		}
	}
}
