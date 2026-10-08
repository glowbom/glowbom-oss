package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCodexSummaryStreamReconcilesIndexedSections(t *testing.T) {
	stream := codexSummaryStream{}
	for _, test := range []struct {
		method, params, want string
	}{
		{"item/reasoning/summaryPartAdded", `{"itemId":"first","summaryIndex":1}`, ""},
		{"item/reasoning/summaryTextDelta", `{"itemId":"first","summaryIndex":1,"delta":"Second section"}`, "Second section"},
		{"item/reasoning/summaryTextDelta", `{"itemId":"first","summaryIndex":0,"delta":"First"}`, "First\n\nSecond section"},
		{"item/reasoning/summaryTextDelta", `{"itemId":"first","summaryIndex":0,"delta":" section"}`, "First section\n\nSecond section"},
		{"item/completed", `{"item":{"id":"first","type":"reasoning","summary":["Revised first section","Completed second section"]}}`, "Revised first section\n\nCompleted second section"},
		{"item/completed", `{"item":{"id":"first","type":"reasoning","summary":["Revised first section","Completed second section"]}}`, ""},
		{"item/reasoning/summaryTextDelta", `{"itemId":"first","summaryIndex":0,"delta":"late duplicate"}`, ""},
		{"item/completed", `{"item":{"id":"second","type":"reasoning","summary":["Completion without deltas"]}}`, "Revised first section\n\nCompleted second section\n\nCompletion without deltas"},
	} {
		got, err := stream.accept(codexTestMessage(test.method, test.params))
		if err != nil || got != test.want {
			t.Fatalf("%s: got %q, %v, want %q", test.method, got, err, test.want)
		}
	}
}

func TestCodexSummaryStreamOnlyDisplaysPublicSummaries(t *testing.T) {
	stream := codexSummaryStream{}
	for _, message := range []codexRPCMessage{
		codexTestMessage("item/reasoning/textDelta", `{"itemId":"raw","contentIndex":0,"delta":"raw reasoning"}`),
		codexTestMessage("item/completed", `{"item":{"id":"raw","type":"reasoning","content":["raw reasoning"],"encryptedContent":"opaque"}}`),
		codexTestMessage("item/completed", `{"item":{"id":"raw","type":"reasoning","summary":[],"content":["raw reasoning"]}}`),
		codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","text":"Answer","summary":["not a reasoning item"]}}`),
		codexTestMessage("item/reasoning/summaryTextDelta", `{"itemId":"invalid","summaryIndex":-1,"delta":"invalid index"}`),
		codexTestMessage("item/reasoning/summaryTextDelta", `{"itemId":"invalid","summaryIndex":1000000000,"delta":"invalid index"}`),
		codexTestMessage("item/reasoning/summaryTextDelta", `{"summaryIndex":0,"delta":"missing item"}`),
	} {
		if got, err := stream.accept(message); err != nil || got != "" {
			t.Fatalf("unrelated or unsupported content reached summaries: %q, %v", got, err)
		}
	}
	if stream.last != "" {
		t.Fatal("invented summary for model without summary output")
	}
}

func TestCodexSummaryStreamBoundsUTF8AndReleasesReplacedText(t *testing.T) {
	stream := codexSummaryStream{}
	long := strings.Repeat("☀", maxChatReasoningBytes)
	for _, payload := range []map[string]any{
		{"itemId": "first", "summaryIndex": 0, "delta": long},
		{"itemId": "first", "summaryIndex": 1, "delta": long},
		{"itemId": "second", "summaryIndex": 0, "delta": long},
	} {
		data, _ := json.Marshal(payload)
		got, err := stream.accept(codexTestMessage("item/reasoning/summaryTextDelta", string(data)))
		if err != nil || len(got) > maxChatReasoningBytes || !utf8.ValidString(got) || stream.bytes > maxChatReasoningBytes {
			t.Fatal("summary exceeded its UTF-8 or memory bound", err)
		}
	}
	got, err := stream.accept(codexTestMessage("item/completed", `{"item":{"id":"first","type":"reasoning","summary":["Short final summary"]}}`))
	if err != nil || got != "Short final summary" || stream.bytes != len(got) {
		t.Fatal("final summary did not replace bounded streaming text", err)
	}
	got, err = stream.accept(codexTestMessage("item/reasoning/summaryTextDelta", `{"itemId":"second","summaryIndex":0,"delta":"Another section"}`))
	if err != nil || got != "Short final summary\n\nAnother section" {
		t.Fatal("replaced summary did not release its text budget", err)
	}
}

func TestCodexSummaryStreamBoundsItemAndSectionMetadata(t *testing.T) {
	stream := codexSummaryStream{}
	for index := 0; index < 300; index++ {
		_, err := stream.accept(codexTestMessage("item/reasoning/summaryPartAdded", fmt.Sprintf(`{"itemId":"item-%d","summaryIndex":127}`, index)))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(stream.items) != 128 || len(stream.order) != 128 || len(stream.items["item-0"].parts) != 128 || stream.bytes != 0 {
		t.Fatal("summary metadata is not bounded")
	}
}

func TestCodexChatEmitsSummaryProgressSeparatelyFromAnswer(t *testing.T) {
	withCodexModels(t, []chatModel{{ID: "codex/test", Name: "Test", Provider: "Codex", Build: true}})
	withCodexTurn(t, func(_ context.Context, options codexRunOptions, emit func(codexRPCMessage) error, _ func(codexRPCMessage) (any, error)) (string, error) {
		if !options.ChatOnly {
			t.Fatal("chat entered a build session")
		}
		for _, message := range []codexRPCMessage{
			codexTestMessage("item/reasoning/summaryTextDelta", `{"itemId":"thought","summaryIndex":0,"delta":"Readable streamed summary"}`),
			codexTestMessage("item/reasoning/textDelta", `{"itemId":"thought","contentIndex":0,"delta":"raw-private-marker"}`),
			codexTestMessage("item/completed", `{"item":{"id":"thought","type":"reasoning","summary":["Readable final summary"],"content":["raw-private-marker"],"encryptedContent":"encrypted-private-marker"}}`),
			codexTestMessage("item/agentMessage/delta", `{"itemId":"answer","delta":"Hello"}`),
			codexTestMessage("item/completed", `{"item":{"id":"answer","type":"agentMessage","phase":"final_answer","text":"Hello"}}`),
		} {
			if err := emit(message); err != nil {
				return "", err
			}
		}
		return "thread-test", nil
	})
	service := &chatService{directory: t.TempDir()}
	response := httptest.NewRecorder()
	service.streamHandler(response, httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(`{"mode":"chat","model":"codex/test","messages":[{"role":"user","text":"Hello"}]}`)))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":true`) {
		t.Fatal(response.Code, response.Body.String())
	}
	var summaries []string
	for _, line := range strings.Split(response.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if summary, ok := event["reasoning"].(string); ok {
			summaries = append(summaries, summary)
		}
		if text, ok := event["text"].(string); ok && text != "Hello" {
			t.Fatalf("summary contaminated the answer: %q", text)
		}
	}
	if strings.Join(summaries, "|") != "Readable streamed summary|Readable final summary" || strings.Contains(response.Body.String(), "private-marker") {
		t.Fatal("readable summaries were lost or nonpublic content was included")
	}
}
