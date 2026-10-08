package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	codexImageSourceLabel = "Glowbom Images (ChatGPT, gpt-image-2)"
	codexImageTimeout     = 5 * time.Minute
	codexImageBaseURL     = "https://chatgpt.com/backend-api/codex/images/"
	codexImageMaxResponse = 24 << 20
)

var codexImageAuthMu sync.Mutex

var codexImageCodexAuthFilePath = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

type codexImageCredential struct {
	Bearer             string
	Refresh            string
	AccountID          string
	Subject            string
	IssuedAt           int64
	AuthFile           string
	Expires            int64
	codexAuthFile      bool
	storedAccountID    string
	storedAccountIDAlt string
}

type codexImageFailure struct {
	Status int
	Code   string
}

func (e *codexImageFailure) Error() string {
	switch e.Code {
	case "reconnect":
		return "Reconnect ChatGPT in OpenCode or Codex to generate images."
	case "subscription":
		return "ChatGPT could not authorize image generation. Check its subscription connection or choose another image source."
	case "limits":
		return "ChatGPT image generation reached its usage limit. Try again later or choose another image source."
	case "refresh":
		return "ChatGPT could not refresh its connection. Reconnect in OpenCode or Codex."
	case "save":
		return "The refreshed ChatGPT connection could not be saved. Reconnect in OpenCode or Codex."
	case "reference":
		return "Use a valid PNG, JPEG, or WebP reference image."
	case "response":
		return "ChatGPT returned an invalid image. Try again."
	default:
		return "ChatGPT could not generate the image. Check its connection or choose another image source."
	}
}

type codexImageJWTClaims struct {
	Auth struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
	Subject string          `json:"sub"`
	Issued  json.RawMessage `json:"iat"`
	Expires json.RawMessage `json:"exp"`
}

func readCodexImageTokenClaims(token string) codexImageJWTClaims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return codexImageJWTClaims{}
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return codexImageJWTClaims{}
	}
	var claims codexImageJWTClaims
	if json.Unmarshal(data, &claims) != nil {
		return codexImageJWTClaims{}
	}
	return claims
}

func codexImageTokenClaims(token string) (string, int64) {
	claims := readCodexImageTokenClaims(token)
	return strings.TrimSpace(claims.Auth.AccountID), int64(parseRawJSONNumber(claims.Expires) * 1000)
}

func codexImageExpiration(raw json.RawMessage) int64 {
	seconds := normalizeOAuthExpiresToReferenceSeconds(parseRawJSONNumber(raw))
	if seconds <= 0 {
		return 0
	}
	return int64((seconds + 978307200) * 1000)
}

func readCodexImageCredential(path string, codexFile bool) (codexImageCredential, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return codexImageCredential{}, false
	}
	var auth map[string]json.RawMessage
	if json.Unmarshal(data, &auth) != nil {
		return codexImageCredential{}, false
	}
	var stored struct {
		Type         string          `json:"type"`
		Access       string          `json:"access"`
		Refresh      string          `json:"refresh"`
		Expires      json.RawMessage `json:"expires"`
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresAt    json.RawMessage `json:"expires_at"`
		AccountID    string          `json:"account_id"`
		AccountIDAlt string          `json:"accountId"`
		IDToken      string          `json:"id_token"`
	}
	key := "openai"
	if codexFile {
		key = "tokens"
	}
	if json.Unmarshal(auth[key], &stored) != nil || (!codexFile && !strings.EqualFold(strings.TrimSpace(stored.Type), "oauth")) {
		return codexImageCredential{}, false
	}
	credential := codexImageCredential{
		AuthFile: path, codexAuthFile: codexFile,
		storedAccountID: strings.TrimSpace(stored.AccountID), storedAccountIDAlt: strings.TrimSpace(stored.AccountIDAlt),
	}
	if codexFile {
		credential.Bearer, credential.Refresh = strings.TrimSpace(stored.AccessToken), strings.TrimSpace(stored.RefreshToken)
		credential.Expires = codexImageExpiration(stored.ExpiresAt)
	} else {
		credential.Bearer, credential.Refresh = strings.TrimSpace(stored.Access), strings.TrimSpace(stored.Refresh)
		credential.Expires = codexImageExpiration(stored.Expires)
	}
	if credential.Bearer == "" {
		return codexImageCredential{}, false
	}
	claims := readCodexImageTokenClaims(credential.Bearer)
	claimAccount := strings.TrimSpace(claims.Auth.AccountID)
	claimExpires := int64(parseRawJSONNumber(claims.Expires) * 1000)
	credential.Subject = strings.TrimSpace(claims.Subject)
	credential.IssuedAt = int64(parseRawJSONNumber(claims.Issued) * 1000)
	if codexFile {
		credential.AccountID = strings.TrimSpace(stored.AccountID)
	} else {
		credential.AccountID = claimAccount
	}
	if credential.AccountID == "" {
		credential.AccountID = strings.TrimSpace(stored.AccountID)
	}
	if credential.AccountID == "" {
		credential.AccountID = strings.TrimSpace(stored.AccountIDAlt)
	}
	if credential.AccountID == "" {
		credential.AccountID = claimAccount
	}
	if credential.AccountID == "" {
		credential.AccountID, _ = codexImageTokenClaims(stored.IDToken)
	}
	// The token's actual expiration takes precedence over a stale file timestamp.
	if claimExpires > 0 {
		credential.Expires = claimExpires
	}
	return credential, true
}

func codexImageCredentialCurrent(credential codexImageCredential) bool {
	return credential.AccountID != "" && (credential.Expires == 0 || credential.Expires > time.Now().Add(2*time.Minute).UnixMilli())
}

// Listing sources only reads local files. A current credential avoids unnecessary refreshes.
func codexImageSubscription() (codexImageCredential, bool) {
	var current, renewable codexImageCredential
	consider := func(credential codexImageCredential) {
		if codexImageCredentialCurrent(credential) {
			// Keep the selected user and workspace, but use their newer sign-in.
			// An older token can be revoked before its advertised expiration.
			newerSameAccount := credential.AccountID == current.AccountID &&
				credential.Subject != "" && credential.Subject == current.Subject &&
				current.IssuedAt > 0 && credential.IssuedAt > current.IssuedAt &&
				credential.IssuedAt <= time.Now().Add(2*time.Minute).UnixMilli()
			if current.AuthFile == "" || newerSameAccount {
				current = credential
			}
		} else if renewable.AuthFile == "" && credential.Refresh != "" {
			renewable = credential
		}
	}
	for _, path := range projectIconAuthFileCandidates() {
		credential, ok := readCodexImageCredential(path, false)
		if !ok {
			continue
		}
		consider(credential)
	}
	if path := codexImageCodexAuthFilePath(); path != "" {
		if credential, ok := readCodexImageCredential(path, true); ok {
			consider(credential)
		}
	}
	if current.AuthFile != "" {
		return current, true
	}
	return renewable, renewable.AuthFile != ""
}

func codexImageHTTPClient(timeout time.Duration) *http.Client {
	client := *http.DefaultClient
	client.Timeout = timeout
	// An image request is charged work and must not be replayed through a redirect.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func resolveCodexImageSubscription(ctx context.Context) (codexImageCredential, error) {
	codexImageAuthMu.Lock()
	defer codexImageAuthMu.Unlock()
	credential, ok := codexImageSubscription()
	if !ok {
		return codexImageCredential{}, &codexImageFailure{Code: "reconnect"}
	}
	if codexImageCredentialCurrent(credential) {
		return credential, nil
	}
	return refreshCodexImageCredential(ctx, credential)
}

func refreshCodexImageCredential(ctx context.Context, credential codexImageCredential) (codexImageCredential, error) {
	if credential.Refresh == "" {
		return codexImageCredential{}, &codexImageFailure{Code: "reconnect"}
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {credential.Refresh}, "client_id": {openAIOAuthClientID()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIOAuthIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return codexImageCredential{}, &codexImageFailure{Code: "refresh"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := codexImageHTTPClient(45 * time.Second).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return codexImageCredential{}, ctx.Err()
		}
		return codexImageCredential{}, &codexImageFailure{Code: "refresh"}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (32<<10)+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// OpenCode can rotate its tokens while this request is in flight.
		if latest, ok := readCodexImageCredential(credential.AuthFile, credential.codexAuthFile); ok && latest.Bearer != credential.Bearer && codexImageCredentialCurrent(latest) {
			return latest, nil
		}
		return codexImageCredential{}, &codexImageFailure{Code: "refresh", Status: resp.StatusCode}
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err != nil || len(body) > 32<<10 || json.Unmarshal(body, &tokens) != nil || strings.TrimSpace(tokens.AccessToken) == "" {
		return codexImageCredential{}, &codexImageFailure{Code: "refresh"}
	}
	refreshed := credential
	refreshed.Bearer = strings.TrimSpace(tokens.AccessToken)
	claims := readCodexImageTokenClaims(refreshed.Bearer)
	refreshed.Subject = strings.TrimSpace(claims.Subject)
	refreshed.IssuedAt = int64(parseRawJSONNumber(claims.Issued) * 1000)
	if token := strings.TrimSpace(tokens.RefreshToken); token != "" {
		refreshed.Refresh = token
	}
	refreshed.Expires = 0
	if tokens.ExpiresIn > 0 && tokens.ExpiresIn < 366*24*60*60 {
		refreshed.Expires = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).UnixMilli()
	}
	if account, expires := codexImageTokenClaims(refreshed.Bearer); account != "" || expires > 0 {
		if account != "" && (!credential.codexAuthFile || credential.AccountID == "") {
			refreshed.AccountID = account
		}
		if expires > 0 {
			refreshed.Expires = expires
		}
	}
	if refreshed.AccountID == "" {
		if account, _ := codexImageTokenClaims(tokens.IDToken); account != "" {
			refreshed.AccountID = account
		}
	}
	// Persist rotated refresh tokens even if reconnecting is required for account metadata.
	if latest, ok := readCodexImageCredential(credential.AuthFile, credential.codexAuthFile); ok && latest.Bearer != credential.Bearer && codexImageCredentialCurrent(latest) {
		return latest, nil
	}
	if ctx.Err() != nil {
		return codexImageCredential{}, ctx.Err()
	}
	if err := persistCodexImageCredential(credential, refreshed, tokens.IDToken); err != nil {
		return codexImageCredential{}, &codexImageFailure{Code: "save"}
	}
	if !codexImageCredentialCurrent(refreshed) {
		return codexImageCredential{}, &codexImageFailure{Code: "reconnect"}
	}
	return refreshed, nil
}

func persistCodexImageCredential(previous, credential codexImageCredential, idToken string) error {
	data, err := os.ReadFile(credential.AuthFile)
	if err != nil {
		return err
	}
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(data, &auth); err != nil {
		return err
	}
	key := "openai"
	if credential.codexAuthFile {
		key = "tokens"
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(auth[key], &stored); err != nil || stored == nil {
		return errors.New("credential record is unavailable")
	}
	readString := func(name string) string {
		var value string
		_ = json.Unmarshal(stored[name], &value)
		return strings.TrimSpace(value)
	}
	accessField, refreshField := "access", "refresh"
	if credential.codexAuthFile {
		accessField, refreshField = "access_token", "refresh_token"
	} else if !strings.EqualFold(readString("type"), "oauth") {
		return errors.New("credential changed during refresh")
	}
	if readString(accessField) != previous.Bearer || readString(refreshField) != previous.Refresh {
		return errors.New("credential changed during refresh")
	}
	if readString("account_id") != previous.storedAccountID || readString("accountId") != previous.storedAccountIDAlt {
		return errors.New("selected account changed during refresh")
	}
	set := func(name string, value any) { stored[name], _ = json.Marshal(value) }
	if credential.codexAuthFile {
		set("access_token", credential.Bearer)
		set("refresh_token", credential.Refresh)
		set("expires_at", credential.Expires)
		if idToken != "" {
			set("id_token", idToken)
		}
		auth["last_refresh"], _ = json.Marshal(time.Now().UTC().Format(time.RFC3339))
	} else {
		set("access", credential.Bearer)
		set("refresh", credential.Refresh)
		set("expires", credential.Expires)
	}
	if credential.AccountID != "" {
		set("account_id", credential.AccountID)
		if _, ok := stored["accountId"]; ok {
			set("accountId", credential.AccountID)
		}
	}
	auth[key], err = json.Marshal(stored)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(credential.AuthFile), ".auth-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), credential.AuthFile)
}

func codexImageReferenceURI(reference string) (string, error) {
	payload := strings.TrimSpace(reference)
	if strings.HasPrefix(payload, "data:") {
		header, encoded, ok := strings.Cut(payload, ",")
		if !ok || (header != "data:image/png;base64" && header != "data:image/jpeg;base64" && header != "data:image/webp;base64") {
			return "", &codexImageFailure{Code: "reference"}
		}
		payload = encoded
	}
	if payload == "" || len(payload) > base64.StdEncoding.EncodedLen(projectIconMaxBytes) {
		return "", &codexImageFailure{Code: "reference"}
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(data) == 0 || len(data) > projectIconMaxBytes {
		return "", &codexImageFailure{Code: "reference"}
	}
	mime := http.DetectContentType(data)
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
		return "", &codexImageFailure{Code: "reference"}
	}
	return "data:" + mime + ";base64," + payload, nil
}

func codexImageSourceAvailability(ctx context.Context) (bool, string) {
	transport := strings.TrimSpace(os.Getenv("GLOWBOM_CHATGPT_IMAGE_TRANSPORT"))
	if codexAppServerImagesEnabled() {
		available, code := codexAppServerImageAvailability(ctx)
		if code != "" {
			code = "codex_" + code
		}
		return available, code
	}
	if transport != "" {
		return false, "codex_transport"
	}
	_, available := codexImageSubscription()
	return available, ""
}

func codexImageTransportError() error {
	return errors.New("Set GLOWBOM_CHATGPT_IMAGE_TRANSPORT to codex-app-server, or unset it to use the default image connection.")
}

// The opt-in transport lets Codex own login and native image generation.
func callCodexImageGeneration(ctx context.Context, prompt, reference, aspect string) (string, error) {
	if codexAppServerImagesEnabled() {
		return callCodexAppServerImageGeneration(ctx, prompt, reference, aspect)
	}
	if strings.TrimSpace(os.Getenv("GLOWBOM_CHATGPT_IMAGE_TRANSPORT")) != "" {
		return "", codexImageTransportError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, codexImageTimeout)
	defer cancel()
	body := map[string]any{
		"model": openAIImageModelID, "prompt": imageAspectPrompt(prompt, aspect, strings.TrimSpace(reference) != ""),
		"quality": "low", "size": imageGenerationSize(aspect), "background": "opaque", "n": 1,
	}
	endpoint := "generations"
	if strings.TrimSpace(reference) != "" {
		imageURI, err := codexImageReferenceURI(reference)
		if err != nil {
			return "", err
		}
		body["images"] = []map[string]string{{"image_url": imageURI}}
		endpoint = "edits"
	}
	credential, err := resolveCodexImageSubscription(ctx)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", &codexImageFailure{Code: "request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexImageBaseURL+endpoint, bytes.NewReader(encoded))
	if err != nil {
		return "", &codexImageFailure{Code: "request"}
	}
	setChatGPTCodexHeaders(req, credential.Bearer, credential.AccountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", "glowbom")
	var turnID [16]byte
	if _, err := rand.Read(turnID[:]); err != nil {
		return "", &codexImageFailure{Code: "request"}
	}
	turnID[6], turnID[8] = (turnID[6]&0x0f)|0x40, (turnID[8]&0x3f)|0x80
	req.Header.Set("x-codex-image-turn-id", fmt.Sprintf("%x-%x-%x-%x-%x", turnID[:4], turnID[4:6], turnID[6:8], turnID[8:10], turnID[10:]))
	resp, err := codexImageHTTPClient(codexImageTimeout).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &codexImageFailure{Code: "request"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "request"
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			code = "reconnect"
		case http.StatusForbidden:
			code = "subscription"
		case http.StatusTooManyRequests:
			code = "limits"
		}
		return "", &codexImageFailure{Status: resp.StatusCode, Code: code}
	}
	response, err := io.ReadAll(io.LimitReader(resp.Body, codexImageMaxResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &codexImageFailure{Code: "response"}
	}
	var result struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if len(response) > codexImageMaxResponse || json.Unmarshal(response, &result) != nil || len(result.Data) == 0 {
		return "", &codexImageFailure{Code: "response"}
	}
	imageData, err := base64.StdEncoding.DecodeString(result.Data[0].B64JSON)
	if err != nil || len(imageData) == 0 {
		return "", &codexImageFailure{Code: "response"}
	}
	mime := http.DetectContentType(imageData)
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
		return "", &codexImageFailure{Code: "response"}
	}
	return fmt.Sprintf("data:%s;base64,%s", mime, result.Data[0].B64JSON), nil
}
