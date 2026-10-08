package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrototypeBookSourceDistinguishesRepeatedRequests(t *testing.T) {
	project, err := createSketchProject(t.TempDir(), "Repeated request")
	if err != nil {
		t.Fatal(err)
	}
	first, second := "", ""
	messages := []chatMessage{{Role: "user", Text: "Use a blue button"}}
	if err := saveChatPrototype(project, translationSource, "", messages, "test/model", nil, &first); err != nil {
		t.Fatal(err)
	}
	if result := readTestChatResult(t, project); result.BookSource != prototypeBookSource(first) {
		t.Fatal("first result lost its record", result.BookSource)
	}
	if err := saveChatPrototype(project, translationSource, translationSource, messages, "test/model", nil, &second); err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two submissions shared a record")
	}
	if result := readTestChatResult(t, project); result.BookSource != prototypeBookSource(second) {
		t.Fatal("same request and HTML selected an older record", result.BookSource)
	}
	requestPath := filepath.Join(second, "request.json")
	data, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	request["saved"] = false
	data, _ = json.Marshal(request)
	if err := os.WriteFile(requestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if result := readTestChatResult(t, project); result.BookSource != prototypeBookSource(first) {
		t.Fatal("unfinished version claimed a result", result.BookSource)
	}
	if err := os.WriteFile(filepath.Join(project, "prototype/index.html"), []byte("<!doctype html><html>Changed elsewhere</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := readTestChatResult(t, project); result.BookSource != "" {
		t.Fatal("unrecorded result was assigned a source", result.BookSource)
	}
}

func TestPrototypeBookSourceSkipsUnsafeAndCorruptRecords(t *testing.T) {
	project, record := chatImagesFixture(t, translationSource)
	resultPath := filepath.Join(record, "result.html")
	if err := os.Remove(resultPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.html")
	if err := os.WriteFile(outside, []byte(translationSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, resultPath); err != nil {
		t.Fatal(err)
	}
	if result := readTestChatResult(t, project); result.BookSource != "" {
		t.Fatal("symlinked result was used for provenance")
	}
	if err := os.Remove(resultPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, []byte(translationSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(record, "request.json"), []byte(`{"saved":true`), 0600); err != nil {
		t.Fatal(err)
	}
	if result := readTestChatResult(t, project); result.BookSource != "" {
		t.Fatal("corrupt record was used for provenance")
	}
}

func TestPrototypeStreamIncludesSavedBookSource(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{true: "invalid", false: "saved"}[invalid], func(t *testing.T) {
			isolateProjectIconCredentials(t)
			t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
			rememberCursorModels(nil)
			project := translationProject(t)
			mockProjectIconProvider(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/provider":
					return chatImageReply(`{"connected":["test"],"all":[{"id":"test","models":{"model":{"name":"Model"}}}]}`), nil
				case "/session":
					return chatImageReply(`{"id":"book-source","permission":[{"permission":"*","pattern":"*","action":"deny"}]}`), nil
				case "/experimental/tool/ids":
					return chatImageReply(`[]`), nil
				case "/event":
					return chatImageReply("data: {}\n\n"), nil
				case "/session/book-source/message":
					html := "<!doctype html><html><body>New result</body></html>"
					if invalid {
						html = "unfinished"
					}
					body, _ := json.Marshal(map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "text", "text": html}}})
					return chatImageReply(string(body)), nil
				default:
					return chatImageReply(`true`), nil
				}
			})
			service := &chatService{directory: "/isolated", serverURL: "http://fixture", client: http.DefaultClient, prepare: func() error { return nil }}
			body, _ := json.Marshal(chatRequest{ProjectPath: project, Mode: "prototype", Model: "test/model", Messages: []chatMessage{{Role: "user", Text: "Use a blue button"}}})
			response := httptest.NewRecorder()
			service.streamHandler(response, httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(string(body))))
			var source string
			for _, line := range strings.Split(response.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event struct {
					Done       bool   `json:"done"`
					Success    bool   `json:"success"`
					BookSource string `json:"bookSource"`
				}
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Done && event.Success {
					source = event.BookSource
				}
			}
			if invalid {
				if source != "" || strings.Contains(response.Body.String(), `"bookSource":`) {
					t.Fatal("failed generation emitted source identity")
				}
			} else if source == "" || source != readTestChatResult(t, project).BookSource || !strings.HasPrefix(source, ".glowbom/prototypes/version-") {
				t.Fatal("completion and persisted result disagree about their source", source, response.Body.String())
			}
		})
	}
}
