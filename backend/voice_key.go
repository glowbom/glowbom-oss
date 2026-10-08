package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zalando/go-keyring"
)

type voiceKeyStore interface {
	Get() (string, error)
	Set(string) error
	Delete() error
}
type systemVoiceKeyStore struct{}

func (systemVoiceKeyStore) Get() (string, error) {
	key, err := keyring.Get("Glowbom Voice", "ElevenLabs")
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return key, err
}
func (systemVoiceKeyStore) Set(key string) error {
	return keyring.Set("Glowbom Voice", "ElevenLabs", key)
}
func (systemVoiceKeyStore) Delete() error {
	err := keyring.Delete("Glowbom Voice", "ElevenLabs")
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func authorizeVoiceKey(w http.ResponseWriter, r *http.Request) bool {
	token := glowbomServerToken()
	if token == "" || !hasValidGlowbomServerToken(r, token) {
		http.Error(w, "Local authentication required.", http.StatusUnauthorized)
		return false
	}
	return true
}

func voiceKeyHandler(store voiceKeyStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !authorizeVoiceKey(w, r) {
			return
		}
		configured := false
		var err error
		switch r.Method {
		case http.MethodGet:
			var key string
			key, err = store.Get()
			configured = strings.TrimSpace(key) != ""
		case http.MethodPost:
			var request struct {
				Key string `json:"key"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&request) != nil || strings.TrimSpace(request.Key) == "" {
				http.Error(w, "Enter an ElevenLabs key.", http.StatusBadRequest)
				return
			}
			err = store.Set(strings.TrimSpace(request.Key))
			configured = err == nil
		case http.MethodDelete:
			err = store.Delete()
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err != nil {
			http.Error(w, "Could not access the system credential store. Unlock it and try again.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"configured": configured})
	}
}

func resolveVoiceKey(w http.ResponseWriter, r *http.Request, key string, useSaved bool, store voiceKeyStore) (string, bool) {
	if !useSaved {
		return strings.TrimSpace(key), true
	}
	if !authorizeVoiceKey(w, r) {
		return "", false
	}
	saved, err := store.Get()
	if err != nil {
		http.Error(w, "Could not access the saved voice key. Unlock your system credential store.", http.StatusServiceUnavailable)
		return "", false
	}
	if strings.TrimSpace(saved) == "" {
		http.Error(w, "Add your ElevenLabs key in Voice settings.", http.StatusBadRequest)
		return "", false
	}
	return strings.TrimSpace(saved), true
}
