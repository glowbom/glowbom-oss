package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
)

// Buzz discovery is request-scoped. Credentials are never saved or logged.
type buzzLookup struct {
	RelayURL   string `json:"relayUrl"`
	ChannelID  string `json:"channelId"`
	PrivateKey string `json:"privateKey"`
	AuthTag    string `json:"authTag,omitempty"`
}

type buzzMember struct {
	Pubkey       string `json:"pubkey"`
	Role         string `json:"role"`
	DisplayName  string `json:"displayName"`
	PictureURL   string `json:"pictureUrl,omitempty"`
	DefaultLabel string `json:"defaultLabel,omitempty"`
	Description  string `json:"description,omitempty"`
	Harness      string `json:"harness,omitempty"`
	Model        string `json:"model,omitempty"`
}

type buzzReadFunc func(context.Context, buzzLookup, ...string) ([]byte, error)

var buzzHexKey = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var buzzNsecKey = regexp.MustCompile(`^nsec1[023456789acdefghjklmnpqrstuvwxyz]{58}$`)
var buzzChannelID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var errBuzzUnavailable = errors.New("buzz CLI unavailable")

func newBuzzMembersHandler(read buzzReadFunc) http.Handler {
	busy := make(chan struct{}, 2)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, message string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			fail(405, "Use POST for a Buzz lookup.")
			return
		}
		// Fail closed for this credential endpoint without changing other routes.
		token := glowbomServerToken()
		if token == "" || !isLoopbackHost(backendBindHost()) {
			fail(503, "Buzz lookup requires a loopback backend with GLOWBOM_SERVER_TOKEN set.")
			return
		}
		if !hasValidGlowbomServerToken(r, token) || !isAllowedOrigin(r, glowbomAllowedOrigins()) {
			fail(403, "Buzz lookup is not authorized.")
			return
		}
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "application/json" {
			fail(415, "Send application/json.")
			return
		}
		var lookup buzzLookup
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&lookup) != nil || decoder.Decode(new(any)) != io.EOF || !validBuzzLookup(lookup) {
			fail(400, "Enter an HTTPS relay URL, channel UUID, identity private key, and optional JSON owner-auth tag.")
			return
		}
		select {
		case busy <- struct{}{}:
			defer func() { <-busy }()
		default:
			fail(429, "Another Buzz lookup is running. Try again shortly.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		members, err := lookupBuzzMembers(ctx, lookup, read)
		if err != nil {
			switch {
			case errors.Is(err, errBuzzUnavailable):
				fail(503, "Install the Buzz CLI or set GLOWBOM_BUZZ_CLI to its executable path, then restart the backend.")
			case ctx.Err() != nil:
				fail(504, "Buzz lookup timed out or was cancelled.")
			default:
				// CLI errors may include credential values. Never forward them.
				fail(502, "Buzz lookup failed. Check the relay, channel, identity key, and owner-auth tag.")
			}
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			ChannelID string       `json:"channelId"`
			Members   []buzzMember `json:"members"`
		}{lookup.ChannelID, members})
	})
}

func validBuzzLookup(v buzzLookup) bool {
	u, err := url.Parse(v.RelayURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(v.RelayURL) > 2048 {
		return false
	}
	if !buzzChannelID.MatchString(v.ChannelID) || (!buzzHexKey.MatchString(v.PrivateKey) && !buzzNsecKey.MatchString(v.PrivateKey)) {
		return false
	}
	if v.AuthTag != "" {
		var tag []string
		if len(v.AuthTag) > 8192 || json.Unmarshal([]byte(v.AuthTag), &tag) != nil || len(tag) < 2 || tag[0] != "auth" {
			return false
		}
	}
	return true
}

func lookupBuzzMembers(ctx context.Context, lookup buzzLookup, read buzzReadFunc) ([]buzzMember, error) {
	raw, err := read(ctx, lookup, "channels", "members", "--channel", lookup.ChannelID)
	if err != nil {
		return nil, err
	}
	var roster []buzzMember
	if json.Unmarshal(raw, &roster) != nil || roster == nil || len(roster) > 1000 {
		return nil, errors.New("invalid roster")
	}
	members := make([]buzzMember, 0, len(roster))
	seen := make(map[string]bool)
	for _, member := range roster {
		if !buzzHexKey.MatchString(member.Pubkey) {
			return nil, errors.New("invalid member")
		}
		member.Pubkey = strings.ToLower(member.Pubkey)
		if seen[member.Pubkey] {
			continue
		}
		seen[member.Pubkey] = true
		member.DisplayName = member.Pubkey[:12]
		member.PictureURL = ""
		member.DefaultLabel = ""
		member.Description = ""
		member.Harness = ""
		member.Model = ""
		if member.Role != "owner" && member.Role != "admin" && member.Role != "moderator" {
			member.Role = "member"
		}
		members = append(members, member)
	}
	// Buzz accepts at most 200 public keys per profile query.
	for start := 0; start < len(members); start += 200 {
		end := min(start+200, len(members))
		args := []string{"users", "get"}
		for _, member := range members[start:end] {
			args = append(args, "--pubkey", member.Pubkey)
		}
		raw, err = read(ctx, lookup, args...)
		if err != nil {
			return nil, err
		}
		var profiles []struct {
			Pubkey      string          `json:"pubkey"`
			DisplayName string          `json:"display_name"`
			Name        string          `json:"name"`
			Picture     string          `json:"picture"`
			About       json.RawMessage `json:"about"`
			Harness     string          `json:"harness"`
			Model       string          `json:"model"`
		}
		if json.Unmarshal(raw, &profiles) != nil || profiles == nil {
			return nil, errors.New("invalid profiles")
		}
		byKey := make(map[string]string)
		pictures := make(map[string]string)
		labels := make(map[string]string)
		descriptions := make(map[string]string)
		runtimes := make(map[string]buzzRuntimeMetadata)
		for _, profile := range profiles {
			var about string
			if json.Unmarshal(profile.About, &about) == nil {
				labels[strings.ToLower(profile.Pubkey)] = buzzLabel(about)
				descriptions[strings.ToLower(profile.Pubkey)] = buzzPublicText(about, 280)
			}
			runtimes[strings.ToLower(profile.Pubkey)] = buzzRuntimeMetadata{Harness: buzzPublicText(profile.Harness, 80), Model: buzzPublicText(profile.Model, 80)}
			pictures[strings.ToLower(profile.Pubkey)] = buzzPictureURL(profile.Picture, lookup.RelayURL)
			name := strings.TrimSpace(profile.DisplayName)
			if name == "" {
				name = strings.TrimSpace(profile.Name)
			}
			if name != "" {
				byKey[strings.ToLower(profile.Pubkey)] = string([]rune(name)[:min(len([]rune(name)), 120)])
			}
		}
		for i := start; i < end; i++ {
			members[i].PictureURL = pictures[members[i].Pubkey]
			members[i].DefaultLabel = labels[members[i].Pubkey]
			members[i].Description = descriptions[members[i].Pubkey]
			members[i].Harness = runtimes[members[i].Pubkey].Harness
			members[i].Model = runtimes[members[i].Pubkey].Model
			if name := byKey[members[i].Pubkey]; name != "" {
				members[i].DisplayName = name
			}
		}
	}
	return members, nil
}

func buzzExecutable() (string, error) {
	if configured := os.Getenv("GLOWBOM_BUZZ_CLI"); configured != "" {
		return exec.LookPath(configured)
	}
	if path, err := exec.LookPath("buzz"); err == nil {
		return path, nil
	}
	if runtime.GOOS == "darwin" {
		return exec.LookPath("/Applications/Buzz.app/Contents/MacOS/buzz")
	}
	return "", errBuzzUnavailable
}

func buzzEnvironment(base []string, lookup buzzLookup) []string {
	env := make([]string, 0, len(base)+3)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "PATH", "HOME", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "USERPROFILE", "SSL_CERT_FILE", "SSL_CERT_DIR":
			env = append(env, entry)
		}
	}
	env = append(env, "BUZZ_RELAY_URL="+lookup.RelayURL, "BUZZ_PRIVATE_KEY="+lookup.PrivateKey)
	if lookup.AuthTag != "" {
		env = append(env, "BUZZ_AUTH_TAG="+lookup.AuthTag)
	}
	return env
}

type buzzLimitedOutput struct{ bytes.Buffer }

func (b *buzzLimitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 2*1024*1024 {
		return 0, errors.New("Buzz output too large")
	}
	return b.Buffer.Write(p)
}

func runBuzzRead(ctx context.Context, lookup buzzLookup, args ...string) ([]byte, error) {
	path, err := buzzExecutable()
	if err != nil {
		return nil, errBuzzUnavailable
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = buzzEnvironment(os.Environ(), lookup)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	var output buzzLimitedOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, errors.New("Buzz command failed")
	}
	if len(args) >= 2 && args[0] == "users" && args[1] == "get" {
		return enrichBuzzRuntimeProfiles(ctx, lookup, output.Bytes(), queryBuzzMetadata), nil
	}
	return output.Bytes(), nil
}

// Plain, bounded public profile text for a compact nameplate subtitle.
func buzzLabel(text string) string {
	return buzzPublicText(text, 80)
}

func buzzPublicText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, text)
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return string(runes)
}
