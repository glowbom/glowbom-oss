package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"unicode"
	"unicode/utf8"
)

const acpMaxConfigBytes = 64 << 10

type acpProfile struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Model   string   `json:"model,omitempty"`
}

type acpSettings struct {
	Connections []acpProfile `json:"connections"`
}

var acpSettingsMu sync.Mutex

func acpConfigPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("GLOWBOM_ACP_CONFIG")); path != "" {
		return filepath.Abs(path)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "glowbom", "acp.json"), nil
}

func validACPProfileID(id string) bool {
	return id == "acp-1" || id == "acp-2" || id == "acp-3"
}

func validACPText(value string, limit int, required bool) bool {
	return len(value) <= limit && utf8.ValidString(value) && (!required || strings.TrimSpace(value) != "") &&
		strings.IndexFunc(value, unicode.IsControl) == -1
}

func validateACPProfiles(profiles []acpProfile) error {
	if len(profiles) > 3 {
		return errors.New("Save up to three ACP connections.")
	}
	seen := make(map[string]bool)
	for _, profile := range profiles {
		if !validACPProfileID(profile.ID) || seen[profile.ID] {
			return errors.New("Choose a different ACP connection slot.")
		}
		seen[profile.ID] = true
		if !validACPText(profile.Name, 80, true) {
			return errors.New("Give each ACP connection a short name on one line.")
		}
		if !validACPText(profile.Command, 4096, true) {
			return errors.New("Enter a program name or path on one line.")
		}
		if profile.Model != "" && !validACPModelID(profile.Model) {
			return errors.New("Choose a valid ACP model on one line.")
		}
		if len(profile.Args) > 32 {
			return errors.New("Use up to 32 arguments for an ACP connection.")
		}
		total := 0
		for _, arg := range profile.Args {
			total += len(arg)
			if !validACPText(arg, 4096, false) || total > 16<<10 {
				return errors.New("ACP arguments must have no control characters and fit within the connection size limit.")
			}
		}
	}
	data, err := json.MarshalIndent(acpSettings{Connections: normalizedACPProfiles(profiles)}, "", "  ")
	if err != nil || len(data)+1 > acpMaxConfigBytes {
		return errors.New("The ACP connections exceed the saved settings size limit.")
	}
	return nil
}

func decodeACPSettings(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("unexpected data after ACP settings")
	}
	return nil
}

func loadACPProfiles() ([]acpProfile, error) {
	acpSettingsMu.Lock()
	defer acpSettingsMu.Unlock()
	path, err := acpConfigPath()
	if err != nil {
		return nil, errors.New("Could not find the ACP settings folder.")
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return []acpProfile{}, nil
	}
	if err != nil {
		return nil, errors.New("Could not read the saved ACP connections.")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > acpMaxConfigBytes {
		return nil, errors.New("Could not read the saved ACP connections.")
	}
	data, err := io.ReadAll(io.LimitReader(file, acpMaxConfigBytes+1))
	if err != nil || len(data) > acpMaxConfigBytes {
		return nil, errors.New("Could not read the saved ACP connections.")
	}
	var settings acpSettings
	if decodeACPSettings(strings.NewReader(string(data)), &settings) != nil || settings.Connections == nil || validateACPProfiles(settings.Connections) != nil {
		return nil, errors.New("The saved ACP connections are invalid. Check the local ACP settings file.")
	}
	return normalizedACPProfiles(settings.Connections), nil
}

func normalizedACPProfiles(profiles []acpProfile) []acpProfile {
	result := make([]acpProfile, len(profiles))
	copy(result, profiles)
	for index := range result {
		result[index].Args = append([]string{}, result[index].Args...)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func saveACPProfiles(profiles []acpProfile) error {
	if err := validateACPProfiles(profiles); err != nil {
		return err
	}
	acpSettingsMu.Lock()
	defer acpSettingsMu.Unlock()
	path, err := acpConfigPath()
	if err != nil {
		return errors.New("Could not find the ACP settings folder.")
	}
	data, err := json.MarshalIndent(acpSettings{Connections: normalizedACPProfiles(profiles)}, "", "  ")
	if err != nil || len(data)+1 > acpMaxConfigBytes {
		return errors.New("The saved ACP connections exceed the size limit. Shorten their arguments.")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("Could not create the ACP settings folder.")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".acp-*.json")
	if err != nil {
		return errors.New("Could not save the ACP connections.")
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil || os.Rename(temporary, path) != nil {
		return errors.New("Could not save the ACP connections.")
	}
	return nil
}

func resolveACPProfile(id string) (acpProfile, error) {
	if !validACPProfileID(id) {
		return acpProfile{}, errors.New("Choose a saved ACP connection in Tools.")
	}
	profiles, err := loadACPProfiles()
	if err != nil {
		return acpProfile{}, err
	}
	for _, profile := range profiles {
		if profile.ID == id {
			return profile, nil
		}
	}
	return acpProfile{}, errors.New("This ACP connection is no longer saved. Choose a connection in Tools.")
}

func acpProfileFingerprint(profile acpProfile) string {
	profile.Args = append([]string{}, profile.Args...)
	data, _ := json.Marshal(profile)
	fingerprint := sha256.Sum256(data)
	return hex.EncodeToString(fingerprint[:])
}

func acpSettingsAccess(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if !isAllowedOrigin(r, glowbomAllowedOrigins()) {
		writeACPSettingsError(w, http.StatusForbidden, "Origin not allowed.")
		return false
	}
	if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
		writeACPSettingsError(w, http.StatusUnauthorized, "Local authentication required.")
		return false
	}
	return true
}

func writeACPSettingsError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func acpSettingsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !acpSettingsAccess(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			profiles, err := loadACPProfiles()
			if err != nil {
				writeACPSettingsError(w, http.StatusInternalServerError, err.Error())
				return
			}
			_ = json.NewEncoder(w).Encode(acpSettings{Connections: profiles})
		case http.MethodPut:
			var settings acpSettings
			if decodeACPSettings(http.MaxBytesReader(w, r.Body, acpMaxConfigBytes), &settings) != nil || settings.Connections == nil {
				writeACPSettingsError(w, http.StatusBadRequest, "Enter valid ACP connection settings.")
				return
			}
			if err := validateACPProfiles(settings.Connections); err != nil {
				writeACPSettingsError(w, http.StatusBadRequest, err.Error())
				return
			}
			if err := saveACPProfiles(settings.Connections); err != nil {
				writeACPSettingsError(w, http.StatusInternalServerError, err.Error())
				return
			}
			_ = json.NewEncoder(w).Encode(acpSettings{Connections: normalizedACPProfiles(settings.Connections)})
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeACPSettingsError(w, http.StatusMethodNotAllowed, "Method not allowed.")
		}
	}
}

func acpTestHandler() http.HandlerFunc {
	return acpTestHandlerWithProbe(probeACP)
}

func acpTestHandlerWithProbe(probe func(context.Context, acpProfile) (acpProbeResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !acpSettingsAccess(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeACPSettingsError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		var body struct {
			Connection acpProfile `json:"connection"`
		}
		if decodeACPSettings(http.MaxBytesReader(w, r.Body, acpMaxConfigBytes), &body) != nil {
			writeACPSettingsError(w, http.StatusBadRequest, "Enter a valid ACP connection to test.")
			return
		}
		if err := validateACPProfiles([]acpProfile{body.Connection}); err != nil {
			writeACPSettingsError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		result, err := probe(ctx, body.Connection)
		if err != nil {
			// Process diagnostics and command arguments can contain credentials.
			writeACPSettingsError(w, http.StatusServiceUnavailable, "Could not connect. Check the executable, ACP arguments, and the agent's sign-in, then try again.")
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}
}
