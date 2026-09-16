package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"
)

// One connection belongs to this authenticated local backend, not a browser tab.
type buzzSession struct {
	mu          sync.Mutex
	read        buzzReadFunc
	credentials buzzLookup
	members     []buzzMember
	generation  uint64
	cancel      context.CancelFunc
	connected   bool
	mediaCtx    context.Context
	mediaCancel context.CancelFunc
	mediaSlots  chan struct{}
	live        *buzzLive
	profiles    *buzzProfileStore
}

func newBuzzSession(read buzzReadFunc) *buzzSession {
	return &buzzSession{read: read, mediaSlots: make(chan struct{}, 4), profiles: newBuzzProfileStore()}
}

func (s *buzzSession) snapshot() map[string]any {
	members := append([]buzzMember{}, s.members...)
	return map[string]any{"connected": s.connected, "connecting": s.cancel != nil, "relayUrl": s.credentials.RelayURL, "channelId": s.credentials.ChannelID, "members": members}
}

func (s *buzzSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
	}
	token := glowbomServerToken()
	if token == "" || !isLoopbackHost(backendBindHost()) {
		fail(503, "local_auth_required")
		return
	}
	if !hasValidGlowbomServerToken(r, token) || !isAllowedOrigin(r, glowbomAllowedOrigins()) {
		fail(403, "unauthorized")
		return
	}
	if r.URL.Path == "/buzz/session/profiles" {
		s.serveProfiles(w, r)
		return
	}
	if r.URL.Path == "/buzz/session/voices" || r.URL.Path == "/buzz/session/voice-preview" {
		s.serveVoices(w, r)
		return
	}
	if r.URL.Path == "/buzz/session/avatar" {
		if r.Method != http.MethodGet {
			fail(405, "method_not_allowed")
			return
		}
		s.serveAvatar(w, r)
		return
	}
	if r.URL.Path == "/buzz/session/messages" || r.URL.Path == "/buzz/session/speech" {
		s.serveLive(w, r)
		return
	}
	refresh := r.URL.Path == "/buzz/session/refresh"
	if r.Method == http.MethodGet && !refresh {
		s.mu.Lock()
		result := s.snapshot()
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	if r.Method == http.MethodDelete && !refresh {
		s.mu.Lock()
		s.generation++
		if s.cancel != nil {
			s.cancel()
		}
		if s.mediaCancel != nil {
			s.mediaCancel()
			s.mediaCtx, s.mediaCancel = nil, nil
		}
		s.cancel = nil
		if s.live != nil {
			s.live.close()
			s.live = nil
		}
		s.credentials = buzzLookup{}
		s.members = nil
		s.connected = false
		result := s.snapshot()
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	if r.Method != http.MethodPost {
		fail(405, "method_not_allowed")
		return
	}
	var credentials buzzLookup
	if !refresh {
		media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if media != "application/json" {
			fail(415, "invalid_content_type")
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&credentials) != nil || decoder.Decode(new(any)) != io.EOF || !validBuzzLookup(credentials) {
			fail(400, "invalid_credentials")
			return
		}
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		fail(409, "busy")
		return
	}
	if refresh {
		if !s.connected {
			s.mu.Unlock()
			fail(409, "disconnected")
			return
		}
		credentials = s.credentials
	} else if s.connected {
		s.mu.Unlock()
		fail(409, "already_connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	s.cancel = cancel
	s.generation++
	generation := s.generation
	s.mu.Unlock()
	defer cancel()
	members, err := lookupBuzzMembers(ctx, credentials, s.read)
	s.mu.Lock()
	if s.generation != generation {
		s.mu.Unlock()
		fail(409, "disconnected")
		return
	}
	s.cancel = nil
	if err != nil || ctx.Err() != nil {
		s.mu.Unlock()
		if ctx.Err() != nil {
			fail(504, "timeout")
		} else if err == errBuzzUnavailable {
			fail(503, "cli_unavailable")
		} else {
			fail(502, "lookup_failed")
		}
		return
	}
	s.credentials = credentials
	s.members = members
	if !s.connected {
		s.mediaCtx, s.mediaCancel = context.WithCancel(context.Background())
	}
	s.connected = true
	result := s.snapshot()
	s.mu.Unlock()
	_ = json.NewEncoder(w).Encode(result)
}
