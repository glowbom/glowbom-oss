package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mockCodexAppServerImages(t *testing.T, scenario string) string {
	t.Helper()
	image := base64.StdEncoding.EncodeToString(projectIconTestImage(t, "png"))
	t.Setenv("GLOWBOM_CODEX_BIN", os.Args[0])
	t.Setenv("GLOWBOM_CODEX_IMAGE_TEST_SCENARIO", scenario)
	t.Setenv("GLOWBOM_CODEX_IMAGE_TEST_DATA", image)
	t.Setenv("OPENAI_API_KEY", "must-not-reach-child")
	t.Setenv("CODEX_API_KEY", "must-not-reach-child")
	t.Setenv("OPENAI_BASE_URL", "https://must-not-reach-child.invalid")
	previous := codexAppServerImageCommand
	codexAppServerImageCommand = func(ctx context.Context, binary string, args ...string) *exec.Cmd {
		if len(args) < 3 || args[0] != "app-server" || args[1] != "--listen" || args[2] != "stdio://" {
			t.Fatal("image transport did not use local stdio app-server")
		}
		return exec.CommandContext(ctx, binary, append([]string{"-test.run=^TestCodexAppServerImageHelperProcess$", "--"}, args...)...)
	}
	codexAppServerImageProbeCache.Lock()
	codexAppServerImageProbeCache.until = time.Time{}
	codexAppServerImageProbeCache.Unlock()
	t.Cleanup(func() {
		codexAppServerImageCommand = previous
		codexAppServerImageProbeCache.Lock()
		codexAppServerImageProbeCache.until = time.Time{}
		codexAppServerImageProbeCache.Unlock()
	})
	return "data:image/png;base64," + image
}

func TestCodexAppServerImageHelperProcess(t *testing.T) {
	scenario := os.Getenv("GLOWBOM_CODEX_IMAGE_TEST_SCENARIO")
	if scenario == "" {
		return
	}
	for _, name := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"} {
		if os.Getenv(name) != "" {
			os.Exit(8)
		}
	}
	required := []string{`model_provider="openai"`, "features.image_generation=true", "features.shell_tool=false", "features.hooks=false", "mcp_servers={}"}
	arguments := strings.Join(os.Args, "\n")
	for _, arg := range required {
		if !strings.Contains(arguments, arg) {
			os.Exit(9)
		}
	}
	send := func(message any) { _ = json.NewEncoder(os.Stdout).Encode(message) }
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		var request struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(10)
		}
		var result any = map[string]any{}
		switch request.Method {
		case "initialized":
			continue
		case "initialize":
			client := request.Params["clientInfo"].(map[string]any)
			capabilities := request.Params["capabilities"].(map[string]any)
			if client["name"] != "glowbom" || capabilities["experimentalApi"] != true {
				os.Exit(11)
			}
		case "account/read":
			if request.Params["refreshToken"] != false {
				os.Exit(12)
			}
			typeName := "chatgpt"
			if scenario == "api-key" {
				typeName = "apiKey"
			}
			result = map[string]any{"account": map[string]string{"type": typeName}, "requiresOpenaiAuth": true}
			if scenario == "logged-out" {
				result = map[string]any{"account": nil, "requiresOpenaiAuth": true}
			}
		case "modelProvider/capabilities/read":
			result = map[string]bool{"imageGeneration": scenario != "unsupported", "namespaceTools": true, "webSearch": true}
		case "config/read":
			if request.Params["includeLayers"] != false {
				os.Exit(18)
			}
			result = map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"inherited-server": map[string]string{"url": "https://must-not-run.invalid", "token": "secret-token"}, "server.with.dots": map[string]string{"token": "secret-token"}}}}
		case "thread/start":
			if request.Params["ephemeral"] != true || request.Params["approvalPolicy"] != "never" || request.Params["sandbox"] != "workspace-write" {
				os.Exit(13)
			}
			configuration := request.Params["config"].(map[string]any)
			servers := configuration["mcp_servers"].(map[string]any)
			if request.Params["modelProvider"] != "openai" || len(servers) != 2 {
				os.Exit(19)
			}
			for _, name := range []string{"inherited-server", "server.with.dots"} {
				entry := servers[name].(map[string]any)
				if len(entry) != 1 || entry["enabled"] != false {
					os.Exit(20)
				}
			}
			result = map[string]any{"thread": map[string]string{"id": "image-thread"}}
		case "mcpServerStatus/list":
			if request.Params["threadId"] != "image-thread" || request.Params["detail"] != "toolsAndAuthOnly" {
				os.Exit(21)
			}
			result = map[string]any{"data": []any{}, "nextCursor": nil}
			if scenario == "inherited-active-tool" {
				result = map[string]any{"data": []any{map[string]any{"tools": map[string]any{"unsafe-tool": map[string]string{"description": "secret-token"}}}}}
			}
		case "turn/start":
			policy := request.Params["sandboxPolicy"].(map[string]any)
			if policy["networkAccess"] != false || policy["excludeSlashTmp"] != true || policy["excludeTmpdirEnvVar"] != true {
				os.Exit(14)
			}
			input := request.Params["input"].([]any)
			if !strings.Contains(input[0].(map[string]any)["text"].(string), "native image generation") {
				os.Exit(15)
			}
			if scenario == "reference" && (len(input) != 2 || input[1].(map[string]any)["url"] != "data:image/png;base64,"+os.Getenv("GLOWBOM_CODEX_IMAGE_TEST_DATA")) {
				os.Exit(16)
			}
			if scenario == "rpc-error" {
				send(map[string]any{"id": request.ID, "error": map[string]any{"code": -1, "message": "secret-token personal-data"}})
				continue
			}
			result = map[string]any{"turn": map[string]string{"id": "image-turn"}}
			send(map[string]any{"id": request.ID, "result": result})
			if scenario == "wait" {
				time.Sleep(time.Hour)
			}
			if scenario == "approval" {
				send(map[string]any{"id": 500, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "image-thread"}})
				continue
			}
			if scenario == "retry" || scenario == "terminal-error" {
				send(map[string]any{"method": "error", "params": map[string]any{"threadId": "image-thread", "turnId": "image-turn", "willRetry": scenario == "retry", "error": map[string]any{"message": "secret-token personal-data"}}})
				if scenario == "terminal-error" {
					continue
				}
			}
			if scenario != "no-image" {
				item := map[string]any{"type": "imageGeneration", "id": "image-item", "status": "completed", "result": os.Getenv("GLOWBOM_CODEX_IMAGE_TEST_DATA")}
				if scenario == "invalid-image" {
					item["result"] = base64.StdEncoding.EncodeToString([]byte("not an image"))
				}
				if scenario == "saved-file" {
					data, _ := base64.StdEncoding.DecodeString(os.Getenv("GLOWBOM_CODEX_IMAGE_TEST_DATA"))
					cwd, _ := os.Getwd()
					path := filepath.Join(cwd, "generated.png")
					_ = os.WriteFile(path, data, 0600)
					item["result"], item["savedPath"] = "", path
				}
				send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "image-thread", "turnId": "image-turn", "item": item}})
			}
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "image-thread", "turn": map[string]string{"id": "image-turn", "status": "completed"}}})
			continue
		default:
			os.Exit(17)
		}
		send(map[string]any{"id": request.ID, "result": result})
	}
	os.Exit(0)
}

func TestCodexAppServerImagesMode(t *testing.T) {
	for _, value := range []string{"", "legacy", "codex-app-server", "CODEX-APP-SERVER"} {
		t.Setenv("GLOWBOM_CHATGPT_IMAGE_TRANSPORT", value)
		if got := codexAppServerImagesEnabled(); got != (value == "codex-app-server") {
			t.Fatalf("unexpected enabled state for %q: %v", value, got)
		}
	}
}

func TestCodexAppServerImageAvailability(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		ok       bool
		code     string
	}{{"success", true, ""}, {"api-key", false, "login"}, {"logged-out", false, "login"}, {"unsupported", false, "capability"}} {
		t.Run(tc.scenario, func(t *testing.T) {
			mockCodexAppServerImages(t, tc.scenario)
			ok, code := codexAppServerImageAvailability(context.Background())
			if ok != tc.ok || code != tc.code {
				t.Fatalf("availability=%v code=%q, want %v %q", ok, code, tc.ok, tc.code)
			}
		})
	}
}

func TestCodexAppServerImageNativeResults(t *testing.T) {
	for _, scenario := range []string{"success", "reference", "saved-file", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			want := mockCodexAppServerImages(t, scenario)
			reference := ""
			if scenario == "reference" {
				reference = want
			}
			got, err := callCodexAppServerImageGeneration(context.Background(), "A small apple", reference, "1:1")
			if err != nil || got != want {
				t.Fatalf("native image failed: %v", err)
			}
		})
	}
}

func TestCodexAppServerImageFailuresStaySafe(t *testing.T) {
	for _, scenario := range []string{"api-key", "unsupported", "rpc-error", "approval", "no-image", "invalid-image", "terminal-error", "inherited-active-tool"} {
		t.Run(scenario, func(t *testing.T) {
			mockCodexAppServerImages(t, scenario)
			_, err := callCodexAppServerImageGeneration(context.Background(), "A small apple", "", "1:1")
			var failure *codexAppServerImageFailure
			if !errors.As(err, &failure) || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "personal-data") {
				t.Fatalf("missing safe failure: %v", err)
			}
		})
	}
}

func TestCodexAppServerImageCancellation(t *testing.T) {
	mockCodexAppServerImages(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := callCodexAppServerImageGeneration(ctx, "A small apple", "", "1:1")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
		t.Fatalf("process cancellation was not bounded: %v", err)
	}
}

func TestCodexAppServerImageRejectsArtifactEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(outside, "outside.png")
	data := projectIconTestImage(t, "png")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escaped.png")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, savedPath := range []string{path, link, root} {
		if _, err := codexAppServerImageResult("", savedPath, root); err == nil {
			t.Fatal("accepted artifact outside the isolated directory")
		}
	}
}

func TestCodexAppServerImageInvalidReferenceDoesNotStartCLI(t *testing.T) {
	previous := codexAppServerImageCommand
	codexAppServerImageCommand = func(context.Context, string, ...string) *exec.Cmd {
		t.Fatal("invalid reference started Codex")
		return nil
	}
	t.Cleanup(func() { codexAppServerImageCommand = previous })
	_, err := callCodexAppServerImageGeneration(context.Background(), "test", "data:text/plain;base64,aGVsbG8=", "1:1")
	if err == nil {
		t.Fatal("invalid reference was accepted")
	}
}

func TestCodexAppServerImageMissingCLI(t *testing.T) {
	t.Setenv("GLOWBOM_CODEX_BIN", filepath.Join(t.TempDir(), "missing-codex"))
	_, err := callCodexAppServerImageGeneration(context.Background(), "test", "", "1:1")
	var failure *codexAppServerImageFailure
	if !errors.As(err, &failure) || failure.Code != "cli" {
		t.Fatal(fmt.Sprintf("missing Codex did not report the executable requirement: %v", err))
	}
}
