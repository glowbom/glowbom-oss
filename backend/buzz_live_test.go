package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nbd-wtf/go-nostr"
)

const liveTestClient = "fixture-client-123456"

func liveEvent(t *testing.T, text, channel string, at int64) nostr.Event {
	t.Helper()
	e := nostr.Event{Kind: 9, CreatedAt: nostr.Timestamp(at), Content: text, Tags: nostr.Tags{{"h", channel}}}
	if err := e.Sign(strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestBuzzLiveDedupAndScope(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	now := time.Now().Unix()
	e := liveEvent(t, "hello", "channel", now)
	l.accept(e, "channel", now, true)
	l.accept(e, "channel", now, true)
	l.accept(liveEvent(t, "wrong channel", "other", now), "channel", now, true)
	l.accept(liveEvent(t, "history", "channel", now-10), "channel", now, true)
	e.Content = "forged"
	l.accept(e, "channel", now, true)
	if len(l.messages) != 1 || l.sequence != 1 {
		t.Fatalf("unexpected messages: %d", len(l.messages))
	}
	l.close()
	l.accept(liveEvent(t, "late", "channel", now), "channel", now, true)
	if len(l.messages) != 0 {
		t.Fatal("late event survived disconnect")
	}
}

func TestBuzzLiveClockSkew(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		name   string
		offset int64
		live   bool
		want   int
	}{
		{"current message", 0, true, 1},
		{"sender clock slightly ahead", 10, true, 1},
		{"catch-up stays silent", 10, false, 1},
		{"far future rejected", 180, true, 0},
		{"before listener start rejected", -10, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newBuzzLive()
			defer l.close()
			l.accept(liveEvent(t, "clock skew", "channel", now+tc.offset), "channel", now, tc.live)
			if len(l.messages) != tc.want {
				t.Fatalf("got %d messages, want %d", len(l.messages), tc.want)
			}
			if tc.want > 0 && l.messages[0].Live != tc.live {
				t.Fatal("clock tolerance changed speech eligibility")
			}
		})
	}
}

func liveRequest(s *buzzSession, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/buzz/session/speech", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.serveLive(w, r)
	return w
}
func TestBuzzLiveSpeechGates(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture-key"
	s := &buzzSession{connected: true, live: l}
	calls := 0
	l.synth = func(context.Context, string, string, string) ([]byte, error) {
		calls++
		return []byte("fixture audio"), nil
	}
	now := time.Now().Unix()
	old := liveEvent(t, "before enabling", "channel", now)
	l.accept(old, "channel", now, true)
	body := func(id string) string {
		b, _ := json.Marshal(map[string]string{"clientId": liveTestClient, "eventId": id})
		return string(b)
	}
	if w := liveRequest(s, body(old.ID)); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true}`); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := liveRequest(s, body(old.ID)); w.Code != 410 {
		t.Fatal(w.Code)
	}
	if w := liveRequest(s, `{"clientId":"another-client-123456","enabled":true}`); w.Code != 409 {
		t.Fatal(w.Code)
	}
	fresh := liveEvent(t, "new message", "channel", now)
	l.accept(fresh, "channel", now, true)
	if w := liveRequest(s, body(fresh.ID)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := liveRequest(s, body(fresh.ID)); w.Code != 410 {
		t.Fatal(w.Code)
	}
	history := liveEvent(t, "reconnect history", "channel", now)
	l.accept(history, "channel", now, false)
	if w := liveRequest(s, body(history.ID)); w.Code != 410 {
		t.Fatal(w.Code)
	}
	if calls != 1 {
		t.Fatalf("generation count %d", calls)
	}
	liveRequest(s, `{"key":""}`)
	if l.owner != "" || l.key != "" {
		t.Fatal("clearing key did not mute")
	}
	if w := liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true}`); w.Code != 412 {
		t.Fatal(w.Code)
	}
}
func TestBuzzLiveMuteCancelsGeneration(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture"
	s := &buzzSession{connected: true, live: l}
	started := make(chan struct{})
	done := make(chan int, 1)
	l.synth = func(ctx context.Context, _ string, _ string, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true}`)
	now := time.Now().Unix()
	e := liveEvent(t, "new", "channel", now)
	l.accept(e, "channel", now, true)
	go func() { done <- liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+e.ID+`"}`).Code }()
	<-started
	liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":false}`)
	select {
	case code := <-done:
		if code != 502 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("generation not cancelled")
	}
}

func TestBuzzLiveAuthenticatedSubscription(t *testing.T) {
	now := time.Now().Unix()
	event := liveEvent(t, "hello from relay", "channel", now)
	verified := make(chan bool, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.WriteJSON([]any{"AUTH", "fixture-challenge"})
		var parts []json.RawMessage
		if c.ReadJSON(&parts) != nil || len(parts) != 2 {
			return
		}
		var auth nostr.Event
		if json.Unmarshal(parts[1], &auth) != nil {
			return
		}
		valid, _ := auth.CheckSignature()
		verified <- valid && auth.Kind == 22242 && auth.Tags.GetFirst([]string{"challenge", "fixture-challenge"}) != nil
		_ = c.WriteJSON([]any{"OK", auth.ID, true, ""})
		if c.ReadJSON(&parts) != nil || len(parts) != 3 {
			return
		}
		// This first event is catch-up and must never be eligible for speech.
		_ = c.WriteJSON([]any{"EVENT", "glowbom-live", event})
		_ = c.WriteJSON([]any{"EOSE", "glowbom-live"})
		_ = c.WriteJSON([]any{"EVENT", "glowbom-live", event})
		for {
			if _, _, err = c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	l := newBuzzLive()
	defer l.close()
	l.dialer = &websocket.Dialer{TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig, HandshakeTimeout: time.Second}
	done := make(chan error, 1)
	go func() {
		done <- l.subscribe(buzzLookup{RelayURL: server.URL, ChannelID: "channel", PrivateKey: strings.Repeat("1", 64)}, now)
	}()
	select {
	case ok := <-verified:
		if !ok {
			t.Fatal("invalid AUTH")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no AUTH")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		ready := l.status == "listening" && len(l.messages) == 1
		historical := len(l.messages) == 1 && !l.messages[0].Live
		l.mu.Unlock()
		if ready {
			if !historical {
				t.Fatal("history became live")
			}
			l.close()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("subscription never ready")
}

func TestBuzzLiveEndpointProtectionAndDisconnect(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "session-test")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	l := newBuzzLive()
	defer l.close()
	s := &buzzSession{connected: true, live: l}
	for _, path := range []string{"/buzz/session/messages", "/buzz/session/speech"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 403 {
			t.Fatal("missing auth accepted", path, w.Code)
		}
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer session-test")
		r.Header.Set("Origin", "https://untrusted.example")
		w = httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("foreign origin accepted")
		}
	}
	l.key = "secret-fixture"
	response := sessionRequest(s, "GET", "/buzz/session/messages", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "secret-fixture") {
		t.Fatal("invalid status or secret exposure")
	}
	if sessionRequest(s, "DELETE", "/buzz/session", "").Code != 200 || l.ctx.Err() == nil || s.live != nil {
		t.Fatal("disconnect retained listener")
	}
	if sessionRequest(s, "GET", "/buzz/session/messages", "").Code != 409 {
		t.Fatal("disconnected feed available")
	}
}

func TestBuzzLiveSpeechLimitsAndLease(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture"
	s := &buzzSession{connected: true, live: l}
	received := 0
	l.synth = func(_ context.Context, _ string, text string, _ string) ([]byte, error) {
		received = len([]rune(text))
		return []byte("audio"), nil
	}
	liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true}`)
	now := time.Now().Unix()
	e := liveEvent(t, strings.Repeat("語", 700), "channel", now)
	l.accept(e, "channel", now, true)
	if liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+e.ID+`"}`).Code != 200 || received != 500 {
		t.Fatal("speech length not bounded")
	}
	stale := liveEvent(t, "stale", "channel", now-40)
	l.accept(stale, "channel", now-60, true)
	if liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+stale.ID+`"}`).Code != 410 {
		t.Fatal("stale speech accepted")
	}
	l.mu.Lock()
	l.lease = time.Now().Add(-time.Second)
	l.mu.Unlock()
	if liveRequest(s, `{"clientId":"`+liveTestClient+`","eventId":"`+e.ID+`"}`).Code != 409 {
		t.Fatal("expired lease accepted")
	}
}

func TestBuzzReadAllBacklogAndChunks(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture"
	s := &buzzSession{connected: true, live: l}
	var spoken []string
	l.synth = func(_ context.Context, _, text, _ string) ([]byte, error) {
		spoken = append(spoken, text)
		return []byte("audio"), nil
	}
	if w := liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true,"readAll":true}`); w.Code != 204 {
		t.Fatal(w.Code)
	}
	now := time.Now().Unix()
	text := strings.Repeat("Hello 世界. ", 170)
	event := liveEvent(t, text, "channel", now-60)
	l.accept(event, "channel", now-120, true)
	// A queued message must survive eviction from the ordinary 128-event feed.
	for i := 0; i < 140; i++ {
		l.accept(liveEvent(t, strings.Repeat("x", i+1), "channel", now), "channel", now-120, true)
	}
	req := func(chunk int) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"clientId": liveTestClient, "eventId": event.ID, "chunk": chunk})
		return liveRequest(s, string(b))
	}
	if w := req(1); w.Code != 410 {
		t.Fatal("out-of-order chunk accepted", w.Code)
	}
	chunks := buzzSpeechChunks(text)
	for i := range chunks {
		w := req(i)
		if w.Code != 200 {
			t.Fatal("backlog chunk rejected", i, w.Code)
		}
		if (w.Header().Get("X-Buzz-Speech-More") == "true") != (i < len(chunks)-1) {
			t.Fatal("incorrect continuation header")
		}
		if w := req(i); w.Code != 410 {
			t.Fatal("duplicate generated", w.Code)
		}
	}
	if strings.Join(spoken, "") != text {
		t.Fatal("text lost during segmentation")
	}
	for _, part := range spoken {
		if len([]rune(part)) > 500 {
			t.Fatal("segment too long")
		}
	}
	if _, ok := l.pendingSpeech[event.ID]; ok {
		t.Fatal("completed message retained")
	}
	liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":false}`)
	if len(l.pendingSpeech) != 0 || len(l.nextChunk) != 0 || l.readAll {
		t.Fatal("mute did not clear backlog")
	}
	if w := req(0); w.Code != 409 {
		t.Fatal("speech survived mute")
	}
}

func TestBuzzReadAllFeedRetentionAndCapacity(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture"
	s := &buzzSession{connected: true, live: l}
	liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true,"readAll":true}`)
	now := time.Now().Unix()
	for i := 0; i < 1030; i++ {
		l.accept(liveEvent(t, strings.Repeat("x", i+1), "channel", now), "channel", now, true)
	}
	if len(l.pendingSpeech) != 1024 || l.speechOverflow != 6 {
		t.Fatal("capacity not enforced", len(l.pendingSpeech), l.speechOverflow)
	}
	w := httptest.NewRecorder()
	s.serveLive(w, httptest.NewRequest("GET", "/buzz/session/messages?clientId="+liveTestClient, nil))
	var data struct {
		Messages  []buzzMessage `json:"messages"`
		Supported bool          `json:"readAllSupported"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if !data.Supported || len(data.Messages) != 1030 || data.Messages[0].Sequence != 1 {
		t.Fatal("queued events missing from feed")
	}
}
