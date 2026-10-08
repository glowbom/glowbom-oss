package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func glowbomImageSourceResponse(t *testing.T, authenticated bool) projectIconSource {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/opencode/icon-sources", nil)
	if authenticated {
		r.Header.Set("Authorization", "Bearer fixture-token")
	}
	w := httptest.NewRecorder()
	openCodeIconSourcesHandler(w, r)
	var response struct {
		Sources []projectIconSource `json:"sources"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal("invalid source discovery response", w.Code)
	}
	if strings.Contains(w.Body.String(), "must-not-leak") || strings.Contains(w.Body.String(), "fixture-owner") {
		t.Fatal("source discovery exposed account details")
	}
	for _, source := range response.Sources {
		if source.ID == "glowbom-api" {
			if source.Label != "Glowbom account" || source.AuthType != "account" || source.Model != "Flux" {
				t.Fatal("incorrect account image source contract", source)
			}
			return source
		}
	}
	t.Fatal("missing Glowbom account image source")
	return projectIconSource{}
}

func TestGlowbomImageSourceAvailabilityRecoversAfterAccountCheck(t *testing.T) {
	isolateProjectIconCredentials(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	bridge := mockGlowbomImageAccount(t, func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "account --json" {
			t.Error("source discovery attempted a non-status command")
		}
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return glowbomSignedIn(), nil
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	accountDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { accountDone <- accountTestRequest(bridge, http.MethodGet, "/account/status") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("account status check did not start")
	}
	source := glowbomImageSourceResponse(t, true)
	if source.Available || source.AvailabilityCode != "account_busy" || calls.Load() != 1 {
		t.Fatal("concurrent account check was mistaken for sign-out", source, calls.Load())
	}
	close(release)
	select {
	case response := <-accountDone:
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"signed_in"`) {
			t.Fatal("account check did not complete")
		}
	case <-time.After(time.Second):
		t.Fatal("account status check stayed blocked")
	}
	source = glowbomImageSourceResponse(t, true)
	if !source.Available || source.AvailabilityCode != "" || calls.Load() != 2 {
		t.Fatal("source did not recover after account check", source, calls.Load())
	}
}

func TestGlowbomImageSourceAvailabilityExplainsProbeFailures(t *testing.T) {
	for _, test := range []struct {
		name, output, code string
		err                error
	}{
		{"signed out", `{"version":1,"status":"signed_out","code":"sign_in_required"}`, "sign_in_required", nil},
		{"signed out with process failure", `{"version":1,"status":"signed_out","code":"sign_in_required"}`, "account_unavailable", errors.New("must-not-leak")},
		{"signed out with expired probe", `{"version":1,"status":"signed_out","code":"sign_in_required"}`, "account_unavailable", context.DeadlineExceeded},
		{"account unavailable", `{"version":1,"status":"unavailable","code":"account_unavailable"}`, "account_unavailable", errors.New("must-not-leak")},
		{"missing CLI", "", "cli_unavailable", errAccountCLIMissing},
		{"old CLI", "Account: must-not-leak", "cli_update_required", nil},
		{"probe failed", "must-not-leak", "account_unavailable", errors.New("must-not-leak")},
		{"expired probe", "", "account_unavailable", context.DeadlineExceeded},
		{"signed in with process failure", string(glowbomSignedIn()), "account_unavailable", errors.New("must-not-leak")},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			mockGlowbomImageAccount(t, func(ctx context.Context, args ...string) ([]byte, error) {
				deadline, ok := ctx.Deadline()
				if strings.Join(args, " ") != "account --json" || !ok || time.Until(deadline) > 25*time.Second || time.Until(deadline) < 24*time.Second {
					t.Fatal("source discovery did not use the Account status timeout")
				}
				return []byte(test.output), test.err
			})
			source := glowbomImageSourceResponse(t, true)
			if source.Available || source.AvailabilityCode != test.code {
				t.Fatal("incorrect account availability reason", source)
			}
		})
	}
}

func TestGlowbomImageSourceAvailabilityRejectsCanceledSignedOutResult(t *testing.T) {
	isolateProjectIconCredentials(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockGlowbomImageAccount(t, func(context.Context, ...string) ([]byte, error) {
		cancel()
		return []byte(`{"version":1,"status":"signed_out","code":"sign_in_required"}`), nil
	})
	available, code := glowbomImagesAvailability(ctx)
	if available || code != "account_unavailable" {
		t.Fatal("canceled account probe was mistaken for sign-out", available, code)
	}
}

func TestGlowbomImageSourceAvailabilityRequiresLocalAuthentication(t *testing.T) {
	for _, token := range []string{"", "fixture-token"} {
		t.Run(token, func(t *testing.T) {
			isolateProjectIconCredentials(t)
			bridge := mockGlowbomImageAccount(t, func(context.Context, ...string) ([]byte, error) {
				t.Fatal("unauthorized source discovery ran account CLI")
				return nil, nil
			})
			bridge.token = token
			source := glowbomImageSourceResponse(t, false)
			if source.Available || source.AvailabilityCode != "backend_auth_required" {
				t.Fatal("missing authentication was mistaken for account sign-out", source)
			}
		})
	}
}
