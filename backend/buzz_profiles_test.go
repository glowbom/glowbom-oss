package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuzzProfilePersistenceAndScope(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "session-test")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	key := strings.Repeat("a", 64)
	store := &buzzProfileStore{path: filepath.Join(t.TempDir(), "profiles.json")}
	s := &buzzSession{connected: true, members: []buzzMember{{Pubkey: key}}, profiles: store}
	profile := buzzProfile{Body: "chick3", Accessory: "hair2", VoiceID: "fixture-voice", CustomLabel: "Codex · My laptop", HideLabel: true, LabelLayout: "structured", CustomName: "Bruna", CustomHarness: "Hermes", CustomModel: "GPT-6 Astra", CustomDescription: "My local assistant", HideHarness: true, HideModel: true}
	body, _ := json.Marshal(map[string]any{"pubkey": key, "profile": profile})
	if w := sessionRequest(s, "PUT", "/buzz/session/profiles", string(body)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	restored := &buzzProfileStore{path: store.path}
	values, err := restored.all()
	if err != nil || values[key] != profile {
		t.Fatal("preferences not restored", err)
	}
	s.members = []buzzMember{{Pubkey: strings.Repeat("b", 64)}}
	if w := sessionRequest(s, "GET", "/buzz/session/profiles", ""); strings.Contains(w.Body.String(), key) {
		t.Fatal("unrelated identity exposed")
	}
	if w := sessionRequest(s, "PUT", "/buzz/session/profiles", string(body)); w.Code != 404 {
		t.Fatal("unknown member updated")
	}
	if validBuzzProfile(buzzProfile{Body: "../../evil"}) || validBuzzProfile(buzzProfile{VoiceID: "https://example.com"}) {
		t.Fatal("unsafe identifier accepted")
	}
	r := httptest.NewRequest("GET", "/buzz/session/profiles", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("missing auth accepted")
	}
}
func TestBuzzSpeechUsesAssignedVoice(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture"
	store := &buzzProfileStore{path: filepath.Join(t.TempDir(), "profiles.json")}
	s := &buzzSession{connected: true, live: l, profiles: store}
	now := time.Now().Unix()
	event := liveEvent(t, "hello", "channel", now)
	if err := store.save(event.PubKey, buzzProfile{VoiceID: "assigned-voice"}); err != nil {
		t.Fatal(err)
	}
	used := ""
	l.synth = func(_ context.Context, _ string, _ string, voice string) ([]byte, error) {
		used = voice
		return []byte("audio"), nil
	}
	liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true}`)
	l.accept(event, "channel", now, true)
	response := liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+event.ID+`"}`)
	if response.Code != 200 || used != "assigned-voice" {
		t.Fatal("wrong voice", response.Code, used)
	}
}
func TestBuzzVoicePreviewRequiresKnownVoice(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture"
	l.voices = []ElevenLabsVoiceOption{{VoiceID: "known"}}
	s := &buzzSession{connected: true, live: l}
	calls := 0
	l.synth = func(_ context.Context, _ string, text string, voice string) ([]byte, error) {
		calls++
		if voice != "known" || !strings.HasPrefix(text, "Hello,") {
			t.Fatal("unexpected synthesis")
		}
		return []byte("audio"), nil
	}
	for _, item := range []struct {
		id     string
		status int
	}{{"unknown", 409}, {"known", 200}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/buzz/session/voice-preview", strings.NewReader(`{"voiceId":"`+item.id+`"}`))
		s.serveVoices(w, r)
		if w.Code != item.status {
			t.Fatal(w.Code)
		}
	}
	if calls != 1 {
		t.Fatal("unexpected preview charge")
	}
	l.key = ""
	w := httptest.NewRecorder()
	s.serveVoices(w, httptest.NewRequest("GET", "/buzz/session/voices", nil))
	if w.Code != 412 {
		t.Fatal("missing key accepted")
	}
}

func TestStructuredLabelProfileValidation(t *testing.T) {
	for _, p := range []buzzProfile{{LabelLayout: "unknown"}, {CustomName: strings.Repeat("x", 81)}, {CustomHarness: "unsafe\nlabel"}, {CustomModel: strings.Repeat("x", 121)}, {CustomDescription: strings.Repeat("x", 281)}} {
		if validBuzzProfile(p) {
			t.Fatalf("invalid structured label accepted: %+v", p)
		}
	}
	// Loading and resaving a legacy profile leaves migration under client control.
	var old buzzProfile
	if err := json.Unmarshal([]byte(`{"customLabel":"GPT-6 Astra (Hermes)","hideLabel":true}`), &old); err != nil {
		t.Fatal(err)
	}
	if !validBuzzProfile(old) || old.LabelLayout != "" || old.CustomLabel != "GPT-6 Astra (Hermes)" || !old.HideLabel {
		t.Fatal("legacy data changed")
	}
}

func TestLiveVoiceProfileProviderCompatibility(t *testing.T) {
	var old buzzProfile
	if err := json.Unmarshal([]byte(`{"voiceId":"old-voice"}`), &old); err != nil {
		t.Fatal(err)
	}
	if !validBuzzProfile(old) || old.VoiceProvider != "" || old.VoiceID != "old-voice" {
		t.Fatal("legacy ElevenLabs selection changed")
	}
	for _, profile := range []buzzProfile{
		{VoiceProvider: "local", VoiceID: "preset:soft"},
		{VoiceProvider: "system", VoiceID: "com.apple.voice.en-US.Samantha"},
		{VoiceProvider: "local"},
		{},
	} {
		if !validBuzzProfile(profile) {
			t.Fatalf("valid provider selection rejected: %+v", profile)
		}
	}
	for _, profile := range []buzzProfile{
		{VoiceProvider: "remote", VoiceID: "voice"},
		{VoiceProvider: "elevenlabs", VoiceID: "preset:soft"},
		{VoiceProvider: "local", VoiceID: "bad\nvoice"},
		{VoiceProvider: "system", VoiceID: strings.Repeat("x", 257)},
	} {
		if validBuzzProfile(profile) {
			t.Fatalf("invalid provider selection accepted: %+v", profile)
		}
	}
}

func TestBuzzLiveRoutesSavedVoicesAndSystemText(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture"
	store := &buzzProfileStore{path: filepath.Join(t.TempDir(), "profiles.json")}
	s := &buzzSession{connected: true, live: l, profiles: store}
	now := time.Now().Unix()
	localEvent := liveEvent(t, "speak locally", "channel", now)
	elevenEvent := liveEvent(t, "speak remotely", "channel", now)
	if err := elevenEvent.Sign(strings.Repeat("2", 64)); err != nil {
		t.Fatal(err)
	}
	systemEvent := liveEvent(t, "speak system", "channel", now)
	if err := systemEvent.Sign(strings.Repeat("3", 64)); err != nil {
		t.Fatal(err)
	}
	if err := store.save(elevenEvent.PubKey, buzzProfile{VoiceID: "legacy-voice"}); err != nil {
		t.Fatal(err)
	}
	if err := store.save(systemEvent.PubKey, buzzProfile{VoiceProvider: "system", VoiceID: "native.voice"}); err != nil {
		t.Fatal(err)
	}
	localCalls, elevenCalls := 0, 0
	l.localSynth = func(_ context.Context, text, voice string) ([]byte, error) {
		localCalls++
		if text != "speak locally" || voice != "preset:soft" {
			t.Fatalf("wrong local voice: %q %q", text, voice)
		}
		return []byte("RIFF\x04\x00\x00\x00WAVE"), nil
	}
	l.synth = func(_ context.Context, key, text, voice string) ([]byte, error) {
		elevenCalls++
		if key != "fixture" || text != "speak remotely" || voice != "legacy-voice" {
			t.Fatalf("wrong ElevenLabs voice: %q %q %q", key, text, voice)
		}
		return []byte("mp3"), nil
	}
	if w := liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true,"defaultVoiceProvider":"local","defaultVoiceId":"preset:soft"}`); w.Code != 204 {
		t.Fatal(w.Code)
	}
	for _, event := range []struct {
		id   string
		mime string
	}{
		{localEvent.ID, "audio/wav"},
		{elevenEvent.ID, "audio/mpeg"},
		{systemEvent.ID, "application/json"},
	} {
		// Accept each event after enabling so all three are speech eligible.
		switch event.id {
		case localEvent.ID:
			l.accept(localEvent, "channel", now, true)
		case elevenEvent.ID:
			l.accept(elevenEvent, "channel", now, true)
		case systemEvent.ID:
			l.accept(systemEvent, "channel", now, true)
		}
		w := liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+event.id+`"}`)
		if w.Code != 200 || w.Header().Get("Content-Type") != event.mime {
			t.Fatalf("wrong speech response: %d %s", w.Code, w.Header().Get("Content-Type"))
		}
		if event.mime == "application/json" {
			var payload struct {
				Text    string `json:"text"`
				VoiceID string `json:"voiceId"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.Text != "speak system" || payload.VoiceID != "native.voice" {
				t.Fatalf("wrong system payload: %+v %v", payload, err)
			}
		}
	}
	if localCalls != 1 || elevenCalls != 1 {
		t.Fatalf("unexpected synthesis calls: local=%d ElevenLabs=%d", localCalls, elevenCalls)
	}
}

func TestBuzzLiveLocalWithoutElevenLabsKey(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = ""
	s := &buzzSession{connected: true, live: l}
	request := func(path string) map[string]any {
		w := httptest.NewRecorder()
		s.serveLive(w, httptest.NewRequest(http.MethodGet, path, nil))
		var data map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &data) != nil {
			t.Fatalf("bad feed status: %d", w.Code)
		}
		return data
	}
	if request("/buzz/session/messages")["speechAvailable"] != false || request("/buzz/session/messages?defaultVoiceProvider=local")["speechAvailable"] != true {
		t.Fatal("provider-aware availability changed legacy behavior")
	}
	if w := liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true,"defaultVoiceProvider":"local"}`); w.Code != 204 {
		t.Fatal(w.Code)
	}
	l.localSynth = func(ctx context.Context, _, voice string) ([]byte, error) {
		if voice != "" {
			t.Fatal("local default was not selected")
		}
		deadline, ok := ctx.Deadline()
		l.mu.Lock()
		lease := l.lease
		l.mu.Unlock()
		if !ok || time.Until(deadline) < time.Minute || time.Until(lease) < time.Minute {
			t.Fatal("local synthesis did not extend timeout and lease")
		}
		return []byte("RIFF\x04\x00\x00\x00WAVE"), nil
	}
	now := time.Now().Unix()
	event := liveEvent(t, "local only", "channel", now)
	l.accept(event, "channel", now, true)
	w := liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+event.ID+`"}`)
	if w.Code != 200 || w.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("local speech failed without key: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if remaining := time.Until(l.lease); remaining > 20*time.Second {
		t.Fatalf("local lease was not restored after synthesis: %s", remaining)
	}
	l.localSynth = func(context.Context, string, string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	unavailable := liveEvent(t, "offline local voice", "channel", now)
	l.accept(unavailable, "channel", now, true)
	failed := liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+unavailable.ID+`"}`)
	if failed.Code != 502 || strings.Contains(failed.Body.String(), "DeadlineExceeded") {
		t.Fatalf("local error leaked or returned wrong status: %d %s", failed.Code, failed.Body.String())
	}
	if duplicate := liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+unavailable.ID+`"}`); duplicate.Code != 410 {
		t.Fatal("failed local event was retried")
	}
}

func TestBuzzLiveSystemWithoutElevenLabsKey(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = ""
	s := &buzzSession{connected: true, live: l}
	if w := liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true,"defaultVoiceProvider":"system","defaultVoiceId":"native.voice"}`); w.Code != 204 {
		t.Fatalf("system voice needs no ElevenLabs key: %d", w.Code)
	}
	now := time.Now().Unix()
	event := liveEvent(t, "system only", "channel", now)
	l.accept(event, "channel", now, true)
	w := liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+event.ID+`"}`)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("system speech failed without key: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var payload struct {
		Text    string `json:"text"`
		VoiceID string `json:"voiceId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.Text != "system only" || payload.VoiceID != "native.voice" {
		t.Fatalf("wrong system speech: %+v %v", payload, err)
	}
	if w := liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+event.ID+`"}`); w.Code != 410 {
		t.Fatal("system event spoke twice")
	}
}

func TestBuzzLiveLocalVoicesAndPreview(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = ""
	l.localClient = localVoiceTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != voiceStudioBaseURL+"/v1/audio/voices" {
			t.Fatalf("wrong local voice source: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"voices":[{"id":"preset:soft","name":"Soft"}]}`)), Header: make(http.Header)}, nil
	})
	l.localSynth = func(_ context.Context, _, voice string) ([]byte, error) {
		if voice != "" && voice != "preset:soft" {
			t.Fatalf("unexpected preview voice: %s", voice)
		}
		return []byte("RIFF\x04\x00\x00\x00WAVE"), nil
	}
	s := &buzzSession{connected: true, live: l}
	w := httptest.NewRecorder()
	s.serveVoices(w, httptest.NewRequest(http.MethodGet, "/buzz/session/voices?provider=local", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "preset:soft") {
		t.Fatalf("local voices unavailable: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		id   string
		code int
	}{{"", 200}, {"preset:soft", 200}, {"unknown", 409}} {
		w = httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"voiceProvider": "local", "voiceId": tc.id})
		s.serveVoices(w, httptest.NewRequest(http.MethodPost, "/buzz/session/voice-preview", bytes.NewReader(body)))
		if w.Code != tc.code || (tc.code == 200 && w.Header().Get("Content-Type") != "audio/wav") {
			t.Fatalf("local preview result: id=%q code=%d mime=%s", tc.id, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

func TestBuzzSynthesizeLocalReturnsRawWAV(t *testing.T) {
	wav := []byte("RIFF\x04\x00\x00\x00WAVEfixture")
	client := localVoiceTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != voiceStudioBaseURL+"/v1/audio/speech" {
			t.Fatalf("wrong local speech endpoint: %s %s", r.Method, r.URL)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["voice"] != "preset:soft" || body["input"] != "Hello" || body["response_format"] != "wav" {
			t.Fatalf("wrong local speech request: %+v %v", body, err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(wav)), Header: make(http.Header)}, nil
	})
	got, err := buzzSynthesizeLocalWithClient(context.Background(), client, "Hello", "preset:soft")
	if err != nil || !bytes.Equal(got, wav) {
		t.Fatalf("raw WAV mismatch: %v", err)
	}
}
