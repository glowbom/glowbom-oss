package main

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var buzzMediaPath = regexp.MustCompile(`^/media/[0-9a-fA-F]{64}(\.[a-zA-Z0-9]+){0,2}$`)

// Only profile metadata supplies picture URLs. Request parameters cannot choose
// an arbitrary download target or a different relay for the connected identity.
func buzzPictureURL(raw, relay string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if strings.HasPrefix(raw, "/media/") {
		base, err := url.Parse(relay)
		if err != nil {
			return ""
		}
		u = base.ResolveReference(u)
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return ""
	}
	return u.String()
}

func buzzRelayPicture(picture, relay string) bool {
	u, err := url.Parse(picture)
	base, baseErr := url.Parse(relay)
	return err == nil && baseErr == nil && u.Scheme == "https" && base.Scheme == "https" &&
		strings.EqualFold(u.Host, base.Host) && u.User == nil && u.Fragment == "" &&
		buzzMediaPath.MatchString(u.Path) && u.RawPath == ""
}

func (s *buzzSession) serveAvatar(w http.ResponseWriter, r *http.Request) {
	fail := func(status int) { w.WriteHeader(status) }
	pubkey := strings.ToLower(r.URL.Query().Get("pubkey"))
	if !buzzHexKey.MatchString(pubkey) {
		fail(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if !s.connected || s.mediaCtx == nil {
		s.mu.Unlock()
		fail(http.StatusConflict)
		return
	}
	picture := ""
	for _, member := range s.members {
		if member.Pubkey == pubkey {
			picture = member.PictureURL
			break
		}
	}
	if !buzzRelayPicture(picture, s.credentials.RelayURL) {
		s.mu.Unlock()
		fail(http.StatusNotFound)
		return
	}
	credentials, generation := s.credentials, s.generation
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	stop := context.AfterFunc(s.mediaCtx, cancel)
	s.mu.Unlock()
	defer cancel()
	defer stop()
	select {
	case s.mediaSlots <- struct{}{}:
		defer func() { <-s.mediaSlots }()
	case <-ctx.Done():
		fail(http.StatusGatewayTimeout)
		return
	}
	data, err := s.read(ctx, credentials, "media", "get", picture, "--output", "-")
	if err != nil || ctx.Err() != nil || len(data) == 0 || len(data) > 2*1024*1024 {
		fail(http.StatusBadGateway)
		return
	}
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		fail(http.StatusUnsupportedMediaType)
		return
	}
	s.mu.Lock()
	active := s.connected && s.generation == generation
	s.mu.Unlock()
	if !active {
		fail(http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(data)
}
