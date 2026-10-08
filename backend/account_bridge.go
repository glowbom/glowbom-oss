// Created by Codex under Jacob's direction, Glowbom Labs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type localAccountStatus struct {
	RemainingCredits   *float64 `json:"remainingCredits,omitempty"`
	AllowanceCredits   *float64 `json:"allowanceCredits,omitempty"`
	Version            int      `json:"version"`
	Status             string   `json:"status"`
	UID                string   `json:"uid,omitempty"`
	Email              string   `json:"email,omitempty"`
	SubscriptionStatus string   `json:"subscriptionStatus,omitempty"`
	Code               string   `json:"code,omitempty"`
}

type accountCLIRunner func(context.Context, ...string) ([]byte, error)

// One operation at a time prevents the bridge from rotating credentials during
// login or restoring them after logout. No account result or token is persisted.
type accountBridge struct {
	mu         sync.Mutex
	run        accountCLIRunner
	busy       bool
	loginState string
	cancel     context.CancelFunc
	loginDone  chan struct{}
	token      string
}

func newAccountBridge(run accountCLIRunner) *accountBridge {
	return &accountBridge{run: run, loginState: "idle", token: glowbomServerToken()}
}

func (b *accountBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Unlike optional local development routes, account operations must always
	// have a backend token, including when the stack was started manually.
	if b.token == "" || !hasValidGlowbomServerToken(r, b.token) {
		accountBridgeReply(w, http.StatusUnauthorized, accountUnavailable("backend_auth_required"))
		return
	}
	switch r.URL.Path {
	case "/account/status":
		if !accountMethod(w, r, http.MethodGet) {
			return
		}
		b.readStatus(w, r)
	case "/account/login":
		if r.Method == http.MethodGet {
			b.mu.Lock()
			state := b.loginState
			b.mu.Unlock()
			accountBridgeReply(w, http.StatusOK, map[string]any{"version": 1, "state": state})
			return
		}
		if !accountMethod(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		b.startLogin(w)
	case "/account/login/cancel":
		if !accountMethod(w, r, http.MethodPost) {
			return
		}
		b.mu.Lock()
		if b.cancel != nil {
			b.cancel()
		}
		b.mu.Unlock()
		accountBridgeReply(w, http.StatusAccepted, map[string]any{"version": 1, "state": "canceling"})
	case "/account/project":
		b.downloadProject(w, r)
	case "/account/logout":
		if !accountMethod(w, r, http.MethodPost) {
			return
		}
		if !b.begin() {
			accountBridgeReply(w, http.StatusConflict, accountUnavailable("account_busy"))
			return
		}
		defer b.end()
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if _, err := b.run(ctx, "logout"); err != nil {
			accountBridgeReply(w, http.StatusServiceUnavailable, accountUnavailable("logout_failed"))
			return
		}
		b.mu.Lock()
		b.loginState = "idle"
		b.mu.Unlock()
		accountBridgeReply(w, http.StatusOK, localAccountStatus{Version: 1, Status: "signed_out"})
	default:
		accountBridgeReply(w, http.StatusNotFound, accountUnavailable("not_found"))
	}
}

func (b *accountBridge) begin() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy {
		return false
	}
	b.busy = true
	return true
}

func (b *accountBridge) end() {
	b.mu.Lock()
	b.busy = false
	b.mu.Unlock()
}

func (b *accountBridge) readStatus(w http.ResponseWriter, r *http.Request) {
	if !b.begin() {
		accountBridgeReply(w, http.StatusConflict, accountUnavailable("account_busy"))
		return
	}
	defer b.end()
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	output, err := b.run(ctx, "account", "--json")
	var status localAccountStatus
	if errors.Is(err, errAccountCLIMissing) {
		status = accountUnavailable("cli_unavailable")
	} else if len(output) > 16384 || json.Unmarshal(output, &status) != nil || !validAccountStatus(status) {
		status = accountUnavailable("cli_update_required")
		if ctx.Err() != nil {
			status.Code = "account_unavailable"
		}
	} else if err != nil && status.Status != "unavailable" {
		status = accountUnavailable("account_unavailable")
	}
	// Re-encode only the public fields, never arbitrary CLI output or stderr.
	if status.Status != "signed_in" {
		status.UID, status.Email, status.SubscriptionStatus = "", "", ""
		status.RemainingCredits, status.AllowanceCredits = nil, nil
	}
	accountBridgeReply(w, http.StatusOK, status)
}

func validAccountStatus(s localAccountStatus) bool {
	for _, value := range []*float64{s.RemainingCredits, s.AllowanceCredits} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return false
		}
	}
	if s.Version != 1 || len(s.UID) > 128 || len(s.Email) > 320 {
		return false
	}
	if s.Status == "signed_in" {
		if s.UID == "" {
			return false
		}
		switch s.SubscriptionStatus {
		case "premium", "trialing", "canceled", "no subs", "unknown":
		default:
			return false
		}
	} else if s.Status != "signed_out" && s.Status != "unavailable" {
		return false
	}
	switch s.Code {
	case "", "sign_in_required", "account_unavailable", "account_not_ready":
		return true
	default:
		return false
	}
}

func (b *accountBridge) startLogin(w http.ResponseWriter) {
	b.mu.Lock()
	if b.busy {
		pending := b.loginState == "pending"
		b.mu.Unlock()
		if pending {
			accountBridgeReply(w, http.StatusAccepted, map[string]any{"version": 1, "state": "pending"})
		} else {
			accountBridgeReply(w, http.StatusConflict, accountUnavailable("account_busy"))
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute+10*time.Second)
	b.busy, b.loginState, b.cancel = true, "pending", cancel
	b.loginDone = make(chan struct{})
	done := b.loginDone
	b.mu.Unlock()
	go func() {
		_, err := b.run(ctx, "login")
		b.mu.Lock()
		b.loginState = "complete"
		if err != nil {
			b.loginState = "failed"
		}
		if ctx.Err() == context.Canceled {
			b.loginState = "canceled"
		}
		b.busy, b.cancel = false, nil
		close(done)
		b.mu.Unlock()
		cancel()
	}()
	accountBridgeReply(w, http.StatusAccepted, map[string]any{"version": 1, "state": "pending"})
}

func accountUnavailable(code string) localAccountStatus {
	return localAccountStatus{Version: 1, Status: "unavailable", Code: code}
}

func accountBridgeReply(w http.ResponseWriter, code int, value any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func accountMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	accountBridgeReply(w, http.StatusMethodNotAllowed, accountUnavailable("method_not_allowed"))
	return false
}

var errAccountCLIMissing = errors.New("Glowbom CLI unavailable")

func accountCommandCapturesStdout(args []string) bool {
	return len(args) > 0 && (args[0] == "account" || args[0] == "export")
}

type accountCommandFailure struct {
	code string
}

func (e *accountCommandFailure) Error() string { return e.code }

type accountOutput struct{ data []byte }

func (w *accountOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > 16384 {
		return 0, errors.New("account output too large")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}

func runAccountCLI(ctx context.Context, args ...string) ([]byte, error) {
	executable := strings.TrimSpace(os.Getenv("GLOWBOM_CLI_BIN"))
	if executable == "" {
		var err error
		executable, err = exec.LookPath("glowbom")
		if err != nil {
			return nil, errAccountCLIMissing
		}
	} else if !filepath.IsAbs(executable) {
		return nil, errAccountCLIMissing
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stderr, cmd.Stdout = io.Discard, io.Discard
	var output, failure accountOutput
	if accountCommandCapturesStdout(args) {
		cmd.Stdout = &output
	}
	classifyFailure := len(args) > 0 && (args[0] == "export" || args[0] == "generate-image")
	if classifyFailure {
		cmd.Stderr = &failure
	}
	err := cmd.Run()
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return nil, errAccountCLIMissing
	}
	if classifyFailure && err != nil {
		if ctx.Err() != nil {
			return output.data, ctx.Err()
		}
		if args[0] == "generate-image" {
			return nil, &accountCommandFailure{code: classifyImageCLI(string(failure.data))}
		}
		return output.data, &accountCommandFailure{code: classifyExportCLI(string(failure.data))}
	}
	return output.data, err
}

// Only classify the diagnostic line. CLI usage text also mentions login, and
// treating that help text as an authentication failure would hide old versions.
// Unknown failures may have spent allowance, so callers must never replay them.
func classifyImageCLI(stderr string) string {
	message := strings.ToLower(strings.TrimSpace(stderr))
	message, _, _ = strings.Cut(message, "\n")
	switch {
	case strings.Contains(message, "unknown command"), strings.Contains(message, "unknown generate-image option"),
		strings.Contains(message, "flag provided but not defined"):
		return "cli_update_required"
	case strings.Contains(message, "not signed in"), strings.Contains(message, "glowbom login"),
		strings.Contains(message, "sign-in was not accepted"), strings.Contains(message, "sign-in expired"):
		return "sign_in_required"
	case strings.Contains(message, "image generation was denied"), strings.Contains(message, "insufficient allowance"):
		return "allowance_required"
	case strings.Contains(message, "rate limited"), strings.Contains(message, "too many requests"):
		return "rate_limited"
	case strings.Contains(message, "image request was rejected"), strings.Contains(message, "image request is too large"),
		strings.Contains(message, "reference image"), strings.Contains(message, "image model supports at most"),
		strings.Contains(message, "image prompt"), strings.Contains(message, "image format must"),
		strings.Contains(message, "image source must"), strings.Contains(message, "image quality must"):
		return "invalid_image_request"
	default:
		return "image_generation_failed"
	}
}

func classifyExportCLI(stderr string) string {
	message := strings.ToLower(stderr)
	switch {
	case strings.Contains(message, "not signed in"), strings.Contains(message, "glowbom login"):
		return "sign_in_required"
	case strings.Contains(message, "no saved project"):
		return "project_not_found"
	case strings.Contains(message, "incomplete"), strings.Contains(message, "no html"):
		return "project_incomplete"
	case strings.Contains(message, "too many"):
		return "rate_limited"
	case strings.Contains(message, "denied"):
		return "download_denied"
	case strings.Contains(message, "already exists"), strings.Contains(message, "without replacing"):
		return "output_exists"
	case strings.Contains(message, "not writable"), strings.Contains(message, "could not resolve"), strings.Contains(message, "parent must"):
		return "invalid_output"
	case strings.Contains(message, "starter"):
		return "starter_unavailable"
	case strings.Contains(message, "exceeds"), strings.Contains(message, "too large"), strings.Contains(message, "9 mib"):
		return "project_too_large"
	case strings.Contains(message, "could not be read"), strings.Contains(message, "not a valid"):
		return "invalid_project"
	case strings.Contains(message, "cancel"):
		return "canceled"
	default:
		return "export_failed"
	}
}
