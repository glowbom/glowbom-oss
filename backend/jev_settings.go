package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const jevDecisionInstruction = "Jev is enabled for this build. After inspecting the relevant project context and before editing files, call the installed jev tool once for a concrete decision between at least two reasonable next steps. Supply a concise summary of relevant evidence as state, a narrow question, and at least two key=description choices. Do not send whole files, credentials, or unrelated project content. Use the result to inform your next step, then continue the build. Make further Jev calls only when another narrow decision would benefit. Do not use Jev for code generation or open-ended reasoning. If the tool fails or is unavailable, continue the build yourself without installing tools or repeatedly retrying."
const jevEndpoint = "https://opencode.ai/zen/v1/systemone"

type jevAvailability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// Probe with fixed public text only. No project content or credentials are sent.
func probeJev(ctx context.Context, client *http.Client, endpoint string) bool {
	body := `{"model":"jev-1.13-free","state":"Availability check. The service received this request.","questions":{"decision":{"type":"choice","instructions":"Did you receive this request?","criteria":{"yes":"Request received","no":"Request not received"}}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return false
	}
	var result struct {
		Answers struct {
			Decision struct {
				Type   string `json:"type"`
				Choice string `json:"choice"`
			} `json:"decision"`
		} `json:"answers"`
	}
	return json.Unmarshal(data, &result) == nil && result.Answers.Decision.Type == "choice" && result.Answers.Decision.Choice == "yes"
}

func jevEndpointAvailable(ctx context.Context) bool {
	return probeJev(ctx, &http.Client{Timeout: 4 * time.Second}, jevEndpoint)
}

func jevStatusHandler() http.HandlerFunc {
	return jevSettingsHandler(jevEndpointAvailable, ensureJevInstalled)
}

func jevSettingsHandler(probe func(context.Context) bool, install func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", 405)
			return
		}
		if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
			http.Error(w, "Local authentication required.", 401)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		status := jevAvailability{Available: probe(ctx), Reason: "Available"}
		if !status.Available {
			status.Reason = "Jev is unavailable right now. Try again later."
		}
		if r.Method == http.MethodPost {
			if !status.Available {
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(map[string]string{"error": status.Reason})
				return
			}
			if err := install(); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": "Could not set up Jev. Try again or use the setup guide."})
				return
			}
		}
		json.NewEncoder(w).Encode(status)
	}
}

func jevBuildPrompt(prompt string, enabled, available bool) string {
	if enabled && available {
		return prompt + "\n\n" + jevDecisionInstruction
	}
	if enabled {
		return prompt + "\n\nJev is unavailable for this build. Continue without calling or installing Jev."
	}
	return prompt + "\n\nDo not carry forward an earlier request to use Jev. Use it only if the current user instructions explicitly request it."
}
