package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fakeACPCachedTokenProfile(t *testing.T, mode, marker string) acpProfile {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return acpProfile{ID: "acp-1", Name: "Cached login fixture", Command: executable,
		Args: []string{"-test.run=^TestACPCachedTokenAgentProcess$", "--", "glowbom-acp-auth-test", mode, marker}}
}

// This subprocess implements only the authentication and session handshake.
func TestACPCachedTokenAgentProcess(t *testing.T) {
	var args []string
	for index, value := range os.Args {
		if value == "glowbom-acp-auth-test" {
			args = os.Args[index+1:]
			break
		}
	}
	if len(args) != 2 {
		return
	}
	mode, marker := args[0], args[1]
	defer os.Exit(0)
	log, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer log.Close()
	encoder := json.NewEncoder(os.Stdout)
	respond := func(id json.RawMessage, result any) {
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	needsAuth := mode != "none" && mode != "interactive" && mode != "terminal" && mode != "unknown-type"
	authenticated := !needsAuth
	initialized := false
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var frame acpRPCFrame
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			os.Exit(3)
		}
		_, _ = log.WriteString(frame.Method + "\n")
		switch frame.Method {
		case "initialize":
			if initialized || os.Getenv("XAI_API_KEY") != "" || os.Getenv("GLOWBOM_SERVER_TOKEN") != "" {
				os.Exit(4)
			}
			initialized = true
			methods := []any{}
			if mode == "interactive" || mode == "mixed" {
				methods = append(methods, map[string]string{"id": "browser_login", "name": "Sign in"})
			}
			if mode != "none" && mode != "interactive" {
				method := map[string]string{"id": "cached_token", "name": "Cached login"}
				switch mode {
				case "agent":
					method["type"] = "agent"
				case "terminal":
					method["type"] = "terminal"
				case "unknown-type":
					method["type"] = "future-type"
				}
				methods = append(methods, method)
			}
			respond(frame.ID, map[string]any{"protocolVersion": 1, "authMethods": methods,
				"agentInfo":         map[string]string{"name": "cached-fixture", "title": "Cached ACP", "version": "1.0.0"},
				"agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]bool{"image": true}}})
		case "authenticate":
			var params map[string]any
			if !initialized || authenticated || !needsAuth || json.Unmarshal(frame.Params, &params) != nil ||
				!reflect.DeepEqual(params, map[string]any{"methodId": "cached_token", "_meta": map[string]any{"headless": true}}) {
				os.Exit(5)
			}
			if mode == "cancel" {
				time.Sleep(30 * time.Second)
				return
			}
			if mode == "denied" || mode == "rpc-error" {
				code := -32000
				if mode == "rpc-error" {
					code = -32603
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{
					"code": code, "message": "private-auth-message", "data": map[string]string{"token": "private-cached-token"}}})
				continue
			}
			if mode == "malformed" {
				respond(frame.ID, []string{"private-cached-token"})
				continue
			}
			authenticated = true
			respond(frame.ID, map[string]string{"extra": "private-cached-token"})
		case "session/new", "session/load":
			if !initialized || !authenticated {
				os.Exit(6)
			}
			respond(frame.ID, map[string]string{"sessionId": "cached-session"})
		case "session/prompt":
			if !authenticated {
				os.Exit(7)
			}
			respond(frame.ID, map[string]string{"stopReason": "end_turn"})
		default:
			os.Exit(8)
		}
	}
}

func runCachedACPWorkflow(ctx context.Context, t *testing.T, profile acpProfile, workflow string) (acpProbeResult, error) {
	t.Helper()
	if workflow == "probe" {
		return probeACP(ctx, profile)
	}
	saved := ""
	if workflow == "resume" {
		saved = "cached-session"
	}
	_, err := runACPTurn(ctx, profile, t.TempDir(), saved, []map[string]any{{"type": "text", "text": "Build"}}, func(message acpMessage) error {
		if strings.Contains(string(message.Params), "private-cached-token") {
			t.Error("authentication response leaked into build events")
		}
		return nil
	}, nil)
	return acpProbeResult{}, err
}

func TestACPCachedTokenAuthenticationOrder(t *testing.T) {
	t.Setenv("XAI_API_KEY", "must-not-be-forwarded")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "must-not-be-forwarded")
	for _, workflow := range []string{"probe", "build", "resume"} {
		for _, mode := range []string{"cached", "agent", "mixed", "none", "interactive", "terminal", "unknown-type"} {
			t.Run(workflow+"/"+mode, func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "methods")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result, err := runCachedACPWorkflow(ctx, t, fakeACPCachedTokenProfile(t, mode, marker), workflow)
				if err != nil || result.AuthRequired {
					t.Fatalf("unexpected connection failure: %v", err)
				}
				if workflow == "probe" && (result.Name != "Cached ACP" || !result.Images || !result.LoadSession) {
					t.Fatal("authentication changed reported capabilities")
				}
				encoded, _ := json.Marshal(result)
				if strings.Contains(string(encoded), "private-cached-token") {
					t.Fatal("authentication response leaked into connection status")
				}
				want := "initialize\n"
				if mode == "cached" || mode == "agent" || mode == "mixed" {
					want += "authenticate\n"
				}
				if workflow == "resume" {
					want += "session/load\n"
				} else {
					want += "session/new\n"
				}
				if workflow != "probe" {
					want += "session/prompt\n"
				}
				if methods, err := os.ReadFile(marker); err != nil || string(methods) != want {
					t.Fatalf("method order=%q, want %q, read error=%v", methods, want, err)
				}
			})
		}
	}

}

func TestACPCachedTokenFailureStopsBeforeSession(t *testing.T) {
	for _, workflow := range []string{"probe", "build", "resume"} {
		for _, mode := range []string{"denied", "rpc-error", "malformed"} {
			t.Run(workflow+"/"+mode, func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "methods")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result, err := runCachedACPWorkflow(ctx, t, fakeACPCachedTokenProfile(t, mode, marker), workflow)
				if workflow == "probe" && mode == "denied" {
					if err != nil || !result.AuthRequired || result.Name != "Cached ACP" {
						t.Fatalf("expected named sign-in-required status, got %+v, %v", result, err)
					}
				} else {
					if err == nil || strings.Contains(err.Error(), "private-") || result.AuthRequired {
						t.Fatalf("expected redacted authentication failure, got %v", err)
					}
					if mode == "denied" && err.Error() != errACPAuthRequired.Error() {
						t.Fatal("build did not explain terminal sign-in")
					}
				}
				if methods, err := os.ReadFile(marker); err != nil || string(methods) != "initialize\nauthenticate\n" {
					t.Fatalf("authentication failure continued the session: %q, %v", methods, err)
				}
			})
		}
	}

}

func TestACPCachedTokenCancellationStopsBeforeSession(t *testing.T) {
	for _, workflow := range []string{"probe", "build"} {
		t.Run(workflow, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "methods")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := runCachedACPWorkflow(ctx, t, fakeACPCachedTokenProfile(t, "cancel", marker), workflow)
				done <- err
			}()
			for {
				methods, _ := os.ReadFile(marker)
				if strings.Contains(string(methods), "authenticate\n") {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("agent stopped before authentication: %v", err)
				case <-ctx.Done():
					t.Fatal("agent did not reach authentication")
				case <-time.After(5 * time.Millisecond):
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation, got %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("authentication cancellation did not stop the owned agent")
			}
			if methods, err := os.ReadFile(marker); err != nil || string(methods) != "initialize\nauthenticate\n" {
				t.Fatalf("canceled authentication continued the session: %q, %v", methods, err)
			}
		})
	}
}
