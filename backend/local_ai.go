package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A separate provider keeps the user's own Ollama endpoints and settings intact.
const localAIProvider = "glowbom-ollama"

type localAIModel struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Connected   bool   `json:"connected"`
	RemoteHost  string `json:"remote_host,omitempty"`
	RemoteModel string `json:"remote_model,omitempty"`
}
type localAIStatus struct {
	Installed bool           `json:"installed"`
	Running   bool           `json:"running"`
	OpenCode  bool           `json:"opencode"`
	Managed   bool           `json:"managed"`
	Models    []localAIModel `json:"models"`
}
type localAIState map[string]map[string]any
type localAIService struct {
	client   *http.Client
	endpoint string
	chat     *chatService
	reload   func() error
	mu       sync.Mutex
}

func newLocalAIService(chat *chatService) *localAIService {
	return &localAIService{chat: chat, endpoint: "http://127.0.0.1:11434", client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, reload: func() error {
		openCodeServerPrepMu.Lock()
		defer openCodeServerPrepMu.Unlock()
		return restartOpenCodeServer("", "", "", "", "", "", "", "", "opencode-config")
	}}
}
func localAIStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".glowbom", "local-ai.json"), nil
}
func readLocalAIState() (localAIState, error) {
	path, err := localAIStatePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return localAIState{}, nil
	}
	if err != nil {
		return nil, err
	}
	state := localAIState{}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state == nil {
		return nil, errors.New("invalid local AI settings")
	}
	return state, nil
}
func saveLocalAIState(state localAIState) error {
	path, err := localAIStatePath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".local-ai-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func addLocalAIOpenCodeConfig(env []string) ([]string, error) {
	state, err := readLocalAIState()
	if err != nil {
		return nil, err
	}
	if len(state) == 0 {
		return env, nil
	}
	config := map[string]any{}
	if raw := envValue(env, "OPENCODE_CONFIG_CONTENT"); strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &config); err != nil || config == nil {
			return nil, errors.New("invalid inline OpenCode configuration")
		}
	}
	mergeJSONObject(config, map[string]any{"provider": map[string]any{localAIProvider: map[string]any{
		"npm": "@ai-sdk/openai-compatible", "name": "Ollama (this computer)",
		"options": map[string]any{"baseURL": "http://127.0.0.1:11434/v1"}, "models": map[string]map[string]any(state),
	}}})
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return setEnvValue(env, "OPENCODE_CONFIG_CONTENT", string(data)), nil
}
func (s *localAIService) ollama(ctx context.Context, path string, body, result any) error {
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		method = http.MethodPost
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("Ollama request failed")
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(result)
}
func (s *localAIService) status(ctx context.Context) (localAIStatus, error) {
	_, installed := appleIntelligenceBinary("ollama")
	_, opencode := openCodeExecutable()
	status := localAIStatus{Installed: installed == nil, OpenCode: opencode == nil, Managed: os.Getenv("OPENCODE_URL") == "", Models: []localAIModel{}}
	state, err := readLocalAIState()
	if err != nil {
		return status, err
	}
	var tags struct {
		Models []localAIModel `json:"models"`
	}
	if s.ollama(ctx, "/api/tags", nil, &tags) != nil {
		return status, nil
	}
	status.Installed, status.Running = true, true
	for _, model := range tags.Models {
		if model.Name == "" || model.RemoteHost != "" || model.RemoteModel != "" {
			continue
		}
		model.Connected = state[model.Name] != nil
		status.Models = append(status.Models, model)
	}
	sort.Slice(status.Models, func(i, j int) bool { return status.Models[i].Name < status.Models[j].Name })
	return status, nil
}
func localAIModelConfig(name string, capabilities []string) (map[string]any, error) {
	completion, vision, tools := false, false, false
	for _, c := range capabilities {
		switch c {
		case "completion":
			completion = true
		case "vision":
			vision = true
		case "tools":
			tools = true
		}
	}
	if !completion {
		return nil, errors.New("This model cannot reply in Chat. Choose a conversation model.")
	}
	inputs := []string{"text"}
	if vision {
		inputs = append(inputs, "image")
	}
	label := name
	if name == "maternion/mimo-v2.6:9b" {
		label = "MiMo V2.6 9B"
	}
	return map[string]any{"name": label, "attachment": vision, "tool_call": tools, "modalities": map[string]any{"input": inputs, "output": []string{"text"}}}, nil
}
func (s *localAIService) connect(ctx context.Context, name string) error {
	status, err := s.status(ctx)
	if err != nil {
		return errors.New("Could not read local AI settings. Check access to your Glowbom settings folder.")
	}
	if !status.Managed {
		return errors.New("This Glowbom uses an external OpenCode server. Add Ollama in that server's configuration using the setup guide.")
	}
	if !status.OpenCode {
		return errors.New("Install OpenCode in Tools, then try again.")
	}
	if !status.Running {
		return errors.New("Open Ollama on this computer, then try again.")
	}
	found := false
	for _, model := range status.Models {
		if model.Name == name {
			found = true
		}
	}
	if !found {
		return errors.New("This model is no longer installed. Refresh the list and choose a downloaded model.")
	}
	var details struct {
		Capabilities []string `json:"capabilities"`
		RemoteHost   string   `json:"remote_host"`
		RemoteModel  string   `json:"remote_model"`
	}
	if s.ollama(ctx, "/api/show", map[string]string{"model": name}, &details) != nil {
		return errors.New("Could not inspect this model. Check Ollama and try again.")
	}
	if details.RemoteHost != "" || details.RemoteModel != "" {
		return errors.New("This model runs on a remote service. Choose a model downloaded to this computer.")
	}
	config, err := localAIModelConfig(name, details.Capabilities)
	if err != nil {
		return err
	}
	if err := s.chat.prepare(); err != nil {
		return errors.New("OpenCode could not start. Check its installation in Tools, then try again.")
	}
	state, err := readLocalAIState()
	if err != nil {
		return errors.New("Could not read local AI settings.")
	}
	if state[name] == nil {
		var sessions map[string]struct {
			Type string `json:"type"`
		}
		if s.chat.json(ctx, "GET", "/session/status", nil, &sessions) != nil {
			return errors.New("Could not check whether OpenCode is busy. Try again when your current work is finished.")
		}
		for _, session := range sessions {
			if session.Type != "idle" {
				return errors.New("Finish the current chat or build before connecting a model.")
			}
		}
		state[name] = config
		if err := saveLocalAIState(state); err != nil {
			return errors.New("Could not save the connection. Check access to your Glowbom settings folder.")
		}
		err = s.reload()
		if err != nil {
			return errors.New("Connection saved, but OpenCode could not reload. Restart Glowbom, then test again.")
		}
	}
	models, err := s.chat.models(ctx)
	if err != nil {
		return errors.New("Connection saved, but the model list could not refresh. Restart Glowbom, then test again.")
	}
	id := localAIProvider + "/" + name
	found = false
	for _, model := range models {
		if model.ID == id {
			found = true
		}
	}
	if !found {
		return errors.New("OpenCode has not loaded this model yet. Restart Glowbom, then test again.")
	}
	var reply string
	if isManagedMiMoModel(id) {
		messages, _ := localChatConversation([]chatMessage{{Role: "user", Text: "Say hello in one short sentence."}}, "")
		reply, err = localMiMoReply(ctx, messages, func(string) {}, func(string) {})
	} else {
		reply, err = s.chat.complete(ctx, id, "Reply briefly to the greeting. Do not use tools.", []map[string]any{{"type": "text", "text": "Say hello in one short sentence."}}, func(string) {})
	}
	if err != nil || strings.TrimSpace(reply) == "" {
		return errors.New("Connected, but the test reply did not finish. Keep Ollama open and try again. Loading a model for the first time can take a minute.")
	}
	return nil
}
func (s *localAIService) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
		http.Error(w, "Local authentication required.", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	problem := func(code int, message string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if r.Method == http.MethodPost {
		var input struct {
			Model string `json:"model"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		if decoder.Decode(&input) != nil || strings.TrimSpace(input.Model) == "" {
			problem(400, "Choose a downloaded model.")
			return
		}
		if !s.mu.TryLock() {
			problem(409, "Another model is being connected. Wait for it to finish.")
			return
		}
		defer s.mu.Unlock()
		if err := s.connect(ctx, input.Model); err != nil {
			problem(503, err.Error())
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"model": localAIProvider + "/" + input.Model})
		return
	}
	status, err := s.status(ctx)
	if err != nil {
		problem(503, "Could not read local AI settings. Check access to your Glowbom settings folder.")
		return
	}
	_ = json.NewEncoder(w).Encode(status)
}
