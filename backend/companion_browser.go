package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const companionBrowserLifetime = 30 * time.Minute
const companionBrowserBodyLimit = 16 << 20
const companionBrowserBootstrap = "/_glowbom_browser/launch"

type companionBrowserSession struct {
	owner          *companionSession
	project        companionProject
	target         string
	runner         *previewSession
	previewID      string
	sourceURL      string
	upstream       *url.URL
	host           string
	origin         string
	expires        time.Time
	cookieName     string
	cookieSecret   string
	allowLoopback  bool
	ctx            context.Context
	cancel         context.CancelFunc
	server         *http.Server
	transport      *http.Transport
	proxy          *httputil.ReverseProxy
	jar            *cookiejar.Jar
	requests       chan struct{}
	closeOnce      sync.Once
	mu             sync.Mutex
	bootstrapToken string
}

func companionBrowserSecret() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *companionSession) previewBrowserSession(w http.ResponseWriter, r *http.Request, project companionProject, target string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		PreviewID      string `json:"previewID"`
		AllowLocalHTTP bool   `json:"allowLocalHTTP"`
	}
	if !companionDecode(w, r, &input, 1024) {
		return
	}
	if !input.AllowLocalHTTP {
		http.Error(w, "Allow the temporary unencrypted Wi-Fi preview before opening it in your browser.", http.StatusForbidden)
		return
	}
	if !validCompanionPreviewTarget(target) || input.PreviewID == "" || len(input.PreviewID) > 160 {
		http.Error(w, "Choose a running project preview.", http.StatusBadRequest)
		return
	}
	if s.previews == nil {
		http.Error(w, "The Desktop preview service is unavailable.", http.StatusServiceUnavailable)
		return
	}
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.ctx.Err() != nil || !s.now().Before(s.expires) {
		http.Error(w, "This connection expired. Pair again on Desktop.", http.StatusUnauthorized)
		return
	}
	if s.browsers == nil {
		s.browsers = map[string]*companionBrowserSession{}
	}
	for key, gateway := range s.browsers {
		if !gateway.current() {
			gateway.close()
			delete(s.browsers, key)
		}
	}
	key := project.path + "/" + target
	gateway := s.browsers[key]
	if gateway != nil && gateway.previewID != input.PreviewID {
		http.Error(w, "This preview stopped or changed. Start it again from Glowbom.", http.StatusConflict)
		return
	}
	if gateway == nil {
		s.previews.mu.Lock()
		runner := s.previews.sessions[key]
		var source previewTarget
		if runner != nil {
			source = runner.snapshot()
		}
		closed := s.previews.closed
		s.previews.mu.Unlock()
		if closed || runner == nil || source.Status != "running" || source.ID != input.PreviewID || !validPreviewBrowserURL(source.URL) {
			http.Error(w, "This preview stopped or changed. Start it again from Glowbom.", http.StatusConflict)
			return
		}
		if len(s.browsers) >= 8 {
			http.Error(w, "Close an existing browser preview before opening another.", http.StatusTooManyRequests)
			return
		}
		var err error
		gateway, err = s.newBrowserSession(project, target, runner, source)
		if err != nil {
			http.Error(w, "Desktop could not open the temporary browser connection. Check the Wi-Fi connection and try again.", http.StatusServiceUnavailable)
			return
		}
		s.browsers[key] = gateway
	}
	token, err := companionBrowserSecret()
	if err != nil || !gateway.current() {
		gateway.close()
		http.Error(w, "This preview stopped or changed. Start it again from Glowbom.", http.StatusConflict)
		return
	}
	gateway.mu.Lock()
	gateway.bootstrapToken = token
	gateway.mu.Unlock()
	writeJSON(w, map[string]string{
		"url":       gateway.origin + companionBrowserBootstrap + "#" + token,
		"expiresAt": gateway.expires.UTC().Format(time.RFC3339),
		"message":   "A temporary, unencrypted preview on this Wi-Fi. Keep Desktop open. Server sessions stay on Desktop; apps that need browser-managed cookies or secure browser features may need additional setup.",
	})
}

func (s *companionSession) newBrowserSession(project companionProject, target string, runner *previewSession, source previewTarget) (*companionBrowserSession, error) {
	address, _, err := net.SplitHostPort(s.host)
	if err != nil || !companionPrivateIPv4(net.ParseIP(address)) {
		return nil, errors.New("invalid companion interface")
	}
	upstream, err := url.Parse(source.URL)
	if err != nil || !validPreviewBrowserURL(source.URL) || net.ParseIP(upstream.Hostname()) == nil || !net.ParseIP(upstream.Hostname()).IsLoopback() {
		return nil, errors.New("preview needs a numeric loopback address")
	}
	upstream.Path, upstream.RawPath, upstream.RawQuery, upstream.Fragment = "", "", "", ""
	secret, err := companionBrowserSecret()
	if err != nil {
		return nil, err
	}
	listen := s.browserListen
	if listen == nil {
		listen = net.Listen
	}
	listener, err := listen("tcp4", net.JoinHostPort(address, "0"))
	if err != nil {
		return nil, err
	}
	expires := s.now().Add(companionBrowserLifetime)
	if s.expires.Before(expires) {
		expires = s.expires
	}
	ctx, cancel := context.WithDeadline(s.ctx, expires)
	jar, _ := cookiejar.New(nil)
	g := &companionBrowserSession{owner: s, project: project, target: target, runner: runner, previewID: source.ID, sourceURL: source.URL,
		upstream: upstream, host: listener.Addr().String(), expires: expires, cookieName: "glowbom_browser_" + secret[:16], cookieSecret: secret,
		allowLoopback: s.browserListen != nil, ctx: ctx, cancel: cancel, jar: jar, requests: make(chan struct{}, 16)}
	g.origin = "http://" + g.host
	g.transport = &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, destination string) (net.Conn, error) {
		if destination != upstream.Host || !g.current() {
			return nil, errors.New("preview destination changed")
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", upstream.Host)
	}, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 8, DisableCompression: true}
	g.proxy = &httputil.ReverseProxy{Rewrite: g.rewrite, ModifyResponse: g.modifyResponse, Transport: g.transport,
		ErrorLog: log.New(io.Discard, "", 0), ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Desktop could not load this preview. Return to Glowbom and try again.", http.StatusBadGateway)
		}}
	g.server = &http.Server{Handler: g, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		_ = g.server.Serve(listener)
		g.close()
	}()
	go g.watch()
	return g, nil
}

func (g *companionBrowserSession) current() bool {
	if g.ctx.Err() != nil || g.owner.ctx.Err() != nil || !g.owner.now().Before(g.expires) || !g.owner.now().Before(g.owner.expires) {
		return false
	}
	g.owner.previews.mu.Lock()
	defer g.owner.previews.mu.Unlock()
	runner := g.owner.previews.sessions[g.project.path+"/"+g.target]
	if g.owner.previews.closed || runner != g.runner {
		return false
	}
	source := runner.snapshot()
	return source.Status == "running" && source.ID == g.previewID && source.URL == g.sourceURL
}

func (g *companionBrowserSession) watch() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	defer g.close()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-g.runner.done:
			return
		case <-ticker.C:
			if !g.current() {
				return
			}
		}
	}
}

func (g *companionBrowserSession) close() {
	g.closeOnce.Do(func() {
		g.cancel()
		_ = g.server.Close()
		g.transport.CloseIdleConnections()
	})
}

func (s *companionSession) closeBrowserSessions() {
	s.browserMu.Lock()
	gateways := s.browsers
	s.browsers = nil
	s.browserMu.Unlock()
	for _, gateway := range gateways {
		gateway.close()
	}
}

func (g *companionBrowserSession) authenticated(r *http.Request) bool {
	count, allowed := 0, false
	for _, cookie := range r.Cookies() {
		if cookie.Name == g.cookieName {
			count++
			allowed = subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(g.cookieSecret)) == 1
		}
	}
	return count == 1 && allowed
}

func (g *companionBrowserSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(remote)
	if err != nil || ip == nil || (!companionPrivateIPv4(ip) && !(g.allowLoopback && ip.IsLoopback())) || r.Host != g.host || r.URL.IsAbs() || r.URL.User != nil {
		http.Error(w, "This request is not allowed.", http.StatusForbidden)
		return
	}
	if !g.current() {
		g.cancel()
		http.Error(w, "This preview closed. Open it again from Glowbom.", http.StatusGone)
		return
	}
	select {
	case g.requests <- struct{}{}:
		defer func() { <-g.requests }()
	default:
		http.Error(w, "Too many preview requests. Try again shortly.", http.StatusTooManyRequests)
		return
	}
	upgrade := r.Header.Get("Upgrade")
	unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead
	origin := r.Header.Get("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	if (origin != "" && origin != g.origin) || ((unsafe || upgrade != "") && origin != g.origin) ||
		(site != "" && site != "none" && site != "same-origin") {
		http.Error(w, "Open this preview from its own browser page.", http.StatusForbidden)
		return
	}
	if r.URL.Path == companionBrowserBootstrap {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		g.bootstrap(w, r)
		return
	}
	if r.URL.Path == companionBrowserBootstrap+"/redeem" {
		g.redeem(w, r)
		return
	}
	if !g.authenticated(r) {
		http.Error(w, "Open this preview from Glowbom to continue.", http.StatusUnauthorized)
		return
	}
	if !companionPreviewAssetPath(strings.TrimPrefix(r.URL.Path, "/")) || len(r.URL.RawQuery) > 4096 || r.URL.Query().Get("glowbom_preview") != "" ||
		(upgrade != "" && (!strings.EqualFold(upgrade, "websocket") || r.Method != http.MethodGet)) {
		http.Error(w, "This preview request is not available.", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength > companionBrowserBodyLimit {
		http.Error(w, "This preview request is too large.", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, companionBrowserBodyLimit+1))
	if err != nil || len(body) > companionBrowserBodyLimit {
		http.Error(w, "This preview request is too large or incomplete.", http.StatusRequestEntityTooLarge)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(g.ctx, cancel)
	defer stop()
	request := r.Clone(ctx)
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.TransferEncoding = nil
	w.Header().Del("Referrer-Policy")
	g.proxy.ServeHTTP(w, request)
}

const companionBrowserScript = `(async()=>{const token=location.hash.slice(1);history.replaceState(null,"",location.pathname);try{const response=await fetch("/_glowbom_browser/launch/redeem",{method:"POST",credentials:"same-origin",headers:{"Content-Type":"application/json"},body:JSON.stringify({token})});if(!response.ok)throw new Error();location.replace("/");}catch{document.getElementById("status").textContent="This preview link expired. Return to Glowbom and open it again.";}})();`

func (g *companionBrowserSession) bootstrap(w http.ResponseWriter, r *http.Request) {
	digest := sha256.Sum256([]byte(companionBrowserScript))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Glowbom preview</title><body><p id="status">Opening your Desktop preview...</p><script>`+companionBrowserScript+`</script></body></html>`)
	}
}

func (g *companionBrowserSession) redeem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "Invalid preview link.", http.StatusBadRequest)
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	if !companionDecode(w, r, &input, 1024) {
		return
	}
	g.mu.Lock()
	allowed := input.Token != "" && g.bootstrapToken != "" && subtle.ConstantTimeCompare([]byte(input.Token), []byte(g.bootstrapToken)) == 1
	if allowed {
		g.bootstrapToken = ""
	}
	g.mu.Unlock()
	if !allowed || !g.current() {
		http.Error(w, "This preview link expired. Open it again from Glowbom.", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: g.cookieName, Value: g.cookieSecret, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: g.expires})
	w.WriteHeader(http.StatusNoContent)
}

func (g *companionBrowserSession) rewrite(request *httputil.ProxyRequest) {
	request.SetURL(g.upstream)
	for header := range request.Out.Header {
		lower := strings.ToLower(header)
		if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "x-glowbom-") {
			request.Out.Header.Del(header)
		}
	}
	if request.In.Header.Get("Origin") != "" {
		request.Out.Header.Set("Origin", g.upstream.Scheme+"://"+g.upstream.Host)
	}
	request.Out.Header.Del("Referer")
	for _, cookie := range g.jar.Cookies(request.Out.URL) {
		request.Out.AddCookie(cookie)
	}
	if source := g.runner.snapshot(); source.Kind == "static" && len(g.previewID) >= 12 {
		request.Out.AddCookie(&http.Cookie{Name: "glowbom_preview_" + g.previewID[:12], Value: g.previewID})
	}
}

func (g *companionBrowserSession) modifyResponse(response *http.Response) error {
	if !g.current() {
		return errors.New("preview closed")
	}
	g.jar.SetCookies(response.Request.URL, response.Cookies())
	response.Header.Del("Set-Cookie")
	response.Header.Del("Alt-Svc")
	response.Header.Del("Clear-Site-Data")
	response.Header.Set("Cache-Control", "no-store")
	// Same-origin form POSTs need their Origin header. The no-referrer policy
	// changes that header to null for browser navigation requests.
	response.Header.Set("Referrer-Policy", "same-origin")
	if location := response.Header.Get("Location"); location != "" {
		address, err := url.Parse(location)
		if err != nil || address.User != nil || (address.Scheme != "" && address.Scheme != "http") ||
			(address.Host != "" && address.Host != g.upstream.Host && address.Host != g.host) {
			return errors.New("preview redirect is outside this session")
		}
		if address.Host != "" {
			address.Scheme, address.Host = "http", g.host
		}
		if !companionPreviewAssetPath(strings.TrimPrefix(address.Path, "/")) || address.Query().Get("glowbom_preview") != "" {
			return errors.New("preview redirect is unavailable")
		}
		response.Header.Set("Location", address.String())
	}
	return nil
}
