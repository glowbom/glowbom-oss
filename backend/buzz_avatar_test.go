package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func avatarFixture() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

func connectedAvatarSession(t *testing.T, read buzzReadFunc) *buzzSession {
	t.Helper()
	t.Setenv("GLOWBOM_SERVER_TOKEN", "session-test")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	s := newBuzzSession(read)
	s.connected = true
	s.credentials = testBuzzLookup()
	s.mediaCtx, s.mediaCancel = context.WithCancel(context.Background())
	t.Cleanup(s.mediaCancel)
	s.members = []buzzMember{{Pubkey: strings.Repeat("a", 64), DisplayName: "Fixture", PictureURL: "https://relay.example/media/" + strings.Repeat("b", 64) + ".png"}}
	return s
}

func TestBuzzProfilePictures(t *testing.T) {
	for _, raw := range []string{"https://pictures.example/avatar.png", "/media/" + strings.Repeat("b", 64) + ".png", "javascript:alert(1)", "file:///secret", "https://user:secret@example.com/a.png", "", "data:image/png;base64,test"} {
		t.Run(raw, func(t *testing.T) {
			pubkey := strings.Repeat("a", 64)
			members, err := lookupBuzzMembers(context.Background(), testBuzzLookup(), func(_ context.Context, _ buzzLookup, args ...string) ([]byte, error) {
				if args[0] == "channels" {
					return []byte(`[{"pubkey":"` + pubkey + `","pictureUrl":"https://untrusted-roster.example/pic.png"}]`), nil
				}
				return json.Marshal([]map[string]string{{"pubkey": pubkey, "picture": raw}})
			})
			if err != nil || len(members) != 1 {
				t.Fatalf("lookup: %v", err)
			}
			want := ""
			if strings.HasPrefix(raw, "https://pictures.example/") {
				want = raw
			}
			if strings.HasPrefix(raw, "/media/") {
				want = "https://relay.example" + raw
			}
			if members[0].PictureURL != want || members[0].DisplayName != pubkey[:12] {
				t.Fatalf("unexpected profile: %+v", members[0])
			}
		})
	}
}

func TestBuzzAvatarDownload(t *testing.T) {
	var s *buzzSession
	s = connectedAvatarSession(t, func(_ context.Context, credentials buzzLookup, args ...string) ([]byte, error) {
		if credentials != testBuzzLookup() || !reflect.DeepEqual(args, []string{"media", "get", s.members[0].PictureURL, "--output", "-"}) {
			t.Fatal("incorrect media request")
		}
		return avatarFixture(), nil
	})
	w := sessionRequest(s, "GET", "/buzz/session/avatar?pubkey="+s.members[0].Pubkey, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" || !bytes.Equal(w.Body.Bytes(), avatarFixture()) {
		t.Fatalf("unexpected image response: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), testBuzzLookup().PrivateKey) {
		t.Fatal("credential in image response")
	}
}

func TestBuzzAvatarGuards(t *testing.T) {
	s := connectedAvatarSession(t, func(context.Context, buzzLookup, ...string) ([]byte, error) {
		t.Fatal("unexpected media request")
		return nil, nil
	})
	path := "/buzz/session/avatar?pubkey=" + s.members[0].Pubkey
	for _, tc := range []struct {
		method, token, origin string
		status                int
	}{
		{"GET", "wrong", "", 403}, {"GET", "session-test", "https://untrusted.example", 403}, {"POST", "session-test", "", 405},
	} {
		r := httptest.NewRequest(tc.method, path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("got %d want %d", w.Code, tc.status)
		}
	}
	if sessionRequest(s, "GET", "/buzz/session/avatar?pubkey=bad", "").Code != 400 {
		t.Fatal("invalid identity accepted")
	}
	if sessionRequest(s, "GET", "/buzz/session/avatar?pubkey="+strings.Repeat("c", 64), "").Code != 404 {
		t.Fatal("nonmember accepted")
	}
	for _, url := range []string{"https://other.example/media/" + strings.Repeat("b", 64) + ".png", "https://relay.example/private", "https://relay.example/media/../private", "https://relay.example/media/%2e%2e", "http://relay.example/media/" + strings.Repeat("b", 64)} {
		s.members[0].PictureURL = url
		if sessionRequest(s, "GET", path, "").Code != 404 {
			t.Fatal("unsafe target accepted")
		}
	}
	s.connected = false
	if sessionRequest(s, "GET", path, "").Code != 409 {
		t.Fatal("disconnected request accepted")
	}
}

func TestBuzzAvatarRejectsNonImagesAndLargeBodies(t *testing.T) {
	for _, data := range [][]byte{[]byte("<svg>untrusted</svg>"), []byte("<html>private diagnostic</html>"), bytes.Repeat([]byte("x"), 2*1024*1024+1)} {
		s := connectedAvatarSession(t, func(context.Context, buzzLookup, ...string) ([]byte, error) { return data, nil })
		w := sessionRequest(s, "GET", "/buzz/session/avatar?pubkey="+s.members[0].Pubkey, "")
		if w.Code == http.StatusOK || w.Body.Len() != 0 {
			t.Fatal("invalid picture forwarded")
		}
	}
}

func TestBuzzDisconnectCancelsAvatar(t *testing.T) {
	started := make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	s := connectedAvatarSession(t, func(ctx context.Context, _ buzzLookup, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return avatarFixture(), nil
	})
	path := "/buzz/session/avatar?pubkey=" + s.members[0].Pubkey
	go func() { done <- sessionRequest(s, "GET", path, "") }()
	<-started
	sessionRequest(s, "DELETE", "/buzz/session", "")
	if w := <-done; w.Code == 200 || w.Body.Len() != 0 {
		t.Fatal("image returned after disconnect")
	}
}
