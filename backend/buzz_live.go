package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

type buzzMessage struct {
	ID        string   `json:"id"`
	Pubkey    string   `json:"pubkey"`
	Text      string   `json:"text"`
	CreatedAt int64    `json:"createdAt"`
	Sequence  uint64   `json:"sequence"`
	Live      bool     `json:"live"`
	Mentions  []string `json:"mentions"`
	ReplyTo   string   `json:"replyTo"`
}

type buzzLive struct {
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	status         string
	messages       []buzzMessage
	seen           map[string]int64
	sequence       uint64
	keyPath        string
	key            string
	owner          string
	enabledAfter   uint64
	lease          time.Time
	speechCtx      context.Context
	speechCancel   context.CancelFunc
	attempted      map[string]bool
	speaking       bool
	readAll        bool
	pendingSpeech  map[string]buzzMessage
	nextChunk      map[string]int
	speechOverflow uint64
	// Injected only by tests. Production always uses the fixed ElevenLabs origin.
	voices []ElevenLabsVoiceOption
	dialer *websocket.Dialer
	synth  func(context.Context, string, string, string) ([]byte, error)
}

func newBuzzLive() *buzzLive {
	ctx, cancel := context.WithCancel(context.Background())
	l := &buzzLive{ctx: ctx, cancel: cancel, status: "connecting", messages: []buzzMessage{}, seen: map[string]int64{}, attempted: map[string]bool{}, key: strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY")), synth: buzzSynthesize, dialer: &websocket.Dialer{HandshakeTimeout: 10 * time.Second}}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.mu.Lock()
				if l.owner != "" && time.Now().After(l.lease) {
					l.muteLocked()
				}
				l.mu.Unlock()
			}
		}
	}()
	return l
}
func (l *buzzLive) muteLocked() {
	if l.speechCancel != nil {
		l.speechCancel()
	}
	l.owner = ""
	l.readAll = false
	l.pendingSpeech = nil
	l.nextChunk = nil
	l.speechOverflow = 0
	l.speechCtx = nil
	l.speechCancel = nil
}
func (l *buzzLive) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancel()
	l.muteLocked()
	l.key = ""
	l.messages = nil
	l.status = "disconnected"
}
func (l *buzzLive) setStatus(status string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx.Err() == nil {
		l.status = status
	}
}

func (s *buzzSession) serveLive(w http.ResponseWriter, r *http.Request) {
	if (r.URL.Path == "/buzz/session/messages" && r.Method != http.MethodGet) || (r.URL.Path == "/buzz/session/speech" && r.Method != http.MethodPost) {
		w.WriteHeader(405)
		return
	}
	s.mu.Lock()
	if !s.connected {
		s.mu.Unlock()
		w.WriteHeader(409)
		return
	}
	if s.live == nil {
		s.live = newBuzzLive()
		s.live.restoreSpeechKey()
		go s.live.listen(s.credentials)
	}
	l := s.live
	s.mu.Unlock()
	if r.Method == http.MethodGet {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.owner != "" && time.Now().After(l.lease) {
			l.muteLocked()
		}
		client := r.URL.Query().Get("clientId")
		if client != "" && client == l.owner {
			l.lease = time.Now().Add(15 * time.Second)
		}
		messages := l.messages
		if client == l.owner && l.readAll {
			merged := make(map[string]buzzMessage, len(messages)+len(l.pendingSpeech))
			for _, m := range messages {
				merged[m.ID] = m
			}
			for _, m := range l.pendingSpeech {
				merged[m.ID] = m
			}
			messages = make([]buzzMessage, 0, len(merged))
			for _, m := range merged {
				messages = append(messages, m)
			}
			sort.Slice(messages, func(i, j int) bool { return messages[i].Sequence < messages[j].Sequence })
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"readAllSupported": true, "readAll": l.readAll && client == l.owner, "speechOverflow": l.speechOverflow, "status": l.status, "messages": messages, "sequence": l.sequence, "speechAvailable": l.key != "", "speakingEnabled": client != "" && l.owner == client})
		return
	}
	var req struct {
		ClientID string  `json:"clientId"`
		Enabled  *bool   `json:"enabled"`
		Key      *string `json:"key"`
		Remember bool    `json:"remember"`
		EventID  string  `json:"eventId"`
		ReadAll  bool    `json:"readAll"`
		Chunk    int     `json:"chunk"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		w.WriteHeader(400)
		return
	}
	l.mu.Lock()
	if l.ctx.Err() != nil {
		l.mu.Unlock()
		w.WriteHeader(409)
		return
	}
	if req.Key != nil {
		value := strings.TrimSpace(*req.Key)
		if len(value) > 4096 {
			l.mu.Unlock()
			w.WriteHeader(400)
			return
		}
		if req.Remember {
			path := l.keyPath
			if path == "" {
				path = speechKeyPath()
			}
			if err := saveSpeechKey(path, value); err != nil {
				l.mu.Unlock()
				w.WriteHeader(500)
				return
			}
			l.keyPath = path
		}
		l.muteLocked()
		l.key = strings.TrimSpace(*req.Key)
		l.voices = nil
		l.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	if len(req.ClientID) < 16 || len(req.ClientID) > 128 {
		l.mu.Unlock()
		w.WriteHeader(400)
		return
	}
	if l.owner != "" && time.Now().After(l.lease) {
		l.muteLocked()
	}
	if req.Enabled != nil {
		if l.owner != "" && l.owner != req.ClientID {
			l.mu.Unlock()
			w.WriteHeader(409)
			return
		}
		l.muteLocked()
		if *req.Enabled {
			if l.key == "" {
				l.mu.Unlock()
				w.WriteHeader(412)
				return
			}
			l.owner = req.ClientID
			l.lease = time.Now().Add(15 * time.Second)
			l.enabledAfter = l.sequence
			l.readAll = req.ReadAll
			l.pendingSpeech = make(map[string]buzzMessage)
			l.nextChunk = make(map[string]int)
			l.speechCtx, l.speechCancel = context.WithCancel(l.ctx)
		}
		l.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	if l.owner != req.ClientID || l.key == "" {
		l.mu.Unlock()
		w.WriteHeader(409)
		return
	}
	var message *buzzMessage
	for i := range l.messages {
		if l.messages[i].ID == req.EventID {
			m := l.messages[i]
			message = &m
			break
		}
	}
	if l.readAll {
		message = nil
		if m, ok := l.pendingSpeech[req.EventID]; ok {
			message = &m
		}
	}
	if message == nil || !message.Live || message.Sequence <= l.enabledAfter || (!l.readAll && time.Now().Unix()-message.CreatedAt > 30) || l.attempted[req.EventID] {
		l.mu.Unlock()
		w.WriteHeader(410)
		return
	}
	if l.speaking {
		l.mu.Unlock()
		w.WriteHeader(429)
		return
	}
	chunks := buzzSpeechChunks(message.Text)
	index := 0
	if l.readAll {
		index = req.Chunk
	}
	if req.Chunk < 0 || (!l.readAll && req.Chunk != 0) || index >= len(chunks) || (l.readAll && index != l.nextChunk[req.EventID]) {
		l.mu.Unlock()
		w.WriteHeader(410)
		return
	}
	more := l.readAll && index+1 < len(chunks)
	if more {
		l.nextChunk[req.EventID] = index + 1
	} else {
		l.attempted[req.EventID] = true
		delete(l.pendingSpeech, req.EventID)
		delete(l.nextChunk, req.EventID)
	}
	l.speaking = true
	ctx, cancel := context.WithTimeout(l.speechCtx, 20*time.Second)
	stop := context.AfterFunc(r.Context(), cancel)
	key := l.key
	text := chunks[index]
	l.mu.Unlock()
	defer cancel()
	defer stop()
	voiceID := ""
	s.mu.Lock()
	store := s.profiles
	s.mu.Unlock()
	if store != nil {
		if profiles, err := store.all(); err == nil {
			voiceID = profiles[message.Pubkey].VoiceID
		}
	}
	audio, err := l.synth(ctx, key, text, voiceID)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.speaking = false
	if err != nil || ctx.Err() != nil || l.owner != req.ClientID || time.Now().After(l.lease) {
		delete(l.pendingSpeech, req.EventID)
		delete(l.nextChunk, req.EventID)
		l.attempted[req.EventID] = true
		w.WriteHeader(502)
		return
	}
	if more {
		w.Header().Set("X-Buzz-Speech-More", "true")
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(audio)
}

// No disk output, prompt logging, retries, or arbitrary text supplied by a client.
func buzzSynthesize(ctx context.Context, key, text, voiceID string) ([]byte, error) {
	if voiceID == "" {
		voiceID = defaultElevenVoiceID
	}
	if !profileID.MatchString(voiceID) {
		return nil, errors.New("invalid voice")
	}
	body, _ := json.Marshal(map[string]string{"text": text, "model_id": defaultElevenVoiceModel})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, elevenLabsBaseURL+"/v1/text-to-speech/"+voiceID+"?output_format="+defaultElevenOutputFormat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("speech unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("speech unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(data) == 0 || len(data) > 2*1024*1024 {
		return nil, errors.New("invalid audio")
	}
	return data, nil
}

func (l *buzzLive) accept(evt nostr.Event, channel string, start int64, live bool) {
	// Sender clocks can run ahead of this computer. Allow the same two-minute
	// tolerance as reconnect catch-up, while still rejecting far-future events.
	now := time.Now().Unix()
	if evt.Kind != 9 || evt.CreatedAt < nostr.Timestamp(start) || int64(evt.CreatedAt) > now+120 || len(evt.Content) > 32768 || strings.TrimSpace(evt.Content) == "" {
		return
	}
	found := false
	for _, tag := range evt.Tags {
		if len(tag) > 1 && tag[0] == "h" && tag[1] == channel {
			found = true
		}
	}
	if !found || !evt.CheckID() {
		return
	}
	valid, err := evt.CheckSignature()
	if err != nil || !valid {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx.Err() != nil {
		return
	}
	if _, ok := l.seen[evt.ID]; ok {
		return
	}
	// The reconnect window is two minutes. Older deliveries cannot be replayed.
	cutoff := now - 120
	if int64(evt.CreatedAt) < cutoff {
		return
	}
	for id, t := range l.seen {
		if t < cutoff {
			delete(l.seen, id)
			delete(l.attempted, id)
		}
	}
	if len(l.seen) >= 8192 {
		l.status = "message capacity reached"
		return
	}
	l.seen[evt.ID] = int64(evt.CreatedAt)
	l.sequence++
	mentions, replyTo := buzzMessageReferences(evt.Tags)
	message := buzzMessage{
		ID: evt.ID, Pubkey: evt.PubKey, Text: evt.Content,
		CreatedAt: int64(evt.CreatedAt), Sequence: l.sequence, Live: live,
		Mentions: mentions, ReplyTo: replyTo,
	}
	l.messages = append(l.messages, message)
	if l.readAll && l.owner != "" && live && time.Now().Before(l.lease) {
		if len(l.pendingSpeech) < 1024 {
			l.pendingSpeech[message.ID] = message
		} else {
			l.speechOverflow++
		}
	}
	if len(l.messages) > 128 {
		l.messages = append([]buzzMessage{}, l.messages[len(l.messages)-128:]...)
	}
}

func (l *buzzLive) listen(credentials buzzLookup) {
	start := time.Now().Unix()
	delay := time.Second
	for l.ctx.Err() == nil {
		l.setStatus("connecting")
		_ = l.subscribe(credentials, start)
		if l.ctx.Err() != nil {
			return
		}
		l.setStatus("reconnecting")
		select {
		case <-l.ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func (l *buzzLive) subscribe(credentials buzzLookup, start int64) error {
	key := credentials.PrivateKey
	if strings.HasPrefix(key, "nsec") {
		prefix, v, err := nip19.Decode(key)
		if err != nil || prefix != "nsec" {
			return errors.New("invalid key")
		}
		key = v.(string)
	}
	u, err := url.Parse(credentials.RelayURL)
	if err != nil {
		return err
	}
	u.Scheme = "wss"
	if u.Path == "" {
		u.Path = "/"
	}
	relay := u.String()
	conn, _, err := l.dialer.DialContext(l.ctx, relay, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(l.ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetReadLimit(128 * 1024)
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))

	pingCtx, pingCancel := context.WithCancel(l.ctx)
	defer pingCancel()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	authID := ""
	subscribed := false
	live := false
	write := func(v any) error {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(v)
	}
	for {
		var parts []json.RawMessage
		if err := conn.ReadJSON(&parts); err != nil {
			return err
		}
		if len(parts) < 2 {
			continue
		}
		var typ string
		_ = json.Unmarshal(parts[0], &typ)
		switch typ {
		case "AUTH":
			var challenge string
			if json.Unmarshal(parts[1], &challenge) != nil || len(challenge) > 4096 {
				return errors.New("invalid challenge")
			}
			if authID != "" {
				return errors.New("repeated challenge")
			}
			evt := nostr.Event{CreatedAt: nostr.Now(), Kind: 22242, Tags: nostr.Tags{{"relay", relay}, {"challenge", challenge}}, Content: ""}
			if credentials.AuthTag != "" {
				var tag nostr.Tag
				if json.Unmarshal([]byte(credentials.AuthTag), &tag) != nil {
					return errors.New("invalid auth tag")
				}
				evt.Tags = append(evt.Tags, tag)
			}
			if err := evt.Sign(key); err != nil {
				return err
			}
			authID = evt.ID
			if err := write([]any{"AUTH", evt}); err != nil {
				return err
			}
		case "OK":
			if len(parts) < 3 || subscribed {
				continue
			}
			var id string
			var ok bool
			_ = json.Unmarshal(parts[1], &id)
			_ = json.Unmarshal(parts[2], &ok)
			if id != authID || authID == "" {
				continue
			}
			if !ok {
				return errors.New("authentication rejected")
			}
			since := start
			if cutoff := time.Now().Unix() - 120; cutoff > since {
				since = cutoff
			}
			if err := write([]any{"REQ", "glowbom-live", map[string]any{"kinds": []int{9}, "#h": []string{credentials.ChannelID}, "since": since, "limit": 128}}); err != nil {
				return err
			}
			subscribed = true
		case "EOSE":
			var id string
			_ = json.Unmarshal(parts[1], &id)
			if subscribed && id == "glowbom-live" {
				live = true
				l.setStatus("listening")
				conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
				_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			}
		case "CLOSED":
			return errors.New("subscription closed")
		case "EVENT":
			if !subscribed || len(parts) < 3 {
				continue
			}
			var id string
			_ = json.Unmarshal(parts[1], &id)
			if id != "glowbom-live" {
				continue
			}
			var evt nostr.Event
			if json.Unmarshal(parts[2], &evt) == nil {
				l.accept(evt, credentials.ChannelID, start, live)
			}
		}
	}
}

// Split on whitespace when possible, preserving all Unicode text exactly.
func buzzSpeechChunks(text string) []string {
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		end := len(runes)
		if end > 500 {
			end = 500
			for i := 499; i >= 250; i-- {
				if unicode.IsSpace(runes[i]) {
					end = i + 1
					break
				}
			}
		}
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}
