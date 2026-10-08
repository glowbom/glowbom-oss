package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var dictationLock sync.Mutex

type dictationStatus struct {
	Engine bool `json:"engine"`
	Model  bool `json:"model"`
	Ready  bool `json:"ready"`
}

func dictationPaths() (string, string) {
	engine := os.Getenv("GLOWBOM_WHISPER_BIN")
	if engine == "" {
		for _, candidate := range []string{"whisper-cli", "/opt/homebrew/bin/whisper-cli", "/usr/local/bin/whisper-cli"} {
			if path, err := exec.LookPath(candidate); err == nil {
				engine = path
				break
			}
		}
	}
	if _, err := exec.LookPath(engine); err != nil {
		engine = ""
	}
	model := os.Getenv("GLOWBOM_WHISPER_MODEL")
	defaultModel := model == ""
	if model == "" {
		home, _ := os.UserHomeDir()
		model = filepath.Join(home, ".glowbom", "models", "whisper", "ggml-base.en.bin")
	}
	if info, err := os.Stat(model); err != nil || !info.Mode().IsRegular() || info.Size() < 1<<20 || (defaultModel && info.Size() != 147964211) {
		model = ""
	}
	return engine, model
}

func dictationAuthorized(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
		http.Error(w, "Local access is required. Reopen Glowbom.", http.StatusUnauthorized)
		return false
	}
	return true
}

func dictationSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if !dictationAuthorized(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method not allowed", 405)
		return
	}
	engine, model := dictationPaths()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dictationStatus{engine != "", model != "", engine != "" && model != ""})
}

// Accept only bounded mono PCM from the recorder, not arbitrary media containers.
func validateDictationWAV(data []byte) error {
	invalid := errors.New("Record between one second and one minute of audio, then try again.")
	if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" || int(binary.LittleEndian.Uint32(data[4:8])) != len(data)-8 {
		return invalid
	}
	format, samples := false, []byte(nil)
	for offset := 12; offset+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		if size > len(data)-start {
			return invalid
		}
		chunk := data[start : start+size]
		switch string(data[offset : offset+4]) {
		case "fmt ":
			if len(chunk) < 16 || binary.LittleEndian.Uint16(chunk) != 1 || binary.LittleEndian.Uint16(chunk[2:]) != 1 || binary.LittleEndian.Uint32(chunk[4:]) != 16000 || binary.LittleEndian.Uint16(chunk[12:]) != 2 || binary.LittleEndian.Uint16(chunk[14:]) != 16 {
				return invalid
			}
			format = true
		case "data":
			if samples != nil {
				return invalid
			}
			samples = chunk
		}
		offset = start + size + (size % 2)
	}
	if !format || len(samples) < 32000 || len(samples) > 1920000 || len(samples)%2 != 0 {
		return invalid
	}
	var energy float64
	for i := 0; i < len(samples); i += 2 {
		value := float64(int16(binary.LittleEndian.Uint16(samples[i:]))) / 32768
		energy += value * value
	}
	if energy/float64(len(samples)/2) < 0.000001 {
		return errors.New("No speech was heard. Check your microphone and try again.")
	}
	return nil
}

func dictationTranscribeHandler(w http.ResponseWriter, r *http.Request) {
	if !dictationAuthorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", 405)
		return
	}
	if !dictationLock.TryLock() {
		http.Error(w, "Another recording is being transcribed. Try again shortly.", 409)
		return
	}
	defer dictationLock.Unlock()
	engine, model := dictationPaths()
	if engine == "" || model == "" {
		http.Error(w, "Set up local dictation in Local AI first.", 503)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1924096))
	if err != nil {
		http.Error(w, "Recording is too large. Keep it under one minute.", 413)
		return
	}
	if err = validateDictationWAV(data); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	dir, err := os.MkdirTemp("", "glowbom-dictation-")
	if err != nil {
		http.Error(w, "Could not prepare local dictation.", 500)
		return
	}
	defer os.RemoveAll(dir)
	input, output := filepath.Join(dir, "recording.wav"), filepath.Join(dir, "transcript")
	if err = os.WriteFile(input, data, 0600); err != nil {
		http.Error(w, "Could not prepare recording.", 500)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	// Arguments are fixed; audio and model paths never pass through a shell.
	command := exec.CommandContext(ctx, engine, "-m", model, "-f", input, "-otxt", "-of", output, "-nt", "-l", "en", "-t", "4")
	if err = command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			http.Error(w, "Local transcription timed out. Retry or record a shorter message.", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "Could not transcribe locally. Check your Whisper model and try a shorter recording.", 502)
		return
	}
	file, err := os.Open(output + ".txt")
	if err != nil {
		http.Error(w, "Whisper did not return a transcript. Try again.", 502)
		return
	}
	defer file.Close()
	transcript, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(transcript) > 65536 {
		http.Error(w, "Could not read the transcript.", 502)
		return
	}
	result := strings.TrimSpace(string(transcript))
	if result == "" || result == "[BLANK_AUDIO]" {
		http.Error(w, "No speech was heard. Try again closer to the microphone.", 422)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"text": result})
}
