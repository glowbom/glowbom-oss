package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync/atomic"
	"time"
)

var callbackCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

//go:embed assets/glowbom-logo.svg
var loginWordmark []byte

// The port is reserved before opening the browser, and only IPv4 loopback is bound.
type loginCallback struct {
	listener net.Listener
	server   *http.Server
	state    string
	result   chan error
	claimed  atomic.Bool
}

func newLoginCallback(state string) (*loginCallback, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("could not listen on localhost; use glowbom login --device-auth for a remote terminal")
	}
	return &loginCallback{listener: listener, state: state, result: make(chan error, 1)}, nil
}

func (c *loginCallback) redirectURI() string {
	return "http://" + c.listener.Addr().String() + "/callback"
}

func (c *loginCallback) serve(ctx context.Context, exchange func(string) error) {
	c.server = &http.Server{
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 90 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192,
		ErrorLog: log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			if r.Host != c.listener.Addr().String() || r.URL.Path != "/callback" || r.URL.RawPath != "" {
				http.Error(w, "Unknown callback address.", http.StatusNotFound)
				return
			}
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				http.Error(w, "Use the sign-in link in your browser.", http.StatusMethodNotAllowed)
				return
			}
			query, err := url.ParseQuery(r.URL.RawQuery)
			valid := err == nil && len(r.RequestURI) <= 2048 && len(query) == 2 &&
				r.ContentLength == 0 && len(r.TransferEncoding) == 0 && len(query["state"]) == 1 &&
				subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(c.state)) == 1
			code := query.Get("code")
			denied := len(query["error"]) == 1 && query.Get("error") == "access_denied"
			valid = valid && ((len(query["code"]) == 1 && callbackCodePattern.MatchString(code)) || denied)
			if !valid {
				http.Error(w, "Invalid callback. Return to the Glowbom sign-in page.", http.StatusBadRequest)
				return
			}
			if ctx.Err() != nil || !c.claimed.CompareAndSwap(false, true) {
				http.Error(w, "This sign-in request expired or was already used.", http.StatusConflict)
				return
			}
			if denied {
				err = errors.New("connection canceled in the browser")
			} else {
				err = exchange(code)
			}
			writeLoginPage(w, err == nil, denied)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			c.result <- err
		}),
	}
	go func() {
		if err := c.server.Serve(c.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case c.result <- errors.New("the localhost listener stopped; run glowbom login again"):
			default:
			}
		}
	}()
}

func (c *loginCallback) close() {
	if c.server == nil {
		_ = c.listener.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.server.Shutdown(ctx); err != nil {
		_ = c.server.Close()
	}
}

func writeLoginPage(w http.ResponseWriter, success, denied bool) {
	// Remove the consumed code from the visible URL without loading any resources.
	const script = `history.replaceState(null, "", "/callback");`
	hash := sha256.Sum256([]byte(script))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; script-src 'sha256-"+
		base64.StdEncoding.EncodeToString(hash[:])+"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	title, message := "Build something great.", "Your terminal is connected to Glowbom."
	next := "Return to your terminal to continue.<br>You can close this window."
	icon := `<path d="m5 12 4 4L19 6"/>`
	if denied {
		title, message = "Connection canceled", "Your account was not connected."
		next = "You can close this window."
		icon = `<path d="m7 7 10 10M17 7 7 17"/>`
	} else if !success {
		title, message = "Connection could not finish", "Return to your terminal for details."
		next = "Run <code>glowbom login</code> again to connect."
		icon = `<path d="M12 5v9M12 18v1"/>`
		w.WriteHeader(http.StatusBadGateway)
	}
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Glowbom CLI</title><style>
:root{color-scheme:light}*{box-sizing:border-box}
body{margin:0;min-height:100vh;min-height:100svh;display:flex;align-items:center;justify-content:center;padding:44px 24px;background:#f8faf8;color:#173d35;font:15px/1.5 system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
main{width:100%%;max-width:440px;text-align:center}header{height:46px;margin:0 auto 48px}header img{display:block;width:156px;height:46px;object-fit:contain;margin:auto}
.status{width:64px;height:64px;margin:0 auto 28px;display:grid;place-items:center;border-radius:20px;background:#e6f5eb;color:#087f5b}.status svg{width:32px;height:32px}
h1{margin:0 0 14px;font-size:30px;font-weight:700;line-height:1.2;overflow-wrap:break-word}p{margin:0}p+p{margin-top:8px}code{font-size:inherit}footer{margin-top:44px;color:#62756b;font-size:13px}
</style></head><body><main><header><img src="data:image/svg+xml;base64,%s" width="156" height="46" alt="Glowbom"></header>
<div class="status" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round">%s</svg></div>
<h1>%s</h1><p>%s</p><p>%s</p><footer>Sketch to software.</footer>
</main><script>%s</script></body></html>`, base64.StdEncoding.EncodeToString(loginWordmark), icon, title, message, next, script)
}
