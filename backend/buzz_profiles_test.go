package main

import (
	"context"
	"encoding/json"
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
