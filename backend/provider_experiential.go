package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

const experientialBaseURL = "https://api.experientiallabs.ai/v1"

// Never forward the credential to a redirected catalog endpoint.
var experientialCatalogClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
}

// Read the documented catalog instead of guessing model IDs or context limits.
// Only register text models with tool support for Glowbom's chat and build loop.
func experientialProviderConfig(ctx context.Context, client *http.Client, key string) (map[string]any, error) {
	models := map[string]any{}
	for offset := 0; offset < 10000; {
		endpoint := fmt.Sprintf("https://api.experientiallabs.ai/api/models?sort=preferred&limit=100&offset=%d", offset)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, errors.New("could not request model catalog")
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			return nil, errors.New("could not reach model catalog")
		}
		var catalog struct {
			Models []struct {
				Model struct {
					Slug             string   `json:"slug"`
					Name             string   `json:"display_name"`
					Status           string   `json:"status"`
					Context          int      `json:"context_window"`
					Output           int      `json:"max_output_tokens"`
					InputModalities  []string `json:"input_modalities"`
					OutputModalities []string `json:"output_modalities"`
					Supported        struct {
						Tools bool `json:"tools"`
					} `json:"supported_params"`
				} `json:"model"`
			} `json:"models"`
			Total *int `json:"total"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&catalog)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil || catalog.Total == nil {
			return nil, errors.New("model catalog unavailable")
		}
		for _, entry := range catalog.Models {
			m := entry.Model
			if m.Status != "active" || !m.Supported.Tools || m.Context <= 0 || m.Output <= 0 || m.Slug == "" || strings.ContainsAny(m.Slug, "{}\r\n\x00") || !slices.Contains(m.InputModalities, "text") || !slices.Contains(m.OutputModalities, "text") {
				continue
			}
			name := m.Name
			if name == "" {
				name = m.Slug
			}
			input := []string{"text"}
			if slices.Contains(m.InputModalities, "image") {
				input = append(input, "image")
			}
			models[m.Slug] = map[string]any{
				"name": name, "tool_call": true,
				"limit":      map[string]int{"context": m.Context, "output": m.Output},
				"modalities": map[string]any{"input": input, "output": []string{"text"}},
			}
		}
		offset += len(catalog.Models)
		if offset >= *catalog.Total {
			if len(models) == 0 {
				return nil, errors.New("no coding models in catalog")
			}
			return map[string]any{"provider": map[string]any{"explabs": map[string]any{
				"npm": "@ai-sdk/openai-compatible", "name": "Experiential Labs",
				"options": map[string]string{"baseURL": experientialBaseURL}, "models": models,
			}}}, nil
		}
		if len(catalog.Models) == 0 {
			break
		}
	}
	return nil, errors.New("incomplete model catalog")
}
