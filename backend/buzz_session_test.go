package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func sessionRequest(s *buzzSession, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer session-test")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestBuzzSessionLifecycle(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "session-test")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	calls := 0
	s := newBuzzSession(func(_ context.Context, v buzzLookup, _ ...string) ([]byte, error) {
		calls++
		if v.PrivateKey != testBuzzLookup().PrivateKey {
			t.Fatal("credentials lost")
		}
		return []byte("[]"), nil
	})
	body, _ := json.Marshal(testBuzzLookup())
	w := sessionRequest(s, "POST", "/buzz/session", string(body))
	if w.Code != 200 || !s.connected || strings.Contains(w.Body.String(), testBuzzLookup().PrivateKey) {
		t.Fatal("connection failed or exposed key")
	}
	if sessionRequest(s, "GET", "/buzz/session", "").Code != 200 || calls != 1 {
		t.Fatal("status unexpectedly queried Buzz")
	}
	if sessionRequest(s, "POST", "/buzz/session", string(body)).Code != 409 {
		t.Fatal("connection overwritten")
	}
	if sessionRequest(s, "POST", "/buzz/session/refresh", "").Code != 200 || calls != 2 {
		t.Fatal("refresh failed")
	}
	if sessionRequest(s, "DELETE", "/buzz/session", "").Code != 200 || s.connected || s.credentials.PrivateKey != "" || len(s.members) != 0 {
		t.Fatal("disconnect retained state")
	}
	if sessionRequest(s, "POST", "/buzz/session/refresh", "").Code != 409 {
		t.Fatal("refresh after disconnect")
	}
}

func TestBuzzDisconnectCancelsPendingConnection(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "session-test")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	started := make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	s := newBuzzSession(func(ctx context.Context, _ buzzLookup, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return []byte("[]"), nil
	})
	body, _ := json.Marshal(testBuzzLookup())
	go func() { done <- sessionRequest(s, "POST", "/buzz/session", string(body)) }()
	<-started
	sessionRequest(s, "DELETE", "/buzz/session", "")
	if (<-done).Code != 409 || s.connected || s.credentials.PrivateKey != "" {
		t.Fatal("cancelled connect resurrected session")
	}
}
