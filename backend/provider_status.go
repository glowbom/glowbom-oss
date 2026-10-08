package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

type providerConnectionStatus struct {
	OpenAI struct {
		CredentialType  string `json:"credentialType"`
		Connected       bool   `json:"connected"`
		Source          string `json:"source"`
		ModelsAvailable bool   `json:"modelsAvailable"`
	} `json:"openai"`
	ModelsChecked bool `json:"modelsChecked"`
}

func readProviderConnectionStatus(ctx context.Context, chat *chatService) providerConnectionStatus {
	status := providerConnectionStatus{}
	status.OpenAI.CredentialType = "unknown"
	status.OpenAI.Source = "opencode"
	if strings.TrimSpace(os.Getenv("OPENCODE_URL")) != "" {
		// A local credential file cannot identify an external server's account.
		status.OpenAI.Source = "external"
	} else {
		// Chat uses the user's normal OpenCode configuration. The legacy workspace
		// has a separate credential store that must not imply a ChatGPT connection.
		if paths, err := userOpenCodeRuntimePaths(); err == nil {
			status.OpenAI.CredentialType = openAICredentialTypeFromAuthFile(paths.AuthFile)
		}
		state := getOpenCodeOpenAIAuthState()
		if state.known && state.mode != "opencode-config" {
			return status
		}
	}

	var catalog struct {
		Connected []string `json:"connected"`
		All       []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				Status string `json:"status"`
			} `json:"models"`
		} `json:"all"`
	}
	// Only inspect the currently running server. Checking this panel must never
	// start OpenCode, refresh an instance, or send a message to an AI provider.
	if err := chat.json(ctx, http.MethodGet, "/provider", nil, &catalog); err != nil || catalog.Connected == nil || catalog.All == nil {
		return status
	}
	status.ModelsChecked = true
	for _, id := range catalog.Connected {
		if id == "openai" {
			status.OpenAI.Connected = true
			break
		}
	}
	if status.OpenAI.Connected {
		for _, provider := range catalog.All {
			if provider.ID != "openai" {
				continue
			}
			for id, model := range provider.Models {
				if strings.TrimSpace(id) != "" && model.Status != "deprecated" {
					status.OpenAI.ModelsAvailable = true
					break
				}
			}
		}
	}
	return status
}

func providerStatusHandler(chat *chatService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			writeJSON(w, map[string]string{"error": "Use GET to check provider connections."})
			return
		}
		if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]string{"error": "Local authentication required. Reopen Glowbom and try again."})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		writeJSON(w, readProviderConnectionStatus(ctx, chat))
	}
}
