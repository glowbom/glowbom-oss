package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallbackRejectsInvalidRequestsAndConsumesOnce(t *testing.T) {
	state, code := strings.Repeat("s", 43), strings.Repeat("c", 43)
	callback, err := newLoginCallback(state)
	if err != nil {
		t.Fatal(err)
	}
	defer callback.close()
	var exchanges atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	callback.serve(context.Background(), func(received string) error {
		exchanges.Add(1)
		if received != code {
			t.Error("unexpected authorization code")
		}
		close(entered)
		<-release
		return nil
	})
	address, _ := url.Parse(callback.redirectURI())
	if address.Hostname() != "127.0.0.1" || !callback.listener.Addr().(*net.TCPAddr).IP.IsLoopback() {
		t.Fatal("listener was not bound to loopback")
	}
	validQuery := "?code=" + code + "&state=" + state
	for _, test := range []struct{ method, path, query, host string }{
		{"GET", "/callback", "?code=" + code + "&state=wrong", ""},
		{"GET", "/callback", validQuery + "&state=" + state, ""},
		{"GET", "/callback", validQuery + "&code=" + code, ""},
		{"GET", "/callback", validQuery + "&error=access_denied", ""},
		{"GET", "/callback", validQuery + "&extra=value", ""},
		{"GET", "/callback", "?code=short&state=" + state, ""},
		{"GET", "/callback", "?error=unknown&state=" + state, ""},
		{"GET", "/callback", validQuery, "evil.example"},
		{"GET", "/elsewhere", validQuery, ""},
		{"GET", "/%63allback", validQuery, ""},
		{"POST", "/callback", validQuery, ""},
		{"OPTIONS", "/callback", validQuery, ""},
	} {
		req, _ := http.NewRequest(test.method, address.Scheme+"://"+address.Host+test.path+test.query, nil)
		if test.host != "" {
			req.Host = test.host
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode < 400 || exchanges.Load() != 0 {
			t.Fatalf("accepted invalid callback: %s %s", test.method, test.path)
		}
	}
	finished := make(chan error, 1)
	go func() {
		response, err := http.Get(callback.redirectURI() + validQuery)
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || !strings.Contains(string(body), "Build something great.") ||
				strings.Contains(string(body), code) || strings.Contains(string(body), state) ||
				response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != "no-referrer" {
				err = errors.New("unsafe or incorrect callback page")
			}
		}
		finished <- err
	}()
	<-entered
	response, err := http.Get(callback.redirectURI() + validQuery)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict || exchanges.Load() != 1 {
		t.Error("concurrent callback was accepted")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-callback.result; err != nil {
		t.Fatal(err)
	}
}

func TestCallbackDenialNeverExchangesCredentials(t *testing.T) {
	callback, err := newLoginCallback(strings.Repeat("s", 43))
	if err != nil {
		t.Fatal(err)
	}
	defer callback.close()
	callback.serve(context.Background(), func(string) error { t.Error("denied callback exchanged credentials"); return nil })
	response, err := http.Get(callback.redirectURI() + "?error=access_denied&state=" + callback.state)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if err := <-callback.result; err == nil || !strings.Contains(string(body), "Connection canceled") {
		t.Fatal("cancellation was not reported")
	}
}

type failingLoginStore struct{ memoryAccountStore }

func (*failingLoginStore) Save(accountCredentials) error { return errors.New("keyring unavailable") }

func TestDesktopLoginExchangesCallbackAndClosesListener(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		for _, noBrowser := range []bool{false, true} {
			t.Run(map[bool]string{false: "success", true: "save failure"}[failSave]+map[bool]string{false: " with browser", true: " without browser"}[noBrowser], func(t *testing.T) {
				var store accountStore = &memoryAccountStore{}
				if failSave {
					store = &failingLoginStore{}
				}
				var output bytes.Buffer
				var redirect, state, challenge string
				var exchanges, completed atomic.Int32
				code := strings.Repeat("c", 43)
				started, browserFinished := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]string
					json.NewDecoder(r.Body).Decode(&body)
					switch body["action"] {
					case "start":
						redirect, state, challenge = body["redirectUri"], body["state"], body["codeChallenge"]
						if body["mode"] != "callback" || !callbackCodePattern.MatchString(state) {
							t.Error("desktop login did not request a secure callback")
						}
						json.NewEncoder(w).Encode(map[string]any{"mode": "callback", "sessionId": strings.Repeat("s", 32), "deviceCode": strings.Repeat("d", 43), "userCode": "ABCD-EFGH", "expiresIn": 30, "interval": 1})
						close(started)
					case "exchange":
						exchanges.Add(1)
						proof := sha256.Sum256([]byte(body["codeVerifier"]))
						if body["authorizationCode"] != code || body["redirectUri"] != redirect ||
							body["deviceCode"] != strings.Repeat("d", 43) || base64.RawURLEncoding.EncodeToString(proof[:]) != challenge {
							t.Error("callback exchange lost its proof or redirect binding")
						}
						json.NewEncoder(w).Encode(struct {
							Status string `json:"status"`
							accountCredentials
						}{"authorized", testCredentials()})
					case "complete":
						completed.Add(1)
						stored, _ := store.Load()
						if failSave || stored.IDToken != "private-id-token" || r.Header.Get("Authorization") != "Bearer private-id-token" {
							t.Error("server completion preceded credential storage")
						}
						json.NewEncoder(w).Encode(map[string]string{"status": "complete"})
					default:
						t.Error("unexpected action")
					}
				}))
				defer server.Close()
				visit := func(openCtx context.Context, link string) error {
					defer close(browserFinished)
					<-started
					if strings.Contains(link, state) || strings.Contains(link, "redirectUri") || strings.Contains(link, "ABCD") {
						t.Error("browser login link contains unnecessary secrets")
					}
					response, err := http.Get(redirect + "?code=" + code + "&state=" + state)
					if err != nil {
						t.Error(err)
						return err
					}
					body, _ := io.ReadAll(response.Body)
					response.Body.Close()
					if strings.Contains(string(body), "Build something great.") == failSave {
						t.Error("browser success did not reflect credential storage")
					}
					// A browser launcher may stay alive after successfully opening the tab.
					if !noBrowser {
						<-openCtx.Done()
					}
					return nil
				}
				client := &accountClient{config: accountConfig{APIURL: server.URL, LoginURL: "https://glowbom.com/draw/"},
					http: server.Client(), store: store, out: &output, open: visit}
				if noBrowser {
					client.open = func(context.Context, string) error { t.Error("--no-browser called the opener"); return nil }
					go visit(context.Background(), "manual browser")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err := client.login(ctx, noBrowser, false)
				<-browserFinished
				if (err != nil) != failSave || exchanges.Load() != 1 || (completed.Load() == 1) == failSave {
					t.Fatalf("unexpected login outcome: %v", err)
				}
				address, _ := url.Parse(redirect)
				connection, err := net.DialTimeout("tcp", address.Host, time.Second)
				if err == nil {
					connection.Close()
					t.Error("listener remained open after login")
				}
				for _, secret := range []string{code, state, "private-", "ABCD-EFGH", strings.Repeat("d", 43)} {
					if strings.Contains(output.String(), secret) {
						t.Error("desktop login printed a secret or unused terminal code")
					}
				}
			})
		}
	}
}

func TestDesktopLoginCancellationClosesListenerWithoutPolling(t *testing.T) {
	var redirect string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["action"] != "start" {
			t.Error("callback flow polled the server")
		}
		redirect = body["redirectUri"]
		json.NewEncoder(w).Encode(map[string]any{"mode": "callback", "sessionId": strings.Repeat("s", 32), "deviceCode": strings.Repeat("d", 43), "userCode": "ABCD-EFGH", "expiresIn": 30, "interval": 1})
	}))
	defer server.Close()
	client := &accountClient{config: accountConfig{APIURL: server.URL, LoginURL: "https://glowbom.com/draw/"},
		http: server.Client(), store: &memoryAccountStore{}, out: io.Discard, open: func(openCtx context.Context, _ string) error {
			cancel()
			<-openCtx.Done()
			return openCtx.Err()
		}}
	if err := client.login(ctx, false, false); err == nil {
		t.Fatal("canceled login succeeded")
	}
	address, _ := url.Parse(redirect)
	connection, err := net.DialTimeout("tcp", address.Host, time.Second)
	if err == nil {
		connection.Close()
		t.Fatal("listener remained open after cancellation")
	}
}
