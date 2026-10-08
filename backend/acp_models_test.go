package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fakeACPModelProfile(t *testing.T, mode, marker, model string) acpProfile {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return acpProfile{ID: "acp-1", Name: "Model agent", Command: executable, Args: []string{"-test.run=^TestACPModelAgentProcess$", "--", "glowbom-acp-model-test", mode, marker}, Model: model}
}

func TestACPModelAgentProcess(t *testing.T) {
	var args []string
	for index, value := range os.Args {
		if value == "glowbom-acp-model-test" {
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
	metadata := func(current string) map[string]any {
		options := []any{map[string]string{"value": "agent/default", "name": "Default"}, map[string]string{"value": "agent/picked", "name": "Picked", "description": "Test model"}}
		if mode == "grouped" {
			options = []any{map[string]any{"group": "test-models", "name": "Test models", "options": options}}
		}
		if mode == "legacy" || mode == "legacy-mismatch" {
			return map[string]any{"models": map[string]any{"currentModelId": current, "availableModels": []any{map[string]string{"modelId": "agent/default", "name": "Default"}, map[string]string{"modelId": "agent/picked", "name": "Picked"}}}}
		}
		if mode == "unsupported" {
			return map[string]any{}
		}
		if mode == "malformed" {
			return map[string]any{"models": false, "configOptions": "unavailable"}
		}
		return map[string]any{"configOptions": []any{
			map[string]any{"id": "provider", "category": "model", "type": "select", "currentValue": "provider/default", "options": []any{map[string]string{"value": "provider/default", "name": "Provider"}}},
			map[string]any{"id": "engine", "category": "model", "type": "select", "currentValue": current, "options": options},
		}}
	}
	log, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer log.Close()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var frame acpRPCFrame
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			os.Exit(3)
		}
		_, _ = log.WriteString(frame.Method + "\n")
		switch frame.Method {
		case "initialize":
			respond(frame.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]bool{"loadSession": true}})
		case "session/new", "session/load":
			result := metadata("agent/default")
			if frame.Method == "session/new" {
				result["sessionId"] = "model-session"
			}
			respond(frame.ID, result)
		case "session/set_model", "session/set_config_option":
			var params struct {
				SessionID string `json:"sessionId"`
				ModelID   string `json:"modelId"`
				ConfigID  string `json:"configId"`
				Value     string `json:"value"`
			}
			if json.Unmarshal(frame.Params, &params) != nil || params.SessionID != "model-session" {
				os.Exit(4)
			}
			legacy := mode == "legacy" || mode == "legacy-mismatch"
			if legacy {
				if frame.Method != "session/set_model" || params.ModelID != "agent/picked" {
					os.Exit(5)
				}
			} else if frame.Method != "session/set_config_option" || params.ConfigID != "engine" || params.Value != "agent/picked" {
				os.Exit(6)
			}
			if mode == "rejected" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{"code": -32602, "message": "private-provider-details"}})
				continue
			}
			if mode == "legacy" || mode == "missing-confirmation" {
				respond(frame.ID, map[string]any{})
			} else if mode == "mismatch" || mode == "legacy-mismatch" {
				respond(frame.ID, metadata("agent/default"))
			} else {
				respond(frame.ID, metadata("agent/picked"))
			}
		case "session/prompt":
			respond(frame.ID, map[string]string{"stopReason": "end_turn"})
		default:
			os.Exit(7)
		}
	}
}

func TestACPModelSelectionBeforePromptAndAfterRestore(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, mode := range []string{"modern", "grouped", "legacy"} {
			t.Run(mode+map[bool]string{false: "/new", true: "/load"}[resume], func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "methods")
				profile := fakeACPModelProfile(t, mode, marker, "agent/picked")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				saved := ""
				setupMethod := "session/new"
				if resume {
					saved, setupMethod = "model-session", "session/load"
				}
				var reported string
				_, err := runACPTurn(ctx, profile, t.TempDir(), saved, []map[string]any{{"type": "text", "text": "Build"}}, func(message acpMessage) error {
					if message.Method == "glowbom/session" {
						var params struct {
							Model string `json:"model"`
						}
						_ = json.Unmarshal(message.Params, &params)
						reported = params.Model
					}
					return nil
				}, nil)
				if err != nil || reported != "agent/picked" {
					t.Fatalf("model=%q err=%v", reported, err)
				}
				method := "session/set_config_option"
				if mode == "legacy" {
					method = "session/set_model"
				}
				data, err := os.ReadFile(marker)
				if err != nil || string(data) != "initialize\n"+setupMethod+"\n"+method+"\nsession/prompt\n" {
					t.Fatalf("methods=%q err=%v", data, err)
				}
			})
		}
	}
}

func TestACPModelSelectionNeverPromptsAfterFailure(t *testing.T) {
	for _, mode := range []string{"unsupported", "unavailable", "malformed", "rejected", "mismatch", "legacy-mismatch", "missing-confirmation"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "methods")
			model := "agent/picked"
			if mode == "unavailable" {
				model = "agent/no-longer-available"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := runACPTurn(ctx, fakeACPModelProfile(t, mode, marker, model), t.TempDir(), "", []map[string]any{{"type": "text", "text": "Build"}}, nil, nil)
			if err == nil || strings.Contains(err.Error(), "private-provider-details") {
				t.Fatalf("unexpected error: %v", err)
			}
			data, readErr := os.ReadFile(marker)
			if readErr != nil || strings.Contains(string(data), "session/prompt") {
				t.Fatalf("methods=%q err=%v", data, readErr)
			}
		})
	}
}

func TestACPModelDefaultNeverSetsAnOption(t *testing.T) {
	for _, mode := range []string{"modern", "legacy", "malformed", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "methods")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := runACPTurn(ctx, fakeACPModelProfile(t, mode, marker, ""), t.TempDir(), "", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "initialize\nsession/new\nsession/prompt\n" {
				t.Fatalf("methods=%q err=%v", data, err)
			}
		})
	}
}

func TestACPModelProbeDiscoversChoicesWithoutApplyingSavedOverride(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "methods")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := probeACP(ctx, fakeACPModelProfile(t, "modern", marker, "agent/unavailable"))
	if err != nil || result.Model != "agent/default" || len(result.Models) != 2 || result.Models[1].ID != "agent/picked" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "initialize\nsession/new\n" {
		t.Fatalf("methods=%q err=%v", data, err)
	}
}

func TestACPModelDiscoveryValidatesMetadata(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata string
		configID string
		ids      []string
	}{
		{"model-preferred", `{"configOptions":[{"id":"related","category":"model","type":"select","options":[{"value":"related","name":"Related"}]},{"id":"model","type":"select","options":[{"value":"preferred","name":"Preferred"}]}]}`, "model", []string{"preferred"}},
		{"legacy-fallback", `{"configOptions":false,"models":{"availableModels":[{"modelId":"legacy","name":"Legacy"}]}}`, "", []string{"legacy"}},
		{"unknown-type", `{"configOptions":[{"id":"model","type":"boolean","options":[{"value":"ignored"}]}]}`, "", nil},
		{"duplicate-model", `{"configOptions":[{"id":"model","type":"select","options":[{"value":"duplicate"},{"value":"duplicate"}]}]}`, "", nil},
		{"duplicate-config", `{"configOptions":[{"id":"model","type":"select","options":[{"value":"a"}]},{"id":"model","type":"select","options":[{"value":"b"}]}]}`, "", nil},
		{"mixed-groups", `{"configOptions":[{"id":"model","type":"select","options":[{"value":"a"},{"group":"group","options":[{"value":"b"}]}]}]}`, "", nil},
		{"duplicate-group-model", `{"configOptions":[{"id":"model","type":"select","options":[{"group":"first","options":[{"value":"a"}]},{"group":"second","options":[{"value":"a"}]}]}]}`, "", nil},
		{"invalid-id", `{"models":{"availableModels":[{"modelId":"bad\nmodel"}]}}`, "", nil},
		{"malformed-option", `{"models":{"availableModels":[false]}}`, "", nil},
		{"too-long-id", `{"models":{"availableModels":[{"modelId":"` + strings.Repeat("x", 257) + `"}]}}`, "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response acpSessionResponse
			if err := json.Unmarshal([]byte(test.metadata), &response); err != nil {
				t.Fatal(err)
			}
			selection := acpDiscoverSessionModels(response)
			var ids []string
			for _, option := range selection.options {
				ids = append(ids, option.ID)
			}
			if selection.configID != test.configID || !reflect.DeepEqual(ids, test.ids) {
				t.Fatalf("config=%q ids=%v", selection.configID, ids)
			}
		})
	}
	values := make([]map[string]string, acpMaxModelOptions+1)
	for index := range values {
		values[index] = map[string]string{"value": "model"}
	}
	encoded, _ := json.Marshal(values)
	if options := acpModelOptions(encoded, false); len(options) != 0 {
		t.Fatal("oversized model list was accepted")
	}
}
