package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	appleIntelligencePort     = "11435"
	appleIntelligenceProvider = "apple-intelligence"
	appleIntelligenceModel    = "apple-foundationmodel"
)

type appleIntelligenceStatus struct {
	Supported bool   `json:"supported"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Enabled   bool   `json:"enabled"`
	Reason    string `json:"reason"`
	Model     string `json:"model,omitempty"`
}

type appleIntelligenceSettingsDependencies struct {
	status  func(context.Context) appleIntelligenceStatus
	enable  func(context.Context) error
	disable func(context.Context) error
}

// appleIntelligenceProblem is a plain-language message that is safe to show in
// the settings dialog. Other errors stay in the server log.
type appleIntelligenceProblem struct{ message string }

func (problem appleIntelligenceProblem) Error() string { return problem.message }

func appleIntelligenceUserError(message string) error {
	return appleIntelligenceProblem{message: message}
}

// Glowbom runs the apfel server itself so the port stays under its control.
var appleIntelligenceBridge struct {
	sync.Mutex
	command *exec.Cmd
	done    chan struct{}
}

func appleIntelligenceConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", errors.New("could not find the home folder")
	}
	return filepath.Join(home, ".glowbom", "apple-intelligence.json"), nil
}

func appleIntelligenceEnabled() bool {
	path, err := appleIntelligenceConfigPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var state struct {
		Enabled bool `json:"enabled"`
	}
	return json.Unmarshal(data, &state) == nil && state.Enabled
}

func saveAppleIntelligenceEnabled(enabled bool) error {
	path, err := appleIntelligenceConfigPath()
	if err != nil {
		return err
	}
	if !enabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"enabled": true, "port": appleIntelligencePort})
	temporary, err := os.CreateTemp(filepath.Dir(path), ".apple-intelligence-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func appleIntelligenceOpenCodeConfig() map[string]any {
	return map[string]any{
		"provider": map[string]any{
			appleIntelligenceProvider: map[string]any{
				"name": "Apple Intelligence",
				"npm":  "@ai-sdk/openai-compatible",
				"options": map[string]any{
					"baseURL": "http://127.0.0.1:" + appleIntelligencePort + "/v1",
				},
				"models": map[string]any{
					appleIntelligenceModel: map[string]any{
						"name":      "Apple Intelligence (On-device)",
						"tool_call": true,
						"limit": map[string]any{
							"context": 4096,
							"output":  1024,
						},
					},
				},
			},
		},
	}
}

func mergeJSONObject(destination, source map[string]any) {
	for key, value := range source {
		sourceObject, sourceIsObject := value.(map[string]any)
		destinationObject, destinationIsObject := destination[key].(map[string]any)
		if sourceIsObject && destinationIsObject {
			mergeJSONObject(destinationObject, sourceObject)
			continue
		}
		destination[key] = value
	}
}

func addAppleIntelligenceOpenCodeConfig(env []string) ([]string, error) {
	if !appleIntelligenceEnabled() {
		return env, nil
	}
	config := map[string]any{}
	if existing := envValue(env, "OPENCODE_CONFIG_CONTENT"); strings.TrimSpace(existing) != "" {
		if err := json.Unmarshal([]byte(existing), &config); err != nil {
			return nil, errors.New("OPENCODE_CONFIG_CONTENT must be valid JSON before Glowbom can add Apple Intelligence")
		}
	}
	mergeJSONObject(config, appleIntelligenceOpenCodeConfig())
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return setEnvValue(env, "OPENCODE_CONFIG_CONTENT", string(data)), nil
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func appleIntelligencePlatformStatus() (bool, string) {
	if runtime.GOOS != "darwin" {
		return false, "Apple Intelligence requires an Apple Silicon Mac."
	}
	if runtime.GOARCH != "arm64" {
		return false, "Apple Intelligence requires an Apple Silicon Mac."
	}
	output, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return false, "Could not check this Mac's software version."
	}
	majorText := strings.Split(strings.TrimSpace(string(output)), ".")[0]
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 26 {
		return false, "Apple Intelligence chat requires macOS 26 or later."
	}
	return true, ""
}

func probeAppleIntelligence(ctx context.Context) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+appleIntelligencePort+"/v1/models", nil)
	if err != nil {
		return false
	}
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// The backend often starts from a launcher or an older terminal whose PATH does
// not include Homebrew, so check the standard install locations as well.
func appleIntelligenceBinary(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, path := range []string{"/opt/homebrew/bin/" + name, "/usr/local/bin/" + name} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}

// apfel only installs with the Apple Silicon build of Homebrew. Prefer it, and
// explain the Intel copy in /usr/local instead of failing inside brew.
func homebrewBinary() (string, error) {
	if runtime.GOARCH == "arm64" {
		if info, err := os.Stat("/opt/homebrew/bin/brew"); err == nil && !info.IsDir() {
			return "/opt/homebrew/bin/brew", nil
		}
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return "", appleIntelligenceUserError("Homebrew is needed to install the apfel bridge. Install it from brew.sh, then try again.")
	}
	if runtime.GOARCH == "arm64" && strings.HasPrefix(brew, "/usr/local/") {
		return "", appleIntelligenceUserError("This Mac has the Intel version of Homebrew, which cannot install apfel. Install the Apple Silicon version from brew.sh, then try again.")
	}
	return brew, nil
}

func currentAppleIntelligenceStatus(ctx context.Context) appleIntelligenceStatus {
	supported, reason := appleIntelligencePlatformStatus()
	_, lookupErr := appleIntelligenceBinary("apfel")
	status := appleIntelligenceStatus{
		Supported: supported,
		Installed: lookupErr == nil,
		Enabled:   appleIntelligenceEnabled(),
		Reason:    reason,
		Model:     appleIntelligenceProvider + "/" + appleIntelligenceModel,
	}
	if !supported {
		return status
	}
	status.Running = status.Installed && probeAppleIntelligence(ctx)
	switch {
	case status.Enabled && status.Running:
		status.Reason = "Ready for Chat."
	case status.Enabled:
		status.Reason = "Apple Intelligence is turned on, but its local service is not responding. Turn it off and on again."
	case status.Installed && status.Running:
		status.Reason = "Available to add to Glowbom."
	case status.Installed:
		status.Reason = "Installed and ready to start."
	default:
		status.Reason = "Ready to install with Homebrew."
	}
	return status
}

func installAppleIntelligenceBridge(ctx context.Context) error {
	if _, err := appleIntelligenceBinary("apfel"); err == nil {
		return nil
	}
	brew, err := homebrewBinary()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, brew, "install", "apfel")
	command.Env = setEnvValue(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE", "1")
	output, err := command.CombinedOutput()
	if err != nil {
		log.Printf("[APPLE-INTELLIGENCE] brew install apfel failed: %s", strings.TrimSpace(string(output)))
		return appleIntelligenceUserError("Homebrew could not install apfel. Open Terminal, run brew install apfel, and fix what it reports. Then try again.")
	}
	if _, err := appleIntelligenceBinary("apfel"); err != nil {
		return appleIntelligenceUserError("Homebrew finished, but the apfel command was not found. Restart Glowbom from a new Terminal window and try again.")
	}
	return nil
}

// startAppleIntelligenceBridge runs apfel on Glowbom's loopback port and waits
// until it answers. It reuses a bridge that is already running.
func startAppleIntelligenceBridge(ctx context.Context) error {
	if probeAppleIntelligence(ctx) {
		return nil
	}
	apfel, err := appleIntelligenceBinary("apfel")
	if err != nil {
		return appleIntelligenceUserError("The apfel bridge is not installed. Turn the setting off and on again to install it.")
	}
	appleIntelligenceBridge.Lock()
	done := appleIntelligenceBridge.done
	if appleIntelligenceBridge.command == nil {
		command := exec.Command(apfel, "--serve", "--host", "127.0.0.1", "--port", appleIntelligencePort)
		reader, writer := io.Pipe()
		command.Stdout = writer
		command.Stderr = writer
		if err := command.Start(); err != nil {
			appleIntelligenceBridge.Unlock()
			log.Printf("[APPLE-INTELLIGENCE] could not start apfel: %v", err)
			return appleIntelligenceUserError("The apfel bridge could not start. Reinstall it with brew reinstall apfel, then try again.")
		}
		done = make(chan struct{})
		appleIntelligenceBridge.command = command
		appleIntelligenceBridge.done = done
		go func() {
			scanner := bufio.NewScanner(reader)
			for scanner.Scan() {
				log.Printf("[APFEL] %s", scanner.Text())
			}
		}()
		go func() {
			err := command.Wait()
			_ = writer.Close()
			appleIntelligenceBridge.Lock()
			if appleIntelligenceBridge.command == command {
				appleIntelligenceBridge.command = nil
				appleIntelligenceBridge.done = nil
			}
			appleIntelligenceBridge.Unlock()
			close(done)
			if err != nil {
				log.Printf("[APPLE-INTELLIGENCE] apfel stopped: %v", err)
			}
		}()
	}
	appleIntelligenceBridge.Unlock()

	deadline := time.Now().Add(15 * time.Second)
	for !probeAppleIntelligence(ctx) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-done:
			return appleIntelligenceUserError("The apfel bridge stopped right after starting. Make sure Apple Intelligence is turned on in System Settings, then try again.")
		default:
		}
		if time.Now().After(deadline) {
			return appleIntelligenceUserError("The apfel bridge did not respond. Make sure Apple Intelligence is turned on in System Settings and the model has finished downloading, then try again.")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

func stopAppleIntelligenceBridge() {
	appleIntelligenceBridge.Lock()
	command := appleIntelligenceBridge.command
	appleIntelligenceBridge.command = nil
	appleIntelligenceBridge.done = nil
	appleIntelligenceBridge.Unlock()
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
}

// Keep the bridge available across backend restarts when the setting is on.
func ensureAppleIntelligenceBridge() {
	if !appleIntelligenceEnabled() {
		return
	}
	go func() {
		if err := startAppleIntelligenceBridge(context.Background()); err != nil {
			log.Printf("[APPLE-INTELLIGENCE] %v", err)
		}
	}()
}

func setupAppleIntelligence(ctx context.Context) error {
	if supported, reason := appleIntelligencePlatformStatus(); !supported {
		return appleIntelligenceUserError(reason)
	}
	if err := installAppleIntelligenceBridge(ctx); err != nil {
		return err
	}
	if err := startAppleIntelligenceBridge(ctx); err != nil {
		return err
	}
	if err := saveAppleIntelligenceEnabled(true); err != nil {
		return err
	}
	if err := restartOpenCodeServer("", "", "", "", "", "", "", "", "opencode-config"); err != nil {
		_ = saveAppleIntelligenceEnabled(false)
		log.Printf("[APPLE-INTELLIGENCE] OpenCode reload failed: %v", err)
		return appleIntelligenceUserError("Apple Intelligence is ready, but OpenCode could not reload. Restart Glowbom and try again.")
	}
	return nil
}

func disableAppleIntelligence(ctx context.Context) error {
	if err := saveAppleIntelligenceEnabled(false); err != nil {
		return err
	}
	stopAppleIntelligenceBridge()
	return restartOpenCodeServer("", "", "", "", "", "", "", "", "opencode-config")
}

func appleIntelligenceSettingsHandler() http.HandlerFunc {
	return appleIntelligenceSettingsHandlerWithDependencies(appleIntelligenceSettingsDependencies{
		status:  currentAppleIntelligenceStatus,
		enable:  setupAppleIntelligence,
		disable: disableAppleIntelligence,
	})
}

func appleIntelligenceSettingsHandlerWithDependencies(dependencies appleIntelligenceSettingsDependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
			http.Error(w, "Local authentication required.", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		var err error
		switch r.Method {
		case http.MethodPost:
			err = dependencies.enable(ctx)
		case http.MethodDelete:
			err = dependencies.disable(ctx)
		}
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			message := "Could not set up Apple Intelligence. Check macOS, Apple Intelligence, and Homebrew, then try again."
			if r.Method == http.MethodDelete {
				message = "Could not remove Apple Intelligence from Glowbom. Restart Glowbom and try again."
			}
			var problem appleIntelligenceProblem
			if errors.As(err, &problem) {
				message = problem.message
			} else {
				log.Printf("[APPLE-INTELLIGENCE] %s failed: %v", r.Method, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
			return
		}
		_ = json.NewEncoder(w).Encode(dependencies.status(ctx))
	}
}
