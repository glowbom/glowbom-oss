package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	voiceStudioBaseURL       = "http://127.0.0.1:3900"
	voiceStudioSpeechModel   = "gpt-4o-mini-tts"
	localVoiceTextLimit      = 2500
	localVoiceResponseLimit  = 32 << 20
	localVoiceVoiceListLimit = 1 << 20
)

var localVoiceHTTPClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 3 * time.Second,
		}).DialContext,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type localVoiceRequest struct {
	Prompt  string `json:"prompt"`
	VoiceID string `json:"voiceId"`
}

func localVoiceVoicesHandler(client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, voiceStudioBaseURL+"/v1/audio/voices", nil)
		if err != nil {
			http.Error(w, "Could not prepare local voice request.", http.StatusInternalServerError)
			return
		}
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, "Start a local voice service to use Local voice.", http.StatusServiceUnavailable)
			return
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			http.Error(w, "The local voice service requires access. Check its API settings.", http.StatusServiceUnavailable)
			return
		}
		if response.StatusCode != http.StatusOK {
			http.Error(w, "Could not load local voices. Check the local voice service.", http.StatusBadGateway)
			return
		}
		data, err := readLimitedLocalVoiceBody(response.Body, localVoiceVoiceListLimit)
		if err != nil {
			http.Error(w, "The local voice service returned an invalid voice list.", http.StatusBadGateway)
			return
		}
		voices, err := parseLocalVoices(data)
		if err != nil {
			http.Error(w, "The local voice service returned an invalid voice list.", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ElevenLabsVoicesResponse{Voices: voices})
	}
}

func localVoiceSpeechHandler(client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input localVoiceRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input); err != nil {
			http.Error(w, "Invalid JSON body.", http.StatusBadRequest)
			return
		}
		input.Prompt = strings.TrimSpace(input.Prompt)
		input.VoiceID = strings.TrimSpace(input.VoiceID)
		if input.Prompt == "" || utf8.RuneCountInString(input.Prompt) > localVoiceTextLimit {
			http.Error(w, "Enter up to 2500 characters of text.", http.StatusBadRequest)
			return
		}
		if len(input.VoiceID) > 256 || strings.ContainsAny(input.VoiceID, "\r\n\x00") {
			http.Error(w, "Invalid voice.", http.StatusBadRequest)
			return
		}
		if input.VoiceID == "" {
			input.VoiceID = "default"
		}
		payload, _ := json.Marshal(map[string]string{
			"model":           voiceStudioSpeechModel,
			"input":           input.Prompt,
			"voice":           input.VoiceID,
			"response_format": "wav",
		})
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, voiceStudioBaseURL+"/v1/audio/speech", bytes.NewReader(payload))
		if err != nil {
			http.Error(w, "Could not prepare local voice request.", http.StatusInternalServerError)
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "audio/wav")
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, "Start a local voice service to use Local voice.", http.StatusServiceUnavailable)
			return
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			http.Error(w, "The local voice service requires access. Check its API settings.", http.StatusServiceUnavailable)
			return
		}
		if response.StatusCode != http.StatusOK {
			http.Error(w, "The local voice service could not generate speech. Check its selected voice and model.", http.StatusBadGateway)
			return
		}
		audio, err := readLimitedLocalVoiceBody(response.Body, localVoiceResponseLimit)
		if err != nil || len(audio) < 12 || string(audio[:4]) != "RIFF" || string(audio[8:12]) != "WAVE" {
			http.Error(w, "The local voice service returned invalid audio.", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ElevenLabsAudioResponse{
			Prompt:    input.Prompt,
			AudioType: "voice",
			Audio:     "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(audio),
			MimeType:  "audio/wav",
		})
	}
}

func readLimitedLocalVoiceBody(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("local voice response too large")
	}
	return data, nil
}

func parseLocalVoices(data []byte) ([]ElevenLabsVoiceOption, error) {
	var payload struct {
		Voices []json.RawMessage `json:"voices"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Voices == nil {
		return nil, errors.New("invalid local voice list")
	}
	voices := make([]ElevenLabsVoiceOption, 0, len(payload.Voices))
	seen := make(map[string]bool)
	for _, raw := range payload.Voices {
		var id, name string
		if err := json.Unmarshal(raw, &id); err == nil {
			name = id
		} else {
			var item struct {
				ID           string `json:"id"`
				VoiceIDCamel string `json:"voiceId"`
				VoiceID      string `json:"voice_id"`
				Name         string `json:"name"`
			}
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			id, name = item.ID, item.Name
			if id == "" {
				id = item.VoiceID
			}
			if id == "" {
				id = item.VoiceIDCamel
			}
		}
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if id == "" || len(id) > 256 || seen[id] {
			continue
		}
		if name == "" {
			name = id
		}
		seen[id] = true
		voices = append(voices, ElevenLabsVoiceOption{VoiceID: id, Name: name})
	}
	return voices, nil
}
