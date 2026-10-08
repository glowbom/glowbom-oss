package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type memoryVoiceKey struct {
	key string
	err error
}

func (s *memoryVoiceKey) Get() (string, error) { return s.key, s.err }
func (s *memoryVoiceKey) Set(key string) error {
	if s.err == nil {
		s.key = key
	}
	return s.err
}
func (s *memoryVoiceKey) Delete() error {
	if s.err == nil {
		s.key = ""
	}
	return s.err
}
func TestVoiceKeyLifecycle(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	store := &memoryVoiceKey{}
	for _, tc := range []struct {
		method, body string
		configured   bool
	}{{"POST", `{"key":" private-test-key "}`, true}, {"GET", "", true}, {"DELETE", "", false}, {"GET", "", false}} {
		r := httptest.NewRequest(tc.method, "/audio/key", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer fixture-token")
		w := httptest.NewRecorder()
		voiceKeyHandler(store)(w, r)
		want := `"configured":false`
		if tc.configured {
			want = `"configured":true`
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) || strings.Contains(w.Body.String(), "private-test-key") {
			t.Fatalf("unexpected safe status: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("status must not be cached")
		}
	}
}
func TestVoiceKeyRequiresAuthentication(t *testing.T) {
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	for _, token := range []string{"", "fixture-token"} {
		t.Setenv("GLOWBOM_SERVER_TOKEN", token)
		for _, method := range []string{"GET", "POST", "DELETE"} {
			store := &memoryVoiceKey{key: "original"}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, "/audio/key", strings.NewReader(`{"key":"replacement"}`))
			voiceKeyHandler(store)(w, r)
			if w.Code != 401 || store.key != "original" {
				t.Fatal("unauthenticated access allowed")
			}
		}
	}
}
func TestVoiceKeyResolution(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	for _, tc := range []struct {
		key    string
		err    error
		status int
	}{{"saved-key", nil, 200}, {"", nil, 400}, {"", errors.New("sensitive vault detail"), 503}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/audio", nil)
		r.Header.Set("Authorization", "Bearer fixture-token")
		key, ok := resolveVoiceKey(w, r, "", true, &memoryVoiceKey{key: tc.key, err: tc.err})
		if w.Code != tc.status || ok != (tc.status == 200) || strings.Contains(w.Body.String(), "sensitive") {
			t.Fatal("incorrect resolution or leaked error")
		}
		if ok && key != tc.key {
			t.Fatal("saved key not used")
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/audio", nil)
	if _, ok := resolveVoiceKey(w, r, "", true, &memoryVoiceKey{key: "secret"}); ok || w.Code != 401 {
		t.Fatal("unauthenticated saved key used")
	}
	if key, ok := resolveVoiceKey(httptest.NewRecorder(), r, " explicit ", false, &memoryVoiceKey{err: errors.New("must not access")}); !ok || key != "explicit" {
		t.Fatal("explicit keys should not access vault")
	}
}
func TestVoiceKeyStorageFailureIsSafe(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-token")
	for _, method := range []string{"GET", "POST", "DELETE"} {
		r := httptest.NewRequest(method, "/audio/key", strings.NewReader(`{"key":"test-key"}`))
		r.Header.Set("Authorization", "Bearer fixture-token")
		w := httptest.NewRecorder()
		voiceKeyHandler(&memoryVoiceKey{err: errors.New("secret detail")})(w, r)
		if w.Code != 503 || strings.Contains(w.Body.String(), "secret detail") {
			t.Fatal("vault error not handled safely")
		}
	}
}
