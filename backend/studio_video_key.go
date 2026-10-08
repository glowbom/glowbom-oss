package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zalando/go-keyring"
	"net/http"
	"strings"
)

type studioVideoKeyStore interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}
type systemStudioVideoKeyStore struct{}

var studioProviderKeyringGet = keyring.Get
var studioProviderKeyringSet = keyring.Set
var studioProviderKeyringDelete = keyring.Delete

func studioVideoKeyAccount(source string) string {
	if source == "xai-api" {
		return "xAI"
	}
	if source == "veo-api" || source == "gemini-api" {
		return "Google Gemini"
	}
	if source == "openai-api" {
		return "OpenAI"
	}
	return ""
}
func (systemStudioVideoKeyStore) Get(source string) (string, error) {
	key, err := studioProviderKeyringGet("Glowbom Providers", studioVideoKeyAccount(source))
	if errors.Is(err, keyring.ErrNotFound) && source != "openai-api" {
		// Older releases stored these same API keys under the video service.
		key, err = studioProviderKeyringGet("Glowbom Video", studioVideoKeyAccount(source))
		if err == nil && key != "" {
			// Keep the legacy entry until the shared write succeeds.
			_ = studioProviderKeyringSet("Glowbom Providers", studioVideoKeyAccount(source), key)
		}
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return key, err
}
func (systemStudioVideoKeyStore) Set(source, key string) error {
	return studioProviderKeyringSet("Glowbom Providers", studioVideoKeyAccount(source), key)
}
func (systemStudioVideoKeyStore) Delete(source string) error {
	for _, service := range []string{"Glowbom Providers", "Glowbom Video"} {
		err := studioProviderKeyringDelete(service, studioVideoKeyAccount(source))
		if err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
	}
	return nil
}

var studioVideoKeys studioVideoKeyStore = systemStudioVideoKeyStore{}

func studioVideoKeyHandler(w http.ResponseWriter, r *http.Request) {
	studioProviderKeyHandler(w, r)
}

// Retain the injected video key interface while sharing its store with images.
func studioProviderKeyStoreSource(source string) string {
	if source == "gemini-api" {
		return "veo-api"
	}
	return source
}

func studioProviderKeyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !authorizeVoiceKey(w, r) {
		return
	}
	if !isAllowedOrigin(r, glowbomAllowedOrigins()) {
		http.Error(w, "This request is not allowed.", http.StatusForbidden)
		return
	}
	source := r.URL.Query().Get("sourceId")
	key := ""
	if r.Method == http.MethodPost {
		var request struct {
			SourceID string `json:"sourceId"`
			Key      string `json:"key"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&request) != nil || strings.TrimSpace(request.Key) == "" {
			http.Error(w, "Enter a provider API key.", http.StatusBadRequest)
			return
		}
		source = request.SourceID
		key = strings.TrimSpace(request.Key)
	}
	source = studioProviderKeyStoreSource(strings.TrimSpace(source))
	if studioVideoKeyAccount(source) == "" {
		http.Error(w, "Unsupported API key source.", http.StatusBadRequest)
		return
	}
	configured := false
	saved := false
	var err error
	switch r.Method {
	case http.MethodGet:
		key, err = studioVideoKeys.Get(source)
		saved = strings.TrimSpace(key) != ""
		configured = saved || studioVideoEnvironmentKey(source) != ""
	case http.MethodPost:
		err = studioVideoKeys.Set(source, key)
		configured = err == nil
		saved = configured
	case http.MethodDelete:
		err = studioVideoKeys.Delete(source)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, "Could not access the system credential store. Unlock it and try again.", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]bool{"configured": configured, "saved": saved})
}
func studioVideoEnvironmentKey(source string) string {
	if source == "veo-api" {
		return projectIconAPIKey("gemini-api")
	}
	if key := projectIconAPIKey(source); key != "" {
		return key
	}
	// Earlier Studio video jobs use the OpenCode runtime credential candidates.
	if source == "xai-api" {
		for _, path := range xAIMediaAuthFileCandidates() {
			credential, ok, err := readXAIStoredCredentialWithSubscription(path, false)
			if err == nil && ok && credential.Kind == "api-key" {
				return credential.Bearer
			}
		}
	}
	return ""
}

// HTTP callers authorize saved-key access before calling this helper.
func resolveStudioVideoKey(ctx context.Context, source, explicit string, useSaved bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if source == "xai-subscription" {
		if strings.TrimSpace(explicit) != "" {
			return "", errors.New("Subscription video uses the connected Grok account.")
		}
		return "", nil
	}
	return resolveStudioProviderKey(ctx, source, explicit, useSaved)
}

// HTTP callers authorize saved-key access before calling this helper.
func resolveStudioProviderKey(ctx context.Context, source, explicit string, useSaved bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source = studioProviderKeyStoreSource(source)
	if studioVideoKeyAccount(source) == "" {
		return "", errors.New("Unsupported API key source.")
	}
	if len(explicit) > 16384 {
		return "", errors.New("The API key is too long.")
	}
	if key := strings.TrimSpace(explicit); key != "" {
		return key, nil
	}
	if useSaved {
		key, err := studioVideoKeys.Get(source)
		if err != nil {
			return "", errors.New("Could not access the saved API key. Unlock your system credential store.")
		}
		if strings.TrimSpace(key) != "" {
			return strings.TrimSpace(key), nil
		}
	}
	if key := studioVideoEnvironmentKey(source); key != "" {
		return key, nil
	}
	return "", errors.New("Add a provider API key before generating.")
}
func studioVideoCapabilitiesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, studioVideoCapabilities())
}
