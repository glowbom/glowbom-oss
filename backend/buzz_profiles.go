package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Local presentation preferences only. No credentials or Buzz profile writes.
type buzzProfile struct {
	Body              string `json:"body"`
	Accessory         string `json:"accessory"`
	VoiceID           string `json:"voiceId"`
	VoiceProvider     string `json:"voiceProvider,omitempty"`
	CustomLabel       string `json:"customLabel,omitempty"`
	HideLabel         bool   `json:"hideLabel,omitempty"`
	LabelLayout       string `json:"labelLayout,omitempty"`
	CustomName        string `json:"customName,omitempty"`
	CustomHarness     string `json:"customHarness,omitempty"`
	CustomModel       string `json:"customModel,omitempty"`
	CustomDescription string `json:"customDescription,omitempty"`
	HideHarness       bool   `json:"hideHarness,omitempty"`
	HideModel         bool   `json:"hideModel,omitempty"`
}
type buzzProfileStore struct {
	mu   sync.Mutex
	path string
}

var profileID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)

func newBuzzProfileStore() *buzzProfileStore {
	dir, err := os.UserConfigDir()
	if err != nil {
		return &buzzProfileStore{}
	}
	return &buzzProfileStore{path: filepath.Join(dir, "Glowbom", "live-profiles.json")}
}
func (p *buzzProfileStore) readLocked() (map[string]buzzProfile, error) {
	result := map[string]buzzProfile{}
	if p.path == "" {
		return result, os.ErrInvalid
	}
	file, err := os.Open(p.path)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024+1))
	err = decoder.Decode(&result)
	if err != nil {
		return nil, err
	}
	if result == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, os.ErrInvalid
	}
	for key, profile := range result {
		if !buzzHexKey.MatchString(key) || !validBuzzProfile(profile) {
			return nil, os.ErrInvalid
		}
	}
	return result, nil
}
func (p *buzzProfileStore) all() (map[string]buzzProfile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readLocked()
}
func (p *buzzProfileStore) save(pubkey string, profile buzzProfile) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	values, err := p.readLocked()
	if err != nil {
		return err
	}
	if len(values) >= 2000 {
		if _, exists := values[pubkey]; !exists {
			return os.ErrInvalid
		}
	}
	values[pubkey] = profile
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p.path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(p.path), ".live-profiles-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, p.path)
}
func validBuzzProfile(p buzzProfile) bool {
	if p.LabelLayout != "" && p.LabelLayout != "legacy" && p.LabelLayout != "structured" {
		return false
	}
	for _, field := range []struct {
		text  string
		limit int
	}{{p.CustomLabel, 80}, {p.CustomName, 80}, {p.CustomHarness, 80}, {p.CustomModel, 120}, {p.CustomDescription, 280}} {
		if !utf8.ValidString(field.text) || utf8.RuneCountInString(field.text) > field.limit {
			return false
		}
		for _, r := range field.text {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return false
			}
		}
	}
	provider := p.VoiceProvider
	if provider == "" && p.VoiceID != "" {
		provider = "elevenlabs"
	}
	if provider != "" && !validVoiceSelection(provider, p.VoiceID) {
		return false
	}
	return (p.Body == "" || profileID.MatchString(p.Body)) && (p.Accessory == "" || profileID.MatchString(p.Accessory))
}

func validLiveVoiceProvider(provider string) bool {
	return provider == "elevenlabs" || provider == "local" || provider == "system"
}

func validVoiceSelection(provider, voiceID string) bool {
	if !validLiveVoiceProvider(provider) {
		return false
	}
	if voiceID == "" {
		return true
	}
	if provider == "elevenlabs" {
		return profileID.MatchString(voiceID)
	}
	if len(voiceID) > 256 || !utf8.ValidString(voiceID) || strings.TrimSpace(voiceID) != voiceID {
		return false
	}
	for _, r := range voiceID {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
func (s *buzzSession) serveProfiles(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		w.WriteHeader(409)
		return
	}
	if s.profiles == nil {
		s.profiles = newBuzzProfileStore()
	}
	if r.Method == http.MethodGet {
		values, err := s.profiles.all()
		if err != nil {
			w.WriteHeader(500)
			return
		}
		filtered := map[string]buzzProfile{}
		for _, m := range s.members {
			if p, ok := values[m.Pubkey]; ok {
				filtered[m.Pubkey] = p
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"profiles": filtered})
		return
	}
	if r.Method != http.MethodPut {
		w.WriteHeader(405)
		return
	}
	var req struct {
		Pubkey  string      `json:"pubkey"`
		Profile buzzProfile `json:"profile"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF || !validBuzzProfile(req.Profile) {
		w.WriteHeader(400)
		return
	}
	member := false
	for _, m := range s.members {
		if m.Pubkey == req.Pubkey {
			member = true
			break
		}
	}
	if !member {
		w.WriteHeader(404)
		return
	}
	if err := s.profiles.save(req.Pubkey, req.Profile); err != nil {
		w.WriteHeader(500)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"profile": req.Profile})
}

// Uses bounded requests and never returns keys or raw provider errors.
func (s *buzzSession) serveVoices(w http.ResponseWriter, r *http.Request) {
	if (r.URL.Path == "/buzz/session/voices" && r.Method != http.MethodGet) || (r.URL.Path == "/buzz/session/voice-preview" && r.Method != http.MethodPost) {
		w.WriteHeader(405)
		return
	}
	s.mu.Lock()
	if s.connected && s.live == nil {
		s.live = newBuzzLive()
		s.live.restoreSpeechKey()
		go s.live.listen(s.credentials)
	}
	l := s.live
	connected := s.connected
	s.mu.Unlock()
	if !connected || l == nil {
		w.WriteHeader(409)
		return
	}
	l.mu.Lock()
	key := l.key
	sessionCtx := l.ctx
	l.mu.Unlock()

	provider := r.URL.Query().Get("provider")
	if provider == "" {
		provider = "elevenlabs"
	}
	if r.Method == http.MethodGet {
		if !validLiveVoiceProvider(provider) || provider == "system" {
			w.WriteHeader(400)
			return
		}
		if provider == "elevenlabs" && key == "" {
			w.WriteHeader(412)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		stop := context.AfterFunc(sessionCtx, cancel)
		defer stop()
		voices := []ElevenLabsVoiceOption{}
		if provider == "local" {
			localVoices, err := fetchLiveLocalVoices(ctx, l.localClient)
			if err != nil {
				w.WriteHeader(503)
				return
			}
			voices = localVoices
		} else {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, elevenLabsBaseURL+"/v1/voices", nil)
			req.Header.Set("xi-api-key", key)
			client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Do(req)
			if err != nil {
				w.WriteHeader(502)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				w.WriteHeader(502)
				return
			}
			var data struct {
				Voices []struct {
					ID   string `json:"voice_id"`
					Name string `json:"name"`
				} `json:"voices"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&data) != nil {
				w.WriteHeader(502)
				return
			}
			for _, v := range data.Voices {
				if profileID.MatchString(v.ID) {
					name := []rune(v.Name)
					if len(name) > 120 {
						name = name[:120]
					}
					voices = append(voices, ElevenLabsVoiceOption{VoiceID: v.ID, Name: string(name)})
				}
				if len(voices) >= 1000 {
					break
				}
			}
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.ctx.Err() != nil || (provider == "elevenlabs" && l.key != key) {
			w.WriteHeader(409)
			return
		}
		if provider == "local" {
			l.localVoices = voices
		} else {
			l.voices = voices
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"voices": voices})
		return
	}
	var input struct {
		VoiceID       string `json:"voiceId"`
		VoiceProvider string `json:"voiceProvider"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		w.WriteHeader(400)
		return
	}
	if input.VoiceProvider != "" {
		provider = input.VoiceProvider
	}
	if !validLiveVoiceProvider(provider) || provider == "system" || !validVoiceSelection(provider, input.VoiceID) {
		w.WriteHeader(400)
		return
	}
	if provider == "elevenlabs" && key == "" {
		w.WriteHeader(412)
		return
	}
	l.mu.Lock()
	valid := input.VoiceID == "" || (provider == "elevenlabs" && input.VoiceID == defaultElevenVoiceID) || (provider == "local" && input.VoiceID == "default")
	knownVoices := l.voices
	if provider == "local" {
		knownVoices = l.localVoices
	}
	for _, v := range knownVoices {
		if v.VoiceID == input.VoiceID {
			valid = true
			break
		}
	}
	if !valid || l.speaking || (provider == "elevenlabs" && l.key != key) {
		l.mu.Unlock()
		w.WriteHeader(409)
		return
	}
	l.speaking = true
	l.mu.Unlock()
	timeout := 15 * time.Second
	if provider == "local" {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	stop := context.AfterFunc(sessionCtx, cancel)
	defer stop()
	var audio []byte
	var err error
	contentType := "audio/mpeg"
	if provider == "local" {
		contentType = "audio/wav"
		audio, err = l.localSynth(ctx, "Hello, I'm ready to work with the team.", input.VoiceID)
	} else {
		audio, err = l.synth(ctx, key, "Hello, I'm ready to work with the team.", input.VoiceID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.speaking = false
	if err != nil || ctx.Err() != nil || (provider == "elevenlabs" && l.key != key) || l.ctx.Err() != nil {
		w.WriteHeader(502)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(audio)
}

func fetchLiveLocalVoices(ctx context.Context, client *http.Client) ([]ElevenLabsVoiceOption, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, voiceStudioBaseURL+"/v1/audio/voices", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, os.ErrInvalid
	}
	data, err := readLimitedLocalVoiceBody(resp.Body, localVoiceVoiceListLimit)
	if err != nil {
		return nil, err
	}
	voices, err := parseLocalVoices(data)
	if err != nil {
		return nil, err
	}
	filtered := make([]ElevenLabsVoiceOption, 0, len(voices))
	for _, voice := range voices {
		if validVoiceSelection("local", voice.VoiceID) {
			name := []rune(voice.Name)
			if len(name) > 120 {
				voice.Name = string(name[:120])
			}
			filtered = append(filtered, voice)
		}
		if len(filtered) >= 1000 {
			break
		}
	}
	return filtered, nil
}
