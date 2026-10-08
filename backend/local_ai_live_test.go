package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt in with GLOWBOM_TEST_LOCAL_AI_MODEL set to an already downloaded model.
// The test uses temporary settings and a separate OpenCode port.
func TestLocalAILiveConnection(t *testing.T) {
	model := os.Getenv("GLOWBOM_TEST_LOCAL_AI_MODEL")
	if model == "" {
		t.Skip("set GLOWBOM_TEST_LOCAL_AI_MODEL to test a downloaded Ollama model")
	}
	binary, err := openCodeExecutable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("OPENCODE_CONFIG_CONTENT", "{}")
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("OPENCODE_URL", "")
	t.Setenv("GLOWBOM_DESKTOP", "")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "local-ai-isolated-test")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	t.Setenv("GLOWBOM_AGENT_PORT", port)
	defer stopOpenCodeServerOnPort(port)
	chat := newChatService(root, root)
	service := newLocalAIService(chat)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := service.connect(ctx, model); err != nil {
		t.Fatal(err)
	}
	if model == localMiMoModel {
		project := translationProject(t)
		largeHTML := "<!doctype html><html>" + strings.Repeat("<p>Project content</p>", 4000) + "</html>"
		if err := os.WriteFile(filepath.Join(project, "prototype", "index.html"), []byte(largeHTML), 0600); err != nil {
			t.Fatal(err)
		}
		request := chatRequest{ProjectPath: project, Model: localAIProvider + "/" + model, Mode: "chat", Messages: []chatMessage{{Role: "user", Text: strings.Repeat("Older project discussion. ", 1000)}, {Role: "assistant", Text: "Earlier answer"}, {Role: "user", Text: "howdy"}}}
		data, _ := json.Marshal(request)
		recorder := httptest.NewRecorder()
		chat.streamHandler(recorder, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(data)).WithContext(ctx))
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"success":true`) {
			t.Fatalf("project chat failed: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}
