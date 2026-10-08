package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type companionDiscoveryFixture struct {
	starts, closes atomic.Int32
	mu             sync.Mutex
	advertisement  companionDiscoveryAdvertisement
	fail           bool
}

func (f *companionDiscoveryFixture) register(ctx context.Context, advertisement companionDiscoveryAdvertisement) (*companionDiscoveryRegistration, error) {
	f.starts.Add(1)
	f.mu.Lock()
	f.advertisement = advertisement
	f.mu.Unlock()
	if f.fail {
		return nil, errors.New("fixture discovery unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	var once sync.Once
	stop := func() { once.Do(func() { f.closes.Add(1); cancel() }) }
	go func() { <-ctx.Done(); done <- ctx.Err(); close(done) }()
	return &companionDiscoveryRegistration{Close: stop, Done: done}, nil
}

type companionNearbyFixture struct {
	s         *companionSession
	b         *companionPairingBridge
	discovery *companionDiscoveryFixture
	now       atomic.Int64
}

func newCompanionNearbyFixture(t *testing.T) *companionNearbyFixture {
	t.Helper()
	f := &companionNearbyFixture{s: testCompanion(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("pairing reached a provider or project handler") })), discovery: &companionDiscoveryFixture{}}
	f.now.Store(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).UnixNano())
	f.s.now = func() time.Time { return time.Unix(0, f.now.Load()) }
	f.s.expires = f.s.now().Add(companionLifetime)
	f.s.pairing = companionPairing{Version: 1, Name: "Fixture Desktop", URL: "https://" + f.s.host + companionPrefix, Token: strings.Repeat("a", 64), CertificateSHA256: strings.Repeat("b", 64), ExpiresAt: f.s.expires.Format(time.RFC3339)}
	f.b = newCompanionPairingBridge(f.s, f.discovery.register)
	_, f.b.lan, _ = net.ParseCIDR("192.168.1.0/24")
	f.s.pairingBridge = f.b
	t.Cleanup(func() { f.b.stop("") })
	f.b.start()
	return f
}

func (f *companionNearbyFixture) request(method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	encoded, _ := json.Marshal(body)
	r := companionRequest(f.s, method, "/pairing/"+path, string(encoded))
	r.Header.Del("Authorization")
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w, result
}

func (f *companionNearbyFixture) create(secret string) (*httptest.ResponseRecorder, map[string]any) {
	return f.request(http.MethodPost, "requests", map[string]any{"version": 1, "desktopID": f.b.id, "certificateSHA256": f.s.pairing.CertificateSHA256, "clientCommitment": companionNearbyCommitment(f.b.id, f.s.pairing.CertificateSHA256, secret), "deviceName": "Glowbom on Vision Pro"})
}

func (f *companionNearbyFixture) confirm(t *testing.T, secret string) (string, string) {
	t.Helper()
	w, initial := f.create(secret)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	id := initial["requestID"].(string)
	w, confirmed := f.request(http.MethodPost, "requests/"+id+"/confirm", map[string]string{"clientChallenge": secret})
	code := companionNearbyCode(f.b.id, f.s.pairing.CertificateSHA256, secret, initial["serverChallenge"].(string))
	if w.Code != 200 || confirmed["status"] != "waiting" || confirmed["code"] != code || confirmed["serverChallenge"] != initial["serverChallenge"] || confirmed["expiresAt"] != initial["expiresAt"] {
		t.Fatal(w.Code, confirmed)
	}
	return id, code
}

func TestCompanionNearbyCommitRevealAndApproval(t *testing.T) {
	f := newCompanionNearbyFixture(t)
	secret := strings.Repeat("1", 64)
	w, initial := f.create(secret)
	if w.Code != 200 || initial["status"] != "confirming" || initial["pairing"] != nil || initial["code"] != nil {
		t.Fatal(w.Code, initial)
	}
	if _, pending := f.b.snapshot(); len(pending) != 0 {
		t.Fatal("unconfirmed request shown on Desktop", pending)
	}
	w, repeated := f.create(secret)
	if w.Code != 200 || !reflect.DeepEqual(initial, repeated) {
		t.Fatal("retry replaced committed nonce", initial, repeated)
	}
	id, code := f.confirm(t, secret)
	_, pending := f.b.snapshot()
	if len(pending) != 1 || pending[0]["code"] != code || pending[0]["deviceName"] != "Glowbom on Vision Pro" {
		t.Fatal(pending)
	}
	public, _ := json.Marshal(pending)
	if strings.Contains(string(public), secret) || strings.Contains(string(public), f.s.pairing.Token) || strings.Contains(string(public), "Commitment") {
		t.Fatal("secret leaked to pending Desktop summary")
	}
	if err := f.b.respond(id, "approve", "000000"); code != "000000" && err == nil {
		t.Fatal("different code approved")
	}
	if err := f.b.respond(id, "approve", code); err != nil {
		t.Fatal(err)
	}
	if err := f.b.respond(id, "approve", code); err != nil {
		t.Fatal("approval retry not idempotent", err)
	}
	w, repeated = f.create(secret)
	if w.Code != 200 || repeated["pairing"] != nil || repeated["code"] != nil || repeated["serverChallenge"] != initial["serverChallenge"] {
		t.Fatal("commitment-only request obtained pairing", repeated)
	}
	f.now.Add(int64(time.Second))
	w, approved := f.request(http.MethodPost, "requests/"+id+"/status", map[string]string{"clientChallenge": secret})
	if w.Code != 200 || approved["status"] != "approved" || approved["code"] != code || approved["pairing"].(map[string]any)["token"] != f.s.pairing.Token {
		t.Fatal(w.Code, approved)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("pairing cached")
	}
	if _, pending := f.b.snapshot(); len(pending) != 0 {
		t.Fatal("approved request remains waiting")
	}
	f.b.mu.Lock()
	stored := *f.b.requests[id]
	f.b.mu.Unlock()
	if strings.Contains(fmt.Sprintf("%+v", stored), secret) {
		t.Fatal("bridge retained revealed client secret")
	}
}

func TestCompanionNearbyRejectedCanceledAndExpiredRequestsNeverDeliverToken(t *testing.T) {
	for _, action := range []string{"reject", "cancel", "stop", "expire", "revoke"} {
		t.Run(action, func(t *testing.T) {
			f := newCompanionNearbyFixture(t)
			secret := strings.Repeat("2", 64)
			id, code := f.confirm(t, secret)
			if action == "reject" {
				if err := f.b.respond(id, "reject", code); err != nil {
					t.Fatal(err)
				}
			} else if action == "cancel" {
				w, _ := f.request(http.MethodDelete, "requests/"+id, map[string]string{"clientChallenge": secret})
				if w.Code != 200 {
					t.Fatal(w.Code)
				}
			} else if action == "stop" {
				f.b.stop("")
			} else if action == "expire" {
				f.now.Add(int64(companionDiscoveryLifetime))
			} else {
				f.s.cancel()
			}
			f.now.Add(int64(time.Second))
			w, result := f.request(http.MethodPost, "requests/"+id+"/status", map[string]string{"clientChallenge": secret})
			if result["pairing"] != nil || (action != "revoke" && (w.Code != 200 || result["status"] == "waiting" || result["status"] == "approved")) || (action == "revoke" && w.Code != 401) {
				t.Fatal(w.Code, result)
			}
			if err := f.b.respond(id, "approve", code); err == nil {
				t.Fatal("terminal request approved")
			}
		})
	}
}

func TestCompanionNearbyIdentityAndCommitmentCannotChange(t *testing.T) {
	f := newCompanionNearbyFixture(t)
	secret := strings.Repeat("3", 64)
	w, initial := f.create(secret)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	id := initial["requestID"].(string)
	for _, path := range []string{"confirm", "status"} {
		w, result := f.request(http.MethodPost, "requests/"+id+"/"+path, map[string]string{"clientChallenge": strings.Repeat("4", 64)})
		if w.Code != 403 || result["pairing"] != nil {
			t.Fatal(w.Code, result)
		}
	}
	if err := f.b.respond(id, "approve", "123456"); err == nil {
		t.Fatal("Desktop approved unconfirmed commitment")
	}
	for _, change := range []string{"id", "pin", "secret-field", "control-name"} {
		body := map[string]any{"version": 1, "desktopID": f.b.id, "certificateSHA256": f.s.pairing.CertificateSHA256, "clientCommitment": strings.Repeat("5", 64), "deviceName": "Glowbom on iPhone"}
		switch change {
		case "id":
			body["desktopID"] = strings.Repeat("0", 32)
		case "pin":
			body["certificateSHA256"] = strings.Repeat("0", 64)
		case "secret-field":
			body["clientChallenge"] = secret
		case "control-name":
			body["deviceName"] = "Phone\nspoofed"
		}
		w, _ := f.request(http.MethodPost, "requests", body)
		if w.Code != 400 {
			t.Fatal(change, w.Code)
		}
	}
	f.now.Add(int64(time.Second))
	w, confirmed := f.request(http.MethodPost, "requests/"+id+"/confirm", map[string]string{"clientChallenge": secret})
	if w.Code != 200 || confirmed["serverChallenge"] != initial["serverChallenge"] || confirmed["expiresAt"] != initial["expiresAt"] {
		t.Fatal(confirmed)
	}
}

func TestCompanionNearbyPrepairRoutesRetainNetworkAndTokenBoundaries(t *testing.T) {
	f := newCompanionNearbyFixture(t)
	for _, test := range []struct {
		name   string
		change func(*http.Request)
		want   int
	}{
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://example.test") }, 403},
		{"host", func(r *http.Request) { r.Host = "192.168.1.8:4443" }, 403},
		{"public", func(r *http.Request) { r.RemoteAddr = "8.8.8.8:1" }, 403},
		{"other subnet", func(r *http.Request) { r.RemoteAddr = "192.168.5.9:1" }, 403},
		{"loopback", func(r *http.Request) { r.RemoteAddr = "127.0.0.1:1" }, 403},
		{"no TLS", func(r *http.Request) { r.TLS = nil }, 404},
		{"query", func(r *http.Request) { r.URL.RawQuery = "token=wrong" }, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := companionRequest(f.s, http.MethodPost, "/pairing/requests", "{}")
			r.Header.Del("Authorization")
			test.change(r)
			w := httptest.NewRecorder()
			f.s.ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatal(w.Code)
			}
		})
	}
	r := companionRequest(f.s, http.MethodGet, "/projects", "")
	r.Header.Del("Authorization")
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("prepair bridge bypassed project token")
	}
	w, _ = f.request(http.MethodGet, "unknown", nil)
	if w.Code != 404 {
		t.Fatal("unknown prepair route allowed")
	}
}

func TestCompanionNearbyDesktopApprovalRetainsLoopbackOriginAndBearerChecks(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-desktop-token")
	f := newCompanionNearbyFixture(t)
	id, code := f.confirm(t, strings.Repeat("6", 64))
	m := newCompanionManager(http.NotFoundHandler())
	m.session = f.s
	m.now = f.s.now
	t.Cleanup(m.Shutdown)
	payload, _ := json.Marshal(map[string]string{"id": id, "code": code, "decision": "approve"})
	for _, test := range []struct {
		name   string
		change func(*http.Request)
		want   int
	}{
		{"remote", func(r *http.Request) { r.RemoteAddr = "192.168.1.3:1" }, 403},
		{"no bearer", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.test") }, 403},
		{"owner", func(*http.Request) {}, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/companion/pairing/respond", strings.NewReader(string(payload)))
			r.RemoteAddr = "127.0.0.1:1"
			r.Header.Set("Authorization", "Bearer fixture-desktop-token")
			test.change(r)
			w := httptest.NewRecorder()
			m.ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestCompanionNearbyDistinctRequestAndPollingLimits(t *testing.T) {
	f := newCompanionNearbyFixture(t)
	secret := strings.Repeat("7", 64)
	id, _ := f.confirm(t, secret)
	w, _ := f.request(http.MethodPost, "requests/"+id+"/status", map[string]string{"clientChallenge": secret})
	if w.Code != 429 {
		t.Fatal("fast polling accepted", w.Code)
	}
	for i := 1; i < 4; i++ {
		w, _ := f.create(fmt.Sprintf("%064x", i))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	w, _ = f.create(fmt.Sprintf("%064x", 5))
	if w.Code != 429 {
		t.Fatal("unbounded per-peer requests", w.Code)
	}
	f.now.Add(int64(time.Minute))
	w, _ = f.create(fmt.Sprintf("%064x", 5))
	if w.Code != 200 {
		t.Fatal("peer budget never recovered", w.Code)
	}
	f.b.mu.Lock()
	f.b.windowCount = companionPairingRequestLimit
	f.b.mu.Unlock()
	w, _ = f.create(fmt.Sprintf("%064x", 6))
	if w.Code != 429 {
		t.Fatal("window budget ignored")
	}
	f.b.start()
	f.b.mu.Lock()
	f.b.totalCount = 1000
	f.b.mu.Unlock()
	w, _ = f.create(fmt.Sprintf("%064x", 6))
	if w.Code != 429 {
		t.Fatal("session budget ignored after reopen")
	}
}

func TestCompanionNearbyReopenStopsAdvertisementAndCancelsOldRequests(t *testing.T) {
	f := newCompanionNearbyFixture(t)
	secret := strings.Repeat("8", 64)
	id, code := f.confirm(t, secret)
	f.b.start()
	if f.discovery.starts.Load() != 2 || f.discovery.closes.Load() != 1 {
		t.Fatal("advertisement duplicated", f.discovery.starts.Load(), f.discovery.closes.Load())
	}
	if err := f.b.respond(id, "approve", code); err == nil {
		t.Fatal("old window approved")
	}
	f.now.Add(int64(time.Second))
	w, result := f.request(http.MethodPost, "requests/"+id+"/status", map[string]string{"clientChallenge": secret})
	if w.Code != 200 || result["status"] != "canceled" || result["pairing"] != nil {
		t.Fatal(w.Code, result)
	}
	f.s.close()
	if f.discovery.closes.Load() != 2 {
		t.Fatal("revocation left advertisement")
	}
}

func TestCompanionNearbyDiscoveryFailureKeepsCopiedPairingAvailable(t *testing.T) {
	f := newCompanionNearbyFixture(t)
	f.b.stop("")
	f.discovery.fail = true
	f.b.start()
	discovery, _ := f.b.snapshot()
	if discovery["active"] != false || discovery["message"] == nil {
		t.Fatal(discovery)
	}
	r := companionRequest(f.s, http.MethodGet, "/projects", "")
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("Bonjour failure disabled existing pairing", w.Code)
	}
	w, _ = f.create(strings.Repeat("9", 64))
	if w.Code != 410 {
		t.Fatal("failed discovery accepts requests", w.Code)
	}
}

func TestCompanionNearbyCancelRacingApprovalNeverChangesTerminalState(t *testing.T) {
	f := newCompanionNearbyFixture(t)
	secret := strings.Repeat("c", 64)
	id, code := f.confirm(t, secret)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = f.b.respond(id, "approve", code) }()
	go func() {
		defer wg.Done()
		f.request(http.MethodDelete, "requests/"+id, map[string]string{"clientChallenge": secret})
	}()
	wg.Wait()
	f.now.Add(int64(time.Second))
	w, result := f.request(http.MethodPost, "requests/"+id+"/status", map[string]string{"clientChallenge": secret})
	if w.Code != 200 || result["status"] != "canceled" || result["pairing"] != nil {
		t.Fatal(w.Code, result)
	}
}

func TestCompanionNearbyCommitmentAndCodeHaveStableTranscript(t *testing.T) {
	id, pin, secret, nonce := strings.Repeat("1", 32), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)
	commitment := companionNearbyCommitment(id, pin, secret)
	code := companionNearbyCode(id, pin, secret, nonce)
	if commitment != "423d004da15762d60718df11222fd23baa8061a0c4eec66589fb48feb7682d56" || code != "920254" {
		t.Fatal(commitment, code)
	}
	for _, changed := range []string{companionNearbyCode(strings.Repeat("5", 32), pin, secret, nonce), companionNearbyCode(id, strings.Repeat("5", 64), secret, nonce), companionNearbyCode(id, pin, strings.Repeat("5", 64), nonce), companionNearbyCode(id, pin, secret, strings.Repeat("5", 64))} {
		if changed == code {
			t.Fatal("code omitted transcript field")
		}
	}
}
