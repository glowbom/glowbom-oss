package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Keep the V1 SDK at the application boundary. V2 has native routes and events.
var openCodeProtocols sync.Map

func openCodeProtocol(serverURL string) string {
	value, _ := openCodeProtocols.Load(strings.TrimRight(serverURL, "/"))
	protocol, _ := value.(string)
	return protocol
}

func setOpenCodeProtocol(serverURL, protocol string) {
	openCodeProtocols.Store(strings.TrimRight(serverURL, "/"), protocol)
}

func openCodeHTTPClient(timeoutClient *http.Client) *http.Client {
	client := *timeoutClient
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = &openCodeTransport{base: base}
	return &client
}

type openCodeTransport struct{ base http.RoundTripper }

func (t *openCodeTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	server := original.URL.Scheme + "://" + original.URL.Host
	if openCodeProtocol(server) != "v2" {
		return t.base.RoundTrip(original)
	}
	req := original.Clone(original.Context())
	endpoint := *original.URL
	req.URL = &endpoint
	path := endpoint.Path
	var body map[string]any
	if original.Body != nil {
		defer original.Body.Close()
		if err := json.NewDecoder(io.LimitReader(original.Body, 32<<20)).Decode(&body); err != nil && err != io.EOF {
			return nil, err
		}
	}
	query := endpoint.Query()
	directory := query.Get("directory")
	query.Del("directory")
	query.Del("recursive")
	if directory != "" && (path == "/session" || path == "/provider" || path == "/config/providers") {
		query.Set("location[directory]", directory)
	}
	endpoint.RawQuery = query.Encode()
	switch {
	case path == "/event":
		endpoint.Path = "/api/event"
		resp, err := t.base.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			return resp, err
		}
		resp.Body = translateOpenCodeEvents(resp.Body)
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return resp, nil
	case path == "/experimental/tool/ids":
		// V2 chat sessions deny every action in the session ruleset.
		return adapterJSON(original, []string{}, http.StatusOK), nil
	case path == "/provider" || path == "/config/providers":
		return t.providers(req, path == "/config/providers")
	case path == "/session/status":
		resp, err := t.native(req, "GET", "/api/session/active", nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			return resp, err
		}
		data, err := readAdapterData(resp)
		if err != nil {
			return nil, err
		}
		statuses := map[string]any{}
		if active, ok := data.(map[string]any); ok {
			for id := range active {
				statuses[id] = map[string]string{"type": "busy"}
			}
		}
		return adapterJSON(original, statuses, http.StatusOK), nil
	case path == "/session" && req.Method == http.MethodPost:
		translated := map[string]any{}
		if title, ok := body["title"]; ok {
			translated["title"] = title
		}
		if directory != "" {
			translated["location"] = map[string]string{"directory": directory}
		}
		if rules, ok := body["permission"].([]any); ok {
			var permissions []map[string]any
			for _, raw := range rules {
				if rule, ok := raw.(map[string]any); ok {
					permissions = append(permissions, map[string]any{"action": rule["permission"], "resource": rule["pattern"], "effect": rule["action"]})
				}
			}
			translated["permissions"] = permissions
		}
		body = translated
	case strings.HasPrefix(path, "/session/") && req.Method == http.MethodPost && (strings.HasSuffix(path, "/message") || strings.HasSuffix(path, "/prompt_async")):
		return t.prompt(req, path, body)
	case strings.HasSuffix(path, "/abort"):
		path = strings.TrimSuffix(path, "/abort") + "/interrupt"
		body = nil
	case strings.Contains(path, "/permissions/"):
		path = strings.Replace(path, "/permissions/", "/permission/", 1) + "/reply"
		resp, err := t.native(req, http.MethodPost, "/api"+path, map[string]any{"decision": body["response"]})
		if err != nil || resp.StatusCode != http.StatusNoContent {
			return resp, err
		}
		resp.Body.Close()
		return adapterJSON(original, true, http.StatusOK), nil
	case strings.HasPrefix(path, "/permission/"):
		return nil, fmt.Errorf("OpenCode V2 permission replies require a session ID")
	case strings.HasPrefix(path, "/auth/"):
		return t.auth(req, path, body)
	case strings.HasPrefix(path, "/question/"):
		return nil, fmt.Errorf("OpenCode V2 question replies require a session ID")
	}
	endpoint.Path = "/api" + path
	setAdapterBody(req, body)
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.StatusCode == http.StatusNoContent {
		return resp, err
	}
	data, err := readAdapterData(resp)
	if err != nil {
		return nil, err
	}
	if path == "/session" || (strings.HasPrefix(path, "/session/") && !strings.Contains(strings.TrimPrefix(path, "/session/"), "/")) {
		if items, ok := data.([]any); ok {
			for _, item := range items {
				translateSession(item)
			}
		} else {
			translateSession(data)
		}
	}
	if strings.HasSuffix(path, "/message") && req.Method == http.MethodGet {
		items, _ := data.([]any)
		messages := make([]any, 0, len(items))
		for _, item := range items {
			if msg, ok := item.(map[string]any); ok {
				messages = append(messages, translateMessage(msg, strings.Split(path, "/")[2]))
			}
		}
		data = messages
	}
	return adapterJSON(original, data, resp.StatusCode), nil
}

// Only manage Glowbom's own credential. Existing OpenCode logins are preserved.
func (t *openCodeTransport) auth(req *http.Request, path string, body map[string]any) (*http.Response, error) {
	if path != "/auth/openai" {
		return nil, fmt.Errorf("This OpenCode V2 credential is not supported")
	}
	credentialID := "cred_glowbom_openai"
	if req.Method == http.MethodDelete {
		resp, err := t.native(req, "DELETE", "/api/credential/"+credentialID, nil)
		if err != nil || (resp.StatusCode != http.StatusNotFound && (resp.StatusCode < 200 || resp.StatusCode >= 300)) {
			return resp, err
		}
		resp.Body.Close()
		return adapterJSON(req, true, http.StatusOK), nil
	}
	if body["type"] != "oauth" {
		return nil, fmt.Errorf("This OpenCode V2 credential type is not supported")
	}
	resp, err := t.native(req, "GET", "/api/provider", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	data, err := readAdapterData(resp)
	if err != nil {
		return nil, err
	}
	integrationID := "openai"
	if providers, ok := data.([]any); ok {
		for _, raw := range providers {
			if provider, ok := raw.(map[string]any); ok && provider["id"] == "openai" {
				if id, ok := provider["integrationID"].(string); ok {
					integrationID = id
				}
			}
		}
	}
	resp, err = t.native(req, "GET", "/api/integration", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	data, err = readAdapterData(resp)
	if err != nil {
		return nil, err
	}
	methodID := ""
	var oauthMethods []string
	if integrations, ok := data.([]any); ok {
		for _, raw := range integrations {
			integration, ok := raw.(map[string]any)
			if !ok || integration["id"] != integrationID {
				continue
			}
			methods, _ := integration["methods"].([]any)
			for _, raw := range methods {
				method, ok := raw.(map[string]any)
				if !ok || method["type"] != "oauth" {
					continue
				}
				if id, ok := method["id"].(string); ok {
					oauthMethods = append(oauthMethods, id)
					// V2's legacy credential migration also selects this method.
					if id == "chatgpt-browser" {
						methodID = id
					}
				}
			}
		}
	}
	if methodID == "" && len(oauthMethods) == 1 {
		methodID = oauthMethods[0]
	}
	if methodID == "" {
		return nil, fmt.Errorf("OpenCode V2 does not expose an OpenAI OAuth method. Sign in with opencode auth login and choose OpenCode's existing login in Glowbom")
	}
	resp, err = t.native(req, "DELETE", "/api/credential/"+credentialID, nil)
	if err != nil || (resp.StatusCode != http.StatusNotFound && (resp.StatusCode < 200 || resp.StatusCode >= 300)) {
		return resp, err
	}
	resp.Body.Close()
	value := map[string]any{"type": "oauth", "methodID": methodID, "access": body["access"], "refresh": body["refresh"], "expires": body["expires"]}
	if accountID, ok := body["accountId"].(string); ok && strings.TrimSpace(accountID) != "" {
		value["accountId"] = strings.TrimSpace(accountID)
	}
	resp, err = t.native(req, "POST", "/api/credential", map[string]any{"id": credentialID, "integrationID": integrationID, "label": "Glowbom", "value": value, "activate": true})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Credential responses contain secrets. Do not pass them to SDK errors or logs.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OpenCode V2 rejected the Glowbom login (HTTP %d)", resp.StatusCode)
	}
	return adapterJSON(req, true, http.StatusOK), nil
}

func setAdapterBody(req *http.Request, body any) {
	if body == nil {
		req.Body = nil
		req.ContentLength = 0
		req.GetBody = nil
		return
	}
	data, _ := json.Marshal(body)
	req.Body = io.NopCloser(bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	req.Header.Set("Content-Type", "application/json")
}

func readAdapterData(resp *http.Response) (any, error) {
	defer resp.Body.Close()
	var value struct {
		Data any `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&value); err != nil {
		return nil, fmt.Errorf("Could not decode the OpenCode V2 response")
	}
	return value.Data, nil
}

func adapterJSON(req *http.Request, value any, status int) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: req}
}

func translateSession(raw any) {
	value, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if location, ok := value["location"].(map[string]any); ok {
		value["directory"] = location["directory"]
	}
	if rules, ok := value["permissions"].([]any); ok {
		var translated []map[string]any
		for _, raw := range rules {
			if rule, ok := raw.(map[string]any); ok {
				translated = append(translated, map[string]any{"permission": rule["action"], "pattern": rule["resource"], "action": rule["effect"]})
			}
		}
		value["permission"] = translated
	}
}

func (t *openCodeTransport) native(req *http.Request, method, path string, body any) (*http.Response, error) {
	next := req.Clone(req.Context())
	endpoint := *req.URL
	next.URL = &endpoint
	next.Method, endpoint.Path = method, path
	setAdapterBody(next, body)
	return t.base.RoundTrip(next)
}

func (t *openCodeTransport) providers(req *http.Request, sdk bool) (*http.Response, error) {
	var providers, models []any
	deadline := time.Now().Add(8 * time.Second)
	for {
		resp, err := t.native(req, "GET", "/api/provider", nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			return resp, err
		}
		data, err := readAdapterData(resp)
		if err != nil {
			return nil, err
		}
		providers, _ = data.([]any)
		resp, err = t.native(req, "GET", "/api/model", nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			return resp, err
		}
		data, err = readAdapterData(resp)
		if err != nil {
			return nil, err
		}
		models, _ = data.([]any)
		// V2 boots each location lazily. Its first catalog snapshots can be empty
		// while plugins register the configured providers and their models.
		if (len(providers) > 0 && len(models) > 0) || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	all := make([]any, 0, len(providers))
	connected := []string{}
	defaults := map[string]string{}
	for _, raw := range providers {
		provider, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := provider["id"].(string)
		catalog := map[string]any{}
		available := false
		for _, rawModel := range models {
			model, ok := rawModel.(map[string]any)
			if !ok || model["providerID"] != id {
				continue
			}
			modelID, _ := model["id"].(string)
			variants := map[string]any{}
			if values, ok := model["variants"].([]any); ok {
				for _, raw := range values {
					if value, ok := raw.(map[string]any); ok {
						if name, ok := value["id"].(string); ok {
							variants[name] = value
						}
					}
				}
			}
			translated := map[string]any{"id": modelID, "name": model["name"], "status": model["status"], "limit": model["limit"], "variants": variants}
			if capabilities, ok := model["capabilities"].(map[string]any); ok {
				translated["tool_call"] = capabilities["tools"]
				attachment := false
				if inputs, ok := capabilities["input"].([]any); ok {
					for _, input := range inputs {
						if input == "image" {
							attachment = true
						}
					}
				}
				translated["attachment"] = attachment
				translated["modalities"] = map[string]any{"input": capabilities["input"], "output": capabilities["output"]}
			}
			catalog[modelID] = translated
			if model["enabled"] == true {
				available = true
				if defaults[id] == "" {
					defaults[id] = modelID
				}
			}
		}
		if available && provider["activation"] != "disabled" {
			connected = append(connected, id)
		}
		all = append(all, map[string]any{"id": id, "name": provider["name"], "models": catalog})
	}
	if sdk {
		return adapterJSON(req, map[string]any{"providers": all, "default": defaults}, http.StatusOK), nil
	}
	return adapterJSON(req, map[string]any{"all": all, "connected": connected, "default": defaults}, http.StatusOK), nil
}

func (t *openCodeTransport) prompt(req *http.Request, path string, body map[string]any) (*http.Response, error) {
	root := path[:strings.LastIndex(path, "/")]
	if system, ok := body["system"].(string); ok && system != "" {
		// Keep conversation guidance in V2's session instructions.
		resp, err := t.native(req, "PUT", "/api/experimental"+root+"/instructions/entries/glowbom", map[string]any{"value": system})
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return resp, err
		}
		resp.Body.Close()
	}
	if model, ok := body["model"].(map[string]any); ok {
		ref := map[string]any{"id": model["modelID"], "providerID": model["providerID"]}
		if variant, ok := body["variant"].(string); ok && variant != "" {
			ref["variant"] = variant
		}
		resp, err := t.native(req, "POST", "/api"+root+"/model", map[string]any{"model": ref})
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return resp, err
		}
		resp.Body.Close()
	}
	var text []string
	files := []map[string]any{}
	if parts, ok := body["parts"].([]any); ok {
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "text":
				if value, ok := part["text"].(string); ok {
					text = append(text, value)
				}
			case "file":
				file := map[string]any{"uri": part["url"]}
				if name, ok := part["filename"]; ok {
					file["name"] = name
				}
				files = append(files, file)
			default:
				return nil, fmt.Errorf("This prompt attachment is not supported by the OpenCode V2 adapter")
			}
		}
	}
	prompt := map[string]any{"text": strings.Join(text, "\n\n")}
	if len(files) > 0 {
		prompt["files"] = files
	}
	resp, err := t.native(req, "POST", "/api"+root+"/prompt", prompt)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	accepted, err := readAdapterData(resp)
	if err != nil {
		return nil, err
	}
	input, _ := accepted.(map[string]any)
	started := messageTime(input)
	if strings.HasSuffix(path, "/prompt_async") {
		return adapterJSON(req, nil, http.StatusOK), nil
	}
	resp, err = t.native(req, "POST", "/api/experimental"+root+"/wait", nil)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	resp.Body.Close()
	resp, err = t.native(req, "GET", "/api"+root+"/message", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	data, err := readAdapterData(resp)
	if err != nil {
		return nil, err
	}
	items, _ := data.([]any)
	var latest map[string]any
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok || msg["type"] != "assistant" || messageTime(msg) < started {
			continue
		}
		if latest == nil || messageTime(msg) > messageTime(latest) {
			latest = msg
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("OpenCode V2 finished without an assistant message")
	}
	return adapterJSON(req, translateMessage(latest, strings.TrimPrefix(root, "/session/")), http.StatusOK), nil
}

func messageTime(msg map[string]any) float64 {
	value, _ := msg["time"].(map[string]any)
	created, _ := value["created"].(float64)
	return created
}

func translateMessage(msg map[string]any, sessionID string) map[string]any {
	info := map[string]any{"id": msg["id"], "sessionID": sessionID, "role": msg["type"], "time": msg["time"], "cost": msg["cost"], "tokens": msg["tokens"], "error": msg["error"]}
	if model, ok := msg["model"].(map[string]any); ok {
		info["modelID"], info["providerID"] = model["id"], model["providerID"]
	}
	parts := []any{}
	if content, ok := msg["content"].([]any); ok {
		for index, raw := range content {
			if part, ok := raw.(map[string]any); ok {
				callID := part["id"]
				part["sessionID"], part["messageID"], part["id"] = sessionID, msg["id"], fmt.Sprintf("%v:%d", msg["id"], index)
				if part["type"] == "tool" {
					part["tool"], part["callID"] = part["name"], callID
				}
				parts = append(parts, part)
			}
		}
	} else if text, ok := msg["text"]; ok {
		parts = append(parts, map[string]any{"type": "text", "text": text, "sessionID": sessionID, "messageID": msg["id"]})
	}
	return map[string]any{"info": info, "parts": parts}
}

// An event stream owns its accumulators. Concurrent builds cannot share text state.
func translateOpenCodeEvents(source io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		scanner := bufio.NewScanner(source)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		state := openCodeEventState{parts: map[string]map[string]any{}}
		var lines []string
		flush := func() error {
			if len(lines) == 0 {
				return nil
			}
			data := strings.Join(lines, "\n")
			lines = nil
			for _, event := range state.translate([]byte(data)) {
				encoded, err := json.Marshal(event)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
					return err
				}
			}
			return nil
		}
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
			if line == "" {
				if err := flush(); err != nil {
					writer.CloseWithError(err)
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			writer.CloseWithError(err)
			return
		}
		writer.CloseWithError(flush())
	}()
	return &adapterEventReader{PipeReader: reader, source: source}
}

type adapterEventReader struct {
	*io.PipeReader
	source io.ReadCloser
}

func (r *adapterEventReader) Close() error { r.source.Close(); return r.PipeReader.Close() }

type openCodeEventState struct{ parts map[string]map[string]any }

func (s *openCodeEventState) translate(encoded []byte) []map[string]any {
	var event struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(encoded, &event) != nil || event.Data == nil {
		return nil
	}
	d := event.Data
	emit := func(kind string, props any) []map[string]any {
		return []map[string]any{{"type": kind, "properties": props}}
	}
	sid, mid := d["sessionID"], d["assistantMessageID"]
	base := map[string]any{"sessionID": sid}
	switch event.Type {
	case "session.execution.started":
		base["status"] = map[string]string{"type": "busy"}
		return emit("session.status", base)
	case "session.execution.succeeded", "session.execution.interrupted":
		for key, part := range s.parts {
			if part["sessionID"] == sid {
				delete(s.parts, key)
			}
		}
		return emit("session.idle", base)
	case "session.execution.failed", "session.step.failed":
		base["error"] = d["error"]
		return emit("session.error", base)
	case "session.retry.scheduled":
		base["status"] = map[string]any{"type": "retry", "attempt": d["attempt"], "next": d["at"], "message": extractSessionErrorMessage(map[string]any{"error": d["error"]})}
		return emit("session.status", base)
	case "session.step.started", "session.step.ended":
		return emit("message.updated", map[string]any{"info": map[string]any{"id": mid, "sessionID": sid, "role": "assistant", "cost": d["cost"], "tokens": d["tokens"], "time": map[string]any{"created": d["started"]}}})
	case "session.text.started", "session.text.delta", "session.text.ended", "session.reasoning.started", "session.reasoning.delta", "session.reasoning.ended":
		kind := strings.Split(event.Type, ".")[1]
		key := fmt.Sprintf("%v:%v:%v", sid, mid, d["ordinal"])
		part := s.parts[key]
		if part == nil {
			part = map[string]any{"id": key, "sessionID": sid, "messageID": mid, "type": kind, "text": ""}
			s.parts[key] = part
		}
		if text, ok := d["text"].(string); ok {
			part["text"] = text
		} else if delta, ok := d["delta"].(string); ok {
			part["text"] = part["text"].(string) + delta
		}
		return emit("message.part.updated", map[string]any{"part": part})
	case "session.tool.input.started", "session.tool.called", "session.tool.progress", "session.tool.success", "session.tool.failed":
		key := fmt.Sprintf("%v:%v:%v", sid, mid, d["id"])
		part := s.parts[key]
		if part == nil {
			part = map[string]any{"id": key, "sessionID": sid, "messageID": mid, "type": "tool", "callID": d["id"], "tool": d["name"]}
			s.parts[key] = part
		}
		status := "running"
		if event.Type == "session.tool.input.started" {
			status = "pending"
		}
		if event.Type == "session.tool.success" {
			status = "completed"
		}
		if event.Type == "session.tool.failed" {
			status = "error"
		}
		state, _ := part["state"].(map[string]any)
		if state == nil {
			state = map[string]any{}
		}
		state["status"] = status
		for _, key := range []string{"input", "metadata", "error"} {
			if value, ok := d[key]; ok {
				state[key] = value
			}
		}
		part["state"] = state
		return emit("message.part.updated", map[string]any{"part": part})
	case "permission.asked":
		d["permission"], d["patterns"] = d["action"], d["resources"]
		return emit("permission.asked", d)
	case "form.created":
		form, ok := d["form"].(map[string]any)
		if !ok {
			return nil
		}
		questions := []any{}
		fields, _ := form["fields"].([]any)
		for _, raw := range fields {
			field, ok := raw.(map[string]any)
			if !ok || field["hidden"] == true {
				continue
			}
			options := []any{}
			if values, ok := field["options"].([]any); ok {
				for _, raw := range values {
					if option, ok := raw.(map[string]any); ok {
						options = append(options, map[string]any{"label": option["label"], "description": option["description"]})
					}
				}
			}
			questions = append(questions, map[string]any{"id": field["key"], "header": form["title"], "question": field["title"], "options": options, "multiple": field["type"] == "multiselect"})
		}
		return emit("question.asked", map[string]any{"id": form["id"], "sessionID": form["sessionID"], "questions": questions})
	}
	return nil
}

// replyV2Form uses the native field keys, rather than V1's positional arrays.
func replyV2Form(req *http.Request, sessionID, formID string, answers [][]string) (*http.Response, error) {
	transport := &openCodeTransport{base: http.DefaultTransport}
	path := "/api/session/" + url.PathEscape(sessionID) + "/form/" + url.PathEscape(formID)
	resp, err := transport.native(req, "GET", path, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	data, err := readAdapterData(resp)
	if err != nil {
		return nil, err
	}
	form, _ := data.(map[string]any)
	fields, _ := form["fields"].([]any)
	answer := map[string]any{}
	index := 0
	for _, raw := range fields {
		field, ok := raw.(map[string]any)
		if !ok || field["hidden"] == true {
			continue
		}
		if index >= len(answers) {
			return nil, fmt.Errorf("Answer every OpenCode question before continuing")
		}
		selected := answers[index]
		index++
		selected = append([]string(nil), selected...)
		if options, ok := field["options"].([]any); ok {
			for index, label := range selected {
				for _, raw := range options {
					if option, ok := raw.(map[string]any); ok && option["label"] == label {
						if value, ok := option["value"].(string); ok {
							selected[index] = value
						}
						break
					}
				}
			}
		}
		if field["type"] == "multiselect" {
			answer[field["key"].(string)] = selected
			continue
		}
		if field["type"] != "string" {
			return nil, fmt.Errorf("This OpenCode form field needs to be answered in OpenCode")
		}
		if len(selected) != 1 {
			return nil, fmt.Errorf("Choose one answer for this OpenCode question")
		}
		answer[field["key"].(string)] = selected[0]
	}
	return transport.native(req, "POST", path+"/reply", map[string]any{"answer": answer})
}
