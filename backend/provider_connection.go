package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// New chat sessions use the user's OpenCode store. Keep that store as the source
// of truth across restarts; the legacy workspace keeps its separate OAuth flow.
func connectChatOpenAIOAuth(projectPath string, credential openCodeOpenAIOAuthCredential) error {
	if strings.TrimSpace(os.Getenv("OPENCODE_URL")) != "" {
		return errors.New("connect ChatGPT on the external OpenCode server")
	}
	if err := ensureOpenCodeServerReady(projectPath, "", "", "", "", "", "", "", "", "opencode-config", "", 0); err != nil {
		return err
	}
	serverURL := "http://" + openCodeServerHostname() + ":" + getAgentPort()
	if err := syncOpenAIAuth(serverURL, credential.AccessToken, credential.RefreshToken, credential.ExpiresAtReferenceSeconds, credential.AccountID); err != nil {
		return err
	}
	return persistChatOpenAIOAuth(credential)
}

func persistChatOpenAIOAuth(credential openCodeOpenAIOAuthCredential) error {
	paths, err := userOpenCodeRuntimePaths()
	if err != nil {
		return err
	}
	return persistOpenAIOAuthToAuthFile(paths.AuthFile, credential)
}

func providerRefreshHandler(chat *chatService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			writeJSON(w, map[string]string{"error": "Use POST to refresh providers."})
			return
		}
		if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]string{"error": "Local authentication required. Reopen Glowbom and try again."})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		var statuses map[string]struct {
			Type string `json:"type"`
		}
		if chat.prepare() != nil || chat.json(ctx, http.MethodGet, "/session/status", nil, &statuses) != nil || statuses == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(w, map[string]string{"error": "Could not refresh OpenCode. Check Tools in Settings, then try again."})
			return
		}
		for _, status := range statuses {
			if status.Type != "idle" {
				w.WriteHeader(http.StatusConflict)
				writeJSON(w, map[string]string{"error": "Wait for your current chat or build to finish, then refresh models."})
				return
			}
		}
		if chat.json(ctx, http.MethodPost, "/instance/dispose", nil, nil) != nil {
			w.WriteHeader(http.StatusBadGateway)
			writeJSON(w, map[string]string{"error": "Your connection is saved. Restart Glowbom to refresh its models."})
			return
		}
		writeJSON(w, map[string]bool{"refreshed": true})
	}
}

// OpenCode owns credential persistence. Never return keys or upstream error bodies.
func providerConnectionHandler(chat *chatService) http.HandlerFunc {
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		problem := func(code int, message string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		}
		if r.Method != http.MethodPost {
			problem(http.StatusMethodNotAllowed, "Use POST to connect a provider.")
			return
		}
		if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
			problem(http.StatusUnauthorized, "Local authentication required. Reopen Glowbom and try again.")
			return
		}
		if strings.TrimSpace(os.Getenv("OPENCODE_URL")) != "" {
			problem(http.StatusConflict, "Connect this provider on your external OpenCode server, then refresh models in Glowbom.")
			return
		}
		var input struct {
			Provider string `json:"provider"`
			APIKey   string `json:"apiKey"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		if decoder.Decode(&input) != nil {
			problem(http.StatusBadRequest, "Enter a provider and API key.")
			return
		}
		input.Provider, input.APIKey = strings.TrimSpace(input.Provider), strings.TrimSpace(input.APIKey)
		switch input.Provider {
		case "openai", "xai", "anthropic", "google", "openrouter", "fireworks-ai", "opencode", "opencode-go", "explabs":
		default:
			problem(http.StatusBadRequest, "Connect this provider in OpenCode, then refresh models in Glowbom.")
			return
		}
		if input.APIKey == "" || len(input.APIKey) > 8192 || strings.ContainsAny(input.APIKey, "\r\n\x00") {
			problem(http.StatusBadRequest, "Enter a valid API key on one line.")
			return
		}
		if !mu.TryLock() {
			problem(http.StatusConflict, "Another provider is being connected. Wait for it to finish.")
			return
		}
		defer mu.Unlock()
		if err := chat.prepare(); err != nil {
			problem(http.StatusServiceUnavailable, "OpenCode could not start. Check Tools in Settings, then try again.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		var statuses map[string]struct {
			Type string `json:"type"`
		}
		if err := chat.json(ctx, http.MethodGet, "/session/status", nil, &statuses); err != nil || statuses == nil {
			problem(http.StatusServiceUnavailable, "Could not check OpenCode. Try again when it is running.")
			return
		}
		for _, status := range statuses {
			if status.Type != "idle" {
				problem(http.StatusConflict, "Wait for your current chat or build to finish, then connect this provider.")
				return
			}
		}
		if input.Provider == "explabs" {
			config, err := experientialProviderConfig(ctx, experientialCatalogClient, input.APIKey)
			if err != nil {
				problem(http.StatusBadGateway, "Could not load Experiential Labs coding models. Check your key and connection, then try again.")
				return
			}
			// OpenCode merges and persists this provider without storing its key in config.
			if err := chat.json(ctx, http.MethodPatch, "/global/config", config, nil); err != nil {
				problem(http.StatusBadGateway, "OpenCode could not configure Experiential Labs. Update OpenCode in Settings, then try again.")
				return
			}
		}
		if err := chat.json(ctx, http.MethodPut, "/auth/"+input.Provider, map[string]string{"type": "api", "key": input.APIKey}, nil); err != nil {
			problem(http.StatusBadGateway, "OpenCode could not save this key. Check the connection and try again.")
			return
		}
		input.APIKey = ""
		// This only refreshes Glowbom's idle chat instance. Other project instances
		// load the new credential when next opened by OpenCode.
		refreshed := chat.json(ctx, http.MethodPost, "/instance/dispose", nil, nil) == nil
		writeJSON(w, map[string]any{"saved": true, "refreshed": refreshed, "provider": input.Provider})
	}
}
