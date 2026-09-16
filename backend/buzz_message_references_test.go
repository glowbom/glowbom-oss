package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

func liveReferenceEvent(t *testing.T, text, channel string, at int64, tags nostr.Tags) nostr.Event {
	t.Helper()
	event := liveEvent(t, text, channel, at)
	event.Tags = append(event.Tags, tags...)
	if err := event.Sign(strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestBuzzMessageMentionReferences(t *testing.T) {
	first := strings.Repeat("ab", 32)
	second := strings.Repeat("cd", 32)
	self, err := nostr.GetPublicKey(strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		tags nostr.Tags
		want []string
	}{
		{"absent", nil, []string{}},
		{"duplicate normalized", nostr.Tags{{"p", first}, {"p", strings.ToUpper(first), "wss://hint.invalid"}}, []string{first}},
		{"multiple in order", nostr.Tags{{"p", second}, {"p", first}, {"p", second}}, []string{second, first}},
		{"self retained", nostr.Tags{{"p", self}, {"p", first}}, []string{self, first}},
		{"malformed", nostr.Tags{{}, {"p"}, {"p", ""}, {"p", strings.Repeat("g", 64)}, {"p", first[:63]}, {"p", first + "0"}, {"p", " " + first}, {"p", "npub1invalid"}}, []string{}},
		{"only explicit p tags", nostr.Tags{{"P", first}, {"e", first}, {"q", first}, {"person", first}}, []string{}},
		{"valid alongside malformed", nostr.Tags{{"p", "invalid"}, {"p", first}, {"p"}}, []string{first}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newBuzzLive()
			defer l.close()
			now := time.Now().Unix()
			// Visible @names and Nostr URIs cannot create presentation targets.
			event := liveReferenceEvent(t, "@someone nostr:npub1invalid", "channel", now, tc.tags)
			l.accept(event, "channel", now, true)
			if len(l.messages) != 1 || !reflect.DeepEqual(l.messages[0].Mentions, tc.want) {
				t.Fatalf("mentions = %+v, want %v", l.messages, tc.want)
			}
		})
	}
}

func TestBuzzMessageReplyReferences(t *testing.T) {
	root := strings.Repeat("a", 64)
	parent := strings.Repeat("b", 64)
	other := strings.Repeat("c", 64)
	cases := []struct {
		name string
		tags nostr.Tags
		want string
	}{
		{"absent", nil, ""},
		{"direct reply", nostr.Tags{{"e", parent, "", "reply"}}, parent},
		{"nested immediate parent", nostr.Tags{{"e", root, "", "root"}, {"e", parent, "", "reply"}}, parent},
		{"bare e is top level", nostr.Tags{{"e", parent}}, ""},
		{"unmarked with relay is top level", nostr.Tags{{"e", parent, "wss://hint.invalid"}}, ""},
		{"root alone is top level", nostr.Tags{{"e", root, "", "root"}}, ""},
		{"wrong marker", nostr.Tags{{"e", parent, "", "mention"}, {"e", parent, "", "Reply"}, {"e", parent, "reply"}}, ""},
		{"malformed reply id", nostr.Tags{{"e", "bad", "", "reply"}, {"e", root, "", "root"}}, ""},
		{"invalid hex reply id", nostr.Tags{{"e", strings.Repeat("z", 64), "", "reply"}}, ""},
		{"overlong reply id", nostr.Tags{{"e", parent + "0", "", "reply"}}, ""},
		{"uppercase normalized", nostr.Tags{{"e", strings.ToUpper(parent), "", "reply"}}, parent},
		{"duplicate same reply", nostr.Tags{{"e", parent, "", "reply"}, {"e", parent, "", "reply"}}, parent},
		{"last valid conflicting reply matches Buzz", nostr.Tags{{"e", parent, "", "reply"}, {"e", other, "", "reply"}}, other},
		{"malformed final marker does not replace valid reply", nostr.Tags{{"e", parent, "", "reply"}, {"e", "bad", "", "reply"}}, parent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newBuzzLive()
			defer l.close()
			now := time.Now().Unix()
			event := liveReferenceEvent(t, "reply text", "channel", now, tc.tags)
			l.accept(event, "channel", now, true)
			if len(l.messages) != 1 || l.messages[0].ReplyTo != tc.want {
				t.Fatalf("reply = %+v, want %q", l.messages, tc.want)
			}
		})
	}
}

func TestBuzzMessageReferencesStayScopedAndDetached(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	now := time.Now().Unix()
	person := strings.Repeat("ab", 32)
	parent := strings.Repeat("cd", 32)
	tags := nostr.Tags{{"p", person}, {"e", parent, "", "reply"}}
	history := liveReferenceEvent(t, "recent catch-up", "channel", now, tags)
	l.accept(history, "channel", now, false)
	l.accept(history, "channel", now, true)
	if len(l.messages) != 1 || l.messages[0].Live || l.messages[0].ReplyTo != parent || !reflect.DeepEqual(l.messages[0].Mentions, []string{person}) {
		t.Fatal("history metadata or no-replay classification changed")
	}
	// The retained message must not borrow the caller's tag array.
	history.Tags[1][1] = strings.Repeat("e", 64)
	history.Tags[2][1] = strings.Repeat("f", 64)
	if l.messages[0].Mentions[0] != person || l.messages[0].ReplyTo != parent {
		t.Fatal("source tag mutation changed the retained references")
	}
	wrongChannel := liveReferenceEvent(t, "elsewhere", "other", now, tags)
	l.accept(wrongChannel, "channel", now, true)
	wrongKind := liveReferenceEvent(t, "another kind", "channel", now, tags)
	wrongKind.Kind = 1
	if err := wrongKind.Sign(strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	l.accept(wrongKind, "channel", now, true)
	forged := liveReferenceEvent(t, "forged tags", "channel", now, tags)
	forged.Tags[1][1] = strings.Repeat("9", 64)
	l.accept(forged, "channel", now, true)
	badSignature := liveReferenceEvent(t, "forged signature", "channel", now, tags)
	badSignature.Sig = strings.Repeat("0", 128)
	l.accept(badSignature, "channel", now, true)
	l.accept(liveReferenceEvent(t, "before session", "channel", now-5, tags), "channel", now, true)
	if len(l.messages) != 1 || l.sequence != 1 {
		t.Fatal("references bypassed event kind, signature, time, or channel checks")
	}
}

func TestBuzzMessageReferencesJSONAndPendingSpeech(t *testing.T) {
	l := newBuzzLive()
	defer l.close()
	l.key = "fixture"
	s := &buzzSession{connected: true, live: l}
	if response := liveRequest(s, `{"clientId":"`+liveTestClient+`","enabled":true,"readAll":true}`); response.Code != 204 {
		t.Fatal(response.Code)
	}
	now := time.Now().Unix()
	person := strings.Repeat("ab", 32)
	parent := strings.Repeat("cd", 32)
	event := liveReferenceEvent(t, "queued direct mention", "channel", now, nostr.Tags{{"p", person}, {"e", parent, "", "reply"}})
	l.accept(event, "channel", now, true)
	for index := 0; index < 128; index++ {
		l.accept(liveEvent(t, fmt.Sprintf("later %d", index), "channel", now), "channel", now, true)
	}
	if len(l.messages) != 128 || l.messages[0].ID == event.ID {
		t.Fatal("fixture did not evict the first message from the ordinary feed")
	}
	response := httptest.NewRecorder()
	s.serveLive(response, httptest.NewRequest(http.MethodGet, "/buzz/session/messages?clientId="+liveTestClient, nil))
	var feed struct {
		Messages []buzzMessage `json:"messages"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &feed) != nil || len(feed.Messages) != 129 {
		t.Fatal("could not read merged pending feed")
	}
	first := feed.Messages[0]
	if first.ID != event.ID || first.ReplyTo != parent || !reflect.DeepEqual(first.Mentions, []string{person}) || !first.Live {
		t.Fatalf("pending metadata changed: %+v", first)
	}
	if feed.Messages[1].Mentions == nil || feed.Messages[1].ReplyTo != "" {
		t.Fatal("empty references must serialize as an array and empty string")
	}
	feed.Messages[0].Mentions[0] = "client-side edit"
	if l.pendingSpeech[event.ID].Mentions[0] != person {
		t.Fatal("response mutation changed the pending reference")
	}
}
