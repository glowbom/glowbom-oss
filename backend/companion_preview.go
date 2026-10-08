package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	htmlnode "golang.org/x/net/html"
)

type companionPreviewTarget struct {
	Target       string   `json:"target"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Available    bool     `json:"available"`
	NeedsInstall bool     `json:"needsInstall"`
	Reason       string   `json:"reason,omitempty"`
	Status       string   `json:"status"`
	ID           string   `json:"id,omitempty"`
	Error        string   `json:"error,omitempty"`
	Logs         []string `json:"logs,omitempty"`
	Revision     string   `json:"revision,omitempty"`
}

func companionPreviewContentRoute(value string) bool {
	parts := strings.Split(strings.TrimPrefix(value, companionPrefix+"/"), "/")
	return strings.HasPrefix(value, companionPrefix+"/") && len(parts) >= 6 && parts[0] == "projects" && parts[2] == "preview" && parts[4] == "content"
}

func validCompanionPreviewTarget(value string) bool {
	return value != "" && len(value) <= 80 && !strings.ContainsAny(value, "/\\%\x00\r\n") && value != "." && value != ".."
}

const companionSnapshotLimit = 16 << 20
const companionSnapshotAssetLimit = 6 << 20

var errCompanionSnapshotLive = errors.New("This preview needs a live browser connection to Desktop. A static copy cannot preserve its files or server behavior.")

// Snapshots export only the current static preview. Browser access never receives
// the pairing token, an arbitrary Desktop URL, or a new network listener.
func (s *companionSession) previewBrowserSnapshot(w http.ResponseWriter, r *http.Request, project companionProject, target string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		PreviewID string `json:"previewID"`
	}
	if !companionDecode(w, r, &input, 1024) {
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
	current := func() (previewTarget, bool) {
		s.previews.mu.Lock()
		defer s.previews.mu.Unlock()
		managed := s.previews.sessions[project.path+"/"+target]
		if s.previews.closed || managed == nil {
			return previewTarget{}, false
		}
		view := managed.snapshot()
		return view, view.Status == "running" && view.ID == input.PreviewID && validPreviewBrowserURL(view.URL)
	}
	source, valid := current()
	if !valid {
		http.Error(w, "This preview stopped or changed. Start it again from Glowbom.", http.StatusConflict)
		return
	}
	if source.Kind != "static" {
		writeJSON(w, map[string]string{"kind": "needs-live-browser-route", "message": "This preview runs a server on Desktop. It needs a live browser connection and cannot be exported as a static copy."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	document, err := companionStaticSnapshot(ctx, project.path, source.directory)
	if s.ctx.Err() != nil || !s.now().Before(s.expires) {
		http.Error(w, "This connection expired. Pair again on Desktop.", http.StatusUnauthorized)
		return
	}
	if ctx.Err() != nil {
		http.Error(w, "Desktop could not finish preparing this preview in time. Try opening it again.", http.StatusGatewayTimeout)
		return
	}
	if _, valid := current(); !valid {
		http.Error(w, "This preview stopped or changed. Start it again from Glowbom.", http.StatusConflict)
		return
	}
	if err != nil {
		writeJSON(w, map[string]string{"kind": "needs-live-browser-route", "message": errCompanionSnapshotLive.Error()})
		return
	}
	writeJSON(w, map[string]string{"kind": "snapshot", "name": companionPublicText(source.Name, 160), "html": document,
		"message": "A static copy of this preview. External resources still need a network connection, and server features are not included."})
}

func companionStaticSnapshot(ctx context.Context, project, directory string) (string, error) {
	relative, err := filepath.Rel(project, directory)
	if err != nil || relative == "." || !isPathWithin(project, directory) || previewHiddenPath(filepath.ToSlash(relative)) {
		return "", errCompanionSnapshotLive
	}
	projectRoot, err := os.OpenRoot(project)
	if err != nil {
		return "", err
	}
	defer projectRoot.Close()
	if err := companionSnapshotRegularPath(projectRoot, filepath.ToSlash(relative), true); err != nil {
		return "", err
	}
	expectedRoot, err := projectRoot.Lstat(relative)
	if err != nil {
		return "", err
	}
	root, err := projectRoot.OpenRoot(relative)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := companionSnapshotValidateRoot(projectRoot, root, filepath.ToSlash(relative), expectedRoot); err != nil {
		return "", err
	}
	document, err := companionSnapshotRead(ctx, root, "index.html", companionSnapshotLimit)
	if err != nil || !utf8.Valid(document) {
		return "", errCompanionSnapshotLive
	}
	// Tokenization preserves original inline scripts, styles and CSP exactly when
	// no local asset needs embedding. It does not execute or fetch document code.
	tokens := htmlnode.NewTokenizer(bytes.NewReader(document))
	var output bytes.Buffer
	changed, hasCSP := false, false
	style, script := false, false
	embeddedCount, embeddedBytes := 0, 0
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		kind := tokens.Next()
		if kind == htmlnode.ErrorToken {
			if tokens.Err() != io.EOF {
				return "", errCompanionSnapshotLive
			}
			break
		}
		raw := append([]byte(nil), tokens.Raw()...)
		if kind == htmlnode.TextToken && style && !companionSnapshotCSS(string(raw)) {
			return "", errCompanionSnapshotLive
		}
		if kind == htmlnode.TextToken && script && !companionSnapshotJavaScript(string(raw)) {
			return "", errCompanionSnapshotLive
		}
		if kind == htmlnode.EndTagToken {
			switch tokens.Token().Data {
			case "style":
				style = false
			case "script":
				script = false
			}
		}
		if kind == htmlnode.StartTagToken || kind == htmlnode.SelfClosingTagToken {
			token := tokens.Token()
			attribute := func(name string) string {
				for _, attr := range token.Attr {
					if attr.Key == name {
						return attr.Val
					}
				}
				return ""
			}
			if token.Data == "style" {
				style = true
			}
			if token.Data == "script" {
				script = true
			}
			if token.Data == "meta" && strings.EqualFold(attribute("http-equiv"), "content-security-policy") {
				hasCSP = true
			}
			if token.Data == "base" || !companionSnapshotCSS(attribute("style")) {
				return "", errCompanionSnapshotLive
			}
			updated := false
			for index, attr := range token.Attr {
				if (attr.Key == "action" || attr.Key == "formaction") && (strings.TrimSpace(attr.Val) == "" || strings.HasPrefix(strings.TrimSpace(attr.Val), "#")) {
					return "", errCompanionSnapshotLive
				}
				if (attr.Key == "srcset" || attr.Key == "imagesrcset") && strings.TrimSpace(attr.Val) != "" {
					return "", errCompanionSnapshotLive
				}
				if strings.HasPrefix(attr.Key, "on") && !companionSnapshotJavaScript(attr.Val) {
					return "", errCompanionSnapshotLive
				}
				resource := attr.Key == "src" || attr.Key == "poster" || (attr.Key == "href" && token.Data == "link") || (attr.Key == "data" && token.Data == "object")
				if !resource {
					if (attr.Key == "href" || attr.Key == "xlink:href" || attr.Key == "action" || attr.Key == "formaction" || attr.Key == "background" || attr.Key == "manifest") && !companionSnapshotExternal(attr.Val) {
						return "", errCompanionSnapshotLive
					}
					continue
				}
				if companionSnapshotExternal(attr.Val) {
					continue
				}
				if token.Data == "iframe" || token.Data == "object" || token.Data == "embed" ||
					(token.Data == "script" && strings.EqualFold(attribute("type"), "module")) ||
					(token.Data == "link" && attribute("rel") != "stylesheet" && attribute("rel") != "icon") {
					return "", errCompanionSnapshotLive
				}
				value, err := companionSnapshotAsset(ctx, root, attr.Val)
				if err != nil {
					return "", err
				}
				embeddedCount++
				embeddedBytes += len(value)
				if embeddedCount > 64 || embeddedBytes > companionSnapshotLimit {
					return "", errCompanionSnapshotLive
				}
				token.Attr[index].Val = value
				updated = true
			}
			if updated {
				changed = true
				raw = []byte(token.String())
			}
		}
		if output.Len()+len(raw) > companionSnapshotLimit {
			return "", errCompanionSnapshotLive
		}
		output.Write(raw)
	}
	// Rewriting asset origins can violate a CSP. Never weaken the project's policy.
	if changed && hasCSP {
		return "", errCompanionSnapshotLive
	}
	if err := companionSnapshotValidateRoot(projectRoot, root, filepath.ToSlash(relative), expectedRoot); err != nil {
		return "", err
	}
	return output.String(), nil
}

// Check both the opened directory and its path after opening. A symlink swap
// must not redirect the export into a different directory inside the project.
func companionSnapshotValidateRoot(project, root *os.Root, name string, expected os.FileInfo) error {
	if err := companionSnapshotRegularPath(project, name, true); err != nil {
		return err
	}
	opened, err := root.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(expected, opened) {
		return errCompanionSnapshotLive
	}
	current, err := project.Lstat(name)
	if err != nil || !os.SameFile(opened, current) {
		return errCompanionSnapshotLive
	}
	return nil
}

func companionSnapshotExternal(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "#") {
		return true
	}
	address, err := url.Parse(value)
	return err == nil && (address.IsAbs() || address.Host != "")
}

var companionSnapshotCSSURL = regexp.MustCompile(`(?i)url\(\s*([^)]*)\)`)
var companionSnapshotCSSImageSet = regexp.MustCompile(`(?i)(?:-webkit-)?image-set\s*\(`)
var companionSnapshotScriptURLs = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:fetch|import|importScripts|Worker|SharedWorker|WebSocket|EventSource)\s*\(\s*["']([^"']*)["']`),
	regexp.MustCompile(`(?i)\.\s*(?:src|href|poster|action)\s*=\s*["']([^"']*)["']`),
	regexp.MustCompile(`(?i)\.\s*setAttribute\s*\(\s*["'](?:src|href|poster|action)["']\s*,\s*["']([^"']*)["']`),
	regexp.MustCompile(`(?i)\b(?:import|export)\s+(?:[^;"']*?\bfrom\s*)?["']([^"']*)["']`),
	regexp.MustCompile(`(?i)\.\s*open\s*\(\s*["'](?:GET|POST|PUT|PATCH|DELETE|HEAD)["']\s*,\s*["']([^"']*)["']`),
}

// These checks reject known literal dependencies. They do not interpret code
// or claim to resolve URLs computed while a prototype is running.
func companionSnapshotJavaScript(value string) bool {
	for _, pattern := range companionSnapshotScriptURLs {
		for _, match := range pattern.FindAllStringSubmatch(value, -1) {
			if strings.Contains(match[1], "\\") || !companionSnapshotExternal(match[1]) {
				return false
			}
		}
	}
	return true
}

func companionSnapshotCSS(value string) bool {
	if strings.Contains(value, "\\") || strings.Contains(strings.ToLower(value), "@import") {
		return false
	}
	for _, match := range companionSnapshotCSSURL.FindAllStringSubmatch(value, -1) {
		if !companionSnapshotExternal(strings.Trim(strings.TrimSpace(match[1]), "\"'")) {
			return false
		}
	}
	for _, match := range companionSnapshotCSSImageSet.FindAllStringIndex(value, -1) {
		depth := 1
		for index := match[1]; index < len(value) && depth > 0; index++ {
			switch value[index] {
			case '(':
				depth++
			case ')':
				depth--
			case '\'', '"':
				end := strings.IndexByte(value[index+1:], value[index])
				if end < 0 {
					return false
				}
				end += index + 1
				if depth == 1 && !companionSnapshotExternal(value[index+1:end]) {
					return false
				}
				index = end
			}
		}
		if depth != 0 {
			return false
		}
	}
	return true
}

func companionSnapshotAsset(ctx context.Context, root *os.Root, value string) (string, error) {
	address, err := url.Parse(strings.TrimSpace(value))
	if err != nil || address.IsAbs() || address.Host != "" || address.User != nil || address.RawQuery != "" || address.Fragment != "" {
		return "", errCompanionSnapshotLive
	}
	name := strings.TrimPrefix(address.Path, "/")
	if !companionPreviewAssetPath(name) || previewHiddenPath(name) || path.Clean(name) != name {
		return "", errCompanionSnapshotLive
	}
	mime := map[string]string{
		".js": "text/javascript", ".css": "text/css", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml", ".ico": "image/x-icon",
		".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".wav": "audio/wav", ".ogg": "audio/ogg",
		".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	}[strings.ToLower(path.Ext(name))]
	if mime == "" {
		return "", errCompanionSnapshotLive
	}
	data, err := companionSnapshotRead(ctx, root, name, companionSnapshotAssetLimit)
	if err != nil || (mime == "text/css" && !companionSnapshotCSS(string(data))) || (mime == "text/javascript" && !companionSnapshotJavaScript(string(data))) {
		return "", errCompanionSnapshotLive
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func companionSnapshotRead(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := companionSnapshotRegularPath(root, name, false); err != nil {
		return nil, err
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errCompanionSnapshotLive
	}
	if err := companionSnapshotRegularPath(root, name, false); err != nil {
		return nil, err
	}
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, current) {
		return nil, errCompanionSnapshotLive
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit || ctx.Err() != nil {
		return nil, errCompanionSnapshotLive
	}
	return data, nil
}

func companionSnapshotRegularPath(root *os.Root, name string, directory bool) error {
	parts := strings.Split(name, "/")
	for index := range parts {
		info, err := root.Lstat(strings.Join(parts[:index+1], "/"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errCompanionSnapshotLive
		}
		if index < len(parts)-1 || directory {
			if !info.IsDir() {
				return errCompanionSnapshotLive
			}
		} else if !info.Mode().IsRegular() {
			return errCompanionSnapshotLive
		}
	}
	return nil
}

func (s *companionSession) preview(w http.ResponseWriter, r *http.Request, project companionProject) {
	request := previewRequest{Path: project.path, Action: "inspect"}
	if r.Method == http.MethodPost {
		var input struct {
			Target  string `json:"target"`
			Action  string `json:"action"`
			Install bool   `json:"install"`
			ID      string `json:"id,omitempty"`
		}
		if !companionDecode(w, r, &input, 1024) {
			return
		}
		if !validCompanionPreviewTarget(input.Target) || (input.Action != "start" && input.Action != "stop" && input.Action != "browser") || len(input.ID) > 160 {
			http.Error(w, "Choose Start, Stop, or Open on Desktop for a project preview.", http.StatusBadRequest)
			return
		}
		request.Target, request.Action, request.Install, request.ID = input.Target, input.Action, input.Install, input.ID
	} else if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	writer := &companionWriter{header: make(http.Header), job: &companionJob{}}
	s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodPost, "/preview", request))
	if writer.code >= 400 || ctx.Err() != nil {
		code := writer.code
		if code < 400 {
			code = http.StatusServiceUnavailable
		}
		http.Error(w, "Desktop could not update this preview. Check the project and its preview settings on Desktop.", code)
		return
	}
	var result struct {
		Targets []previewTarget `json:"targets"`
	}
	if json.Unmarshal(writer.buffer, &result) != nil {
		http.Error(w, "Desktop returned an invalid preview response.", http.StatusBadGateway)
		return
	}
	targets := []companionPreviewTarget{}
	for _, value := range result.Targets {
		if !validCompanionPreviewTarget(value.Target) {
			continue
		}
		target := companionPreviewTarget{Target: value.Target, Name: companionPublicText(value.Name, 160), Kind: value.Kind, Available: value.Available,
			NeedsInstall: value.NeedsInstall, Reason: companionPublicText(value.Reason, 2000), Status: value.Status, ID: value.ID, Error: companionPublicText(value.Error, 2000), Revision: value.Revision}
		logs := value.Logs
		if len(logs) > 40 {
			logs = logs[len(logs)-40:]
		}
		for _, log := range logs {
			target.Logs = append(target.Logs, companionPublicRunText(cleanPreviewLog(log), 1500))
		}
		targets = append(targets, target)
		if len(targets) == 22 {
			break
		}
	}
	writeJSON(w, map[string]any{"targets": targets})
}

func companionPreviewAssetPath(value string) bool {
	if len(value) > 4096 || strings.ContainsAny(value, "\\%\x00\r\n") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." || (strings.HasPrefix(part, ".") && part != ".vite") {
			return false
		}
	}
	// Vite's file-system endpoint can address files outside the shared project.
	if strings.HasPrefix(value, "@fs/") || value == "@fs" {
		return false
	}
	if strings.HasPrefix(value, "node_modules/") {
		return strings.HasPrefix(value, "node_modules/.vite/deps/") && (strings.HasSuffix(value, ".js") || strings.HasSuffix(value, ".js.map"))
	}
	return true
}

func (s *companionSession) previewContent(w http.ResponseWriter, r *http.Request, project companionProject, target, asset string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	if !validCompanionPreviewTarget(target) || !companionPreviewAssetPath(asset) || r.Header.Get("Upgrade") != "" || r.URL.Query().Get("glowbom_preview") != "" {
		http.Error(w, "This preview asset is not available.", http.StatusForbidden)
		return
	}
	if s.previews == nil {
		http.Error(w, "The Desktop preview service is unavailable.", http.StatusServiceUnavailable)
		return
	}
	s.previews.mu.Lock()
	managed := s.previews.sessions[project.path+"/"+target]
	var source previewTarget
	if managed != nil {
		source = managed.snapshot()
	}
	closed := s.previews.closed
	s.previews.mu.Unlock()
	if closed || managed == nil || source.Status != "running" || source.ID == "" || r.Header.Get("X-Glowbom-Preview-ID") != source.ID || !validPreviewBrowserURL(source.URL) {
		http.Error(w, "This preview stopped or changed. Start it again from Glowbom.", http.StatusConflict)
		return
	}
	address, _ := url.Parse(source.URL)
	address.Path = "/" + asset
	address.RawPath = ""
	address.RawQuery = r.URL.RawQuery
	address.Fragment = ""
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// A revoked pairing cancels requests even when the caller remains connected.
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	request, err := http.NewRequestWithContext(ctx, r.Method, address.String(), nil)
	if err != nil {
		http.Error(w, "Invalid preview asset.", http.StatusBadRequest)
		return
	}
	for _, header := range []string{"Accept", "Accept-Language", "Range", "If-None-Match", "If-Modified-Since"} {
		if value := r.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	if source.Kind == "static" && len(source.ID) >= 12 {
		request.AddCookie(&http.Cookie{Name: "glowbom_preview_" + source.ID[:12], Value: source.ID})
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(next *http.Request, previous []*http.Request) error {
		if len(previous) >= 5 || next.URL.Scheme != address.Scheme || next.URL.Host != address.Host || next.URL.User != nil || !companionPreviewAssetPath(strings.TrimPrefix(next.URL.Path, "/")) {
			return errors.New("preview redirect is not allowed")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		http.Error(w, "Desktop could not load this preview asset.", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	// Buffer one bounded asset so oversized or interrupted responses never report
	// a partial successful page, and no upstream cookies or credentials are sent.
	const maxPreviewAsset = 16 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPreviewAsset+1))
	if err != nil || len(data) > maxPreviewAsset {
		http.Error(w, "This preview asset is too large or could not be loaded.", http.StatusBadGateway)
		return
	}
	for _, header := range []string{"Content-Type", "Content-Encoding", "Content-Security-Policy", "ETag", "Last-Modified", "Accept-Ranges", "Content-Range"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Glowbom-Preview-Content", "1")
	if r.Method == http.MethodHead {
		if length := response.Header.Get("Content-Length"); length != "" {
			w.Header().Set("Content-Length", length)
		}
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
