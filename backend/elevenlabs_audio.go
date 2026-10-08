package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	elevenLabsBaseURL           = "https://api.elevenlabs.io"
	defaultElevenVoiceID        = "JBFqnCBsd6RMkjVDRZzb"
	defaultElevenVoiceModel     = "eleven_multilingual_v2"
	defaultElevenOutputFormat   = "mp3_44100_128"
	defaultElevenSoundModel     = "eleven_text_to_sound_v2"
	defaultElevenMusicModel     = "music_v1"
	defaultElevenMusicDuration  = 30.0
	minElevenMusicDuration      = 3.0
	maxElevenMusicDuration      = 600.0
	minElevenSoundDuration      = 0.5
	maxElevenSoundDuration      = 30.0
	elevenLabsAudioMaxBytes     = 32 << 20
	elevenLabsGenerationTimeout = 10 * time.Minute
)

type ElevenLabsAudioRequest struct {
	UseSavedKey       bool     `json:"useSavedKey,omitempty"`
	Prompt            string   `json:"prompt"`
	AudioType         string   `json:"audioType"` // "voice" | "sound" | "music"
	ElevenLabsKey     string   `json:"elevenLabsKey,omitempty"`
	VoiceID           string   `json:"voiceId,omitempty"`
	VoiceModel        string   `json:"voiceModel,omitempty"`
	SoundModel        string   `json:"soundModel,omitempty"`
	MusicModel        string   `json:"musicModel,omitempty"`
	OutputFormat      string   `json:"outputFormat,omitempty"`
	DurationSeconds   float64  `json:"durationSeconds,omitempty"`
	PromptInfluence   *float64 `json:"promptInfluence,omitempty"`
	Loop              bool     `json:"loop,omitempty"`
	ForceInstrumental bool     `json:"forceInstrumental,omitempty"`
	Ephemeral         bool     `json:"ephemeral,omitempty"`
}

type ElevenLabsAudioResponse struct {
	Prompt    string `json:"prompt"`
	AudioType string `json:"audioType"`
	Filename  string `json:"filename"`
	SavedPath string `json:"saved_path"`
	Audio     string `json:"audio"`
	MimeType  string `json:"mimeType"`
}

type ElevenLabsVoicesRequest struct {
	UseSavedKey   bool   `json:"useSavedKey,omitempty"`
	ElevenLabsKey string `json:"elevenLabsKey"`
}

type ElevenLabsVoiceOption struct {
	VoiceID     string            `json:"voiceId"`
	Name        string            `json:"name"`
	Category    string            `json:"category,omitempty"`
	Description string            `json:"description,omitempty"`
	PreviewURL  string            `json:"previewUrl,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type ElevenLabsVoicesResponse struct {
	Voices []ElevenLabsVoiceOption `json:"voices"`
}

func listElevenLabsVoicesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ElevenLabsVoicesRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	apiKey, ok := resolveVoiceKey(w, r, req.ElevenLabsKey, req.UseSavedKey, systemVoiceKeyStore{})
	if !ok {
		return
	}
	if apiKey == "" {
		http.Error(w, "elevenLabsKey is required", http.StatusBadRequest)
		return
	}

	voices, err := fetchElevenLabsVoices(apiKey, r.Context())
	if err != nil {
		http.Error(w, "Could not load ElevenLabs voices. Check your key and connection.", http.StatusBadGateway)
		return
	}

	json.NewEncoder(w).Encode(ElevenLabsVoicesResponse{Voices: voices})
}

func generateAudioHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ElevenLabsAudioRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	var validationErr error
	req, validationErr = normalizeElevenLabsAudioRequest(req)
	if validationErr != nil {
		http.Error(w, validationErr.Error(), http.StatusBadRequest)
		return
	}
	audioType := req.AudioType
	apiKey, ok := resolveVoiceKey(w, r, req.ElevenLabsKey, req.UseSavedKey, systemVoiceKeyStore{})
	if !ok {
		return
	}
	if req.Ephemeral {
		log.Printf("[ELEVENLABS] generation request type=%s ephemeral=true prompt_len=%d", audioType, len(req.Prompt))
	} else {
		log.Printf("[ELEVENLABS] generation request type=%s prompt_len=%d", audioType, len(req.Prompt))
	}

	if req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}

	if apiKey == "" {
		http.Error(w, "elevenLabsKey is required", http.StatusBadRequest)
		return
	}

	var (
		audioBytes []byte
		mimeType   string
		err        error
	)

	switch audioType {
	case "voice":
		audioBytes, mimeType, err = callElevenLabsVoice(req, apiKey, r.Context())
	case "sound":
		audioBytes, mimeType, err = callElevenLabsSound(req, apiKey, r.Context())
	case "music":
		audioBytes, mimeType, err = callElevenLabsMusic(req, apiKey, r.Context())
	default:
		http.Error(w, "audioType must be one of: voice, sound, music", http.StatusBadRequest)
		return
	}

	if err != nil {
		log.Printf("[ELEVENLABS] generation failed type=%s err=%s", audioType, sanitizeElevenLabsError(err))
		http.Error(w, "ElevenLabs could not generate this audio. Check your key, options, and connection.", http.StatusBadGateway)
		return
	}

	filename := ""
	savedPath := ""
	if !req.Ephemeral {
		audioOutputDir := filepath.Join("saved_images", "audio")
		if err := os.MkdirAll(audioOutputDir, 0755); err != nil {
			fmt.Printf("[WARN] Failed creating %s folder: %v\n", audioOutputDir, err)
		}

		ext := extensionForAudioMimeType(mimeType)
		filename = fmt.Sprintf("%s_%d.%s", audioType, time.Now().UnixNano(), ext)
		savedPath = filepath.Join(audioOutputDir, filename)

		if err := os.WriteFile(savedPath, audioBytes, 0644); err != nil {
			msg := fmt.Sprintf("[ERROR] writing audio file: %v", err)
			fmt.Println(msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
	}

	resp := ElevenLabsAudioResponse{
		Prompt:    req.Prompt,
		AudioType: audioType,
		Filename:  filename,
		SavedPath: savedPath,
		Audio:     fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(audioBytes)),
		MimeType:  mimeType,
	}

	log.Printf("[ELEVENLABS] generation success type=%s bytes=%d mime=%s file=%s", audioType, len(audioBytes), mimeType, filename)
	json.NewEncoder(w).Encode(resp)
}

func callElevenLabsVoice(req ElevenLabsAudioRequest, apiKey string, contexts ...context.Context) ([]byte, string, error) {
	req.AudioType = "voice"
	var err error
	req, err = normalizeElevenLabsAudioRequest(req)
	if err != nil {
		return nil, "", err
	}
	voiceID := strings.TrimSpace(req.VoiceID)
	if voiceID == "" {
		voiceID = defaultElevenVoiceID
	}

	modelID := normalizeElevenLabsVoiceModel(req.VoiceModel)
	if modelID == "" {
		modelID = defaultElevenVoiceModel
	}

	params := neturl.Values{}
	outputFormat := strings.TrimSpace(req.OutputFormat)
	if outputFormat == "" {
		outputFormat = defaultElevenOutputFormat
	}
	params.Set("output_format", outputFormat)

	body := map[string]interface{}{
		"text":     req.Prompt,
		"model_id": modelID,
	}

	endpoint := fmt.Sprintf("%s/v1/text-to-speech/%s", elevenLabsBaseURL, neturl.PathEscape(voiceID))
	if modelID == "eleven_v4" {
		endpoint = elevenLabsBaseURL + "/v1/text-to-dialogue"
		body = map[string]interface{}{"model_id": modelID, "inputs": []map[string]string{{"text": req.Prompt, "voice_id": voiceID}}}
	}
	return callElevenLabsBinaryAPI(endpoint, params, body, apiKey, contexts...)
}

func callElevenLabsSound(req ElevenLabsAudioRequest, apiKey string, contexts ...context.Context) ([]byte, string, error) {
	req.AudioType = "sound"
	var err error
	req, err = normalizeElevenLabsAudioRequest(req)
	if err != nil {
		return nil, "", err
	}
	params := neturl.Values{}
	outputFormat := strings.TrimSpace(req.OutputFormat)
	if outputFormat == "" {
		outputFormat = defaultElevenOutputFormat
	}
	params.Set("output_format", outputFormat)

	body := map[string]interface{}{
		"text": req.Prompt,
	}
	if modelID := strings.TrimSpace(req.SoundModel); modelID != "" {
		body["model_id"] = modelID
	}

	if req.DurationSeconds > 0 {
		body["duration_seconds"] = req.DurationSeconds
	}
	if req.PromptInfluence != nil {
		body["prompt_influence"] = *req.PromptInfluence
	}
	if req.Loop {
		body["loop"] = true
	}

	return callElevenLabsBinaryAPI(elevenLabsBaseURL+"/v1/sound-generation", params, body, apiKey, contexts...)
}

func callElevenLabsMusic(req ElevenLabsAudioRequest, apiKey string, contexts ...context.Context) ([]byte, string, error) {
	req.AudioType = "music"
	var err error
	req, err = normalizeElevenLabsAudioRequest(req)
	if err != nil {
		return nil, "", err
	}
	params := neturl.Values{}
	outputFormat := strings.TrimSpace(req.OutputFormat)
	if outputFormat == "" {
		outputFormat = defaultElevenOutputFormat
	}
	params.Set("output_format", outputFormat)

	body := map[string]interface{}{
		"prompt": req.Prompt,
	}
	if modelID := strings.TrimSpace(req.MusicModel); modelID != "" {
		body["model_id"] = modelID
	}

	if req.DurationSeconds > 0 {
		body["music_length_ms"] = int(req.DurationSeconds * 1000.0)
	}
	if req.ForceInstrumental {
		body["force_instrumental"] = true
	}

	return callElevenLabsBinaryAPI(elevenLabsBaseURL+"/v1/music", params, body, apiKey, contexts...)
}

func callElevenLabsBinaryAPI(endpoint string, query neturl.Values, body map[string]interface{}, apiKey string, contexts ...context.Context) ([]byte, string, error) {
	if strings.TrimSpace(apiKey) == "" || len(apiKey) > 16384 {
		return nil, "", errors.New("Add a valid ElevenLabs key in Voice settings.")
	}
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), elevenLabsGenerationTimeout)
	defer cancel()
	url := endpoint
	if len(query) > 0 {
		url += "?" + query.Encode()
	}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("xi-api-key", apiKey)

	client := *http.DefaultClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("The audio provider redirected this generation request.")
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", errors.New("Could not reach ElevenLabs. Check your connection and try again.")
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, elevenLabsAudioMaxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("failed to read ElevenLabs response: %w", err)
	}

	if len(data) > elevenLabsAudioMaxBytes {
		return nil, "", errors.New("ElevenLabs returned audio larger than 32 MB.")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", elevenLabsResponseError(response.StatusCode, data)
	}
	if len(data) == 0 {
		return nil, "", errors.New("ElevenLabs returned empty audio.")
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json") {
		return nil, "", errors.New("ElevenLabs returned an unsupported audio response.")
	}

	mimeType := inferAudioMimeType(response.Header.Get("Content-Type"), data)
	if !strings.HasPrefix(mimeType, "audio/") {
		return nil, "", errors.New("ElevenLabs returned an unsupported audio response.")
	}
	log.Printf("[ELEVENLABS] success endpoint=%s status=%d bytes=%d mime=%s", endpoint, response.StatusCode, len(data), mimeType)
	return data, mimeType, nil
}

func fetchElevenLabsVoices(apiKey string, contexts ...context.Context) ([]ElevenLabsVoiceOption, error) {
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, elevenLabsBaseURL+"/v1/voices", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	request.Header.Set("xi-api-key", apiKey)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, errors.New("Could not reach ElevenLabs. Check your connection and try again.")
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read ElevenLabs response: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, elevenLabsResponseError(response.StatusCode, data)
	}

	if len(data) > 2<<20 {
		return nil, errors.New("ElevenLabs returned an oversized voice list.")
	}
	var decoded struct {
		Voices []struct {
			VoiceID     string            `json:"voice_id"`
			Name        string            `json:"name"`
			Category    string            `json:"category"`
			Description string            `json:"description"`
			PreviewURL  string            `json:"preview_url"`
			Labels      map[string]string `json:"labels"`
		} `json:"voices"`
	}

	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("failed to parse ElevenLabs response: %w", err)
	}

	voices := make([]ElevenLabsVoiceOption, 0, len(decoded.Voices))
	for _, v := range decoded.Voices {
		if strings.TrimSpace(v.VoiceID) == "" {
			continue
		}
		voices = append(voices, ElevenLabsVoiceOption{
			VoiceID:     v.VoiceID,
			Name:        v.Name,
			Category:    v.Category,
			Description: v.Description,
			PreviewURL:  v.PreviewURL,
			Labels:      v.Labels,
		})
	}

	sort.SliceStable(voices, func(i, j int) bool {
		left := strings.ToLower(strings.TrimSpace(voices[i].Name))
		right := strings.ToLower(strings.TrimSpace(voices[j].Name))
		if left == right {
			return voices[i].VoiceID < voices[j].VoiceID
		}
		return left < right
	})

	return voices, nil
}

// Normalize before any provider call, including legacy requests and approved run media.
// Music always has an explicit length so the provider cannot choose a costly song.
func normalizeElevenLabsAudioRequest(req ElevenLabsAudioRequest) (ElevenLabsAudioRequest, error) {
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.AudioType = normalizeAudioType(req.AudioType)
	if req.Prompt == "" {
		return req, errors.New("Enter text or a description for the audio.")
	}
	limit := 5000
	if req.AudioType == "music" {
		limit = 4100
	}
	if utf8.RuneCountInString(req.Prompt) > limit {
		return req, fmt.Errorf("Use an audio prompt up to %d characters.", limit)
	}
	if len(req.ElevenLabsKey) > 16384 {
		return req, errors.New("The ElevenLabs key is too long.")
	}
	if math.IsNaN(req.DurationSeconds) || math.IsInf(req.DurationSeconds, 0) || req.DurationSeconds < 0 {
		return req, errors.New("Choose a valid audio duration.")
	}
	if req.PromptInfluence != nil && (math.IsNaN(*req.PromptInfluence) || math.IsInf(*req.PromptInfluence, 0) || *req.PromptInfluence < 0 || *req.PromptInfluence > 1) {
		return req, errors.New("Prompt influence must be between 0 and 1.")
	}
	req.OutputFormat = strings.TrimSpace(req.OutputFormat)
	if req.OutputFormat == "" {
		req.OutputFormat = defaultElevenOutputFormat
	}
	if len(req.OutputFormat) > 128 {
		return req, errors.New("Choose a supported audio output format.")
	}
	req.VoiceID = strings.TrimSpace(req.VoiceID)
	if req.VoiceID == "" {
		req.VoiceID = defaultElevenVoiceID
	}
	req.VoiceModel = normalizeElevenLabsVoiceModel(req.VoiceModel)
	if req.VoiceModel == "" {
		req.VoiceModel = defaultElevenVoiceModel
	}
	req.SoundModel = strings.TrimSpace(req.SoundModel)
	if req.SoundModel == "" {
		req.SoundModel = defaultElevenSoundModel
	}
	req.MusicModel = strings.TrimSpace(req.MusicModel)
	if req.MusicModel == "" {
		req.MusicModel = defaultElevenMusicModel
	}
	if len(req.VoiceID) > 256 || len(req.VoiceModel) > 256 || len(req.SoundModel) > 256 || len(req.MusicModel) > 256 {
		return req, errors.New("The selected voice or model identifier is too long.")
	}
	for _, id := range []string{req.VoiceID, req.VoiceModel, req.SoundModel, req.MusicModel} {
		if !elevenLabsIdentifier.MatchString(id) {
			return req, errors.New("Use a voice or model identifier containing letters, numbers, dots, underscores, or hyphens.")
		}
	}
	switch req.AudioType {
	case "voice":
		if req.VoiceModel == "eleven_v4_turbo" {
			return req, errors.New("Eleven v4 Turbo requires the Text to Dialogue WebSocket API, which this audio flow does not yet support. Choose Eleven v4 or another voice model.")
		}
		if req.VoiceModel == "eleven_v4" && utf8.RuneCountInString(req.Prompt) > 2000 {
			return req, errors.New("Use up to 2,000 characters for reliable Eleven v4 dialogue generation.")
		}
	case "sound":
		if req.DurationSeconds != 0 && (req.DurationSeconds < minElevenSoundDuration || req.DurationSeconds > maxElevenSoundDuration) {
			return req, errors.New("Sound effect length must be between 0.5 and 30 seconds.")
		}
		if req.Loop && req.SoundModel != defaultElevenSoundModel {
			return req, errors.New("Looping sound effects require eleven_text_to_sound_v2.")
		}
	case "music":
		if req.DurationSeconds == 0 {
			req.DurationSeconds = defaultElevenMusicDuration
		}
		if req.DurationSeconds < minElevenMusicDuration || req.DurationSeconds > maxElevenMusicDuration {
			return req, errors.New("Music length must be between 3 and 600 seconds.")
		}
	default:
		return req, errors.New("Choose voice, sound effects, or music.")
	}
	return req, nil
}

// Keep only a recognized error code. Provider bodies can echo keys or private text.
func elevenLabsResponseError(status int, data []byte) error {
	var payload struct {
		Detail struct {
			Status string `json:"status"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(data, &payload)
	if payload.Detail.Status == "model_not_found" {
		return fmt.Errorf("ElevenLabs API error (status %d): model_not_found", status)
	}
	return fmt.Errorf("ElevenLabs API error (status %d)", status)
}

func normalizeAudioType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "voice", "speech", "tts":
		return "voice"
	case "sound", "sfx", "sound_effect":
		return "sound"
	case "music", "song":
		return "music"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeElevenLabsVoiceModel(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return ""
	}

	switch normalized {
	case "default", "standard", "base", "multilingual", "multilingual_v2":
		return defaultElevenVoiceModel
	case "eleven_v4", "eleven_v4_turbo":
		return normalized
	default:
		return strings.TrimSpace(value)
	}
}

var elevenLabsIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)

func isElevenLabsModelNotFound(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "model_not_found") ||
		(strings.Contains(lower, "model id") && strings.Contains(lower, "does not exist"))
}

func sanitizeElevenLabsError(err error) string {
	if err == nil {
		return ""
	}
	return truncateElevenLabsLog(strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", " "), 320)
}

func truncateElevenLabsLog(value string, max int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= max {
		return trimmed
	}
	if max < 4 {
		return trimmed[:max]
	}
	return trimmed[:max-3] + "..."
}

func inferAudioMimeType(contentType string, data []byte) string {
	trimmed := strings.TrimSpace(strings.ToLower(contentType))
	if trimmed != "" {
		if semi := strings.Index(trimmed, ";"); semi >= 0 {
			trimmed = strings.TrimSpace(trimmed[:semi])
		}
		if trimmed != "" && trimmed != "application/octet-stream" {
			return trimmed
		}
	}

	if len(data) >= 3 {
		if string(data[:3]) == "ID3" {
			return "audio/mpeg"
		}
		// MPEG frame sync.
		if data[0] == 0xFF && (data[1]&0xE0) == 0xE0 {
			return "audio/mpeg"
		}
	}

	if len(data) >= 12 {
		if string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
			return "audio/wav"
		}
	}

	if len(data) >= 4 {
		if string(data[:4]) == "OggS" {
			return "audio/ogg"
		}
		if string(data[:4]) == "fLaC" {
			return "audio/flac"
		}
	}

	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		return "audio/mp4"
	}

	return "audio/mpeg"
}

func extensionForAudioMimeType(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav"
	case "audio/ogg":
		return "ogg"
	case "audio/opus":
		return "opus"
	case "audio/flac":
		return "flac"
	case "audio/mp4", "audio/x-m4a":
		return "m4a"
	default:
		return "mp3"
	}
}
