package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const translationSource = "<!doctype html><html><body>My portrait app</body></html>"

func translationProject(t *testing.T) string {
	t.Helper()
	project, err := createSketchProject(t.TempDir(), "Translate")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveChatPrototype(project, translationSource, "", nil, "test/model", nil); err != nil {
		t.Fatal(err)
	}
	return project
}

func readTestChatResult(t *testing.T, project string) chatResult {
	t.Helper()
	w := httptest.NewRecorder()
	(&chatService{}).resultHandler(w, httptest.NewRequest("GET", "/chat/result?path="+url.QueryEscape(project), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("result status %d: %s", w.Code, w.Body.String())
	}
	var result chatResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestChatTranslationRoundTripAndOutdatedSource(t *testing.T) {
	project := translationProject(t)
	stack := chatStack{ID: "php", Name: "PHP", Description: "PHP 8 with responsive HTML and CSS."}
	record, err := saveChatTranslation(project, stack, "```php\n<?php echo 'Hello';\n```", translationSource, "", "test/model")
	if err != nil {
		t.Fatal(err)
	}
	result := readTestChatResult(t, project)
	if len(result.Translations) != 1 || result.HTML != translationSource || result.SourceHash != record.SourceHash {
		t.Fatalf("result lost source or translation: %+v", result)
	}
	if got := result.Translations[0]; got.ID != "php" || got.Description != stack.Description || got.Code != "<?php echo 'Hello';" || got.Outdated {
		t.Fatalf("translation lost saved definition: %+v", got)
	}
	newHTML := "<!doctype html><html><body>Updated portrait app</body></html>"
	if err := saveChatPrototype(project, newHTML, translationSource, nil, "test/model", nil); err != nil {
		t.Fatal(err)
	}
	result = readTestChatResult(t, project)
	if !result.Translations[0].Outdated || result.Translations[0].Code != record.Code {
		t.Fatal("old translation was lost or not marked outdated")
	}
}

func TestChatTranslationFailurePreservesPreviousResult(t *testing.T) {
	project := translationProject(t)
	stack := chatStack{ID: "custom-portrait", Name: "Portrait", Description: "An Elm app with inline styles."}
	if _, err := saveChatTranslation(project, stack, "old code", translationSource, "", "test/model"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := chatTranslationSnapshot(project, stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, code, source, expected string }{
		{"empty", "   ", translationSource, snapshot},
		{"size", strings.Repeat("x", maxChatResultBytes+1), translationSource, snapshot},
		{"source changed", "new code", "stale source", snapshot},
		{"concurrent translation", "new code", translationSource, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := saveChatTranslation(project, stack, test.code, test.source, test.expected, "test/model"); err == nil {
				t.Fatal("accepted invalid or conflicting save")
			}
			result := readTestChatResult(t, project)
			if result.HTML != translationSource || len(result.Translations) != 1 || result.Translations[0].Code != "old code" {
				t.Fatal("failed save changed the previous result")
			}
		})
	}
	if _, err := saveChatTranslation(project, stack, "updated code", translationSource, snapshot, "test/model"); err != nil {
		t.Fatal(err)
	}
	if result := readTestChatResult(t, project); len(result.Translations) != 1 || result.Translations[0].Code != "updated code" {
		t.Fatal("successful regeneration did not replace the saved code")
	}
	files, _ := os.ReadDir(filepath.Join(project, ".glowbom/translations"))
	if len(files) != 2 {
		t.Fatal("did not clean up the previous completed version")
	}
}

func TestChatTranslationIgnoresInterruptedSave(t *testing.T) {
	project := translationProject(t)
	stack := chatStack{ID: "php", Name: "PHP", Description: "PHP 8"}
	if _, err := saveChatTranslation(project, stack, "old code", translationSource, "", "test/model"); err != nil {
		t.Fatal(err)
	}
	filename := "php.0123456789abcdef0123456789abcdef.json"
	if err := os.WriteFile(filepath.Join(project, ".glowbom/translations", filename), []byte(`{"code":"interrupted`), 0600); err != nil {
		t.Fatal(err)
	}
	if result := readTestChatResult(t, project); len(result.Translations) != 1 || result.Translations[0].Code != "old code" {
		t.Fatal("interrupted file hid the previous complete result")
	}
}

func TestChatResultBoundedAndContained(t *testing.T) {
	for _, location := range []string{"prototype/index.html", ".glowbom/translations"} {
		t.Run(location, func(t *testing.T) {
			project := translationProject(t)
			outside := t.TempDir()
			target := filepath.Join(outside, "secret")
			if location == ".glowbom/translations" {
				target = outside
			} else if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			os.Remove(filepath.Join(project, location))
			if err := os.Symlink(target, filepath.Join(project, location)); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			(&chatService{}).resultHandler(w, httptest.NewRequest("GET", "/chat/result?path="+url.QueryEscape(project), nil))
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("read outside project: %d %s", w.Code, w.Body.String())
			}
			stack := chatStack{ID: "php", Name: "PHP", Description: "PHP 8"}
			if _, err := saveChatTranslation(project, stack, "new code", translationSource, "", "test/model"); err == nil {
				t.Fatal("followed translation output symlink")
			}
			files, _ := os.ReadDir(outside)
			if location == ".glowbom/translations" && len(files) != 0 {
				t.Fatal("wrote outside project")
			}
		})
	}
	project := translationProject(t)
	if err := os.WriteFile(filepath.Join(project, "prototype/index.html"), []byte(strings.Repeat("x", maxChatResultBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	(&chatService{}).resultHandler(w, httptest.NewRequest("GET", "/chat/result?path="+url.QueryEscape(project), nil))
	if w.Code != http.StatusBadRequest {
		t.Fatal("accepted oversized HTML")
	}
}

func TestChatTranslationValidatesStack(t *testing.T) {
	for _, stack := range []chatStack{
		{ID: "../escape", Name: "PHP", Description: "PHP 8"},
		{ID: "php", Name: "\n", Description: "PHP 8"},
		{ID: "php", Name: "PHP", Description: ""},
		{ID: "php", Name: "PHP", Description: strings.Repeat("x", 8001)},
	} {
		if err := validateChatRequest(chatRequest{Mode: "translation", Model: "test/model", Stack: &stack, Messages: []chatMessage{{Role: "user", Text: "Translate"}}}); err == nil {
			t.Fatalf("accepted invalid stack: %+v", stack)
		}
	}
}

func TestChatTranslationStreamsAndSavesOnlySuccessfulOutput(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			project := translationProject(t)
			events := make(chan string, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/provider":
					fmt.Fprint(w, `{"connected":["test"],"all":[{"id":"test","name":"Test","models":{"model":{"name":"Model"}}}]}`)
				case "/session":
					writeJSON(w, map[string]any{"id": "ses_translation", "permission": []map[string]string{{"permission": "*", "pattern": "*", "action": "deny"}}})
				case "/experimental/tool/ids":
					writeJSON(w, []string{"read", "edit", "bash"})
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
				case "/session/ses_translation/message":
					var body struct {
						System string `json:"system"`
						Parts  []struct {
							Text string `json:"text"`
						} `json:"parts"`
						Tools map[string]bool `json:"tools"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if !strings.Contains(body.System, "code translator") || len(body.Parts) != 1 || !strings.Contains(body.Parts[0].Text, translationSource) || !strings.Contains(body.Parts[0].Text, "PHP 8") {
						t.Error("translation prompt lost stack or saved source")
					}
					if disabled, ok := body.Tools["*"]; !ok || disabled {
						t.Error("translation enabled tools")
					}
					events <- `{"type":"message.updated","properties":{"info":{"id":"msg_code","role":"assistant","sessionID":"ses_translation"}}}`
					events <- `{"type":"message.part.updated","properties":{"part":{"id":"prt_code","type":"text","text":"<?php","messageID":"msg_code","sessionID":"ses_translation"}}}`
					time.Sleep(30 * time.Millisecond)
					if failure {
						fmt.Fprint(w, `{"info":{"error":{"message":"The usage limit has been reached"}},"parts":[]}`)
					} else {
						writeJSON(w, map[string]any{"info": map[string]any{}, "parts": []map[string]string{{"type": "text", "text": "<?php echo 'Portrait';"}}})
					}
				default:
					writeJSON(w, true)
				}
			}))
			defer server.Close()
			service := &chatService{directory: "/isolated", serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
			body, _ := json.Marshal(chatRequest{ProjectPath: project, Model: "test/model", Mode: "translation", Stack: &chatStack{ID: "php", Name: "PHP", Description: "PHP 8"}, Messages: []chatMessage{{Role: "user", Text: "Translate this prototype"}}})
			w := httptest.NewRecorder()
			service.streamHandler(w, httptest.NewRequest("POST", "/chat/stream", strings.NewReader(string(body))))
			if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(w.Body.String(), `"text":"\u003c?php"`) {
				t.Fatalf("missing streamed code: %d %s", w.Code, w.Body.String())
			}
			result := readTestChatResult(t, project)
			if result.HTML != translationSource {
				t.Fatal("translation overwrote prototype")
			}
			if failure {
				if len(result.Translations) != 0 || !strings.Contains(w.Body.String(), `"success":false`) {
					t.Fatal("failed provider response saved partial output")
				}
			} else if len(result.Translations) != 1 || !strings.Contains(w.Body.String(), `"translation":`) || !strings.Contains(w.Body.String(), `"success":true`) {
				t.Fatal("successful response did not return saved translation")
			}
		})
	}
}
