package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testBuzzLookup() buzzLookup {
	return buzzLookup{RelayURL: "https://relay.example", ChannelID: "00000000-0000-0000-0000-000000000001", PrivateKey: strings.Repeat("1", 64)}
}

func TestBuzzMembersLookup(t *testing.T) {
	one, two := strings.Repeat("a", 64), strings.Repeat("b", 64)
	calls := 0
	members, err := lookupBuzzMembers(context.Background(), testBuzzLookup(), func(_ context.Context, _ buzzLookup, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			if !reflect.DeepEqual(args, []string{"channels", "members", "--channel", testBuzzLookup().ChannelID}) {
				t.Fatalf("unexpected member command: %v", args)
			}
			return []byte(`[{"pubkey":"` + one + `","role":"admin"},{"pubkey":"` + two + `"},{"pubkey":"` + one + `"}]`), nil
		}
		if !reflect.DeepEqual(args, []string{"users", "get", "--pubkey", one, "--pubkey", two}) {
			t.Fatalf("unexpected profile command: %v", args)
		}
		return []byte(`[{"pubkey":"` + one + `","display_name":"Pulse","about":"Hermes agent\nLocal workspace","private_field":"excluded"}]`), nil
	})
	if err != nil || calls != 2 || len(members) != 2 || members[0].DisplayName != "Pulse" || members[0].DefaultLabel != "Hermes agent Local workspace" || members[1].DisplayName != two[:12] || members[1].Role != "member" {
		t.Fatalf("unexpected result: %+v, %v", members, err)
	}
}

func TestBuzzMembersHandlerGuards(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "test-server-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://localhost:4572")
	for _, tc := range []struct {
		name, method, token, origin, body, contentType string
		status                                         int
	}{
		{"method", "GET", "test-server-token", "", `{}`, "application/json", 405},
		{"token", "POST", "wrong", "", `{}`, "application/json", 403},
		{"origin", "POST", "test-server-token", "https://untrusted.example", `{}`, "application/json", 403},
		{"content type", "POST", "test-server-token", "", `{}`, "text/plain", 415},
		{"invalid input", "POST", "test-server-token", "", `{}`, "application/json", 400},
		{"too large", "POST", "test-server-token", "", strings.Repeat(" ", 17000) + `{}`, "application/json", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBuzzMembersHandler(func(context.Context, buzzLookup, ...string) ([]byte, error) {
				t.Fatal("unexpected CLI call")
				return nil, nil
			})
			r := httptest.NewRequest(tc.method, "/buzz/members", strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
		})
	}
}

func TestBuzzHandlerResponseAndCredentialRedaction(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "test-server-token")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	for _, tc := range []struct {
		name   string
		result string
		err    error
		status int
	}{
		{"empty", `[]`, nil, 200},
		{"malformed", `{}`, nil, 502},
		{"null", `null`, nil, 502},
		{"CLI error", "", errors.New(testBuzzLookup().PrivateKey), 502},
		{"missing CLI", "", errBuzzUnavailable, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(testBuzzLookup())
			r := httptest.NewRequest("POST", "/buzz/members", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer test-server-token")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h := withGlowbomSecurity(newBuzzMembersHandler(func(context.Context, buzzLookup, ...string) ([]byte, error) { return []byte(tc.result), tc.err }))
			h.ServeHTTP(w, r)
			if w.Code != tc.status || strings.Contains(w.Body.String(), testBuzzLookup().PrivateKey) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unsafe or unexpected response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestBuzzRequiresConfiguredLocalAuthentication(t *testing.T) {
	for _, tc := range []struct{ token, bind string }{{"", "127.0.0.1"}, {"test", "0.0.0.0"}} {
		t.Setenv("GLOWBOM_SERVER_TOKEN", tc.token)
		t.Setenv("GLOWBY_SERVER_TOKEN", "")
		t.Setenv("GLOWBOM_BIND_HOST", tc.bind)
		r := httptest.NewRequest("POST", "/buzz/members", nil)
		w := httptest.NewRecorder()
		newBuzzMembersHandler(nil).ServeHTTP(w, r)
		if w.Code != 503 {
			t.Fatalf("expected disabled endpoint, got %d", w.Code)
		}
	}
}

func TestBuzzInputValidation(t *testing.T) {
	if !validBuzzLookup(testBuzzLookup()) {
		t.Fatal("valid request rejected")
	}
	for _, mutate := range []func(*buzzLookup){
		func(v *buzzLookup) { v.RelayURL = "http://relay.example" },
		func(v *buzzLookup) { v.RelayURL = "https://secret@relay.example" },
		func(v *buzzLookup) { v.RelayURL = "https://relay.example?key=secret" },
		func(v *buzzLookup) { v.ChannelID = "--help" },
		func(v *buzzLookup) { v.PrivateKey = "invalid" },
		func(v *buzzLookup) { v.AuthTag = `{"auth":"secret"}` },
	} {
		v := testBuzzLookup()
		mutate(&v)
		if validBuzzLookup(v) {
			t.Fatal("invalid input accepted")
		}
	}
}

func TestBuzzEnvironmentDoesNotReuseCredentials(t *testing.T) {
	v := testBuzzLookup()
	env := buzzEnvironment([]string{"PATH=/bin", "BUZZ_AUTH_TAG=old", "BUZZ_PRIVATE_KEY=old", "BUZZ_RELAY_URL=old", "GLOWBOM_SERVER_TOKEN=private", "OPENAI_API_KEY=private"}, v)
	if !reflect.DeepEqual(env, []string{"PATH=/bin", "BUZZ_RELAY_URL=" + v.RelayURL, "BUZZ_PRIVATE_KEY=" + v.PrivateKey}) {
		t.Fatal("environment included unrelated credentials")
	}
	v.AuthTag = `["auth","test"]`
	env = buzzEnvironment(nil, v)
	if env[len(env)-1] != "BUZZ_AUTH_TAG="+v.AuthTag {
		t.Fatal("missing explicit auth tag")
	}
}

func TestBuzzSubprocessUsesEnvironmentAndHonorsCancellation(t *testing.T) {
	// Run this test executable as a fake CLI. No relay or actual key is used.
	t.Setenv("GLOWBOM_BUZZ_CLI", os.Args[0])
	v := testBuzzLookup()
	raw, err := runBuzzRead(context.Background(), v, "-test.run=^TestBuzzCLIHelper$", "--", "fake-cli")
	if err != nil || string(raw) != "[]\n" {
		t.Fatalf("helper failed: %q %v", raw, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runBuzzRead(ctx, v, "-test.run=^TestBuzzCLIHelper$", "--", "fake-cli"); err == nil {
		t.Fatal("cancelled command succeeded")
	}
	t.Setenv("GLOWBOM_BUZZ_CLI", filepath.Join(t.TempDir(), "missing"))
	if _, err := runBuzzRead(context.Background(), v); !errors.Is(err, errBuzzUnavailable) {
		t.Fatal("missing executable not reported")
	}
}

func TestBuzzCLIHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "fake-cli" {
		return
	}
	if os.Getenv("BUZZ_PRIVATE_KEY") != strings.Repeat("1", 64) || strings.Contains(strings.Join(os.Args, " "), strings.Repeat("1", 64)) || os.Getenv("BUZZ_AUTH_TAG") != "" {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString("[]\n")
	os.Exit(0)
}

func TestBuzzOutputLimit(t *testing.T) {
	var output buzzLimitedOutput
	if _, err := output.Write(make([]byte, 2*1024*1024+1)); err == nil || output.Len() != 0 {
		t.Fatal("unbounded output")
	}
}

func TestBuzzCancelledLookup(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "test")
	t.Setenv("GLOWBOM_BIND_HOST", "127.0.0.1")
	body, _ := json.Marshal(testBuzzLookup())
	r := httptest.NewRequest(http.MethodPost, "/buzz/members", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer test")
	r.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(r.Context(), time.Nanosecond)
	defer cancel()
	w := httptest.NewRecorder()
	newBuzzMembersHandler(func(ctx context.Context, _ buzzLookup, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}).ServeHTTP(w, r.WithContext(ctx))
	if w.Code != 504 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestBuzzLabelBounds(t *testing.T) {
	if buzzLabel("  one\n two\tthree ") != "one two three" {
		t.Fatal("whitespace")
	}
	if len([]rune(buzzLabel(strings.Repeat("界", 100)))) != 80 {
		t.Fatal("unbounded label")
	}
	if validBuzzProfile(buzzProfile{CustomLabel: strings.Repeat("x", 81)}) || validBuzzProfile(buzzProfile{CustomLabel: "line\nbreak"}) {
		t.Fatal("invalid custom label")
	}
	if !validBuzzProfile(buzzProfile{CustomLabel: "Codex · My laptop", HideLabel: true}) {
		t.Fatal("valid custom label rejected")
	}
}
