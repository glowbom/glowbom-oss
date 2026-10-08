package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeACPProfile(t *testing.T, mode, marker string) acpProfile {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return acpProfile{ID: "acp-1", Name: "Test agent", Command: executable, Args: []string{"-test.run=^TestACPAgentProcess$", "--", "glowbom-acp-test", mode, marker}}
}

// A real subprocess exercises framing, process cleanup, and both RPC directions.
func TestACPAgentProcess(t *testing.T) {
	var args []string
	for index, value := range os.Args {
		if value == "glowbom-acp-test" {
			args = os.Args[index+1:]
			break
		}
	}
	if len(args) != 2 {
		return
	}
	mode, marker := args[0], args[1]
	defer os.Exit(0)
	encoder := json.NewEncoder(os.Stdout)
	respond := func(id json.RawMessage, result any) {
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	sessionResponse := func() map[string]any {
		result := map[string]any{}
		if mode == "reported-model" || mode == "load-reported-model" {
			if json.Unmarshal([]byte(marker), &result) != nil {
				os.Exit(12)
			}
		}
		return result
	}
	notify := func(session, text string) {
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}}}})
	}
	permission := func(session string) {
		secondID := "always"
		if mode == "duplicate-options" {
			secondID = "approve"
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{
			"sessionId": session, "toolCall": map[string]string{"toolCallId": "tool-1", "title": "Edit project"},
			"options": []any{map[string]string{"optionId": "approve", "name": "Allow once", "kind": "allow_once"}, map[string]string{"optionId": secondID, "name": "Always", "kind": "allow_always"}},
		}})
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), acpMaxMessageBytes)
	var promptID json.RawMessage
	for scanner.Scan() {
		var frame acpRPCFrame
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			os.Exit(2)
		}
		switch frame.Method {
		case "initialize":
			var request struct {
				ProtocolVersion    int `json:"protocolVersion"`
				ClientCapabilities struct {
					FS struct {
						Read  bool `json:"readTextFile"`
						Write bool `json:"writeTextFile"`
					} `json:"fs"`
					Terminal bool `json:"terminal"`
				} `json:"clientCapabilities"`
			}
			if json.Unmarshal(frame.Params, &request) != nil || request.ProtocolVersion != 1 || request.ClientCapabilities.FS.Read || request.ClientCapabilities.FS.Write || request.ClientCapabilities.Terminal {
				os.Exit(3)
			}
			if os.Getenv("GLOWBOM_BACKEND_TOKEN") != "" || os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("NODE_OPTIONS") != "" {
				os.Exit(4)
			}
			if mode == "handshake-timeout" {
				time.Sleep(30 * time.Second)
				return
			}
			version := 1
			if mode == "version" {
				version = 99
			}
			result := map[string]any{"protocolVersion": version, "agentInfo": map[string]string{"name": "fake", "title": "Fake ACP", "version": "1.2.3"}, "authMethods": []any{map[string]string{"id": "login", "name": "Sign in"}}}
			if mode != "no-capabilities" && mode != "no-images" {
				result["agentCapabilities"] = map[string]any{"loadSession": true, "promptCapabilities": map[string]bool{"image": true}}
			}
			respond(frame.ID, result)
		case "session/new":
			var params struct {
				CWD        string `json:"cwd"`
				MCPServers []any  `json:"mcpServers"`
			}
			if json.Unmarshal(frame.Params, &params) != nil || !filepath.IsAbs(params.CWD) || params.MCPServers == nil {
				os.Exit(5)
			}
			if mode == "auth" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{"code": -32000, "message": "private secret sk-do-not-display"}})
				continue
			}
			if mode == "load" || mode == "load-reported-model" {
				os.Exit(6)
			}
			result := sessionResponse()
			result["sessionId"] = "test-session"
			respond(frame.ID, result)
			if mode == "blocked-input" {
				time.Sleep(30 * time.Second)
				return
			}
		case "session/load":
			var params struct {
				SessionID string `json:"sessionId"`
			}
			if (mode != "load" && mode != "load-reported-model") || json.Unmarshal(frame.Params, &params) != nil || params.SessionID != "test-session" {
				os.Exit(7)
			}
			notify("test-session", "Previous conversation")
			respond(frame.ID, sessionResponse())
		case "session/prompt":
			promptID = frame.ID
			if mode == "probe" || mode == "auth" || mode == "no-images" {
				os.Exit(8)
			}
			switch mode {
			case "permission", "always", "cancel", "duplicate-options":
				permission("test-session")
				notify("test-session", "Permission pending")
			case "foreign-permission":
				permission("different-session")
			case "foreign-update":
				notify("different-session", "Wrong project")
			case "unknown-request":
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "unsupported", "method": "terminal/create", "params": map[string]string{"sessionId": "test-session"}})
			case "malformed":
				fmt.Fprintln(os.Stdout, "Not ACP JSON")
			case "eof":
				return
			case "rpc-error":
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{"code": -32603, "message": "secret-auth-token", "data": map[string]string{"apiKey": "private-provider-key"}}})
			case "max_tokens", "max_turn_requests", "refusal", "cancelled", "unknown-stop":
				respond(frame.ID, map[string]string{"stopReason": mode})
			default:
				// Split one frame across writes to exercise partial pipe reads.
				fmt.Fprint(os.Stdout, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"test-session",`)
				fmt.Fprintln(os.Stdout, `"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done"}}}}`)
				respond(frame.ID, map[string]string{"stopReason": "end_turn"})
			}
		case "session/cancel":
			if marker != "" {
				_ = os.WriteFile(marker, []byte("cancelled"), 0600)
			}
			respond(promptID, map[string]string{"stopReason": "cancelled"})
		case "":
			if string(frame.ID) == `"unsupported"` {
				if frame.Error == nil || frame.Error.Code != -32601 {
					os.Exit(9)
				}
			} else {
				var result struct {
					Outcome struct {
						Outcome  string `json:"outcome"`
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				}
				_ = json.Unmarshal(frame.Result, &result)
				if mode == "permission" {
					if result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "approve" {
						os.Exit(10)
					}
				} else if result.Outcome.Outcome != "cancelled" {
					os.Exit(11)
				}
			}
			if mode != "cancel" {
				respond(promptID, map[string]string{"stopReason": "end_turn"})
			}
		}
	}
}

func TestACPProbeCapabilitiesAndAuthentication(t *testing.T) {
	t.Setenv("GLOWBOM_BACKEND_TOKEN", "should-not-be-inherited")
	t.Setenv("OPENAI_API_KEY", "should-not-be-inherited")
	t.Setenv("NODE_OPTIONS", "should-not-be-inherited")
	for _, mode := range []string{"probe", "auth", "no-capabilities"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := probeACP(ctx, fakeACPProfile(t, mode, ""))
			if err != nil || result.Name != "Fake ACP" || result.Version != "1.2.3" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if result.Images != (mode != "no-capabilities") || result.LoadSession != (mode != "no-capabilities") || result.AuthRequired != (mode == "auth") {
				t.Fatalf("incorrect capabilities: %+v", result)
			}
		})
	}
}

func TestACPTurnStreamingAndSessionRecovery(t *testing.T) {
	for _, mode := range []string{"normal", "load", "no-capabilities", "unknown-request"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var events []acpMessage
			savedSession := ""
			if mode == "load" || mode == "no-capabilities" {
				savedSession = "test-session"
			}
			session, err := runACPTurn(ctx, fakeACPProfile(t, mode, ""), t.TempDir(), savedSession, []map[string]any{{"type": "text", "text": "Build"}}, func(message acpMessage) error {
				events = append(events, message)
				return nil
			}, nil)
			if err != nil || session != "test-session" || len(events) == 0 || events[0].Method != "glowbom/session" {
				t.Fatalf("session=%q err=%v events=%+v", session, err, events)
			}
			for _, event := range events {
				if strings.Contains(string(event.Params), "Previous conversation") {
					t.Fatal("session replay leaked into current turn")
				}
			}
			if mode != "unknown-request" && len(events) != 2 {
				t.Fatalf("expected session and one live update, got %+v", events)
			}
		})
	}
}

func TestACPProbeReportsConfiguredModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := probeACP(ctx, fakeACPProfile(t, "reported-model", `{"models":{"currentModelId":"agent/free-model"}}`))
	if err != nil || result.Model != "agent/free-model" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestACPTurnReportsSessionModelWithoutRequiringMetadata(t *testing.T) {
	for _, savedSession := range []string{"", "test-session"} {
		mode := "reported-model"
		if savedSession != "" {
			mode = "load-reported-model"
		}
		for _, test := range []struct {
			name     string
			metadata string
			model    string
		}{
			{"models", `{"models":{"currentModelId":"agent/free-model"}}`, "agent/free-model"},
			{"config", `{"configOptions":[{"id":"model","category":"model","currentValue":"agent/config-model"}]}`, "agent/config-model"},
			{"malformed", `{"models":123,"configOptions":"unavailable"}`, ""},
		} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var model string
				var reported bool
				session, err := runACPTurn(ctx, fakeACPProfile(t, mode, test.metadata), t.TempDir(), savedSession, []map[string]any{{"type": "text", "text": "Build"}}, func(message acpMessage) error {
					if message.Method == "glowbom/session" {
						var params map[string]json.RawMessage
						if err := json.Unmarshal(message.Params, &params); err != nil {
							return err
						}
						_, reported = params["model"]
						if reported {
							return json.Unmarshal(params["model"], &model)
						}
					}
					return nil
				}, nil)
				if err != nil || session != "test-session" || model != test.model || reported != (test.model != "") {
					t.Fatalf("session=%q model=%q reported=%v err=%v", session, model, reported, err)
				}
			})
		}
	}
}

func TestACPReportedModelMetadataIsOptionalAndBounded(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata string
		model    string
	}{
		{"absent", `{}`, ""},
		{"legacy-model", `{"configOptions":[{"id":"model","currentValue":"legacy"}]}`, "legacy"},
		{"category-model", `{"configOptions":[{"id":"engine","category":"model","currentValue":"category"}]}`, "category"},
		{"models-first", `{"models":{"currentModelId":"current"},"configOptions":[{"id":"model","currentValue":"other"}]}`, "current"},
		{"model-option-first", `{"configOptions":[{"id":"engine","category":"model","currentValue":"other"},{"id":"model","currentValue":"explicit"}]}`, "explicit"},
		{"provider-is-not-model", `{"configOptions":[{"id":"provider","category":"model","currentValue":"provider"}]}`, ""},
		{"malformed-option", `{"models":false,"configOptions":[12,{"id":"model","currentValue":false},{"id":"engine","category":"model","currentValue":"valid"}]}`, "valid"},
		{"control-character", `{"models":{"currentModelId":"bad\nmodel"}}`, ""},
		{"line-separator", `{"models":{"currentModelId":"bad\u2028model"}}`, ""},
		{"empty", `{"models":{"currentModelId":"   "}}`, ""},
		{"too-long", `{"models":{"currentModelId":"` + strings.Repeat("x", 257) + `"}}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response acpSessionResponse
			if err := json.Unmarshal([]byte(test.metadata), &response); err != nil {
				t.Fatal(err)
			}
			if got := acpReportedSessionModel(response); got != test.model {
				t.Fatalf("model=%q want=%q", got, test.model)
			}
		})
	}
}

func TestACPPermissionDoesNotBlockUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updated := make(chan struct{})
	var once sync.Once
	_, err := runACPTurn(ctx, fakeACPProfile(t, "permission", ""), t.TempDir(), "", []map[string]any{{"type": "text", "text": "Build"}}, func(message acpMessage) error {
		if message.Method == "session/update" {
			once.Do(func() { close(updated) })
		}
		return nil
	}, func(ctx context.Context, message acpMessage) (any, error) {
		select {
		case <-updated:
			return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "approve"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestACPPermissionCannotGrantPersistentOrForeignApproval(t *testing.T) {
	for _, mode := range []string{"always", "foreign-permission", "duplicate-options"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			called := false
			_, err := runACPTurn(ctx, fakeACPProfile(t, mode, ""), t.TempDir(), "", nil, nil, func(context.Context, acpMessage) (any, error) {
				called = true
				return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "always"}}, nil
			})
			if err != nil || called != (mode == "always") {
				t.Fatalf("called=%v err=%v", called, err)
			}
		})
	}
}

func TestACPCancellationDrainsPermissionAndNotifiesAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "cancelled")
	callbackStopped := make(chan struct{})
	started := time.Now()
	_, err := runACPTurn(ctx, fakeACPProfile(t, "cancel", marker), t.TempDir(), "", nil, nil, func(ctx context.Context, _ acpMessage) (any, error) {
		cancel()
		<-ctx.Done()
		close(callbackStopped)
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || time.Since(started) > 3*time.Second {
		t.Fatalf("err=%v duration=%v", err, time.Since(started))
	}
	select {
	case <-callbackStopped:
	default:
		t.Fatal("permission callback is still running")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "cancelled" {
		t.Fatalf("agent did not receive cancellation: %q %v", data, err)
	}
}

func TestACPTurnRejectsInvalidOrIncompleteResults(t *testing.T) {
	for _, mode := range []string{"version", "malformed", "eof", "foreign-update", "rpc-error", "max_tokens", "max_turn_requests", "refusal", "cancelled", "unknown-stop"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := runACPTurn(ctx, fakeACPProfile(t, mode, ""), t.TempDir(), "", nil, nil, nil)
			if err == nil || strings.Contains(err.Error(), "secret-auth-token") || strings.Contains(err.Error(), "private-provider-key") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestACPRejectsUnsupportedImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runACPTurn(ctx, fakeACPProfile(t, "no-images", ""), t.TempDir(), "", []map[string]any{{"type": "image", "mimeType": "image/png", "data": "abc"}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "does not support image attachments") {
		t.Fatal(err)
	}
}

func TestACPCancellationInterruptsSetupAndBlockedWrite(t *testing.T) {
	for _, mode := range []string{"handshake-timeout", "blocked-input"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err := runACPTurn(ctx, fakeACPProfile(t, mode, ""), t.TempDir(), "", []map[string]any{{"type": "text", "text": strings.Repeat("x", 2<<20)}}, nil, nil)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
				t.Fatalf("err=%v duration=%v", err, time.Since(started))
			}
		})
	}
}

func TestACPEnvironmentOmitsBackendAndProviderSecrets(t *testing.T) {
	environment := []string{"PATH=/bin", "HOME=/home/person", "XDG_CONFIG_HOME=/config", "GLOWBOM_BACKEND_TOKEN=private", "OPENAI_API_KEY=private", "ANTHROPIC_API_KEY=private", "AWS_SECRET_ACCESS_KEY=private", "NODE_OPTIONS=--require=bad.js", "BASH_ENV=bad.sh", "DYLD_INSERT_LIBRARIES=bad.dylib", "UNRELATED_TOKEN=private"}
	want := environment[:3]
	if got := acpRuntimeEnvironment(environment); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected environment: %v", got)
	}
}
