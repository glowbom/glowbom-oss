package main

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
)

type glowbomHealthResponse struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

func backendBindHost() string {
	for _, key := range []string{"GLOWBOM_BIND_HOST", "GLOWBY_BIND_HOST"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "127.0.0.1"
}

func backendListenAddr(port string) string {
	host := strings.TrimSpace(backendBindHost())
	if host == "" {
		return ":" + port
	}
	return net.JoinHostPort(host, port)
}

func glowbomServerToken() string {
	for _, key := range []string{"GLOWBOM_SERVER_TOKEN", "GLOWBY_SERVER_TOKEN"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func glowbomAllowedOrigins() map[string]struct{} {
	raw := ""
	for _, key := range []string{"GLOWBOM_ALLOWED_ORIGINS", "GLOWBY_ALLOWED_ORIGINS"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			raw = value
			break
		}
	}
	if raw == "" {
		return nil
	}

	allowed := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(item)
		if origin == "" {
			continue
		}
		allowed[origin] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil
	}
	return allowed
}

func glowbomHealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(glowbomHealthResponse{
		Name: "Glowbom OSS",
		OK:   true,
	})
}

func withGlowbomSecurity(next http.Handler) http.Handler {
	token := glowbomServerToken()
	allowedOrigins := glowbomAllowedOrigins()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicBackendRoute(r) {
			next.ServeHTTP(w, r)
			return
		}

		if !isAllowedOrigin(r, allowedOrigins) {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}

		if token != "" && !hasValidGlowbomServerToken(r, token) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isPublicBackendRoute(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz":
		return true
	case "/opencode/auth/openai/oauth/callback":
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	default:
		return false
	}
}

func isAllowedOrigin(r *http.Request, allowedOrigins map[string]struct{}) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}

	if len(allowedOrigins) > 0 {
		_, ok := allowedOrigins[origin]
		return ok
	}

	parsed, err := neturl.Parse(origin)
	if err != nil {
		return false
	}

	originHost := normalizeHost(parsed.Hostname())
	if originHost == "" {
		return false
	}

	if isLoopbackHost(originHost) {
		return true
	}

	requestHost := normalizeHost(r.Host)
	if originHost == requestHost {
		return true
	}

	bindHost := normalizeHost(backendBindHost())
	return bindHost != "" && originHost == bindHost
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}

	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}

	return strings.ToLower(strings.Trim(host, "[]"))
}

func isLoopbackHost(host string) bool {
	switch normalizeHost(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func hasValidGlowbomServerToken(r *http.Request, expected string) bool {
	provided := strings.TrimSpace(r.Header.Get("X-Glowbom-Token"))
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("X-Glowby-Token"))
	}
	if provided == "" {
		provided = bearerToken(r.Header.Get("Authorization"))
	}
	if provided == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func bearerToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	const prefix = "Bearer "
	if strings.HasPrefix(strings.ToLower(raw), strings.ToLower(prefix)) {
		return strings.TrimSpace(raw[len(prefix):])
	}
	return ""
}
