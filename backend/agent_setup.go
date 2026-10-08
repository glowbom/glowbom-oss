package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/glowbom/glowbom-oss/cli/opencodecompat"
)

type codingAgentSpec struct {
	id      string
	name    string
	binary  string
	url     string
	manual  string
	windows string
	homes   []string
}

type codingAgentStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
}

type agentSetupStatus struct {
	Agents   []codingAgentStatus    `json:"agents"`
	OpenCode *openCodeRuntimeStatus `json:"opencode,omitempty"`
}

type openCodeRuntimeStatus struct {
	Selection  string `json:"selection"`
	Version    string `json:"version,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Executable string `json:"executable,omitempty"`
	Error      string `json:"error,omitempty"`
	Locked     bool   `json:"locked"`
	External   bool   `json:"external"`
}

type agentSetupProblem struct{ message string }

func (problem agentSetupProblem) Error() string { return problem.message }

func codingAgents() []codingAgentSpec {
	return []codingAgentSpec{
		{
			id: "opencode", name: "OpenCode", binary: "opencode",
			url:     "https://opencode.ai/install",
			manual:  "OpenCode could not be installed from here. Open Terminal and run: curl -fsSL https://opencode.ai/install | bash",
			windows: "Install OpenCode from opencode.ai, then choose Check again.",
			homes:   []string{filepath.Join(".opencode", "bin"), filepath.Join(".local", "bin")},
		},
		{
			id: "cursor", name: "Cursor", binary: "cursor-agent",
			url:     "https://cursor.com/install",
			manual:  "Cursor could not be installed from here. Open Terminal and run: curl https://cursor.com/install -fsS | bash",
			windows: "Install Cursor from cursor.com/docs/cli/installation, then choose Check again.",
			homes:   []string{filepath.Join(".local", "bin")},
		},
		{
			id: "claude-code", name: "Claude Code", binary: "claude",
			manual: "Install Claude Code from code.claude.com/docs/en/setup, then choose Check again.",
			homes:  []string{filepath.Join(".local", "bin")},
		},
		{
			id: "codex", name: "Codex", binary: "codex",
			manual: "Install Codex in Terminal with npm install -g @openai/codex, then choose Check again.",
			homes:  []string{filepath.Join(".local", "bin"), filepath.Join(".npm-global", "bin")},
		},
	}
}

func codingAgentByID(id string) (codingAgentSpec, bool) {
	for _, agent := range codingAgents() {
		if agent.id == id {
			return agent, true
		}
	}
	return codingAgentSpec{}, false
}

func openCodeCLIMissing(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "opencode cli not found")
}

func openCodeExecutable() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("GLOWBOM_OPENCODE_BIN")); configured != "" {
		return exec.LookPath(configured)
	}
	agent, _ := codingAgentByID("opencode")
	path, err := codingAgentExecutable(agent)
	if err == nil {
		return path, nil
	}
	agent.binary = "opencode2"
	return codingAgentExecutable(agent)
}

func currentAgentSetupStatus() agentSetupStatus {
	status := agentSetupStatus{Agents: make([]codingAgentStatus, 0, len(codingAgents()))}
	for _, agent := range codingAgents() {
		status.Agents = append(status.Agents, codingAgentStatus{
			ID:        agent.id,
			Name:      agent.name,
			Installed: codingAgentInstalled(agent),
		})
	}
	selection, err := opencodecompat.Selection()
	status.OpenCode = &openCodeRuntimeStatus{Selection: selection, Locked: os.Getenv("GLOWBOM_OPENCODE_VERSION") != "", External: strings.TrimSpace(os.Getenv("OPENCODE_URL")) != ""}
	if err != nil {
		status.OpenCode.Selection = "auto"
		status.OpenCode.Error = err.Error()
	} else if !status.OpenCode.External {
		value, err := opencodecompat.Resolve(context.Background())
		if err != nil {
			status.OpenCode.Error = err.Error()
		} else {
			status.OpenCode.Version, status.OpenCode.Protocol, status.OpenCode.Executable = value.Version, value.Protocol, value.Executable
		}
	}
	return status
}

func codingAgentInstalled(agent codingAgentSpec) bool {
	if agent.id == "claude-code" {
		_, err := claudeCodeExecutable()
		return err == nil
	}
	if agent.id == "codex" {
		_, err := codexExecutable()
		return err == nil
	}
	if agent.id == "opencode" && strings.TrimSpace(os.Getenv("OPENCODE_URL")) != "" {
		return true
	}
	_, err := codingAgentExecutable(agent)
	return err == nil
}

func codingAgentExecutable(agent codingAgentSpec) (string, error) {
	if agent.id == "cursor" {
		if path, err := cursorExecutable(); err == nil {
			return path, nil
		}
	}
	name := agent.binary
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	var homes []string
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, home)
	}
	candidates := []string{
		filepath.Join("/opt/homebrew/bin", name),
		filepath.Join("/usr/local/bin", name),
	}
	for _, home := range homes {
		for _, relative := range agent.homes {
			candidates = append(candidates, filepath.Join(home, relative, name))
		}
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err == nil && isRunnableFile(info) {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}

func isRunnableFile(info os.FileInfo) bool {
	if info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0111 != 0
}

func preferExecutable(path string) {
	dir := filepath.Dir(path)
	current := os.Getenv("PATH")
	parts := strings.Split(current, string(os.PathListSeparator))
	for _, part := range parts {
		if part == dir {
			return
		}
	}
	if current == "" {
		_ = os.Setenv("PATH", dir)
		return
	}
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+current)
}

func installCodingAgent(ctx context.Context, id string) error {
	agent, ok := codingAgentByID(id)
	if !ok {
		return agentSetupProblem{message: "Choose OpenCode, Cursor, Claude Code, or Codex."}
	}
	if codingAgentInstalled(agent) {
		return nil
	}
	if agent.url == "" {
		return agentSetupProblem{message: agent.manual}
	}
	if runtime.GOOS == "windows" {
		return agentSetupProblem{message: agent.windows}
	}
	if _, err := exec.LookPath("bash"); err != nil {
		return agentSetupProblem{message: agent.manual}
	}
	script, err := downloadInstallScript(ctx, agent.url)
	if err != nil {
		log.Printf("[AGENTS] download %s failed: %v", id, err)
		return agentSetupProblem{message: agent.manual}
	}
	defer os.Remove(script)
	command := exec.CommandContext(ctx, "bash", script)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		log.Printf("[AGENTS] install %s failed: %s", id, strings.TrimSpace(string(output)))
		return agentSetupProblem{message: agent.manual}
	}
	if !codingAgentInstalled(agent) {
		return agentSetupProblem{message: agent.manual}
	}
	if path, err := codingAgentExecutable(agent); err == nil {
		preferExecutable(path)
	}
	return nil
}

func downloadInstallScript(ctx context.Context, rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return "", errors.New("invalid installer")
	}
	if parsed.Host != "opencode.ai" && parsed.Host != "cursor.com" {
		return "", errors.New("invalid installer")
	}
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 3 || request.URL.Scheme != "https" || request.URL.Host != parsed.Host {
				return errors.New("installer redirect was not accepted")
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("installer download failed")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > 2<<20 {
		return "", errors.New("installer download was empty or too large")
	}
	file, err := os.CreateTemp("", "glowbom-agent-install-*.sh")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0600); err != nil {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func agentSetupHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
				http.Error(w, "Local authentication required.", http.StatusUnauthorized)
				return
			}
			var body struct {
				Version string `json:"version"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body) != nil {
				http.Error(w, "Choose an OpenCode version.", http.StatusBadRequest)
				return
			}
			if body.Version != "auto" && body.Version != "v1" && body.Version != "v2" {
				http.Error(w, "Choose auto, v1, or v2.", http.StatusBadRequest)
				return
			}
			if err := opencodecompat.SaveSelection(body.Version); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		} else if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeAgentSetup(w, r, nil)
	}
}

func agentSetupInstallHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
			http.Error(w, "Local authentication required.", http.StatusUnauthorized)
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
			http.Error(w, "Choose OpenCode, Cursor, Claude Code, or Codex.", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		writeAgentSetup(w, r, func() error { return installCodingAgent(ctx, body.ID) })
	}
}

func writeAgentSetup(w http.ResponseWriter, r *http.Request, action func() error) {
	if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
		http.Error(w, "Local authentication required.", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if action != nil {
		if err := action(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			message := "Could not install the coding agent. Choose Check again after installing it yourself."
			var problem agentSetupProblem
			if errors.As(err, &problem) {
				message = problem.message
			} else {
				log.Printf("[AGENTS] install failed: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
			return
		}
	}
	_ = json.NewEncoder(w).Encode(currentAgentSetupStatus())
}
