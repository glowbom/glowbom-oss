package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
)

// Opt in with a local executable to verify the adapter against a real V2 server.
// The temporary server uses its own settings, data, credentials, and project.
func startOpenCodeV2LiveFixture(t *testing.T, config ...map[string]any) (context.Context, string, string) {
	t.Helper()
	binary := os.Getenv("GLOWBOM_TEST_OPENCODE_V2_BIN")
	if binary == "" {
		t.Skip("set GLOWBOM_TEST_OPENCODE_V2_BIN to run the live OpenCode 2 check")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	env := os.Environ()
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		path := filepath.Join(root, key)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		env = setEnvValue(env, key, path)
	}
	for _, key := range []string{"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT", "OPENCODE_MODEL", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "OPENCODE_API_KEY", "OPENROUTER_API_KEY", "FIREWORKS_API_KEY", "XAI_API_KEY"} {
		env = setEnvValue(env, key, "")
	}
	if len(config) != 0 {
		data, err := json.Marshal(config[0])
		if err != nil {
			t.Fatal(err)
		}
		env = setEnvValue(env, "OPENCODE_CONFIG_CONTENT", string(data))
	}
	password := "glowbom-isolated-live-fixture"
	env = setEnvValue(env, "OPENCODE_PASSWORD", password)
	env = setEnvValue(env, "OPENCODE_DISABLE_AUTOUPDATE", "true")
	t.Setenv("OPENCODE_SERVER_PASSWORD", password)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	server := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	command := exec.CommandContext(ctx, binary, "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprint(port), "--log-level", "error")
	command.Env, command.Dir = env, project
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	setOpenCodeProtocol(server, "v2")
	for started := time.Now(); time.Since(started) < 20*time.Second; {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("isolated OpenCode exited before readiness: %v", err)
		default:
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/info", nil)
		applyOpenCodeServerAuthorization(request)
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return ctx, server, project
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("isolated OpenCode did not become ready")
	return nil, "", ""
}

func TestLiveV2ColdProjectCatalogAndApproval(t *testing.T) {
	ctx, server, project := startOpenCodeV2LiveFixture(t)
	driver := NewOpenCodeDriver(server)
	providers, err := driver.client.App.Providers(ctx, opencode.AppProvidersParams{Directory: opencode.F(project)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, provider := range providers.Providers {
		if provider.ID == "opencode" {
			_, found = provider.Models["big-pickle"]
		}
	}
	if !found {
		t.Fatal("first catalog request on a cold project omitted opencode/big-pickle")
	}
	t.Log("cold project catalog exposes opencode/big-pickle")
	service := &chatService{directory: project, serverURL: server, client: openCodeHTTPClient(&http.Client{})}
	models, err := service.models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, model := range models {
		found = found || model.ID == "opencode/big-pickle"
	}
	if !found {
		t.Fatal("chat model discovery omitted opencode/big-pickle")
	}

	var session struct{ ID string }
	if err := service.json(ctx, http.MethodPost, "/session", map[string]any{"permission": []map[string]string{{"permission": "*", "pattern": "*", "action": "ask"}}}, &session); err != nil {
		t.Fatal(err)
	}
	native := func(method, path string, body any, result any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			data, _ := json.Marshal(body)
			reader = strings.NewReader(string(data))
		}
		req, _ := http.NewRequestWithContext(ctx, method, server+path, reader)
		req.Header.Set("Content-Type", "application/json")
		applyOpenCodeServerAuthorization(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.Fatalf("live API rejected %s %s (HTTP %d)", method, path, resp.StatusCode)
		}
		if result != nil {
			if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
				t.Fatal(err)
			}
		}
	}
	root := "/api/session/" + session.ID
	native(http.MethodPost, root+"/model", map[string]any{"model": map[string]string{"id": "big-pickle", "providerID": "opencode"}}, nil)
	for _, decision := range []string{"once", "always", "reject"} {
		var permission struct {
			Data struct{ ID, Effect string }
		}
		resource := decision + "-fixture.txt"
		native(http.MethodPost, root+"/permission", map[string]any{"action": "edit", "resources": []string{resource}, "save": []string{resource}, "metadata": map[string]any{}}, &permission)
		if permission.Data.Effect != "ask" {
			t.Fatalf("expected a real pending approval, got %q", permission.Data.Effect)
		}
		if err := driver.respondToPermission(ctx, session.ID, permission.Data.ID, decision, project); err != nil {
			t.Fatalf("live %s permission response: %v", decision, err)
		}
		t.Logf("real V2 server accepted %s permission decision", decision)
	}
}

// Run the real agent and filesystem tools with a deterministic local model.
func TestLiveV2AgentWritesFileAfterApproval(t *testing.T) {
	if os.Getenv("GLOWBOM_TEST_OPENCODE_V2_BIN") == "" {
		t.Skip("set GLOWBOM_TEST_OPENCODE_V2_BIN to run the live OpenCode 2 check")
	}
	const filename = "glowbom-live-test.txt"
	const content = "Glowbom live approval test passed."
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Messages []struct{ Role string }
			Tools    []struct{ Function struct{ Name string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("fixture model could not decode its prompt: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		finished, writeAvailable := false, false
		for _, message := range request.Messages {
			finished = finished || message.Role == "tool"
		}
		for _, tool := range request.Tools {
			writeAvailable = writeAvailable || tool.Function.Name == "write"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta any, finish any) {
			chunk := map[string]any{"id": "glowbom-fixture", "object": "chat.completion.chunk", "created": 1, "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		// Background title requests have no tools and also need a text response.
		if finished || !writeAvailable {
			send(map[string]any{"role": "assistant", "content": "DONE"}, nil)
			send(map[string]any{}, "stop")
		} else {
			arguments, _ := json.Marshal(map[string]string{"path": filename, "content": content})
			send(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_glowbom_fixture", "type": "function", "function": map[string]string{"name": "write", "arguments": string(arguments)}}}}, nil)
			send(map[string]any{}, "tool_calls")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx, server, project := startOpenCodeV2LiveFixture(t, map[string]any{"providers": map[string]any{"glowbomfixture": map[string]any{
		"name": "Glowbom local test fixture", "package": "@ai-sdk/openai-compatible",
		"settings": map[string]string{"baseURL": provider.URL + "/v1", "apiKey": "glowbom-local-fixture"},
		"models":   map[string]any{"fixture": map[string]any{"name": "Fixture", "capabilities": map[string]any{"tools": true, "input": []string{"text"}, "output": []string{"text"}}, "limit": map[string]int{"context": 32000, "output": 1024}}},
	}}})
	driver := NewOpenCodeDriver(server)
	if _, err := driver.client.App.Providers(ctx, opencode.AppProvidersParams{Directory: opencode.F(project)}); err != nil {
		t.Fatal(err)
	}
	service := &chatService{directory: project, serverURL: server, client: openCodeHTTPClient(&http.Client{})}
	var session struct{ ID string }
	if err := service.json(ctx, http.MethodPost, "/session", map[string]any{"permission": []map[string]string{
		{"permission": "*", "pattern": "*", "action": "deny"},
		{"permission": "edit", "pattern": "*", "action": "ask"},
	}}, &session); err != nil {
		t.Fatal(err)
	}
	eventRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/event", nil)
	applyOpenCodeServerAuthorization(eventRequest)
	eventResponse, err := http.DefaultClient.Do(eventRequest)
	if err != nil {
		t.Fatal(err)
	}
	if eventResponse.StatusCode != http.StatusOK {
		eventResponse.Body.Close()
		t.Fatalf("could not observe live build events: HTTP %d", eventResponse.StatusCode)
	}
	type observation struct{ kind, detail string }
	observations := make(chan observation, 16)
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		scanner := bufio.NewScanner(eventResponse.Body)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event struct {
				Type string
				Data map[string]any
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil || event.Data["sessionID"] != session.ID {
				continue
			}
			switch event.Type {
			case "session.execution.failed", "session.step.failed", "session.retry.scheduled", "session.execution.succeeded":
				detail := sanitizeProviderError(fmt.Errorf("%s", extractSessionErrorMessage(event.Data)))
				select {
				case observations <- observation{event.Type, detail}:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		eventResponse.Body.Close()
		<-eventsDone
	})
	if err := service.json(ctx, http.MethodPost, "/session/"+session.ID+"/prompt_async", map[string]any{
		"model": map[string]string{"providerID": "glowbomfixture", "modelID": "fixture"},
		"parts": []map[string]string{{"type": "text", "text": "Use the write tool to create exactly one file named " + filename + " in the current project with exactly this content: " + content + " Do not inspect or modify any other files. Do not use shell commands. After writing, reply DONE."}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	approved := false
	root := "/api/session/" + session.ID
	for {
		select {
		case event := <-observations:
			if event.kind == "session.retry.scheduled" {
				t.Logf("live model retry: %s", event.detail)
			} else if event.kind != "session.execution.succeeded" {
				t.Fatalf("live build failed before completion: %s", event.detail)
			} else if _, err := os.Stat(filepath.Join(project, filename)); err != nil {
				t.Fatal("live agent finished without writing its expected scratch file")
			}
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server+root+"/permission", nil)
		applyOpenCodeServerAuthorization(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var permissions struct {
			Data []struct {
				ID, Action string
				Resources  []string
			}
		}
		err = json.NewDecoder(resp.Body).Decode(&permissions)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("could not inspect live build approvals: HTTP %d, %v", resp.StatusCode, err)
		}
		for _, permission := range permissions.Data {
			if permission.Action != "edit" || len(permission.Resources) == 0 {
				t.Fatal("live test requested an unexpected action")
			}
			for _, resource := range permission.Resources {
				path := resource
				if !filepath.IsAbs(path) {
					path = filepath.Join(project, path)
				}
				if filepath.Clean(path) != filepath.Join(project, filename) {
					t.Fatal("live test requested access outside its expected scratch file")
				}
			}
			if err := driver.respondToPermission(ctx, session.ID, permission.ID, "once", project); err != nil {
				t.Fatal(err)
			}
			approved = true
			t.Log("allowed the real agent's scratch-file write")
		}
		if data, err := os.ReadFile(filepath.Join(project, filename)); err == nil {
			if !approved || strings.TrimSpace(string(data)) != content {
				t.Fatal("live build wrote unexpected content or bypassed its approval")
			}
			if err := service.json(ctx, http.MethodPost, "/experimental/session/"+session.ID+"/wait", nil, nil); err != nil {
				t.Fatal(err)
			}
			t.Log("real OpenCode agent resumed after approval and wrote the expected file")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("live build did not write its scratch file: %v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
