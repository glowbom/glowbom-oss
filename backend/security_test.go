package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateBackendSecurity(t *testing.T) {
	cases := []struct {
		name, host, legacyHost, token, legacyToken string
		allowed                                    bool
	}{
		{name: "default loopback", allowed: true},
		{name: "IPv4 loopback", host: "127.0.0.1", allowed: true},
		{name: "IPv4 loopback range", host: "127.0.0.2", allowed: true},
		{name: "IPv6 loopback", host: "::1", allowed: true},
		{name: "mapped IPv4 loopback", host: "::ffff:127.0.0.1", allowed: true},
		{name: "localhost", host: "localhost", allowed: true},
		{name: "trimmed localhost", host: " LOCALHOST ", allowed: true},
		{name: "IPv4 wildcard", host: "0.0.0.0"},
		{name: "IPv6 wildcard", host: "::"},
		{name: "wildcard hostname", host: "*"},
		{name: "private IPv4", host: "192.168.1.20"},
		{name: "public IPv4", host: "203.0.113.20"},
		{name: "private IPv6", host: "fd00::1"},
		{name: "public IPv6", host: "2001:db8::1"},
		{name: "mapped non-loopback", host: "::ffff:192.168.1.20"},
		{name: "hostname", host: "workstation.example"},
		{name: "localhost suffix", host: "localhost.example"},
		{name: "invalid host stays out of error", host: "private-value@example"},
		{name: "whitespace token", host: "0.0.0.0", token: "  "},
		{name: "authenticated IPv4", host: "0.0.0.0", token: "test-runtime-token", allowed: true},
		{name: "authenticated IPv6", host: "::", token: "test-runtime-token", allowed: true},
		{name: "authenticated hostname", host: "workstation.example", token: "test-runtime-token", allowed: true},
		{name: "legacy loopback", legacyHost: "::1", allowed: true},
		{name: "legacy wildcard", legacyHost: "0.0.0.0"},
		{name: "legacy token", legacyHost: "0.0.0.0", legacyToken: "test-legacy-token", allowed: true},
		{name: "current host overrides legacy", host: "127.0.0.1", legacyHost: "0.0.0.0", allowed: true},
		{name: "current wildcard overrides legacy", host: "0.0.0.0", legacyHost: "127.0.0.1"},
		{name: "empty current host uses legacy", host: " ", legacyHost: "0.0.0.0"},
		{name: "current host accepts legacy token", host: "::", legacyToken: "test-legacy-token", allowed: true},
		{name: "legacy host accepts current token", legacyHost: "::", token: "test-runtime-token", allowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GLOWBOM_BIND_HOST", tc.host)
			t.Setenv("GLOWBY_BIND_HOST", tc.legacyHost)
			t.Setenv("GLOWBOM_SERVER_TOKEN", tc.token)
			t.Setenv("GLOWBY_SERVER_TOKEN", tc.legacyToken)
			err := validateBackendSecurity()
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed = %v, want %v", err == nil, tc.allowed)
			}
			const safeError = "GLOWBOM_SERVER_TOKEN is required for a non-loopback GLOWBOM_BIND_HOST; use glowbom start for local development"
			if err != nil && err.Error() != safeError {
				t.Fatal("startup error must contain only static configuration guidance")
			}
		})
	}
}

func TestAuthenticatedNetworkBindingPreservesRequestAuthentication(t *testing.T) {
	t.Setenv("GLOWBOM_BIND_HOST", "0.0.0.0")
	t.Setenv("GLOWBY_BIND_HOST", "")
	t.Setenv("GLOWBOM_SERVER_TOKEN", "test-runtime-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	t.Setenv("GLOWBOM_ALLOWED_ORIGINS", "http://127.0.0.1:4572")
	t.Setenv("GLOWBY_ALLOWED_ORIGINS", "")
	if err := validateBackendSecurity(); err != nil {
		t.Fatal("authenticated network configuration must pass startup validation")
	}
	handler := withGlowbomSecurity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name, token, origin string
		status              int
	}{
		{name: "missing token", status: http.StatusUnauthorized},
		{name: "incorrect token", token: "incorrect-token", status: http.StatusUnauthorized},
		{name: "valid token", token: "test-runtime-token", status: http.StatusNoContent},
		{name: "allowed origin", token: "test-runtime-token", origin: "http://127.0.0.1:4572", status: http.StatusNoContent},
		{name: "rejected origin", token: "test-runtime-token", origin: "https://untrusted.example", status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/opencode/project", nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
		})
	}
}
