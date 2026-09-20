package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

// OpenCodeDriver manages interactions with the OpenCode agent
type OpenCodeDriver struct {
	client    *opencode.Client
	serverURL string

	// Guards endpoint cache and question dedupe map.
	mu sync.Mutex
	// Endpoint path for question replies. Default is stable in OpenCode server.
	questionReplyPath string
	// Endpoint path for permission replies.
	permissionReplyPath string
	// Prevent accidental duplicate replies for the same pending question request.
	repliedQuestions map[string]time.Time
}

type usageLimitHint struct {
	ResetAt           time.Time
	HasResetAt        bool
	ResetInSeconds    int64
	HasResetInSeconds bool
	WindowMinutes     int64
	HasWindowMinutes  bool
	UsedPercent       int64
	HasUsedPercent    bool
	UpdatedAt         time.Time
}

var usageLimitHintState struct {
	mu   sync.RWMutex
	hint usageLimitHint
}

// Serialize server start/restart/auth-sync so mode transitions do not race.
var openCodeServerPrepMu sync.Mutex

type openCodeOpenAIAuthState struct {
	known             bool
	mode              string
	apiKeyFingerprint string
}

var openCodeOpenAIAuthStateCache struct {
	mu    sync.RWMutex
	state openCodeOpenAIAuthState
}

var openCodeOpenAIOAuthSessions struct {
	mu       sync.Mutex
	sessions map[string]*openCodeOpenAIOAuthSession
}

var openCodeOpenAIOAuthLoopback struct {
	mu      sync.Mutex
	server  *http.Server
	started bool
}

const (
	openAIOAuthIssuer             = "https://auth.openai.com"
	openAIOAuthDefaultClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAIOAuthDefaultOriginator  = "Codex Desktop"
	openAIOAuthDefaultRedirectURI = "http://localhost:1455/auth/callback"
	openAIOAuthLoopbackListenAddr = "127.0.0.1:1455"
)

type openCodeRuntimePaths struct {
	DataHome   string
	StateHome  string
	AuthFile   string
	ConfigHome string
}

type openCodeAuthStatusResponse struct {
	ServerRunning         bool   `json:"serverRunning"`
	ConfiguredDataHome    string `json:"configuredDataHome"`
	ConfiguredStateHome   string `json:"configuredStateHome"`
	AuthFilePath          string `json:"authFilePath"`
	OpenAICredentialType  string `json:"openaiCredentialType"`  // none | oauth | api | unknown
	CachedGlowbomAuthMode string `json:"cachedGlowbomAuthMode"` // api-key | codex-jwt | opencode-config | unknown
}

var openCodeNoisyServerPaths = []string{
	"/session/status",
	"/mcp",
	"/lsp",
	"/vcs",
	"/permission",
	"/question",
	"/path",
	"/command",
}

func shouldSuppressOpenCodeServerLogLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}

	lower := strings.ToLower(trimmed)
	if !strings.Contains(lower, "service=server") {
		return false
	}
	if !strings.Contains(lower, "method=get") && !strings.Contains(lower, "method=head") {
		return false
	}

	for _, p := range openCodeNoisyServerPaths {
		if strings.Contains(lower, "path="+p) {
			return true
		}
	}
	return false
}

func shouldSuppressOpenCodeServerStderrInfoLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}

	lower := strings.ToLower(trimmed)
	// OpenCode emits very high-volume INFO/DEBUG logs on stderr (bus/session/tool chatter).
	// Keep WARN/ERROR visibility while dropping routine diagnostics.
	return strings.HasPrefix(lower, "info ") || strings.HasPrefix(lower, "debug ")
}

// GlowbomProject represents a Glowbom project workspace
type GlowbomProject struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description,omitempty"`
	BundleID    string            `json:"bundleID,omitempty"`
	DisplayName string            `json:"displayName,omitempty"`
	BuildNumber string            `json:"buildNumber,omitempty"`
	Targets     map[string]Target `json:"targets"` // "ios", "android", "web", "godot"
	CreatedAt   string            `json:"createdAt"`
	UpdatedAt   string            `json:"updatedAt"`
}

// Target represents a build target configuration
type Target struct {
	Enabled   bool   `json:"enabled"`
	OutputDir string `json:"outputDir"` // relative path, e.g., "ios", "android"
	LastBuild string `json:"lastBuild,omitempty"`
	Stack     string `json:"stack,omitempty"`
}

// ProjectPaths holds standard paths within a project
type ProjectPaths struct {
	Root      string // Project root directory
	Prototype string // prototype/ - HTML/Tailwind source
	Assets    string // prototype/assets/ - images, fonts
	iOS       string // ios/ - SwiftUI output
	Android   string // android/ - Kotlin output
	Web       string // web/ - Next.js output
	Godot     string // godot/ - Godot output
	Manifest  string // glowbom.json
}

// OpenCodeTranslateRequest represents a translation task
type OpenCodeTranslateRequest struct {
	// Option 1: Direct HTML (legacy/simple mode)
	SourceHTML string `json:"sourceHtml,omitempty"`

	// Option 2: Project-based (preferred)
	ProjectPath string `json:"projectPath,omitempty"` // Path to Glowbom project root

	TargetLang         string  `json:"targetLang"`           // stack id: "swiftui", "kotlin", "nextjs", "godot", etc.
	TargetID           string  `json:"targetID,omitempty"`   // destination id: "ios", "ipados", "macos", etc.
	TargetDir          string  `json:"targetDir,omitempty"`  // relative output dir for project mode
	ProjectDir         string  `json:"projectDir,omitempty"` // Override output dir (legacy)
	Model              string  `json:"model,omitempty"`
	AnthropicKey       string  `json:"anthropicKey,omitempty"`
	OpenAIKey          string  `json:"openaiKey,omitempty"`
	GeminiKey          string  `json:"geminiKey,omitempty"`
	FireworksKey       string  `json:"fireworksKey,omitempty"`
	OpenRouterKey      string  `json:"openrouterKey,omitempty"`
	OpenCodeZenKey     string  `json:"opencodeZenKey,omitempty"`
	XaiKey             string  `json:"xaiKey,omitempty"`
	OpenAIAuthMode     string  `json:"openaiAuthMode,omitempty"`     // "api-key" or "codex-jwt"
	OpenAIAccountID    string  `json:"openaiAccountID,omitempty"`    // chatgpt_account_id for JWT mode
	OpenAIRefreshToken string  `json:"openaiRefreshToken,omitempty"` // refresh token for OpenCode auth sync
	OpenAIExpiresAt    float64 `json:"openaiExpiresAt,omitempty"`    // token expiry (seconds since reference date)
}

// OpenCodeTranslateResponse represents the translation result
type OpenCodeTranslateResponse struct {
	SessionID   string   `json:"sessionId"`
	Success     bool     `json:"success"`
	Files       []string `json:"files"`
	BuildOutput string   `json:"buildOutput,omitempty"`
	Error       string   `json:"error,omitempty"`
	TokensUsed  float64  `json:"tokensUsed"`
	Cost        float64  `json:"cost"`
}

type OpenAIModelsRequest struct {
	ProjectPath        string  `json:"projectPath,omitempty"`
	OpenAIKey          string  `json:"openaiKey,omitempty"`
	OpenAIAuthMode     string  `json:"openaiAuthMode,omitempty"`     // "api-key" or "codex-jwt"
	OpenAIRefreshToken string  `json:"openaiRefreshToken,omitempty"` // refresh token for OpenCode auth sync
	OpenAIExpiresAt    float64 `json:"openaiExpiresAt,omitempty"`    // token expiry (seconds since reference date)
	AnthropicKey       string  `json:"anthropicKey,omitempty"`
	GeminiKey          string  `json:"geminiKey,omitempty"`
}

type OpenAIModelOption struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type OpenAIModelsDebug struct {
	AuthMode              string   `json:"authMode"`
	ProviderFound         bool     `json:"providerFound"`
	ProviderModelCount    int      `json:"providerModelCount"`
	ProviderModelSample   []string `json:"providerModelSample,omitempty"`
	AllowlistModelIDs     []string `json:"allowlistModelIDs"`
	MatchedModelIDs       []string `json:"matchedModelIDs"`
	AddedForwardCompatIDs []string `json:"addedForwardCompatIDs,omitempty"`
	UsedFallbackAllowlist bool     `json:"usedFallbackAllowlist"`
}

type OpenAIModelsResponse struct {
	Provider  string              `json:"provider"`
	Source    string              `json:"source"`
	Models    []OpenAIModelOption `json:"models"`
	FetchedAt string              `json:"fetchedAt"`
	Debug     OpenAIModelsDebug   `json:"debug"`
}

type OpenCodeAvailableModelsRequest struct {
	ProjectPath        string  `json:"projectPath,omitempty"`
	OpenAIKey          string  `json:"openaiKey,omitempty"`
	OpenAIAuthMode     string  `json:"openaiAuthMode,omitempty"`     // "api-key" | "codex-jwt" | "opencode-config"
	OpenAIRefreshToken string  `json:"openaiRefreshToken,omitempty"` // refresh token for OpenCode auth sync
	OpenAIExpiresAt    float64 `json:"openaiExpiresAt,omitempty"`    // token expiry (seconds since reference date)
	AnthropicKey       string  `json:"anthropicKey,omitempty"`
	GeminiKey          string  `json:"geminiKey,omitempty"`
	FireworksKey       string  `json:"fireworksKey,omitempty"`
	OpenRouterKey      string  `json:"openrouterKey,omitempty"`
	OpenCodeZenKey     string  `json:"opencodeZenKey,omitempty"`
	XaiKey             string  `json:"xaiKey,omitempty"`
}

type OpenCodeProviderModelOption struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type OpenCodeProviderOption struct {
	ID          string                        `json:"id"`
	DisplayName string                        `json:"displayName"`
	Models      []OpenCodeProviderModelOption `json:"models"`
}

type OpenCodeAvailableModelsResponse struct {
	Source    string                   `json:"source"`
	Providers []OpenCodeProviderOption `json:"providers"`
	FetchedAt string                   `json:"fetchedAt"`
}

type OpenCodeAuthConnectRequest struct {
	ProjectPath        string  `json:"projectPath,omitempty"`
	OpenAIKey          string  `json:"openaiKey,omitempty"`
	OpenAIRefreshToken string  `json:"openaiRefreshToken,omitempty"`
	OpenAIExpiresAt    float64 `json:"openaiExpiresAt,omitempty"`
}

type OpenCodeAuthDisconnectRequest struct {
	ProjectPath string `json:"projectPath,omitempty"`
}

type OpenCodeAuthConnectionResponse struct {
	Success   bool                       `json:"success"`
	Provider  string                     `json:"provider"`
	Connected bool                       `json:"connected"`
	Status    openCodeAuthStatusResponse `json:"status"`
	Error     string                     `json:"error,omitempty"`
}

type OpenCodeAuthOAuthStartRequest struct {
	ProjectPath string `json:"projectPath,omitempty"`
}

type OpenCodeAuthOAuthStartResponse struct {
	Success          bool   `json:"success"`
	Provider         string `json:"provider"`
	State            string `json:"state"`
	AuthorizationURL string `json:"authorizationURL"`
	RedirectURI      string `json:"redirectURI"`
	Error            string `json:"error,omitempty"`
}

type OpenCodeAuthOAuthStatusResponse struct {
	Success   bool                       `json:"success"`
	Provider  string                     `json:"provider"`
	State     string                     `json:"state"`
	Phase     string                     `json:"phase"` // pending | succeeded | failed
	Connected bool                       `json:"connected"`
	Status    openCodeAuthStatusResponse `json:"status"`
	Error     string                     `json:"error,omitempty"`
}

type openCodeOpenAIOAuthSession struct {
	State        string
	CodeVerifier string
	RedirectURI  string
	ProjectPath  string
	CreatedAt    time.Time
	CompletedAt  time.Time
	Phase        string
	Connected    bool
	Error        string
	Status       openCodeAuthStatusResponse
}

var openAIChatGPTModelAllowlist = map[string]string{
	"gpt-5.5":            "GPT-5.5",
	"gpt-5.4":            "GPT-5.4",
	"gpt-5.3-codex":      "GPT-5.3 Codex",
	"gpt-5.2":            "GPT-5.2",
	"gpt-5.2-codex":      "GPT-5.2 Codex",
	"gpt-5.1-codex-mini": "GPT-5.1 Codex mini",
	"gpt-5.1-codex-max":  "GPT-5.1 Codex Max",
}

var openAICodexForwardCompatModelIDs = []string{
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
}

func isOpenAICodexForwardCompatModelID(modelID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	if normalized == "" {
		return false
	}
	return normalized == "gpt-5.5" || normalized == "gpt-5.4" || normalized == "gpt-5.3-codex"
}

func appendOpenAICodexForwardCompatModels(models []OpenAIModelOption, providerModelIDs []string, authMode string) ([]OpenAIModelOption, []string) {
	if strings.TrimSpace(authMode) != "codex-jwt" {
		return models, nil
	}

	providerModelSet := make(map[string]struct{}, len(providerModelIDs))
	for _, id := range providerModelIDs {
		providerModelSet[id] = struct{}{}
	}

	hasTemplate := false
	if _, ok := providerModelSet["gpt-5.2-codex"]; ok {
		hasTemplate = true
	}
	if _, ok := providerModelSet["gpt-5.3-codex"]; ok {
		hasTemplate = true
	}
	if !hasTemplate {
		return models, nil
	}

	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		seen[model.ID] = struct{}{}
	}

	added := make([]string, 0, len(openAICodexForwardCompatModelIDs))
	for _, id := range openAICodexForwardCompatModelIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		name := strings.TrimSpace(openAIChatGPTModelAllowlist[id])
		if name == "" {
			name = id
		}
		models = append(models, OpenAIModelOption{
			ID:          id,
			DisplayName: name,
		})
		seen[id] = struct{}{}
		added = append(added, id)
	}

	return models, added
}

func previewStringList(values []string, max int) []string {
	if max <= 0 || len(values) <= max {
		return values
	}
	return values[:max]
}

// OpenCodeQuestionRespondRequest represents a response to an agent question
type OpenCodeQuestionRespondRequest struct {
	SessionID          string             `json:"sessionID"`
	QuestionID         string             `json:"questionID"`
	Answer             string             `json:"answer"`
	ProjectPath        string             `json:"projectPath,omitempty"`
	Answers            QuestionAnswers    `json:"answers,omitempty"`
	AnswerByQuestionID AnswerByQuestionID `json:"answerByQuestionID,omitempty"`
}

type QuestionAnswers [][]string

func (qa *QuestionAnswers) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var nested [][]string
	if err := json.Unmarshal(b, &nested); err == nil {
		*qa = nested
		return nil
	}
	var flat []string
	if err := json.Unmarshal(b, &flat); err == nil {
		out := make([][]string, 0, len(flat))
		for _, v := range flat {
			out = append(out, []string{v})
		}
		*qa = out
		return nil
	}
	return fmt.Errorf("invalid answers payload")
}

type AnswerByQuestionID map[string][]string

func (abq *AnswerByQuestionID) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var nested map[string][]string
	if err := json.Unmarshal(b, &nested); err == nil {
		*abq = nested
		return nil
	}
	var flat map[string]string
	if err := json.Unmarshal(b, &flat); err == nil {
		out := make(map[string][]string, len(flat))
		for k, v := range flat {
			out[k] = []string{v}
		}
		*abq = out
		return nil
	}
	return fmt.Errorf("invalid answerByQuestionID payload")
}

// OpenCodePermissionRespondRequest represents a response to a permission prompt
type OpenCodePermissionRespondRequest struct {
	SessionID    string `json:"sessionID"`
	PermissionID string `json:"permissionID"`
	Response     string `json:"response"` // "once" | "always" | "reject"
	ProjectPath  string `json:"projectPath,omitempty"`
}

// getAgentPort returns the OpenCode agent port from GLOWBOM_AGENT_PORT env var,
// falling back to "4571".
func getAgentPort() string {
	if port := os.Getenv("GLOWBOM_AGENT_PORT"); port != "" {
		return port
	}
	return "4571"
}

func openCodeServerHostname() string {
	if host := strings.TrimSpace(os.Getenv("OPENCODE_SERVER_HOSTNAME")); host != "" {
		return host
	}
	return "127.0.0.1"
}

func openCodeServerUsername() string {
	if username := strings.TrimSpace(os.Getenv("OPENCODE_SERVER_USERNAME")); username != "" {
		return username
	}
	return "opencode"
}

func openCodeServerPassword() string {
	return strings.TrimSpace(os.Getenv("OPENCODE_SERVER_PASSWORD"))
}

func openCodeServerAuthorizationHeader() string {
	password := openCodeServerPassword()
	if password == "" {
		return ""
	}
	token := base64.StdEncoding.EncodeToString([]byte(openCodeServerUsername() + ":" + password))
	return "Basic " + token
}

func applyOpenCodeServerAuthorization(req *http.Request) {
	if header := openCodeServerAuthorizationHeader(); header != "" {
		req.Header.Set("Authorization", header)
	}
}

func glowbomOpenCodeRuntimePaths() (openCodeRuntimePaths, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(homeDir) == "" {
		return openCodeRuntimePaths{}, fmt.Errorf("failed to resolve user home directory for OpenCode runtime paths")
	}

	dataHome := filepath.Join(homeDir, ".glowbom", "opencode-data")
	stateHome := filepath.Join(homeDir, ".glowbom", "opencode-state")
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if strings.TrimSpace(configHome) == "" {
		configHome = filepath.Join(homeDir, ".config")
	}

	return openCodeRuntimePaths{
		DataHome:   dataHome,
		StateHome:  stateHome,
		AuthFile:   filepath.Join(dataHome, "opencode", "auth.json"),
		ConfigHome: configHome,
	}, nil
}

func userOpenCodeRuntimePaths() (openCodeRuntimePaths, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(homeDir) == "" {
		return openCodeRuntimePaths{}, fmt.Errorf("failed to resolve user home directory for OpenCode runtime paths")
	}

	dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if dataHome == "" {
		dataHome = filepath.Join(homeDir, ".local", "share")
	}
	stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if stateHome == "" {
		stateHome = filepath.Join(homeDir, ".local", "state")
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = filepath.Join(homeDir, ".config")
	}

	return openCodeRuntimePaths{
		DataHome:   dataHome,
		StateHome:  stateHome,
		AuthFile:   filepath.Join(dataHome, "opencode", "auth.json"),
		ConfigHome: configHome,
	}, nil
}

func authStatusRuntimePaths(mode string, glowbomPaths, userPaths openCodeRuntimePaths) openCodeRuntimePaths {
	if mode == "opencode-config" || mode == "unknown" {
		return userPaths
	}
	return glowbomPaths
}

func ensureOpenCodeRuntimeDirs(paths openCodeRuntimePaths) error {
	if strings.TrimSpace(paths.DataHome) == "" || strings.TrimSpace(paths.StateHome) == "" {
		return fmt.Errorf("invalid OpenCode runtime paths")
	}
	if err := os.MkdirAll(paths.DataHome, 0755); err != nil {
		return fmt.Errorf("failed creating OpenCode data directory %s: %w", paths.DataHome, err)
	}
	if err := os.MkdirAll(paths.StateHome, 0755); err != nil {
		return fmt.Errorf("failed creating OpenCode state directory %s: %w", paths.StateHome, err)
	}
	return nil
}

func setEnvValue(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func normalizeOpenAIAuthMode(mode string) string {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	if normalized == "codex-jwt" {
		return "codex-jwt"
	}
	if normalized == "opencode-config" {
		return "opencode-config"
	}
	return "api-key"
}

func openAIKeyFingerprint(openAIKey string) string {
	trimmed := strings.TrimSpace(openAIKey)
	if trimmed == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(trimmed))
	// Store only a short non-reversible fingerprint for change detection.
	return hex.EncodeToString(digest[:8])
}

func getOpenCodeOpenAIAuthState() openCodeOpenAIAuthState {
	openCodeOpenAIAuthStateCache.mu.RLock()
	defer openCodeOpenAIAuthStateCache.mu.RUnlock()
	return openCodeOpenAIAuthStateCache.state
}

func setOpenCodeOpenAIAuthState(mode, openAIKey string) {
	normalizedMode := normalizeOpenAIAuthMode(mode)
	state := openCodeOpenAIAuthState{
		known:             true,
		mode:              normalizedMode,
		apiKeyFingerprint: "",
	}
	if normalizedMode == "api-key" {
		state.apiKeyFingerprint = openAIKeyFingerprint(openAIKey)
	}

	openCodeOpenAIAuthStateCache.mu.Lock()
	openCodeOpenAIAuthStateCache.state = state
	openCodeOpenAIAuthStateCache.mu.Unlock()
}

func shouldRestartForOpenAIAuthChange(serverWasAlreadyRunning bool, requestedMode, openAIKey string) bool {
	if normalizeOpenAIAuthMode(requestedMode) == "opencode-config" {
		return false
	}
	if !serverWasAlreadyRunning {
		return false
	}

	requestedMode = normalizeOpenAIAuthMode(requestedMode)
	current := getOpenCodeOpenAIAuthState()
	if !current.known {
		// Unknown inherited server state; restart when a concrete auth transition was requested.
		return strings.TrimSpace(openAIKey) != "" || requestedMode != "api-key"
	}
	if current.mode != requestedMode {
		return true
	}
	if requestedMode == "api-key" && strings.TrimSpace(openAIKey) != "" && current.apiKeyFingerprint != openAIKeyFingerprint(openAIKey) {
		return true
	}
	return false
}

func isSessionNotFoundError(err error) bool {
	if err == nil {
		return false
	}

	lower := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(lower, "session not found") || strings.Contains(lower, "404 not found")
}

func (d *OpenCodeDriver) createRefineSession(ctx context.Context, projectPath string) (*opencode.Session, error) {
	session, err := d.client.Session.New(ctx, opencode.SessionNewParams{
		Title:     opencode.F("Refine Glowbom Project"),
		Directory: opencode.F(projectPath),
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (d *OpenCodeDriver) resolveRefineSession(ctx context.Context, projectPath, requestedSessionID string) (*opencode.Session, bool, error) {
	trimmedSessionID := strings.TrimSpace(requestedSessionID)
	if trimmedSessionID != "" {
		session, err := d.client.Session.Get(ctx, trimmedSessionID, opencode.SessionGetParams{})
		if err == nil && session != nil {
			return session, true, nil
		}
		if err != nil && !isSessionNotFoundError(err) {
			return nil, false, err
		}
		if err != nil {
			log.Printf("[OPENCODE] Stored session %s is unavailable, creating a fresh session: %v", trimmedSessionID, err)
		}
	}

	session, err := d.createRefineSession(ctx, projectPath)
	if err != nil {
		return nil, false, err
	}
	return session, false, nil
}

func openAIOAuthClientID() string {
	if value := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_CLIENT_ID")); value != "" {
		return value
	}
	return openAIOAuthDefaultClientID
}

func openAIOAuthOriginator() string {
	if value := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ORIGINATOR")); value != "" {
		return value
	}
	return openAIOAuthDefaultOriginator
}

func openAIOAuthRedirectURI() string {
	if value := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_REDIRECT_URI")); value != "" {
		return value
	}
	return openAIOAuthDefaultRedirectURI
}

func randomURLSafeBase64(byteCount int) (string, error) {
	if byteCount <= 0 {
		byteCount = 32
	}
	buf := make([]byte, byteCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(buf), "="), nil
}

func sha256Base64URL(value string) string {
	digest := sha256.Sum256([]byte(value))
	return strings.TrimRight(base64.URLEncoding.EncodeToString(digest[:]), "=")
}

func ensureOpenAIOAuthSessionStore() {
	openCodeOpenAIOAuthSessions.mu.Lock()
	defer openCodeOpenAIOAuthSessions.mu.Unlock()
	if openCodeOpenAIOAuthSessions.sessions == nil {
		openCodeOpenAIOAuthSessions.sessions = make(map[string]*openCodeOpenAIOAuthSession)
	}
}

func cleanupExpiredOpenAIOAuthSessionsLocked(now time.Time) {
	for state, session := range openCodeOpenAIOAuthSessions.sessions {
		if session == nil {
			delete(openCodeOpenAIOAuthSessions.sessions, state)
			continue
		}
		if now.Sub(session.CreatedAt) > 30*time.Minute {
			delete(openCodeOpenAIOAuthSessions.sessions, state)
			continue
		}
		if session.Phase != "pending" && !session.CompletedAt.IsZero() && now.Sub(session.CompletedAt) > 10*time.Minute {
			delete(openCodeOpenAIOAuthSessions.sessions, state)
		}
	}
}

func ensureOpenAIOAuthLoopbackServer() error {
	openCodeOpenAIOAuthLoopback.mu.Lock()
	defer openCodeOpenAIOAuthLoopback.mu.Unlock()

	if openCodeOpenAIOAuthLoopback.started && openCodeOpenAIOAuthLoopback.server != nil {
		return nil
	}

	loopbackMux := http.NewServeMux()
	loopbackMux.HandleFunc("/auth/callback", openCodeOpenAIOAuthLoopbackCallbackHandler)
	loopbackMux.HandleFunc("/cancel", openCodeOpenAIOAuthLoopbackCancelHandler)

	listener, err := net.Listen("tcp", openAIOAuthLoopbackListenAddr)
	if err != nil {
		return fmt.Errorf("could not start OAuth callback listener on %s: %w", openAIOAuthLoopbackListenAddr, err)
	}

	server := &http.Server{
		Handler:           loopbackMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	openCodeOpenAIOAuthLoopback.server = server
	openCodeOpenAIOAuthLoopback.started = true

	go func(s *http.Server, ln net.Listener) {
		if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[OPENCODE] OAuth loopback listener stopped: %v", err)
			openCodeOpenAIOAuthLoopback.mu.Lock()
			if openCodeOpenAIOAuthLoopback.server == s {
				openCodeOpenAIOAuthLoopback.server = nil
				openCodeOpenAIOAuthLoopback.started = false
			}
			openCodeOpenAIOAuthLoopback.mu.Unlock()
		}
	}(server, listener)

	return nil
}

func openCodeOpenAIOAuthLoopbackCallbackHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	success, message := finalizeOpenAIOAuthCallback(
		strings.TrimSpace(r.URL.Query().Get("state")),
		strings.TrimSpace(r.URL.Query().Get("code")),
		strings.TrimSpace(r.URL.Query().Get("error")),
		strings.TrimSpace(r.URL.Query().Get("error_description")),
	)
	writeOpenAIOAuthCallbackHTML(w, success, message)
}

func openCodeOpenAIOAuthLoopbackCancelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state != "" {
		setOpenAIOAuthSessionFailed(state, "Login was canceled.")
	}
	writeOpenAIOAuthCallbackHTML(w, false, "Login was canceled.")
}

func writeOpenAIOAuthCallbackHTML(w http.ResponseWriter, success bool, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	statusClass := "error"
	title := "ChatGPT login failed"
	if success {
		statusClass = "success"
		title = "ChatGPT connected"
	}

	escapedTitle := html.EscapeString(title)
	escapedMessage := html.EscapeString(message)
	page := fmt.Sprintf(`<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <title>%s</title>
    <style>
      body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #f7faf8; color: #24332b; padding: 28px; }
      .card { max-width: 560px; margin: 24px auto; background: white; border: 1px solid #dbeae2; border-radius: 14px; padding: 20px; box-shadow: 0 8px 28px rgba(17, 35, 27, 0.08); }
      .success { color: #16784a; }
      .error { color: #b23a3a; }
      p { line-height: 1.45; }
      code { background: #f1f6f3; padding: 2px 6px; border-radius: 6px; }
    </style>
  </head>
  <body>
    <div class="card">
      <h2 class="%s">%s</h2>
      <p>%s</p>
      <p>You can close this tab and return to Glowbom OSS.</p>
    </div>
    <script>setTimeout(function(){ try { window.close(); } catch (_) {} }, 900);</script>
  </body>
</html>`, escapedTitle, statusClass, escapedTitle, escapedMessage)
	_, _ = io.WriteString(w, page)
}

func exchangeOpenAIOAuthCodeForCredential(code, codeVerifier, redirectURI string) (openCodeOpenAIOAuthCredential, error) {
	form := neturl.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", strings.TrimSpace(code))
	form.Set("redirect_uri", strings.TrimSpace(redirectURI))
	form.Set("client_id", openAIOAuthClientID())
	form.Set("code_verifier", strings.TrimSpace(codeVerifier))

	req, err := http.NewRequest(http.MethodPost, openAIOAuthIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return openCodeOpenAIOAuthCredential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return openCodeOpenAIOAuthCredential{}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if len(message) > 500 {
			message = message[:500]
		}
		return openCodeOpenAIOAuthCredential{}, fmt.Errorf("oauth token exchange failed (%d): %s", resp.StatusCode, message)
	}

	var decoded struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return openCodeOpenAIOAuthCredential{}, fmt.Errorf("oauth token decode failed: %w", err)
	}

	access := strings.TrimSpace(decoded.AccessToken)
	if access == "" {
		return openCodeOpenAIOAuthCredential{}, fmt.Errorf("oauth token exchange returned empty access token")
	}

	expiresAtReference := 0.0
	if decoded.ExpiresIn > 0 {
		expiresAtReference = float64(time.Now().Add(time.Duration(decoded.ExpiresIn)*time.Second).Unix()) - 978307200
	}

	return openCodeOpenAIOAuthCredential{
		AccessToken:               access,
		RefreshToken:              strings.TrimSpace(decoded.RefreshToken),
		ExpiresAtReferenceSeconds: expiresAtReference,
		Source:                    "oauth_callback",
	}, nil
}

// isServerRunning checks if OpenCode server is accessible
func isServerRunning(serverURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, "GET", serverURL+"/health", nil)
	if err != nil {
		return false
	}
	applyOpenCodeServerAuthorization(req)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// startOpenCodeServer attempts to start the OpenCode server with provided API keys.
// When openAIAuthMode is "codex-jwt", the OpenAI key is a JWT token and should NOT be
// passed as OPENAI_API_KEY; instead, auth will be synced via PUT /auth/openai after startup.
func startOpenCodeServer(openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey, model, openAIAuthMode string) error {
	openAIAuthMode = normalizeOpenAIAuthMode(openAIAuthMode)
	useOpenCodeConfigRuntime := openAIAuthMode == "opencode-config"

	// Check if opencode CLI is available
	if _, err := exec.LookPath("opencode"); err != nil {
		return fmt.Errorf("opencode CLI not found in PATH: %w", err)
	}

	var runtimePaths openCodeRuntimePaths
	if !useOpenCodeConfigRuntime {
		var err error
		runtimePaths, err = glowbomOpenCodeRuntimePaths()
		if err != nil {
			return err
		}
		if err := ensureOpenCodeRuntimeDirs(runtimePaths); err != nil {
			return err
		}
	}

	// Set environment variables for model and API keys
	env := os.Environ()
	if !useOpenCodeConfigRuntime {
		env = setEnvValue(env, "XDG_DATA_HOME", runtimePaths.DataHome)
		env = setEnvValue(env, "XDG_STATE_HOME", runtimePaths.StateHome)
	}

	// Only pass OPENAI_API_KEY for standard API key mode.
	// In codex-jwt mode, auth is synced via the OpenCode REST API after startup.
	if openAIKey != "" && openAIAuthMode != "codex-jwt" {
		env = append(env, "OPENAI_API_KEY="+openAIKey)
	}
	if anthropicKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+anthropicKey)
	}
	if geminiKey != "" {
		env = append(env, "GOOGLE_GENERATIVE_AI_API_KEY="+geminiKey)
		env = append(env, "GOOGLE_API_KEY="+geminiKey)
	}
	if fireworksKey != "" {
		env = append(env, "FIREWORKS_API_KEY="+fireworksKey)
	}
	if openRouterKey != "" {
		env = append(env, "OPENROUTER_API_KEY="+openRouterKey)
	}
	if openCodeZenKey != "" {
		env = append(env, "OPENCODE_API_KEY="+openCodeZenKey)
	}
	if xaiKey != "" {
		env = append(env, "XAI_API_KEY="+xaiKey)
	}

	// Set model
	modelToUse := model
	if modelToUse != "" {
		env = append(env, "OPENCODE_MODEL="+modelToUse)
	} else {
		if useOpenCodeConfigRuntime {
			// Keep OpenCode's configured default model unchanged.
			modelToUse = "(opencode configured default)"
		} else {
			// Default model based on available keys
			if anthropicKey != "" {
				modelToUse = "anthropic/claude-sonnet-4-6"
			} else if openAIKey != "" {
				modelToUse = "openai/gpt-5.5"
			} else if xaiKey != "" {
				modelToUse = "xai/" + grokTextModelID
			} else if openCodeZenKey != "" {
				modelToUse = "opencode/kimi-k2.5-free"
			} else {
				modelToUse = "anthropic/claude-sonnet-4-6"
			}
			env = append(env, "OPENCODE_MODEL="+modelToUse)
		}
	}

	log.Printf("[OPENCODE] Starting server with model: %s (authMode: %s)", modelToUse, openAIAuthMode)
	if useOpenCodeConfigRuntime {
		log.Printf("[OPENCODE] Runtime paths - using existing OpenCode runtime/config from environment")
	} else {
		log.Printf("[OPENCODE] Runtime paths - dataHome=%s stateHome=%s configHome=%s", runtimePaths.DataHome, runtimePaths.StateHome, runtimePaths.ConfigHome)
	}
	log.Printf("[OPENCODE] API keys provided - Anthropic: %t, OpenAI: %t, Gemini: %t, Fireworks: %t, OpenRouter: %t, OpenCodeZen: %t, xAI: %t",
		anthropicKey != "", openAIKey != "", geminiKey != "", fireworksKey != "", openRouterKey != "", openCodeZenKey != "", xaiKey != "")

	// Start server in background with manageable logging by default.
	// Override via GLOWBOM_OPENCODE_LOG_LEVEL (e.g. DEBUG) when deeper diagnostics are needed.
	openCodeLogLevel := strings.ToUpper(strings.TrimSpace(os.Getenv("GLOWBOM_OPENCODE_LOG_LEVEL")))
	if openCodeLogLevel == "" {
		openCodeLogLevel = "WARN"
	}
	cmd := exec.Command(
		"opencode",
		"serve",
		"--port", getAgentPort(),
		"--hostname", openCodeServerHostname(),
		"--print-logs",
		"--log-level", openCodeLogLevel,
	)
	cmd.Env = env

	// Create pipes to capture stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start opencode serve: %w", err)
	}

	setOpenCodeOpenAIAuthState(openAIAuthMode, openAIKey)

	// Log stdout in background
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			recordUsageLimitHintFromText(line)
			if shouldSuppressOpenCodeServerLogLine(line) {
				continue
			}
			log.Printf("[OPENCODE-SERVER] %s", line)
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[OPENCODE-SERVER] stdout scanner error: %v", err)
		}
	}()

	// Log stderr in background
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			recordUsageLimitHintFromText(line)
			if shouldSuppressOpenCodeServerLogLine(line) || shouldSuppressOpenCodeServerStderrInfoLine(line) {
				continue
			}
			log.Printf("[OPENCODE-SERVER-ERR] %s", line)
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[OPENCODE-SERVER-ERR] stderr scanner error: %v", err)
		}
	}()

	// Wait for command completion in background
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("[OPENCODE] Server process exited: %v", err)
		}
	}()

	return nil
}

// syncOpenAIAuth syncs ChatGPT OAuth credentials to the running OpenCode server
// via PUT /auth/openai so OpenCode routes calls to chatgpt.com/backend-api/codex/responses
// instead of api.openai.com/v1/responses.
func syncOpenAIAuth(serverURL, accessToken, refreshToken string, expiresAt float64) error {
	if accessToken == "" {
		return fmt.Errorf("no access token to sync")
	}

	// Convert expiresAt from NSDate reference (2001-01-01) to Unix milliseconds
	// NSDate reference date is 978307200 seconds after Unix epoch
	var expiresMs int64
	if expiresAt > 0 {
		expiresUnix := expiresAt + 978307200
		expiresMs = int64(expiresUnix * 1000)
	} else {
		// Default: 1 hour from now
		expiresMs = time.Now().Add(time.Hour).UnixMilli()
	}

	// If no refresh token, use a placeholder (OpenCode requires it)
	if refreshToken == "" {
		refreshToken = "none"
	}

	payload := map[string]interface{}{
		"type":    "oauth",
		"access":  accessToken,
		"refresh": refreshToken,
		"expires": expiresMs,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal auth payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPut, serverURL+"/auth/openai", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create auth sync request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	applyOpenCodeServerAuthorization(req)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth sync request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("auth sync returned %d: %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[OPENCODE] Successfully synced OpenAI OAuth credentials to OpenCode server")
	return nil
}

func clearOpenAIAuth(serverURL string) error {
	req, err := http.NewRequest(http.MethodDelete, serverURL+"/auth/openai", nil)
	if err != nil {
		return fmt.Errorf("failed creating OpenAI auth clear request: %w", err)
	}
	applyOpenCodeServerAuthorization(req)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("OpenAI auth clear request failed: %w", err)
	}
	defer resp.Body.Close()

	// Treat not found as already cleared.
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("OpenAI auth clear returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

func applyOpenAIAuthModeToRunningServer(serverURL, openAIAuthMode, openAIKey, openAIRefreshToken string, openAIExpiresAt float64) error {
	openAIAuthMode = normalizeOpenAIAuthMode(openAIAuthMode)

	if openAIAuthMode == "opencode-config" {
		setOpenCodeOpenAIAuthState(openAIAuthMode, "")
		return nil
	}

	if openAIAuthMode == "codex-jwt" {
		if strings.TrimSpace(openAIKey) != "" {
			if err := syncOpenAIAuth(serverURL, openAIKey, openAIRefreshToken, openAIExpiresAt); err != nil {
				return fmt.Errorf("failed to sync OpenAI oauth auth: %w", err)
			}
		}
		setOpenCodeOpenAIAuthState(openAIAuthMode, openAIKey)
		return nil
	}

	if openAIAuthMode == "api-key" {
		if err := clearOpenAIAuth(serverURL); err != nil {
			return fmt.Errorf("failed clearing OpenAI oauth auth: %w", err)
		}
		setOpenCodeOpenAIAuthState(openAIAuthMode, openAIKey)
	}

	return nil
}

func waitForOpenCodeServerReady(serverURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if isServerRunning(serverURL) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("OpenCode server did not become healthy within %s", timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func stopOpenCodeServerOnPort(port string) error {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-t").CombinedOutput()
	if err != nil {
		// lsof exits with status 1 when no process is listening on the port.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("failed to discover process on port %s: %w", port, err)
	}

	pidStrings := strings.Fields(string(out))
	if len(pidStrings) == 0 {
		return nil
	}

	var stopErrs []string
	for _, pidStr := range pidStrings {
		pid, convErr := strconv.Atoi(strings.TrimSpace(pidStr))
		if convErr != nil {
			stopErrs = append(stopErrs, fmt.Sprintf("invalid pid %q", pidStr))
			continue
		}

		proc, findErr := os.FindProcess(pid)
		if findErr != nil {
			stopErrs = append(stopErrs, fmt.Sprintf("pid %d lookup failed: %v", pid, findErr))
			continue
		}

		if killErr := proc.Kill(); killErr != nil {
			stopErrs = append(stopErrs, fmt.Sprintf("pid %d kill failed: %v", pid, killErr))
			continue
		}

		log.Printf("[OPENCODE] Stopped existing server process on port %s (pid=%d)", port, pid)
	}

	if len(stopErrs) > 0 {
		return fmt.Errorf("%s", strings.Join(stopErrs, "; "))
	}
	return nil
}

func restartOpenCodeServer(openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey, model, openAIAuthMode string) error {
	port := getAgentPort()
	serverURL := "http://" + openCodeServerHostname() + ":" + port

	if err := stopOpenCodeServerOnPort(port); err != nil {
		return fmt.Errorf("failed to stop existing OpenCode server: %w", err)
	}

	time.Sleep(250 * time.Millisecond)

	if err := startOpenCodeServer(openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey, model, openAIAuthMode); err != nil {
		return fmt.Errorf("failed to restart OpenCode server: %w", err)
	}

	if err := waitForOpenCodeServerReady(serverURL, 12*time.Second); err != nil {
		return err
	}

	return nil
}

func shouldAttemptOpenCodeRestartForProvider(providerID, openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey string) bool {
	switch providerID {
	case "openai":
		return strings.TrimSpace(openAIKey) != ""
	case "anthropic":
		return strings.TrimSpace(anthropicKey) != ""
	case "google":
		return strings.TrimSpace(geminiKey) != ""
	case "fireworks", "fireworks-ai":
		return strings.TrimSpace(fireworksKey) != ""
	case "openrouter":
		return strings.TrimSpace(openRouterKey) != ""
	case "opencode":
		return strings.TrimSpace(openCodeZenKey) != ""
	case "xai":
		return strings.TrimSpace(xaiKey) != ""
	default:
		return false
	}
}

func normalizeOpenCodeProviderID(providerID string) string {
	switch strings.ToLower(strings.TrimSpace(providerID)) {
	case "fireworks":
		return "fireworks-ai"
	default:
		return providerID
	}
}

func isStrictModelPreflightProvider(providerID string) bool {
	switch strings.ToLower(strings.TrimSpace(providerID)) {
	case "openai":
		return true
	default:
		return false
	}
}

func ensureOpenCodeModelAvailable(projectPath, providerID, modelID string) error {
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(modelID) == "" {
		return nil
	}
	providerID = normalizeOpenCodeProviderID(providerID)

	driver := GetOpenCodeDriver()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	params := opencode.AppProvidersParams{}
	if dir := strings.TrimSpace(projectPath); dir != "" {
		params.Directory = opencode.F(dir)
	}

	providersResp, err := driver.client.App.Providers(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to fetch OpenCode providers: %w", err)
	}

	var selectedProvider *opencode.Provider
	for i := range providersResp.Providers {
		p := &providersResp.Providers[i]
		if strings.EqualFold(p.ID, providerID) {
			selectedProvider = p
			break
		}
	}

	if selectedProvider == nil {
		return fmt.Errorf("provider %q is not available on the running OpenCode server", providerID)
	}

	if _, ok := selectedProvider.Models[modelID]; ok {
		return nil
	}

	modelIDs := make([]string, 0, len(selectedProvider.Models))
	for id := range selectedProvider.Models {
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)

	if len(modelIDs) == 0 {
		return fmt.Errorf("provider %q is available but returned no models", providerID)
	}

	preview := modelIDs
	if len(preview) > 6 {
		preview = preview[:6]
	}

	if strings.EqualFold(providerID, "openai") && isOpenAICodexForwardCompatModelID(modelID) {
		if _, ok := selectedProvider.Models["gpt-5.2-codex"]; ok {
			log.Printf("[OPENCODE] Model %q not present in provider %q registry; allowing forward-compat via template gpt-5.2-codex", modelID, providerID)
			return nil
		}
		if _, ok := selectedProvider.Models["gpt-5.3-codex"]; ok {
			log.Printf("[OPENCODE] Model %q not present in provider %q registry; allowing forward-compat via template gpt-5.3-codex", modelID, providerID)
			return nil
		}
	}

	if !isStrictModelPreflightProvider(providerID) {
		log.Printf("[OPENCODE] Model %q not present in provider %q registry; proceeding without strict block (sample available: %s)", modelID, providerID, strings.Join(preview, ", "))
		return nil
	}

	return fmt.Errorf("model %q is not available for provider %q (available: %s)", modelID, providerID, strings.Join(preview, ", "))
}

type openCodeProviderModelProbe struct {
	ProviderFound       bool
	ModelRegistered     bool
	ProviderModelIDs    []string
	ProviderModelSample []string
}

func probeOpenCodeProviderModel(projectPath, providerID, modelID string) (openCodeProviderModelProbe, error) {
	result := openCodeProviderModelProbe{}
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(modelID) == "" {
		return result, nil
	}

	providerID = normalizeOpenCodeProviderID(providerID)
	driver := GetOpenCodeDriver()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	params := opencode.AppProvidersParams{}
	if dir := strings.TrimSpace(projectPath); dir != "" {
		params.Directory = opencode.F(dir)
	}

	providersResp, err := driver.client.App.Providers(ctx, params)
	if err != nil {
		return result, err
	}

	var selectedProvider *opencode.Provider
	for i := range providersResp.Providers {
		p := &providersResp.Providers[i]
		if strings.EqualFold(p.ID, providerID) {
			selectedProvider = p
			break
		}
	}
	if selectedProvider == nil {
		return result, nil
	}

	result.ProviderFound = true
	modelIDs := make([]string, 0, len(selectedProvider.Models))
	for id := range selectedProvider.Models {
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)
	result.ProviderModelIDs = modelIDs

	if len(modelIDs) > 12 {
		result.ProviderModelSample = modelIDs[:12]
	} else {
		result.ProviderModelSample = modelIDs
	}

	_, result.ModelRegistered = selectedProvider.Models[modelID]
	return result, nil
}

func openCodeZenAgentModelFallbackCandidates(modelID string) []string {
	normalized := normalizeOpenCodeZenModelID(modelID)
	candidates := []string{normalized}

	switch normalized {
	case "minimax-m2.5-free":
		candidates = append(candidates, "kimi-k2.5-free", "glm-5-free", "big-pickle")
	case "glm-5-free":
		candidates = append(candidates, "kimi-k2.5-free", "big-pickle")
	case "big-pickle":
		candidates = append(candidates, "kimi-k2.5-free", "glm-5-free")
	case "kimi-k2.5-free":
		candidates = append(candidates, "glm-5-free", "big-pickle")
	default:
		candidates = append(candidates, "kimi-k2.5-free", "glm-5-free", "big-pickle")
	}

	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func ollamaAgentModelFallbackCandidates(modelID string) []string {
	trimmed := strings.TrimSpace(strings.ToLower(modelID))
	trimmed = strings.TrimPrefix(trimmed, "ollama/")
	if trimmed == "" {
		return nil
	}

	candidates := []string{}
	switch trimmed {
	case "qwen3.5", "qwen-3.5", "qwen3.5:latest", "qwen-3.5:latest":
		candidates = append(candidates,
			"qwen3.5",
			"qwen3.5:latest",
			"qwen-3.5",
			"qwen-3.5:latest",
		)
	case "gpt-oss", "gpt-oss:20b", "gpt-oss:20b:latest":
		candidates = append(candidates,
			"gpt-oss:20b",
			"gpt-oss:20b:latest",
			"gpt-oss",
			"gpt-oss:latest",
		)
	case "qwen3-coder", "qwen3-coder:30b", "qwen3-coder:30b:latest":
		candidates = append(candidates,
			"qwen3-coder:30b",
			"qwen3-coder:30b:latest",
			"qwen3-coder",
		)
	default:
		candidates = append(candidates, trimmed)
		if strings.HasSuffix(trimmed, ":latest") {
			base := strings.TrimSuffix(trimmed, ":latest")
			if base != "" {
				candidates = append(candidates, base)
			}
		} else {
			candidates = append(candidates, trimmed+":latest")
		}
	}

	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func resolveSessionModelForRequest(projectPath, requestedModel string) (modelID, providerID string, fallbackNote string) {
	modelID, providerID = parseModel(requestedModel)

	trimmedRequested := strings.TrimSpace(requestedModel)
	requestedModelID := trimmedRequested
	if slash := strings.Index(trimmedRequested, "/"); slash > 0 && slash+1 < len(trimmedRequested) {
		requestedProvider := strings.TrimSpace(trimmedRequested[:slash])
		if strings.EqualFold(requestedProvider, providerID) {
			requestedModelID = strings.TrimSpace(trimmedRequested[slash+1:])
		}
	}

	if strings.EqualFold(providerID, "opencode") {
		probe, probeErr := probeOpenCodeProviderModel(projectPath, providerID, modelID)
		if probeErr != nil {
			log.Printf("[OPENCODE] Warning: failed probing provider registry for %s/%s: %v", providerID, modelID, probeErr)
			return modelID, providerID, ""
		}
		if !probe.ProviderFound || probe.ModelRegistered {
			return modelID, providerID, ""
		}

		available := make(map[string]struct{}, len(probe.ProviderModelIDs))
		for _, id := range probe.ProviderModelIDs {
			available[strings.TrimSpace(strings.ToLower(id))] = struct{}{}
		}

		for _, candidate := range openCodeZenAgentModelFallbackCandidates(modelID) {
			if _, ok := available[strings.ToLower(candidate)]; !ok {
				continue
			}
			if strings.EqualFold(candidate, modelID) {
				return modelID, providerID, ""
			}
			fallbackNote = fmt.Sprintf("OpenCode model %s/%s is unavailable on this server build; using %s/%s instead.", providerID, modelID, providerID, candidate)
			log.Printf("[OPENCODE] %s Available sample: %s", fallbackNote, strings.Join(probe.ProviderModelSample, ", "))
			return candidate, providerID, fallbackNote
		}

		log.Printf("[OPENCODE] No compatible OpenCode Zen fallback found for requested model %s/%s. Available sample: %s", providerID, modelID, strings.Join(probe.ProviderModelSample, ", "))
		return modelID, providerID, ""
	}

	if strings.EqualFold(providerID, "ollama") {
		probe, probeErr := probeOpenCodeProviderModel(projectPath, providerID, modelID)
		if probeErr != nil {
			log.Printf("[OPENCODE] Warning: failed probing provider registry for %s/%s: %v", providerID, modelID, probeErr)
			return modelID, providerID, ""
		}
		if !probe.ProviderFound || probe.ModelRegistered {
			return modelID, providerID, ""
		}

		availableByLower := make(map[string]string, len(probe.ProviderModelIDs))
		for _, id := range probe.ProviderModelIDs {
			availableByLower[strings.TrimSpace(strings.ToLower(id))] = id
		}

		for _, candidate := range ollamaAgentModelFallbackCandidates(modelID) {
			actualID, ok := availableByLower[strings.ToLower(candidate)]
			if !ok {
				continue
			}
			if strings.EqualFold(actualID, modelID) {
				return actualID, providerID, ""
			}
			fallbackNote = fmt.Sprintf("OpenCode model %s/%s is unavailable on this server build; using %s/%s instead.", providerID, modelID, providerID, actualID)
			log.Printf("[OPENCODE] %s Available sample: %s", fallbackNote, strings.Join(probe.ProviderModelSample, ", "))
			return actualID, providerID, fallbackNote
		}

		log.Printf("[OPENCODE] No compatible Ollama fallback found for requested model %s/%s. Available sample: %s", providerID, modelID, strings.Join(probe.ProviderModelSample, ", "))
		return modelID, providerID, ""
	}

	if strings.EqualFold(providerID, "xai") &&
		strings.Contains(trimmedRequested, "/") &&
		requestedModelID != "" &&
		!strings.EqualFold(requestedModelID, modelID) {
		fallbackNote = fmt.Sprintf("OpenCode xAI provider does not expose %s yet; using xai/%s instead.", requestedModelID, modelID)
	}
	return modelID, providerID, fallbackNote
}

func ensureOpenCodeServerReady(projectPath, model, openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey, openAIAuthMode, openAIRefreshToken string, openAIExpiresAt float64) error {
	openCodeServerPrepMu.Lock()
	defer openCodeServerPrepMu.Unlock()

	serverURL := "http://" + openCodeServerHostname() + ":" + getAgentPort()
	openAIAuthMode = normalizeOpenAIAuthMode(openAIAuthMode)
	if openAIAuthMode == "codex-jwt" && strings.TrimSpace(openAIKey) == "" {
		resolved, err := resolveOpenAIOAuthCredentialForConnect(OpenCodeAuthConnectRequest{
			OpenAIKey:          openAIKey,
			OpenAIRefreshToken: openAIRefreshToken,
			OpenAIExpiresAt:    openAIExpiresAt,
		})
		if err == nil {
			openAIKey = resolved.AccessToken
			if strings.TrimSpace(openAIRefreshToken) == "" {
				openAIRefreshToken = resolved.RefreshToken
			}
			if openAIExpiresAt <= 0 {
				openAIExpiresAt = resolved.ExpiresAtReferenceSeconds
			}
			log.Printf("[OPENCODE] Reusing ChatGPT OAuth credential from %s for codex-jwt mode", resolved.Source)
		}
	}
	modelForServer := strings.TrimSpace(model)
	if modelForServer != "" {
		normalizedModelID, normalizedProviderID := parseModel(modelForServer)
		if strings.TrimSpace(normalizedModelID) != "" && strings.TrimSpace(normalizedProviderID) != "" {
			modelForServer = normalizedProviderID + "/" + normalizedModelID
		}
	}

	serverWasAlreadyRunning := isServerRunning(serverURL)
	if serverWasAlreadyRunning {
		if !getOpenCodeOpenAIAuthState().known {
			if openAIAuthMode == "opencode-config" {
				log.Printf("[OPENCODE] Running server detected with unknown auth/runtime state; preserving server in opencode-config mode")
				setOpenCodeOpenAIAuthState(openAIAuthMode, "")
			} else {
				log.Printf("[OPENCODE] Running server detected with unknown auth/runtime state; restarting under Glowbom-managed runtime")
				if restartErr := restartOpenCodeServer(openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey, modelForServer, openAIAuthMode); restartErr != nil {
					return fmt.Errorf("failed restarting inherited OpenCode server: %w", restartErr)
				}
			}
		}
	} else {
		log.Printf("[OPENCODE] Server not running, starting with provided keys...")
		if err := startOpenCodeServer(openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey, modelForServer, openAIAuthMode); err != nil {
			return fmt.Errorf("failed to start OpenCode server: %w", err)
		}
		if err := waitForOpenCodeServerReady(serverURL, 12*time.Second); err != nil {
			return err
		}
	}

	if shouldRestartForOpenAIAuthChange(serverWasAlreadyRunning, openAIAuthMode, openAIKey) {
		log.Printf("[OPENCODE] Restarting server to apply OpenAI auth transition (mode=%s)", openAIAuthMode)
		if restartErr := restartOpenCodeServer(openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey, modelForServer, openAIAuthMode); restartErr != nil {
			return fmt.Errorf("failed to restart OpenCode server for OpenAI auth transition: %w", restartErr)
		}
	}

	if err := applyOpenAIAuthModeToRunningServer(serverURL, openAIAuthMode, openAIKey, openAIRefreshToken, openAIExpiresAt); err != nil {
		return err
	}

	requestedModel := strings.TrimSpace(modelForServer)
	if requestedModel == "" {
		return nil
	}

	modelID, providerID := parseModel(requestedModel)
	if err := ensureOpenCodeModelAvailable(projectPath, providerID, modelID); err == nil {
		return nil
	} else {
		log.Printf("[OPENCODE] Model availability preflight failed for %s/%s: %v", providerID, modelID, err)
		if !shouldAttemptOpenCodeRestartForProvider(providerID, openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey) {
			return err
		}

		log.Printf("[OPENCODE] Attempting OpenCode server restart to refresh credentials for provider=%s", providerID)
		if restartErr := restartOpenCodeServer(openAIKey, anthropicKey, geminiKey, fireworksKey, openRouterKey, openCodeZenKey, xaiKey, modelForServer, openAIAuthMode); restartErr != nil {
			return fmt.Errorf("OpenCode restart failed while preparing %s/%s: %w", providerID, modelID, restartErr)
		}

		if err := applyOpenAIAuthModeToRunningServer(serverURL, openAIAuthMode, openAIKey, openAIRefreshToken, openAIExpiresAt); err != nil {
			return err
		}

		if finalErr := ensureOpenCodeModelAvailable(projectPath, providerID, modelID); finalErr != nil {
			return fmt.Errorf("requested model %s/%s is unavailable after restart: %w", providerID, modelID, finalErr)
		}

		log.Printf("[OPENCODE] Model availability preflight succeeded after restart for %s/%s", providerID, modelID)
	}

	return nil
}

// NewOpenCodeDriver creates a new OpenCode driver instance
// Checks OPENCODE_URL env var, then falls back to default http://127.0.0.1:<GLOWBOM_AGENT_PORT>
// Note: Server auto-start is now handled per-request with API keys
func NewOpenCodeDriver(serverURL string) *OpenCodeDriver {
	if serverURL == "" {
		serverURL = os.Getenv("OPENCODE_URL")
	}
	if serverURL == "" {
		serverURL = "http://" + openCodeServerHostname() + ":" + getAgentPort()
	}

	log.Printf("[OPENCODE] Connecting to server at %s", serverURL)

	clientOptions := []option.RequestOption{
		option.WithBaseURL(serverURL),
	}
	if header := openCodeServerAuthorizationHeader(); header != "" {
		clientOptions = append(clientOptions, option.WithHeader("Authorization", header))
	}
	client := opencode.NewClient(clientOptions...)

	return &OpenCodeDriver{
		client:              client,
		serverURL:           serverURL,
		questionReplyPath:   "/question/{requestID}/reply",
		permissionReplyPath: "/permission/{requestID}/reply",
		repliedQuestions:    make(map[string]time.Time),
	}
}

// ============================================================
// Project Management
// ============================================================

// GetProjectPaths returns standard paths for a project root
func GetProjectPaths(projectRoot string) ProjectPaths {
	return ProjectPaths{
		Root:      projectRoot,
		Prototype: filepath.Join(projectRoot, "prototype"),
		Assets:    filepath.Join(projectRoot, "prototype", "assets"),
		iOS:       filepath.Join(projectRoot, "ios"),
		Android:   filepath.Join(projectRoot, "android"),
		Web:       filepath.Join(projectRoot, "web"),
		Godot:     filepath.Join(projectRoot, "godot"),
		Manifest:  filepath.Join(projectRoot, "glowbom.json"),
	}
}

// InitProject creates a new Glowbom project structure
func InitProject(projectRoot, name string) (*GlowbomProject, error) {
	paths := GetProjectPaths(projectRoot)

	// Create directory structure
	dirs := []string{
		paths.Root,
		paths.Prototype,
		paths.Assets,
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Create project manifest
	now := time.Now().UTC().Format(time.RFC3339)
	project := &GlowbomProject{
		Name:    name,
		Version: "1.0.0",
		Targets: map[string]Target{
			"ios":     {Enabled: true, OutputDir: "ios", Stack: "swiftui"},
			"android": {Enabled: true, OutputDir: "android", Stack: "kotlin"},
			"web":     {Enabled: true, OutputDir: "web", Stack: "nextjs"},
			"godot":   {Enabled: false, OutputDir: "godot", Stack: "godot"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Write manifest
	if err := SaveProject(paths.Manifest, project); err != nil {
		return nil, err
	}

	fmt.Printf("[PROJECT] Initialized project '%s' at %s\n", name, projectRoot)
	return project, nil
}

// LoadProject loads a project from its manifest
func LoadProject(manifestPath string) (*GlowbomProject, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}

	var project GlowbomProject
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	return &project, nil
}

// SaveProject saves a project manifest
func SaveProject(manifestPath string, project *GlowbomProject) error {
	project.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize manifest: %w", err)
	}

	if err := os.WriteFile(manifestPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	return nil
}

// SavePrototype saves HTML and assets to the project's prototype directory
func SavePrototype(projectRoot, html string, assets map[string][]byte) error {
	paths := GetProjectPaths(projectRoot)

	// Ensure directories exist
	if err := os.MkdirAll(paths.Prototype, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.Assets, 0755); err != nil {
		return err
	}

	// Write HTML
	htmlPath := filepath.Join(paths.Prototype, "index.html")
	if err := os.WriteFile(htmlPath, []byte(html), 0644); err != nil {
		return fmt.Errorf("failed to write prototype HTML: %w", err)
	}

	// Write assets
	for name, data := range assets {
		assetPath := filepath.Join(paths.Assets, name)
		if err := os.WriteFile(assetPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write asset %s: %w", name, err)
		}
	}

	fmt.Printf("[PROJECT] Saved prototype with %d assets\n", len(assets))
	return nil
}

// LoadPrototype loads the prototype HTML from a project
func LoadPrototype(projectRoot string) (string, error) {
	paths := GetProjectPaths(projectRoot)
	htmlPath := filepath.Join(paths.Prototype, "index.html")

	data, err := os.ReadFile(htmlPath)
	if err != nil {
		return "", fmt.Errorf("failed to read prototype: %w", err)
	}

	return string(data), nil
}

// GetTargetDir returns the output directory for a target language
func GetTargetDir(projectRoot, targetLang string) string {
	paths := GetProjectPaths(projectRoot)
	switch targetLang {
	case "swiftui", "ios":
		return paths.iOS
	case "kotlin", "android":
		return paths.Android
	case "nextjs", "web":
		return paths.Web
	case "godot", "games":
		return paths.Godot
	case "ipados", "macos", "visionos", "watchos", "tvos", "wearos", "androidtv", "androidauto", "windows", "linux", "server":
		return filepath.Join(projectRoot, targetLang)
	default:
		return filepath.Join(projectRoot, targetLang)
	}
}

// ============================================================
// Translation
// ============================================================

// TranslateToProduction translates HTML prototype to production code
func (d *OpenCodeDriver) TranslateToProduction(ctx context.Context, req OpenCodeTranslateRequest) (*OpenCodeTranslateResponse, error) {
	log.Printf("[OPENCODE] Starting translation to %s", req.TargetLang)
	var projectRoot, sourceHTML, targetDir string

	// Determine source and target based on request type
	if req.ProjectPath != "" {
		// Project-based mode: read from project structure
		projectRoot = req.ProjectPath
		paths := GetProjectPaths(projectRoot)

		// Verify project exists
		if _, err := os.Stat(paths.Manifest); os.IsNotExist(err) {
			log.Printf("[OPENCODE] Error: Not a Glowbom project: %s (missing glowbom.json)", projectRoot)
			return nil, fmt.Errorf("not a Glowbom project: %s (missing glowbom.json)", projectRoot)
		}

		// Load prototype HTML
		html, err := LoadPrototype(projectRoot)
		if err != nil {
			log.Printf("[OPENCODE] Error: Failed to load prototype from %s: %v", paths.Prototype, err)
			return nil, fmt.Errorf("failed to load prototype: %w", err)
		}
		sourceHTML = html
		if req.TargetDir != "" {
			targetDir = filepath.Join(projectRoot, req.TargetDir)
		} else if project, err := LoadProject(paths.Manifest); err == nil {
			targetKey := req.TargetID
			if targetKey == "" {
				targetKey = req.TargetLang
			}
			if target, ok := project.Targets[targetKey]; ok && target.OutputDir != "" {
				targetDir = filepath.Join(projectRoot, target.OutputDir)
			} else {
				for _, target := range project.Targets {
					if target.Stack == req.TargetLang && target.OutputDir != "" {
						targetDir = filepath.Join(projectRoot, target.OutputDir)
						break
					}
				}
			}
		}
		if targetDir == "" {
			targetDir = GetTargetDir(projectRoot, req.TargetLang)
		}

		log.Printf("[OPENCODE] Project mode: Loading from %s, translating to %s", paths.Prototype, targetDir)
	} else {
		// Legacy mode: direct HTML input
		if req.SourceHTML == "" {
			log.Printf("[OPENCODE] Error: Neither projectPath nor sourceHtml provided")
			return nil, fmt.Errorf("either projectPath or sourceHtml is required")
		}
		sourceHTML = req.SourceHTML

		// Use provided dir or create temp
		if req.ProjectDir != "" {
			projectRoot = req.ProjectDir
			targetDir = req.ProjectDir
		} else {
			tmpDir, err := os.MkdirTemp("", "glowbom-translate-*")
			if err != nil {
				log.Printf("[OPENCODE] Error: Failed to create temp directory: %v", err)
				return nil, fmt.Errorf("failed to create temp directory: %w", err)
			}
			projectRoot = tmpDir
			targetDir = tmpDir
		}

		// Write source HTML for agent reference
		if err := os.MkdirAll(projectRoot, 0755); err != nil {
			log.Printf("[OPENCODE] Error: Failed to create directory %s: %v", projectRoot, err)
			return nil, fmt.Errorf("failed to create directory: %w", err)
		}
		htmlPath := filepath.Join(projectRoot, "prototype.html")
		if err := os.WriteFile(htmlPath, []byte(sourceHTML), 0644); err != nil {
			log.Printf("[OPENCODE] Error: Failed to write prototype HTML: %v", err)
			return nil, fmt.Errorf("failed to write prototype HTML: %w", err)
		}

		log.Printf("[OPENCODE] Legacy mode: Translating to %s", targetDir)
	}

	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		log.Printf("[OPENCODE] Error: Failed to create target directory %s: %v", targetDir, err)
		return nil, fmt.Errorf("failed to create target directory: %w", err)
	}

	log.Printf("[OPENCODE] Creating new session for translation task")

	// Create a new session for this translation task
	session, err := d.client.Session.New(ctx, opencode.SessionNewParams{
		Title:     opencode.F(fmt.Sprintf("Translate to %s", req.TargetLang)),
		Directory: opencode.F(projectRoot),
	})
	if err != nil {
		log.Printf("[OPENCODE] Error: Failed to create session: %v", err)
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	log.Printf("[OPENCODE] Session created: %s", session.ID)

	// Build the translation prompt with project context
	prompt := buildProjectTranslationPrompt(sourceHTML, req.TargetLang, projectRoot, targetDir)
	log.Printf("[OPENCODE] Built translation prompt (%d chars)", len(prompt))

	// Determine model to use
	modelID, providerID, fallbackNote := resolveSessionModelForRequest(projectRoot, req.Model)
	log.Printf("[OPENCODE] Requested model: %s, resolved to: %s/%s", req.Model, providerID, modelID)
	if fallbackNote != "" {
		log.Printf("[OPENCODE] %s", fallbackNote)
	}

	// Send the translation prompt to the agent
	log.Printf("[OPENCODE] Sending prompt to agent")
	response, err := d.sendSessionPrompt(ctx, session.ID, opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Type: opencode.F(opencode.TextPartInputTypeText),
				Text: opencode.F(prompt),
			},
		}),
		Model: opencode.F(opencode.SessionPromptParamsModel{
			ModelID:    opencode.F(modelID),
			ProviderID: opencode.F(providerID),
		}),
		Directory: opencode.F(projectRoot),
	})
	if err != nil {
		log.Printf("[OPENCODE] Error: Failed to send prompt: %v", err)
		return nil, fmt.Errorf("failed to send prompt: %w", err)
	}

	if response != nil {
		log.Printf("[OPENCODE] Prompt sent successfully, tokens used: %.0f input + %.0f output", response.Info.Tokens.Input, response.Info.Tokens.Output)
	} else {
		log.Printf("[OPENCODE] Prompt sent successfully (no synchronous response payload)")
	}

	// Collect generated files from target directory
	files, err := d.collectGeneratedFiles(ctx, targetDir)
	if err != nil {
		log.Printf("[OPENCODE] Warning: Failed to collect files: %v", err)
	} else {
		log.Printf("[OPENCODE] Collected %d generated files", len(files))
	}

	// Update project manifest if project-based
	if req.ProjectPath != "" {
		paths := GetProjectPaths(projectRoot)
		if project, err := LoadProject(paths.Manifest); err == nil {
			targetKey := req.TargetID
			if targetKey == "" {
				targetKey = req.TargetLang
			}
			if _, ok := project.Targets[targetKey]; !ok {
				for key, target := range project.Targets {
					if target.Stack == req.TargetLang {
						targetKey = key
						break
					}
				}
			}
			if _, ok := project.Targets[targetKey]; !ok {
				switch req.TargetLang {
				case "swiftui":
					targetKey = "ios"
				case "kotlin":
					targetKey = "android"
				case "nextjs":
					targetKey = "web"
				case "godot", "games":
					targetKey = "godot"
				}
			}
			if target, ok := project.Targets[targetKey]; ok {
				target.LastBuild = time.Now().UTC().Format(time.RFC3339)
				project.Targets[targetKey] = target
				SaveProject(paths.Manifest, project)
				log.Printf("[OPENCODE] Updated project manifest for target %s", targetKey)
			}
		} else {
			log.Printf("[OPENCODE] Warning: Could not update project manifest: %v", err)
		}
	}

	log.Printf("[OPENCODE] Translation completed successfully")
	return &OpenCodeTranslateResponse{
		SessionID:  session.ID,
		Success:    true,
		Files:      files,
		TokensUsed: response.Info.Tokens.Input + response.Info.Tokens.Output,
		Cost:       response.Info.Cost,
	}, nil
}

// TranslateToProductionStreaming translates with real-time streaming to HTTP response
func (d *OpenCodeDriver) TranslateToProductionStreaming(ctx context.Context, w http.ResponseWriter, req OpenCodeTranslateRequest) error {
	log.Printf("[OPENCODE] Starting streaming translation to %s", req.TargetLang)

	// Set up SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[OPENCODE] Error: Streaming not supported")
		return fmt.Errorf("streaming not supported")
	}

	sendSSEData(w, flusher, map[string]interface{}{"output": "Starting translation with OpenCode agent..."})

	var projectRoot, sourceHTML, targetDir string

	// Determine source and target based on request type.
	if req.ProjectPath != "" {
		projectRoot = req.ProjectPath
		paths := GetProjectPaths(projectRoot)

		if _, err := os.Stat(paths.Manifest); os.IsNotExist(err) {
			msg := fmt.Sprintf("Not a Glowbom project: %s (missing glowbom.json)", projectRoot)
			sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": msg})
			return errors.New(msg)
		}

		html, err := LoadPrototype(projectRoot)
		if err != nil {
			msg := userFacingAgentErrorMessage(fmt.Sprintf("failed to load prototype: %v", err))
			sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": msg})
			return err
		}
		sourceHTML = html

		if req.TargetDir != "" {
			targetDir = filepath.Join(projectRoot, req.TargetDir)
		} else if project, err := LoadProject(paths.Manifest); err == nil {
			targetKey := req.TargetID
			if targetKey == "" {
				targetKey = req.TargetLang
			}
			if target, ok := project.Targets[targetKey]; ok && target.OutputDir != "" {
				targetDir = filepath.Join(projectRoot, target.OutputDir)
			} else {
				for _, target := range project.Targets {
					if target.Stack == req.TargetLang && target.OutputDir != "" {
						targetDir = filepath.Join(projectRoot, target.OutputDir)
						break
					}
				}
			}
		}
		if targetDir == "" {
			targetDir = GetTargetDir(projectRoot, req.TargetLang)
		}
	} else {
		// Legacy mode: direct HTML input.
		if req.SourceHTML == "" {
			msg := "either projectPath or sourceHtml is required"
			sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": msg})
			return errors.New(msg)
		}
		sourceHTML = req.SourceHTML
		if req.ProjectDir != "" {
			projectRoot = req.ProjectDir
			targetDir = req.ProjectDir
		} else {
			tmpDir, err := os.MkdirTemp("", "glowbom-translate-*")
			if err != nil {
				msg := userFacingAgentErrorMessage(fmt.Sprintf("failed to create temp directory: %v", err))
				sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": msg})
				return err
			}
			projectRoot = tmpDir
			targetDir = tmpDir
		}

		if err := os.MkdirAll(projectRoot, 0755); err != nil {
			msg := userFacingAgentErrorMessage(fmt.Sprintf("failed to create directory: %v", err))
			sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": msg})
			return err
		}
		htmlPath := filepath.Join(projectRoot, "prototype.html")
		if err := os.WriteFile(htmlPath, []byte(sourceHTML), 0644); err != nil {
			msg := userFacingAgentErrorMessage(fmt.Sprintf("failed to write prototype HTML: %v", err))
			sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": msg})
			return err
		}
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		msg := userFacingAgentErrorMessage(fmt.Sprintf("failed to create target directory: %v", err))
		sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": msg})
		return err
	}

	sendSSEData(w, flusher, map[string]interface{}{"output": "Creating translation session..."})
	session, err := d.client.Session.New(ctx, opencode.SessionNewParams{
		Title:     opencode.F(fmt.Sprintf("Translate to %s", req.TargetLang)),
		Directory: opencode.F(projectRoot),
	})
	if err != nil {
		friendly := userFacingAgentErrorMessage(err.Error())
		sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": friendly})
		return err
	}

	sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("Session created: %s", session.ID)})

	prompt := buildProjectTranslationPrompt(sourceHTML, req.TargetLang, projectRoot, targetDir)
	modelID, providerID, fallbackNote := resolveSessionModelForRequest(projectRoot, req.Model)
	sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("Using model: %s/%s", providerID, modelID)})
	if fallbackNote != "" {
		sendSSEData(w, flusher, map[string]interface{}{"output": "⚠️ " + fallbackNote})
	}

	// Capture a pre-run snapshot so we can recover changed files when stream file events are missing.
	preRunSnapshot, snapshotErr := captureProjectFileSnapshot(projectRoot)
	if snapshotErr != nil {
		log.Printf("[OPENCODE] Warning: Failed to capture pre-run snapshot for translation: %v", snapshotErr)
	}

	// Start event stream in background before sending prompt to avoid race conditions.
	type sessionStreamResult struct {
		completed    bool
		changedFiles []string
		hadActivity  bool
		errorMessage string
	}
	eventStreamDone := make(chan sessionStreamResult, 1)
	promptDispatched := make(chan struct{})

	go func() {
		completed, changedFiles, hadActivity, errorMessage := d.streamEventsAndWaitForCompletion(ctx, w, flusher, projectRoot, session.ID, promptDispatched)
		eventStreamDone <- sessionStreamResult{
			completed:    completed,
			changedFiles: changedFiles,
			hadActivity:  hadActivity,
			errorMessage: errorMessage,
		}
	}()

	time.Sleep(100 * time.Millisecond)
	sendSSEData(w, flusher, map[string]interface{}{"output": "Agent is translating the project..."})

	response, err := d.sendSessionPrompt(ctx, session.ID, opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Type: opencode.F(opencode.TextPartInputTypeText),
				Text: opencode.F(prompt),
			},
		}),
		Model: opencode.F(opencode.SessionPromptParamsModel{
			ModelID:    opencode.F(modelID),
			ProviderID: opencode.F(providerID),
		}),
		Directory: opencode.F(projectRoot),
	})
	close(promptDispatched)
	if err != nil {
		friendly := userFacingAgentErrorMessage(err.Error())
		if isUsageLimitErrorMessage(err.Error()) || isUsageLimitErrorMessage(friendly) {
			go d.abortSessionBestEffort(session.ID, projectRoot, "usage limit while sending translation prompt")
		}
		sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": friendly})
		return err
	}

	streamResult := <-eventStreamDone
	if !streamResult.completed {
		sessionErr := userFacingAgentErrorMessage(streamResult.errorMessage)
		if strings.TrimSpace(sessionErr) == "" || strings.EqualFold(sessionErr, "Unknown error occurred") {
			sessionErr = "Session did not complete"
		}
		sendSSEData(w, flusher, map[string]interface{}{
			"done":    true,
			"success": false,
			"error":   sessionErr,
		})
		return fmt.Errorf("translation session did not complete: %s", sessionErr)
	}

	changedFiles := streamResult.changedFiles
	if len(changedFiles) == 0 && preRunSnapshot != nil {
		snapshotChanges, err := detectChangedFilesFromSnapshot(projectRoot, preRunSnapshot)
		if err != nil {
			log.Printf("[OPENCODE] Warning: Failed translation snapshot fallback: %v", err)
		} else if len(snapshotChanges) > 0 {
			changedFiles = snapshotChanges
			sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("📡 Recovered %d changed files via snapshot fallback", len(changedFiles))})
		}
	}
	if !streamResult.hadActivity {
		sendSSEData(w, flusher, map[string]interface{}{
			"output": "⚠️  Session reached idle without assistant/tool activity. The selected model may be only partially supported by this OpenCode server build.",
		})
	}

	files, collectErr := d.collectGeneratedFiles(ctx, targetDir)
	if collectErr != nil {
		log.Printf("[OPENCODE] Warning: Failed to collect generated files after translation: %v", collectErr)
		files = []string{}
	}

	// Update project manifest if project-based.
	if req.ProjectPath != "" {
		paths := GetProjectPaths(projectRoot)
		if project, err := LoadProject(paths.Manifest); err == nil {
			targetKey := req.TargetID
			if targetKey == "" {
				targetKey = req.TargetLang
			}
			if _, ok := project.Targets[targetKey]; !ok {
				for key, target := range project.Targets {
					if target.Stack == req.TargetLang {
						targetKey = key
						break
					}
				}
			}
			if _, ok := project.Targets[targetKey]; !ok {
				switch req.TargetLang {
				case "swiftui":
					targetKey = "ios"
				case "kotlin":
					targetKey = "android"
				case "nextjs":
					targetKey = "web"
				case "godot", "games":
					targetKey = "godot"
				}
			}
			if target, ok := project.Targets[targetKey]; ok {
				target.LastBuild = time.Now().UTC().Format(time.RFC3339)
				project.Targets[targetKey] = target
				SaveProject(paths.Manifest, project)
			}
		} else {
			log.Printf("[OPENCODE] Warning: Could not update project manifest after translation stream: %v", err)
		}
	}

	tokensUsed := 0.0
	cost := 0.0
	if response != nil {
		tokensUsed = response.Info.Tokens.Input + response.Info.Tokens.Output
		cost = response.Info.Cost
	}

	sendSSEData(w, flusher, map[string]interface{}{
		"done":           true,
		"success":        true,
		"output":         "✅ Translation completed successfully!",
		"sessionId":      session.ID,
		"files":          len(files),
		"generatedFiles": files,
		"changedFiles":   changedFiles,
		"tokensUsed":     tokensUsed,
		"cost":           cost,
	})

	return nil
}

// streamEvents streams OpenCode events to the HTTP response
func (d *OpenCodeDriver) streamEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, projectDir, sessionID string) {
	log.Printf("[OPENCODE] Starting event streaming for directory: %s", projectDir)
	stream := d.client.Event.ListStreaming(ctx, opencode.EventListParams{
		Directory: opencode.F(projectDir),
	})

	for stream.Next() {
		event := stream.Current()
		eventSID := extractEventSessionID(event)
		if sessionID != "" && eventSID != "" && eventSID != sessionID {
			continue
		}

		// Stream relevant events to client
		switch event.Type {
		case "message.updated":
			if props, ok := event.Properties.(map[string]interface{}); ok {
				if content, ok := props["content"].(string); ok && content != "" {
					log.Printf("[OPENCODE] Agent message: %s", content[:min(100, len(content))])
					sendSSEEvent(w, flusher, "message", content)
				}
			}
		case "file.updated", "file.edited":
			filePath := getPropertyString(event.Properties, "Path", "path", "File", "file")
			if filePath != "" {
				log.Printf("[OPENCODE] File updated: %s", filePath)
				sendSSEEvent(w, flusher, "file", fmt.Sprintf("Updated: %s", filePath))
			}
		case "tool.start":
			if props, ok := event.Properties.(map[string]interface{}); ok {
				if toolName, ok := props["name"].(string); ok {
					log.Printf("[OPENCODE] Tool started: %s", toolName)
					sendSSEEvent(w, flusher, "tool", fmt.Sprintf("Running: %s", toolName))
					sendSSEEvent(w, flusher, "progress", fmt.Sprintf("Executing %s...", toolName))
				}
			}
		case "tool.end":
			if props, ok := event.Properties.(map[string]interface{}); ok {
				if toolName, ok := props["name"].(string); ok {
					log.Printf("[OPENCODE] Tool completed: %s", toolName)
					sendSSEEvent(w, flusher, "tool_done", fmt.Sprintf("Completed: %s", toolName))
				}
			}
		case "session.status":
			details := extractSessionStatusMessage(event.Properties)
			if details == "" {
				if rawProps := parseEventPropertiesFromRaw(event.JSON.RawJSON()); rawProps != nil {
					details = extractSessionStatusMessage(rawProps)
				}
			}
			rawDetails := strings.TrimSpace(string(event.JSON.RawJSON()))
			details = usageLimitContext(details, rawDetails)
			if details != "" {
				if isUsageLimitErrorMessage(details) {
					sendSSEEvent(w, flusher, "error", userFacingAgentErrorMessage(details))
					d.abortSessionBestEffort(sessionID, projectDir, "usage limit in session.status")
					return
				} else {
					sendSSEEvent(w, flusher, "status", details)
				}
			}
		case "session.error":
			errorMsg := extractSessionErrorMessage(event.Properties)
			if errorMsg == "" {
				if rawProps := parseEventPropertiesFromRaw(event.JSON.RawJSON()); rawProps != nil {
					errorMsg = extractSessionErrorMessage(rawProps)
				}
			}
			rawError := strings.TrimSpace(string(event.JSON.RawJSON()))
			errorMsg = usageLimitContext(errorMsg, rawError)
			if errorMsg == "" {
				errorMsg = "Unknown error occurred"
			}
			sendSSEEvent(w, flusher, "error", userFacingAgentErrorMessage(errorMsg))
			if isUsageLimitErrorMessage(errorMsg) {
				d.abortSessionBestEffort(sessionID, projectDir, "usage limit in session.error")
			}
			return
		default:
			log.Printf("[OPENCODE] Unhandled event type: %s", event.Type)
		}
	}

	if err := stream.Err(); err != nil {
		log.Printf("[OPENCODE] Event stream error: %v", err)
	} else {
		log.Printf("[OPENCODE] Event streaming ended")
	}
}

// collectGeneratedFiles lists files created in the project directory
func (d *OpenCodeDriver) collectGeneratedFiles(ctx context.Context, projectDir string) ([]string, error) {
	var files []string

	err := filepath.Walk(projectDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() != "prototype.html" {
			relPath, _ := filepath.Rel(projectDir, path)
			files = append(files, relPath)
		}
		return nil
	})

	return files, err
}

// CheckHealth verifies OpenCode server is running
func (d *OpenCodeDriver) CheckHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Try to list sessions as a health check
	_, err := d.client.Session.List(ctx, opencode.SessionListParams{})
	if err != nil {
		return fmt.Errorf("OpenCode server not responding at %s: %w", d.serverURL, err)
	}
	return nil
}

// buildTranslationPrompt creates the prompt for translation tasks
func buildTranslationPrompt(sourceHTML, targetLang string) string {
	instructions := stackInstructionsFor(targetLang, false)

	return fmt.Sprintf(`Translate this HTML/Tailwind prototype into a production %s project.

## Source Prototype
%s

## Requirements
%s

## Important
- Create ALL necessary files for a complete, runnable project
- Do NOT just generate a single file - create the full project structure
- Verify the build passes before completing
- If build fails, fix the errors and try again
`, targetLang, sourceHTML, instructions)
}

// buildProjectTranslationPrompt creates a context-aware prompt for project-based translation
func buildProjectTranslationPrompt(sourceHTML, targetLang, projectRoot, targetDir string) string {
	instructions := stackInstructionsFor(targetLang, true)
	targetDirName := filepath.Base(targetDir)

	return fmt.Sprintf(`You are working in a Glowbom project at: %s

## Project Structure
- prototype/index.html - The HTML/Tailwind prototype (source)
- prototype/assets/ - Images and other assets
- %s/ - Target directory for %s code (create files here)

## Source Prototype
%s

## Task
Translate the prototype into a production %s project. Report your progress and actions clearly.

## Requirements
%s

## Instructions for Agent
- Work exclusively in the target directory: %s
- Preserve the visual design and layout from the prototype exactly
- Include ALL assets - copy from prototype/assets/ to appropriate location
- Create complete, runnable project structure
- Report progress after each major step (e.g., "Created main component", "Copied assets", "Running build")
- Verify the build passes before completing
- If build fails, fix the errors and try again, reporting each attempt and fix
- Be specific about what you're doing in the directory
`, projectRoot, targetDirName, targetLang, sourceHTML, targetLang, instructions, targetDir)
}

// parseModel extracts provider and model ID from combined string
func parseModel(model string) (modelID, providerID string) {
	// Default to Claude Sonnet 4.6
	if model == "" {
		return "claude-sonnet-4-6", "anthropic"
	}

	// Handle "provider/model" format
	for _, prefix := range []string{"anthropic/", "openai/", "google/", "ollama/", "fireworks/", "fireworks-ai/", "openrouter/", "opencode/", "xai/"} {
		if len(model) > len(prefix) && model[:len(prefix)] == prefix {
			modelID = model[len(prefix):]
			providerID = model[:len(prefix)-1]
			if providerID == "openai" {
				modelID = normalizeOpenAIModelID(modelID)
			}
			if providerID == "google" {
				modelID = normalizeGeminiModelID(modelID)
			}
			if providerID == "fireworks" || providerID == "fireworks-ai" {
				providerID = "fireworks-ai"
				modelID = normalizeFireworksModelID(modelID)
			}
			if providerID == "openrouter" {
				modelID = normalizeOpenRouterModelID(modelID)
			}
			if providerID == "opencode" {
				modelID = normalizeOpenCodeZenModelID(modelID)
			}
			if providerID == "xai" {
				modelID = normalizeXAIModelID(modelID)
			}
			return modelID, providerID
		}
	}

	// Pass through unknown provider/model strings so OpenCode can resolve custom providers.
	if slash := strings.Index(model, "/"); slash > 0 && slash+1 < len(model) {
		return strings.TrimSpace(model[slash+1:]), strings.TrimSpace(model[:slash])
	}

	// Handle shorthand names
	switch model {
	case "claude", "claude-sonnet":
		return "claude-sonnet-4-6", "anthropic"
	case "claude-opus":
		return "claude-opus-4-7", "anthropic"
	case "gpt-5", "gpt-5.1", "gpt-5.4", "gpt-5.5":
		return "gpt-5.5", "openai"
	case "gpt-5.2":
		return "gpt-5.2", "openai"
	case "gpt-4o":
		return "gpt-4o", "openai"
	case "gemini":
		return "gemini-3.1-pro-preview", "google"
	case "grok", "xai", "grok-4.1", "grok-4-1", "grok-4.1-fast", "grok-4-1-fast", "grok-4.1-fast-reasoning", "grok-4-1-fast-reasoning", "grok-4.1-fast-non-reasoning", "grok-4-1-fast-non-reasoning", "grok-4.3", "grok-4-3", "grok-4.3-latest", "grok-4-3-latest", "grok-latest":
		return normalizeXAIModelID(model), "xai"
	case "fireworks", "glm-5":
		return "accounts/fireworks/models/glm-5", "fireworks-ai"
	case "kimi-k2.5", "kimi-k2p5":
		return "accounts/fireworks/models/kimi-k2p5", "fireworks-ai"
	case "minimax-m2.5", "minimax-m2p5", "minimax-m2.5-free", "minimax-m2p5-free":
		return "accounts/fireworks/models/minimax-m2p5", "fireworks-ai"
	case "openrouter", "openrouter/glm-5":
		return "z-ai/glm-5", "openrouter"
	case "openrouter/kimi-k2", "openrouter/kimi-k2.5", "openrouter/kimi-k2p5":
		return "moonshotai/kimi-k2.5", "openrouter"
	case "openrouter/minimax-m2.5", "openrouter/minimax-m2p5":
		return "minimax/minimax-m2.5", "openrouter"
	case "opencode":
		return "kimi-k2.5-free", "opencode"
	case "opencode/kimi-k2", "opencode/kimi-k2.5", "opencode/kimi-k2p5", "opencode/kimi-k2.5-free", "opencode/kimi-k2p5-free":
		return "kimi-k2.5-free", "opencode"
	case "opencode/minimax-m2", "opencode/minimax-m2.5", "opencode/minimax-m2p5", "opencode/minimax-m2.5-free", "opencode/minimax-m2p5-free":
		return "minimax-m2.5-free", "opencode"
	case "opencode/glm-5", "opencode/glm-5-free":
		return "glm-5-free", "opencode"
	case "opencode/big-pickle", "big-pickle", "bigpickle":
		return "big-pickle", "opencode"
	default:
		return model, "anthropic"
	}
}

func containsEmbeddedImageDataURI(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "data:image/") && strings.Contains(lower, ";base64,")
}

func isIgnorablePromptParseError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "error parsing response json: eof")
}

func (d *OpenCodeDriver) sendSessionPrompt(
	ctx context.Context,
	sessionID string,
	params opencode.SessionPromptParams,
) (*opencode.SessionPromptResponse, error) {
	var resp *http.Response
	out, err := d.client.Session.Prompt(ctx, sessionID, params, option.WithResponseInto(&resp))
	if err == nil {
		return out, nil
	}

	// OpenCode may acknowledge async prompt submission with an empty JSON body.
	// Treat JSON EOF parse failures on 2xx responses as success.
	if isIgnorablePromptParseError(err) && (resp == nil || (resp.StatusCode >= 200 && resp.StatusCode < 300)) {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		log.Printf("[OPENCODE] Prompt accepted with empty response body (status=%d)", status)
		return nil, nil
	}

	return nil, err
}

// sendSSEEvent sends a Server-Sent Event
func sendSSEEvent(w http.ResponseWriter, flusher http.Flusher, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	flusher.Flush()
}

// Global OpenCode driver instance
var openCodeDriver *OpenCodeDriver

// GetOpenCodeDriver returns the singleton driver instance
func GetOpenCodeDriver() *OpenCodeDriver {
	if openCodeDriver == nil {
		openCodeDriver = NewOpenCodeDriver("")
	}
	return openCodeDriver
}

// ============================================================
// HTTP Handlers
// ============================================================

// openCodeTranslateHandler handles translation requests
func openCodeTranslateHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("[OPENCODE] Received translation request")
	if r.Method != http.MethodPost {
		log.Printf("[OPENCODE] Error: Method not allowed: %s", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeTranslateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[OPENCODE] Error: Invalid JSON body: %v", err)
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	// Validate: need either projectPath or sourceHtml
	if req.ProjectPath == "" && req.SourceHTML == "" {
		log.Printf("[OPENCODE] Error: Neither projectPath nor sourceHtml provided")
		http.Error(w, "either projectPath or sourceHtml is required", http.StatusBadRequest)
		return
	}
	if req.TargetLang == "" {
		req.TargetLang = "swiftui" // default
	}

	// Guardrail: never translate embedded base64 image HTML. It explodes token counts
	// and breaks provider limits (especially Fireworks). Require glowbyimage placeholders instead.
	if req.ProjectPath != "" {
		if html, err := LoadPrototype(req.ProjectPath); err == nil && containsEmbeddedImageDataURI(html) {
			msg := "translation blocked: prototype HTML contains embedded base64 images. Regenerate using glowbyimage placeholders before translation."
			log.Printf("[OPENCODE] %s project=%s", msg, req.ProjectPath)
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	}
	if req.SourceHTML != "" && containsEmbeddedImageDataURI(req.SourceHTML) {
		msg := "translation blocked: source HTML contains embedded base64 images. Regenerate using glowbyimage placeholders before translation."
		log.Printf("[OPENCODE] %s", msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	if err := ensureOpenCodeServerReady(req.ProjectPath, req.Model, req.OpenAIKey, req.AnthropicKey, req.GeminiKey, req.FireworksKey, req.OpenRouterKey, req.OpenCodeZenKey, req.XaiKey, req.OpenAIAuthMode, req.OpenAIRefreshToken, req.OpenAIExpiresAt); err != nil {
		log.Printf("[OPENCODE] Failed preparing OpenCode server: %v", err)
		http.Error(w, fmt.Sprintf("Failed to prepare OpenCode server: %v", err), http.StatusInternalServerError)
		return
	}

	driver := GetOpenCodeDriver()
	ctx := r.Context()

	// Check if streaming requested via query or SSE Accept header.
	acceptHeader := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
	isStream := r.URL.Query().Get("stream") == "true" || strings.Contains(acceptHeader, "text/event-stream")

	if isStream {
		log.Printf("[OPENCODE] Starting streaming translation to %s", req.TargetLang)
		if err := driver.TranslateToProductionStreaming(ctx, w, req); err != nil {
			log.Printf("[OPENCODE] Streaming error: %v", err)
		}
		return
	}

	// Non-streaming response
	log.Printf("[OPENCODE] Starting non-streaming translation to %s", req.TargetLang)
	w.Header().Set("Content-Type", "application/json")

	response, err := driver.TranslateToProduction(ctx, req)
	if err != nil {
		log.Printf("[OPENCODE] Translation error: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	log.Printf("[OPENCODE] Translation completed successfully, session: %s", response.SessionID)
	json.NewEncoder(w).Encode(response)
}

// openCodeInitProjectHandler creates a new Glowbom project
func openCodeInitProjectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
		HTML string `json:"html,omitempty"` // Optional: initial prototype HTML
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Path == "" || req.Name == "" {
		http.Error(w, "path and name are required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Initialize project
	project, err := InitProject(req.Path, req.Name)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// Save initial prototype if provided
	if req.HTML != "" {
		if err := SavePrototype(req.Path, req.HTML, nil); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("project created but failed to save prototype: %v", err),
			})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"project": project,
		"paths":   GetProjectPaths(req.Path),
	})
}

// openCodeGetProjectHandler loads project info
func openCodeGetProjectHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}

	paths := GetProjectPaths(path)
	project, err := LoadProject(paths.Manifest)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// Check what exists
	var existingTargets []string
	for targetID, targetInfo := range project.Targets {
		targetDir := GetTargetDir(path, targetID)
		if targetInfo.OutputDir != "" {
			targetDir = filepath.Join(path, targetInfo.OutputDir)
		}
		if info, err := os.Stat(targetDir); err == nil && info.IsDir() {
			existingTargets = append(existingTargets, targetID)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"project":         project,
		"paths":           paths,
		"existingTargets": existingTargets,
	})
}

// openCodeRenameProjectHandler renames a project by updating its manifest
func openCodeRenameProjectHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" || req.Name == "" {
		http.Error(w, "path and name are required", http.StatusBadRequest)
		return
	}

	paths := GetProjectPaths(req.Path)
	project, err := LoadProject(paths.Manifest)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	project.Name = strings.TrimSpace(req.Name)
	if err := SaveProject(paths.Manifest, project); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"project": project,
	})
}

// openCodeUpdateProjectSettingsHandler updates project metadata (bundleID, displayName, buildNumber, version)
func openCodeUpdateProjectSettingsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path        string  `json:"path"`
		BundleID    *string `json:"bundleID"`
		DisplayName *string `json:"displayName"`
		BuildNumber *string `json:"buildNumber"`
		Version     *string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	paths := GetProjectPaths(req.Path)
	project, err := LoadProject(paths.Manifest)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if req.BundleID != nil {
		project.BundleID = strings.TrimSpace(*req.BundleID)
	}
	if req.DisplayName != nil {
		project.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.BuildNumber != nil {
		project.BuildNumber = strings.TrimSpace(*req.BuildNumber)
	}
	if req.Version != nil {
		project.Version = strings.TrimSpace(*req.Version)
	}

	if err := SaveProject(paths.Manifest, project); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"project": project,
	})
}

// openCodeGenerateIconHandler generates an app icon and saves it to the project root
func openCodeGenerateIconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path           string  `json:"path"`
		Prompt         string  `json:"prompt"`
		ImageSource    string  `json:"imageSource,omitempty"`
		OpenAIKey      string  `json:"openaiKey,omitempty"`
		GeminiKey      string  `json:"geminiKey,omitempty"`
		XaiKey         string  `json:"xaiKey,omitempty"`
		ReferenceImage *string `json:"referenceImage,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" || req.Prompt == "" {
		http.Error(w, "path and prompt are required", http.StatusBadRequest)
		return
	}

	// Build a post-pass style request to reuse generateImageForPostPass
	postPassReq := OpenCodeMediaPostPassRequest{
		ProjectPath: req.Path,
		ImageSource: req.ImageSource,
		OpenAIKey:   req.OpenAIKey,
		GeminiKey:   req.GeminiKey,
		XaiKey:      req.XaiKey,
	}

	refImage := ""
	if req.ReferenceImage != nil {
		refImage = *req.ReferenceImage
		// Strip data URI prefix — Gemini API expects raw base64
		if idx := strings.Index(refImage, ";base64,"); idx >= 0 {
			refImage = refImage[idx+len(";base64,"):]
		}
	}

	// Enhance prompt for app icon generation
	iconPrompt := fmt.Sprintf("Generate a square app icon (1024x1024) for: %s. The icon should be simple, modern, and look great at small sizes. No text on the icon.", req.Prompt)

	dataURI, sourceService, err := generateImageForPostPass(postPassReq, iconPrompt, refImage)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("icon generation failed: %v", err),
		})
		return
	}

	// Decode data URI to bytes
	imageBytes, _, err := decodeBase64Payload(dataURI, "image/png")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("failed to decode generated image: %v", err),
		})
		return
	}

	// Write icon.png to project root
	iconPath := filepath.Join(req.Path, "icon.png")
	if err := os.WriteFile(iconPath, imageBytes, 0644); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("failed to save icon: %v", err),
		})
		return
	}

	fmt.Printf("[ICON] Generated icon for project %s using %s\n", req.Path, sourceService)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"iconPath":      iconPath,
		"sourceService": sourceService,
		"image":         dataURI,
	})
}

// openCodeProjectIconHandler serves the current icon.png as a base64 data URI
func openCodeProjectIconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}

	iconPath := filepath.Join(path, "icon.png")
	data, err := os.ReadFile(iconPath)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"exists":  false,
		})
		return
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	dataURI := fmt.Sprintf("data:image/png;base64,%s", encoded)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"exists":  true,
		"image":   dataURI,
	})
}

type openCodeOpenAIOAuthCredential struct {
	AccessToken               string
	RefreshToken              string
	ExpiresAtReferenceSeconds float64
	Source                    string
}

func parseRawJSONNumber(raw json.RawMessage) float64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0
	}

	var asNumber float64
	if err := json.Unmarshal(trimmed, &asNumber); err == nil {
		return asNumber
	}

	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			return 0
		}
		parsed, err := strconv.ParseFloat(asString, 64)
		if err == nil {
			return parsed
		}
	}

	return 0
}

func normalizeOAuthExpiresToReferenceSeconds(raw float64) float64 {
	if raw <= 0 || !isFinite(raw) {
		return 0
	}

	// Unix milliseconds.
	if raw > 1_000_000_000_000 {
		return (raw / 1000.0) - 978307200
	}
	// Unix seconds.
	if raw > 978307200 {
		return raw - 978307200
	}
	// Already NSDate reference seconds.
	return raw
}

func isFinite(v float64) bool {
	return !math.IsInf(v, 0) && !math.IsNaN(v)
}

func readOpenAIOAuthCredentialFromOpenCodeAuthFile(authFilePath string) (openCodeOpenAIOAuthCredential, bool, error) {
	trimmedPath := strings.TrimSpace(authFilePath)
	if trimmedPath == "" {
		return openCodeOpenAIOAuthCredential{}, false, nil
	}

	data, err := os.ReadFile(trimmedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return openCodeOpenAIOAuthCredential{}, false, nil
		}
		return openCodeOpenAIOAuthCredential{}, false, err
	}

	var authMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &authMap); err != nil {
		return openCodeOpenAIOAuthCredential{}, false, err
	}

	raw, ok := authMap["openai"]
	if !ok {
		return openCodeOpenAIOAuthCredential{}, false, nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return openCodeOpenAIOAuthCredential{}, false, nil
	}

	var openAI struct {
		Type    string          `json:"type"`
		Access  string          `json:"access"`
		Refresh string          `json:"refresh"`
		Expires json.RawMessage `json:"expires"`
	}
	if err := json.Unmarshal(raw, &openAI); err != nil {
		return openCodeOpenAIOAuthCredential{}, false, err
	}

	if !strings.EqualFold(strings.TrimSpace(openAI.Type), "oauth") {
		return openCodeOpenAIOAuthCredential{}, false, nil
	}

	access := strings.TrimSpace(openAI.Access)
	if access == "" {
		return openCodeOpenAIOAuthCredential{}, false, nil
	}

	return openCodeOpenAIOAuthCredential{
		AccessToken:               access,
		RefreshToken:              strings.TrimSpace(openAI.Refresh),
		ExpiresAtReferenceSeconds: normalizeOAuthExpiresToReferenceSeconds(parseRawJSONNumber(openAI.Expires)),
		Source:                    trimmedPath,
	}, true, nil
}

func readOpenAIOAuthCredentialFromCodexAuthFile() (openCodeOpenAIOAuthCredential, bool, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return openCodeOpenAIOAuthCredential{}, false, err
	}
	codexAuthPath := filepath.Join(homeDir, ".codex", "auth.json")

	data, err := os.ReadFile(codexAuthPath)
	if err != nil {
		if os.IsNotExist(err) {
			return openCodeOpenAIOAuthCredential{}, false, nil
		}
		return openCodeOpenAIOAuthCredential{}, false, err
	}

	var decoded struct {
		Tokens *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    any    `json:"expires_at"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return openCodeOpenAIOAuthCredential{}, false, err
	}

	if decoded.Tokens == nil {
		return openCodeOpenAIOAuthCredential{}, false, nil
	}

	access := strings.TrimSpace(decoded.Tokens.AccessToken)
	if access == "" {
		return openCodeOpenAIOAuthCredential{}, false, nil
	}

	expiresAt := 0.0
	switch value := decoded.Tokens.ExpiresAt.(type) {
	case float64:
		expiresAt = normalizeOAuthExpiresToReferenceSeconds(value)
	case string:
		parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if parseErr == nil {
			expiresAt = normalizeOAuthExpiresToReferenceSeconds(parsed)
		}
	}

	return openCodeOpenAIOAuthCredential{
		AccessToken:               access,
		RefreshToken:              strings.TrimSpace(decoded.Tokens.RefreshToken),
		ExpiresAtReferenceSeconds: expiresAt,
		Source:                    codexAuthPath,
	}, true, nil
}

func resolveOpenAIOAuthCredentialForConnect(req OpenCodeAuthConnectRequest) (openCodeOpenAIOAuthCredential, error) {
	access := strings.TrimSpace(req.OpenAIKey)
	refresh := strings.TrimSpace(req.OpenAIRefreshToken)
	expiresAt := normalizeOAuthExpiresToReferenceSeconds(req.OpenAIExpiresAt)

	if access != "" {
		return openCodeOpenAIOAuthCredential{
			AccessToken:               access,
			RefreshToken:              refresh,
			ExpiresAtReferenceSeconds: expiresAt,
			Source:                    "request",
		}, nil
	}

	runtimePaths, err := glowbomOpenCodeRuntimePaths()
	if err == nil {
		fromOpenCodeAuth, ok, readErr := readOpenAIOAuthCredentialFromOpenCodeAuthFile(runtimePaths.AuthFile)
		if readErr != nil {
			log.Printf("[OPENCODE] Warning: failed reading OpenCode auth file for connect fallback: %v", readErr)
		}
		if ok {
			return fromOpenCodeAuth, nil
		}
	}

	fromCodexAuth, ok, readErr := readOpenAIOAuthCredentialFromCodexAuthFile()
	if readErr != nil {
		log.Printf("[OPENCODE] Warning: failed reading ~/.codex/auth.json for connect fallback: %v", readErr)
	}
	if ok {
		return fromCodexAuth, nil
	}

	return openCodeOpenAIOAuthCredential{}, fmt.Errorf("no ChatGPT OAuth credential found. Run `codex login` first, or connect ChatGPT in Desktop")
}

func clearOpenAIAuthFromAuthFile(authFilePath string) error {
	trimmedPath := strings.TrimSpace(authFilePath)
	if trimmedPath == "" {
		return nil
	}

	data, err := os.ReadFile(trimmedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var authMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &authMap); err != nil {
		return err
	}

	if _, exists := authMap["openai"]; !exists {
		return nil
	}

	delete(authMap, "openai")
	encoded, err := json.MarshalIndent(authMap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, encoded, 0600)
}

func persistOpenAIOAuthToAuthFile(authFilePath string, credential openCodeOpenAIOAuthCredential) error {
	trimmedPath := strings.TrimSpace(authFilePath)
	if trimmedPath == "" {
		return nil
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return fmt.Errorf("missing OAuth access token")
	}

	authMap := map[string]json.RawMessage{}
	data, err := os.ReadFile(trimmedPath)
	if err == nil {
		if unmarshalErr := json.Unmarshal(data, &authMap); unmarshalErr != nil {
			return unmarshalErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	expiresAtReference := credential.ExpiresAtReferenceSeconds
	if expiresAtReference <= 0 {
		expiresAtReference = float64(time.Now().Add(time.Hour).Unix()) - 978307200
	}
	expiresAtUnixMs := int64((expiresAtReference + 978307200) * 1000)

	openAIPayload := map[string]any{
		"type":    "oauth",
		"access":  credential.AccessToken,
		"refresh": strings.TrimSpace(credential.RefreshToken),
		"expires": expiresAtUnixMs,
	}
	if openAIPayload["refresh"] == "" {
		openAIPayload["refresh"] = "none"
	}

	encodedOpenAI, err := json.Marshal(openAIPayload)
	if err != nil {
		return err
	}
	authMap["openai"] = encodedOpenAI

	encoded, err := json.MarshalIndent(authMap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, encoded, 0600)
}

func openAICredentialTypeFromAuthFile(authFilePath string) string {
	if strings.TrimSpace(authFilePath) == "" {
		return "unknown"
	}

	data, err := os.ReadFile(authFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "none"
		}
		log.Printf("[OPENCODE] Warning: failed reading auth file for diagnostics: %v", err)
		return "unknown"
	}

	type authTypeProbe struct {
		Type string `json:"type"`
	}
	var authMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &authMap); err != nil {
		log.Printf("[OPENCODE] Warning: failed parsing auth file for diagnostics: %v", err)
		return "unknown"
	}

	raw, ok := authMap["openai"]
	if !ok {
		return "none"
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "none"
	}

	var probe authTypeProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		log.Printf("[OPENCODE] Warning: failed parsing OpenAI auth payload for diagnostics: %v", err)
		return "unknown"
	}

	switch strings.ToLower(strings.TrimSpace(probe.Type)) {
	case "oauth":
		return "oauth"
	case "api":
		return "api"
	case "":
		return "unknown"
	default:
		return "unknown"
	}
}

func cachedGlowbomOpenAIAuthMode() string {
	state := getOpenCodeOpenAIAuthState()
	if !state.known {
		return "unknown"
	}
	mode := normalizeOpenAIAuthMode(state.mode)
	if mode == "" {
		return "unknown"
	}
	return mode
}

func currentOpenCodeAuthStatus() openCodeAuthStatusResponse {
	glowbomPaths, err := glowbomOpenCodeRuntimePaths()
	if err != nil {
		log.Printf("[OPENCODE] Warning: failed resolving runtime paths for auth diagnostics: %v", err)
	}
	userPaths, userPathsErr := userOpenCodeRuntimePaths()
	if userPathsErr != nil {
		log.Printf("[OPENCODE] Warning: failed resolving user OpenCode paths for auth diagnostics: %v", userPathsErr)
		userPaths = glowbomPaths
	}
	authMode := cachedGlowbomOpenAIAuthMode()
	runtimePaths := authStatusRuntimePaths(authMode, glowbomPaths, userPaths)

	serverURL := "http://" + openCodeServerHostname() + ":" + getAgentPort()
	return openCodeAuthStatusResponse{
		ServerRunning:         isServerRunning(serverURL),
		ConfiguredDataHome:    runtimePaths.DataHome,
		ConfiguredStateHome:   runtimePaths.StateHome,
		AuthFilePath:          runtimePaths.AuthFile,
		OpenAICredentialType:  openAICredentialTypeFromAuthFile(runtimePaths.AuthFile),
		CachedGlowbomAuthMode: authMode,
	}
}

func openCodeAuthStatusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	json.NewEncoder(w).Encode(currentOpenCodeAuthStatus())
}

func openCodeOpenAIOAuthStartHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeAuthOAuthStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	redirectURI := openAIOAuthRedirectURI()
	if strings.EqualFold(strings.TrimSpace(redirectURI), openAIOAuthDefaultRedirectURI) {
		if err := ensureOpenAIOAuthLoopbackServer(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	codeVerifier, err := randomURLSafeBase64(32)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed creating OAuth verifier: %v", err), http.StatusInternalServerError)
		return
	}
	state, err := randomURLSafeBase64(32)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed creating OAuth state: %v", err), http.StatusInternalServerError)
		return
	}

	params := neturl.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", openAIOAuthClientID())
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", "openid profile email offline_access")
	params.Set("code_challenge", sha256Base64URL(codeVerifier))
	params.Set("code_challenge_method", "S256")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	params.Set("state", state)
	params.Set("originator", openAIOAuthOriginator())

	authorizationURL := openAIOAuthIssuer + "/oauth/authorize?" + params.Encode()

	ensureOpenAIOAuthSessionStore()
	now := time.Now()
	openCodeOpenAIOAuthSessions.mu.Lock()
	cleanupExpiredOpenAIOAuthSessionsLocked(now)
	openCodeOpenAIOAuthSessions.sessions[state] = &openCodeOpenAIOAuthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		RedirectURI:  redirectURI,
		ProjectPath:  strings.TrimSpace(req.ProjectPath),
		CreatedAt:    now,
		Phase:        "pending",
		Connected:    false,
		Error:        "",
		Status:       currentOpenCodeAuthStatus(),
	}
	openCodeOpenAIOAuthSessions.mu.Unlock()

	writeJSON(w, OpenCodeAuthOAuthStartResponse{
		Success:          true,
		Provider:         "chatgpt",
		State:            state,
		AuthorizationURL: authorizationURL,
		RedirectURI:      redirectURI,
	})
}

func openCodeOpenAIOAuthStatusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		http.Error(w, "state query parameter required", http.StatusBadRequest)
		return
	}

	ensureOpenAIOAuthSessionStore()
	openCodeOpenAIOAuthSessions.mu.Lock()
	cleanupExpiredOpenAIOAuthSessionsLocked(time.Now())
	session := openCodeOpenAIOAuthSessions.sessions[state]
	var sessionCopy openCodeOpenAIOAuthSession
	if session != nil {
		sessionCopy = *session
	}
	openCodeOpenAIOAuthSessions.mu.Unlock()

	if session == nil {
		http.Error(w, "OAuth session not found or expired", http.StatusNotFound)
		return
	}

	writeJSON(w, OpenCodeAuthOAuthStatusResponse{
		Success:   sessionCopy.Phase != "failed",
		Provider:  "chatgpt",
		State:     sessionCopy.State,
		Phase:     sessionCopy.Phase,
		Connected: sessionCopy.Connected,
		Status:    sessionCopy.Status,
		Error:     sessionCopy.Error,
	})
}

func setOpenAIOAuthSessionFailed(state, message string) {
	openCodeOpenAIOAuthSessions.mu.Lock()
	if target := openCodeOpenAIOAuthSessions.sessions[state]; target != nil {
		target.Phase = "failed"
		target.Connected = false
		target.Error = strings.TrimSpace(message)
		target.CompletedAt = time.Now()
		target.Status = currentOpenCodeAuthStatus()
	}
	openCodeOpenAIOAuthSessions.mu.Unlock()
}

func setOpenAIOAuthSessionSucceeded(state string, status openCodeAuthStatusResponse) {
	openCodeOpenAIOAuthSessions.mu.Lock()
	if target := openCodeOpenAIOAuthSessions.sessions[state]; target != nil {
		target.Phase = "succeeded"
		target.Connected = status.OpenAICredentialType == "oauth" || status.CachedGlowbomAuthMode == "codex-jwt"
		target.Error = ""
		target.CompletedAt = time.Now()
		target.Status = status
	}
	openCodeOpenAIOAuthSessions.mu.Unlock()
}

func finalizeOpenAIOAuthCallback(state, code, oauthError, oauthErrorDescription string) (bool, string) {
	if state == "" {
		return false, "Missing OAuth state. Please restart login from Glowbom OSS."
	}

	ensureOpenAIOAuthSessionStore()
	openCodeOpenAIOAuthSessions.mu.Lock()
	cleanupExpiredOpenAIOAuthSessionsLocked(time.Now())
	session := openCodeOpenAIOAuthSessions.sessions[state]
	if session == nil {
		openCodeOpenAIOAuthSessions.mu.Unlock()
		return false, "OAuth session expired. Restart login from Glowbom OSS."
	}

	if session.Phase == "succeeded" {
		openCodeOpenAIOAuthSessions.mu.Unlock()
		return true, "ChatGPT account is already connected."
	}
	if session.Phase == "failed" {
		message := strings.TrimSpace(session.Error)
		openCodeOpenAIOAuthSessions.mu.Unlock()
		if message == "" {
			message = "OAuth session failed. Please restart login."
		}
		return false, message
	}

	codeVerifier := session.CodeVerifier
	redirectURI := session.RedirectURI
	projectPath := session.ProjectPath
	openCodeOpenAIOAuthSessions.mu.Unlock()

	if oauthError != "" {
		message := oauthError
		if oauthErrorDescription != "" {
			message += ": " + oauthErrorDescription
		}
		setOpenAIOAuthSessionFailed(state, message)
		return false, "Login was canceled or rejected. " + message
	}

	if code == "" {
		setOpenAIOAuthSessionFailed(state, "Missing OAuth authorization code")
		return false, "Missing authorization code from callback."
	}

	credential, err := exchangeOpenAIOAuthCodeForCredential(code, codeVerifier, redirectURI)
	if err != nil {
		setOpenAIOAuthSessionFailed(state, err.Error())
		return false, "Could not complete ChatGPT login. " + err.Error()
	}

	if err := ensureOpenCodeServerReady(
		projectPath,
		"",
		credential.AccessToken,
		"",
		"",
		"",
		"",
		"",
		"",
		"codex-jwt",
		credential.RefreshToken,
		credential.ExpiresAtReferenceSeconds,
	); err != nil {
		setOpenAIOAuthSessionFailed(state, err.Error())
		return false, "ChatGPT login completed, but backend sync failed. " + err.Error()
	}

	if runtimePaths, err := glowbomOpenCodeRuntimePaths(); err == nil {
		if persistErr := persistOpenAIOAuthToAuthFile(runtimePaths.AuthFile, credential); persistErr != nil {
			log.Printf("[OPENCODE] Warning: failed persisting OpenAI OAuth credential from callback: %v", persistErr)
		}
	}

	status := currentOpenCodeAuthStatus()
	setOpenAIOAuthSessionSucceeded(state, status)
	return true, "ChatGPT account is now connected."
}

func openCodeOpenAIOAuthCallbackHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	success, message := finalizeOpenAIOAuthCallback(
		strings.TrimSpace(r.URL.Query().Get("state")),
		strings.TrimSpace(r.URL.Query().Get("code")),
		strings.TrimSpace(r.URL.Query().Get("error")),
		strings.TrimSpace(r.URL.Query().Get("error_description")),
	)
	writeOpenAIOAuthCallbackHTML(w, success, message)
}

func openCodeOpenAIConnectHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeAuthConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	resolvedCredential, err := resolveOpenAIOAuthCredentialForConnect(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := ensureOpenCodeServerReady(
		strings.TrimSpace(req.ProjectPath),
		"",
		resolvedCredential.AccessToken,
		"",
		"",
		"",
		"",
		"",
		"",
		"codex-jwt",
		resolvedCredential.RefreshToken,
		resolvedCredential.ExpiresAtReferenceSeconds,
	); err != nil {
		http.Error(w, fmt.Sprintf("Failed to connect ChatGPT auth: %v", err), http.StatusInternalServerError)
		return
	}

	if runtimePaths, err := glowbomOpenCodeRuntimePaths(); err == nil {
		if persistErr := persistOpenAIOAuthToAuthFile(runtimePaths.AuthFile, resolvedCredential); persistErr != nil {
			log.Printf("[OPENCODE] Warning: failed persisting OpenAI OAuth credential: %v", persistErr)
		}
	}

	writeJSON(w, OpenCodeAuthConnectionResponse{
		Success:   true,
		Provider:  "chatgpt",
		Connected: true,
		Status:    currentOpenCodeAuthStatus(),
	})
}

func openCodeOpenAIDisconnectHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeAuthDisconnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := ensureOpenCodeServerReady(
		strings.TrimSpace(req.ProjectPath),
		"",
		"",
		"",
		"",
		"",
		"",
		"",
		"",
		"api-key",
		"",
		0,
	); err != nil {
		http.Error(w, fmt.Sprintf("Failed to disconnect ChatGPT auth: %v", err), http.StatusInternalServerError)
		return
	}

	if runtimePaths, err := glowbomOpenCodeRuntimePaths(); err == nil {
		if clearErr := clearOpenAIAuthFromAuthFile(runtimePaths.AuthFile); clearErr != nil {
			log.Printf("[OPENCODE] Warning: failed clearing OpenAI auth file after disconnect: %v", clearErr)
		}
	}

	writeJSON(w, OpenCodeAuthConnectionResponse{
		Success:   true,
		Provider:  "chatgpt",
		Connected: false,
		Status:    currentOpenCodeAuthStatus(),
	})
}

// openCodeHealthHandler checks if OpenCode server is running
func openCodeHealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Query().Get("agentDriver") == "cursor" {
		cursorHealthHandler(w, r)
		return
	}

	driver := GetOpenCodeDriver()
	ctx := r.Context()

	if err := driver.CheckHealth(ctx); err != nil {
		// The OSS UI uses the user's OpenCode configuration by default.
		log.Printf("[OPENCODE] Server unhealthy, attempting auto-start...")
		if startErr := startOpenCodeServer(os.Getenv("OPENAI_API_KEY"), os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("GEMINI_API_KEY"), os.Getenv("FIREWORKS_API_KEY"), os.Getenv("OPENROUTER_API_KEY"), os.Getenv("OPENCODE_API_KEY"), os.Getenv("XAI_API_KEY"), "", "opencode-config"); startErr != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"healthy": false,
				"error":   err.Error(),
				"hint":    "Failed to auto-start server. Install OpenCode CLI and complete OpenCode setup.",
			})
			return
		}

		// Wait and retry health check
		time.Sleep(3 * time.Second)
		if err := driver.CheckHealth(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"healthy": false,
				"error":   err.Error(),
				"hint":    "Server started but not responding. Check logs.",
			})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"healthy": true,
		"server":  driver.serverURL,
	})
}

func openAIModelsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenAIModelsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.OpenAIAuthMode) == "" {
		req.OpenAIAuthMode = "api-key"
	}

	if normalizeOpenAIAuthMode(req.OpenAIAuthMode) == "codex-jwt" && strings.TrimSpace(req.OpenAIKey) == "" {
		resolvedCredential, err := resolveOpenAIOAuthCredentialForConnect(OpenCodeAuthConnectRequest{
			ProjectPath:        req.ProjectPath,
			OpenAIKey:          req.OpenAIKey,
			OpenAIRefreshToken: req.OpenAIRefreshToken,
			OpenAIExpiresAt:    req.OpenAIExpiresAt,
		})
		if err != nil {
			http.Error(w, "ChatGPT account is not connected. Connect first.", http.StatusBadRequest)
			return
		}
		req.OpenAIKey = resolvedCredential.AccessToken
		if strings.TrimSpace(req.OpenAIRefreshToken) == "" {
			req.OpenAIRefreshToken = resolvedCredential.RefreshToken
		}
		if req.OpenAIExpiresAt <= 0 {
			req.OpenAIExpiresAt = resolvedCredential.ExpiresAtReferenceSeconds
		}
	}

	if err := ensureOpenCodeServerReady(req.ProjectPath, "", req.OpenAIKey, req.AnthropicKey, req.GeminiKey, "", "", "", "", req.OpenAIAuthMode, req.OpenAIRefreshToken, req.OpenAIExpiresAt); err != nil {
		http.Error(w, fmt.Sprintf("Failed to prepare OpenCode server: %v", err), http.StatusInternalServerError)
		return
	}

	driver := GetOpenCodeDriver()
	params := opencode.AppProvidersParams{}
	if dir := strings.TrimSpace(req.ProjectPath); dir != "" {
		params.Directory = opencode.F(dir)
	}

	providersResp, err := driver.client.App.Providers(r.Context(), params)
	if err != nil {
		log.Printf("[OPENCODE] Failed to fetch providers: %v", err)
		http.Error(w, fmt.Sprintf("Failed to fetch provider models: %v", err), http.StatusBadGateway)
		return
	}

	var openaiProvider *opencode.Provider
	for i := range providersResp.Providers {
		p := &providersResp.Providers[i]
		if strings.EqualFold(p.ID, "openai") {
			openaiProvider = p
			break
		}
	}

	allowlistModelIDs := make([]string, 0, len(openAIChatGPTModelAllowlist))
	for id := range openAIChatGPTModelAllowlist {
		allowlistModelIDs = append(allowlistModelIDs, id)
	}
	sort.Strings(allowlistModelIDs)

	providerModelIDs := make([]string, 0)
	if openaiProvider != nil {
		providerModelIDs = make([]string, 0, len(openaiProvider.Models))
		for id := range openaiProvider.Models {
			providerModelIDs = append(providerModelIDs, id)
		}
		sort.Strings(providerModelIDs)
	}

	models := make([]OpenAIModelOption, 0, len(openAIChatGPTModelAllowlist))
	if openaiProvider != nil {
		for _, id := range allowlistModelIDs {
			model, ok := openaiProvider.Models[id]
			if !ok {
				continue
			}
			allowName := openAIChatGPTModelAllowlist[id]
			name := strings.TrimSpace(model.Name)
			if name == "" {
				name = allowName
			}
			models = append(models, OpenAIModelOption{
				ID:          id,
				DisplayName: name,
			})
		}
	}

	fallbackUsed := false
	// Fallback to allowlist defaults if provider call returns no matching models.
	if len(models) == 0 {
		fallbackUsed = true
		for _, id := range allowlistModelIDs {
			name := openAIChatGPTModelAllowlist[id]
			models = append(models, OpenAIModelOption{
				ID:          id,
				DisplayName: name,
			})
		}
	}

	models, addedForwardCompatIDs := appendOpenAICodexForwardCompatModels(models, providerModelIDs, req.OpenAIAuthMode)

	matchedModelIDs := make([]string, 0, len(models))
	for _, model := range models {
		matchedModelIDs = append(matchedModelIDs, model.ID)
	}
	sort.Strings(matchedModelIDs)

	sort.Slice(models, func(i, j int) bool {
		if models[i].DisplayName == models[j].DisplayName {
			return models[i].ID < models[j].ID
		}
		return models[i].DisplayName < models[j].DisplayName
	})

	debugInfo := OpenAIModelsDebug{
		AuthMode:              req.OpenAIAuthMode,
		ProviderFound:         openaiProvider != nil,
		ProviderModelCount:    len(providerModelIDs),
		ProviderModelSample:   previewStringList(providerModelIDs, 12),
		AllowlistModelIDs:     allowlistModelIDs,
		MatchedModelIDs:       matchedModelIDs,
		AddedForwardCompatIDs: addedForwardCompatIDs,
		UsedFallbackAllowlist: fallbackUsed,
	}

	log.Printf(
		"[OPENCODE] OpenAI model fetch authMode=%s providerFound=%t providerModelCount=%d matched=%d fallback=%t addedForwardCompat=[%s] providerSample=[%s] matchedIDs=[%s]",
		debugInfo.AuthMode,
		debugInfo.ProviderFound,
		debugInfo.ProviderModelCount,
		len(debugInfo.MatchedModelIDs),
		debugInfo.UsedFallbackAllowlist,
		strings.Join(debugInfo.AddedForwardCompatIDs, ", "),
		strings.Join(debugInfo.ProviderModelSample, ", "),
		strings.Join(debugInfo.MatchedModelIDs, ", "),
	)

	resp := OpenAIModelsResponse{
		Provider:  "openai",
		Source:    "opencode-config/providers",
		Models:    models,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Debug:     debugInfo,
	}

	writeJSON(w, resp)
}

func openCodeAvailableModelsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeAvailableModelsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.OpenAIAuthMode) == "" {
		req.OpenAIAuthMode = "opencode-config"
	}

	if err := ensureOpenCodeServerReady(
		req.ProjectPath,
		"",
		req.OpenAIKey,
		req.AnthropicKey,
		req.GeminiKey,
		req.FireworksKey,
		req.OpenRouterKey,
		req.OpenCodeZenKey,
		req.XaiKey,
		req.OpenAIAuthMode,
		req.OpenAIRefreshToken,
		req.OpenAIExpiresAt,
	); err != nil {
		http.Error(w, fmt.Sprintf("Failed to prepare OpenCode server: %v", err), http.StatusInternalServerError)
		return
	}

	driver := GetOpenCodeDriver()
	params := opencode.AppProvidersParams{}
	if dir := strings.TrimSpace(req.ProjectPath); dir != "" {
		params.Directory = opencode.F(dir)
	}

	providersResp, err := driver.client.App.Providers(r.Context(), params)
	if err != nil {
		log.Printf("[OPENCODE] Failed to fetch providers for available model catalog: %v", err)
		http.Error(w, fmt.Sprintf("Failed to fetch providers: %v", err), http.StatusBadGateway)
		return
	}

	providers := make([]OpenCodeProviderOption, 0, len(providersResp.Providers))
	for _, provider := range providersResp.Providers {
		models := make([]OpenCodeProviderModelOption, 0, len(provider.Models))
		for modelID, model := range provider.Models {
			displayName := strings.TrimSpace(model.Name)
			if displayName == "" {
				displayName = modelID
			}
			models = append(models, OpenCodeProviderModelOption{
				ID:          modelID,
				DisplayName: displayName,
			})
		}

		sort.Slice(models, func(i, j int) bool {
			if models[i].DisplayName == models[j].DisplayName {
				return models[i].ID < models[j].ID
			}
			return models[i].DisplayName < models[j].DisplayName
		})

		displayName := strings.TrimSpace(provider.Name)
		if displayName == "" {
			displayName = provider.ID
		}

		providers = append(providers, OpenCodeProviderOption{
			ID:          provider.ID,
			DisplayName: displayName,
			Models:      models,
		})
	}

	sort.Slice(providers, func(i, j int) bool {
		if providers[i].DisplayName == providers[j].DisplayName {
			return providers[i].ID < providers[j].ID
		}
		return providers[i].DisplayName < providers[j].DisplayName
	})

	writeJSON(w, OpenCodeAvailableModelsResponse{
		Source:    "opencode-config/providers",
		Providers: providers,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// OpenCodeAgentRequest represents a request for agent operations (refine/verify)
type OpenCodeAgentRequest struct {
	AgentDriver                         string   `json:"agentDriver,omitempty"`
	ProjectPath                         string   `json:"projectPath"`
	SessionID                           string   `json:"sessionID,omitempty"`    // Reuse existing session if provided
	Instructions                        string   `json:"instructions,omitempty"` // Optional user instructions
	BuildTargets                        []string `json:"buildTargets,omitempty"` // Omitted by older clients
	PersistCurrentInstructionsToHistory bool     `json:"persistCurrentInstructionsToHistory,omitempty"`
	InstructionAttachmentPaths          []string `json:"instructionAttachmentPaths,omitempty"`
	Model                               string   `json:"model,omitempty"`
	AnthropicKey                        string   `json:"anthropicKey,omitempty"`
	OpenAIKey                           string   `json:"openaiKey,omitempty"`
	OpenAIImageKey                      string   `json:"openaiImageKey,omitempty"` // plain API key for image gen (codex-jwt lacks image scope)
	GeminiKey                           string   `json:"geminiKey,omitempty"`
	GeminiImageKey                      string   `json:"geminiImageKey,omitempty"`
	FireworksKey                        string   `json:"fireworksKey,omitempty"`
	OpenRouterKey                       string   `json:"openrouterKey,omitempty"`
	OpenCodeZenKey                      string   `json:"opencodeZenKey,omitempty"`
	XaiKey                              string   `json:"xaiKey,omitempty"`
	XaiImageKey                         string   `json:"xaiImageKey,omitempty"`
	VeoGeminiKey                        string   `json:"veoGeminiKey,omitempty"`
	ElevenLabsKey                       string   `json:"elevenLabsKey,omitempty"`
	ElevenLabsVoiceID                   string   `json:"elevenLabsVoiceID,omitempty"`
	ElevenLabsVoiceModel                string   `json:"elevenLabsVoiceModel,omitempty"`
	ImageSource                         string   `json:"imageSource,omitempty"`
	ReferenceImagePath                  string   `json:"referenceImagePath,omitempty"`
	ReferenceAssetID                    string   `json:"referenceAssetID,omitempty"`
	MediaGenerationPolicy               string   `json:"mediaGenerationPolicy,omitempty"` // "auto" | "ask" | "skip"
	OpenAIAuthMode                      string   `json:"openaiAuthMode,omitempty"`        // "api-key" | "codex-jwt" | "opencode-config"
	OpenAIAccountID                     string   `json:"openaiAccountID,omitempty"`       // chatgpt_account_id for JWT mode
	OpenAIRefreshToken                  string   `json:"openaiRefreshToken,omitempty"`    // refresh token for OpenCode auth sync
	OpenAIExpiresAt                     float64  `json:"openaiExpiresAt,omitempty"`       // token expiry (seconds since reference date)
}

func resolveAgentImageProviderKeys(req OpenCodeAgentRequest) (string, string, string) {
	openAIKey := strings.TrimSpace(req.OpenAIImageKey)
	if openAIKey == "" {
		openAIKey = strings.TrimSpace(req.OpenAIKey)
	}

	geminiKey := strings.TrimSpace(req.GeminiImageKey)
	if geminiKey == "" {
		geminiKey = strings.TrimSpace(req.GeminiKey)
	}

	xaiKey := strings.TrimSpace(req.XaiImageKey)
	if xaiKey == "" {
		xaiKey = strings.TrimSpace(req.XaiKey)
	}

	return openAIKey, geminiKey, xaiKey
}

// openCodeRefineHandler handles refine requests - uses agent to improve code
func openCodeRefineHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("[OPENCODE] Received refine request")
	if r.Method != http.MethodPost {
		log.Printf("[OPENCODE] Error: Method not allowed: %s", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[OPENCODE] Error: Invalid JSON body: %v", err)
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.ProjectPath == "" {
		log.Printf("[OPENCODE] Error: projectPath is required")
		http.Error(w, "projectPath is required", http.StatusBadRequest)
		return
	}
	if req.AgentDriver != "" && req.AgentDriver != "opencode" && req.AgentDriver != "cursor" {
		http.Error(w, "Unknown agent driver", http.StatusBadRequest)
		return
	}
	if req.AgentDriver == "cursor" {
		if _, err := cursorExecutable(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}

	// Verify project exists
	paths := GetProjectPaths(req.ProjectPath)
	if _, err := os.Stat(paths.Manifest); os.IsNotExist(err) {
		log.Printf("[OPENCODE] Error: Not a Glowbom project at %s", req.ProjectPath)
		http.Error(w, "Not a Glowbom project (missing glowbom.json)", http.StatusBadRequest)
		return
	}

	preparedInstructions, err := prepareStackBuildInstructions(req.ProjectPath, req.Instructions, req.BuildTargets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Instructions = preparedInstructions
	trimmedInstructions := strings.TrimSpace(req.Instructions)
	shouldPrepareCurrentInstructions := trimmedInstructions != "" || hasAnyInstructionAttachmentPath(req.InstructionAttachmentPaths)
	if shouldPrepareCurrentInstructions {
		if err := resetCurrentInstructionsDirectory(req.ProjectPath); err != nil {
			log.Printf("[OPENCODE] Error: failed preparing current_instructions: %v", err)
			http.Error(w, fmt.Sprintf("Failed to prepare current_instructions: %v", err), http.StatusInternalServerError)
			return
		}
	}

	stagedInstructionAttachments, err := stageInstructionAttachments(req.ProjectPath, req.InstructionAttachmentPaths)
	if err != nil {
		log.Printf("[OPENCODE] Error: invalid instruction attachments: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stagedInstructionPath, err := stageInstructionTextFile(req.ProjectPath, trimmedInstructions)
	if err != nil {
		log.Printf("[OPENCODE] Error: invalid instructions file staging: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	effectiveInstructions := req.Instructions
	if stagedInstructionPath != "" || len(stagedInstructionAttachments) > 0 {
		effectiveInstructions = mergeInstructionAttachmentContext(req.Instructions, stagedInstructionPath, stagedInstructionAttachments)
	}

	shouldPersistCurrentInstructionsHistory := req.PersistCurrentInstructionsToHistory && shouldPrepareCurrentInstructions
	historyStatus := "failed"
	historySummary := ""
	if shouldPersistCurrentInstructionsHistory {
		defer func() {
			if err := persistCurrentInstructionsHistory(req.ProjectPath, trimmedInstructions, historyStatus, historySummary); err != nil {
				log.Printf("[OPENCODE] Warning: failed archiving current_instructions to history: %v", err)
			}
		}()
	}
	if req.AgentDriver == "cursor" {
		historyStatus, historySummary = runCursorRefine(w, r, req, effectiveInstructions)
		return
	}
	if strings.HasPrefix(req.SessionID, "cursor-") {
		req.SessionID = ""
	}

	if err := ensureOpenCodeServerReady(req.ProjectPath, req.Model, req.OpenAIKey, req.AnthropicKey, req.GeminiKey, req.FireworksKey, req.OpenRouterKey, req.OpenCodeZenKey, req.XaiKey, req.OpenAIAuthMode, req.OpenAIRefreshToken, req.OpenAIExpiresAt); err != nil {
		log.Printf("[OPENCODE] Failed preparing OpenCode server: %v", err)
		historySummary = "Failed to prepare OpenCode server."
		http.Error(w, fmt.Sprintf("Failed to prepare OpenCode server: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[OPENCODE] Starting refinement for project: %s", req.ProjectPath)
	if req.Instructions != "" {
		log.Printf("[OPENCODE] Custom instructions provided (%d chars)", len(req.Instructions))
	} else {
		log.Printf("[OPENCODE] No custom instructions provided")
	}

	driver := GetOpenCodeDriver()
	ctx := r.Context()

	// Set up SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[OPENCODE] Error: Streaming not supported")
		historySummary = "Streaming not supported by HTTP response writer."
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	sendSSEData(w, flusher, map[string]interface{}{"output": "Starting refinement with OpenCode agent..."})
	if req.Instructions != "" {
		sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("📋 Custom instructions: %s", req.Instructions)})
	}
	if stagedInstructionPath != "" {
		sendSSEData(w, flusher, map[string]interface{}{
			"output": fmt.Sprintf("📝 Staged: %s", stagedInstructionPath),
		})
	}
	for _, attachment := range stagedInstructionAttachments {
		sendSSEData(w, flusher, map[string]interface{}{
			"output": fmt.Sprintf("📎 Attached: %s (%s)", attachment.RelativePath, humanReadableBytes(attachment.SizeBytes)),
		})
	}

	// Reuse existing session or create a new one
	session, reusedSession, err := driver.resolveRefineSession(ctx, req.ProjectPath, req.SessionID)
	if err != nil {
		log.Printf("[OPENCODE] Error: Failed to prepare session: %v", err)
		historySummary = userFacingAgentErrorMessage(err.Error())
		sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": userFacingAgentErrorMessage(err.Error())})
		return
	}
	if reusedSession {
		log.Printf("[OPENCODE] Reusing existing session: %s", session.ID)
		sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("Session resumed: %s", session.ID)})
	} else {
		if strings.TrimSpace(req.SessionID) != "" {
			sendSSEData(w, flusher, map[string]interface{}{"output": "Previous session was unavailable. Starting a new session."})
		}
		sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("Session created: %s", session.ID)})
		log.Printf("[OPENCODE] Session created for refinement: %s", session.ID)
	}

	// Build refine prompt
	prompt := buildRefinePrompt(req.ProjectPath, effectiveInstructions)
	log.Printf("[OPENCODE] Built refine prompt (%d chars)", len(prompt))

	authMode := normalizeOpenAIAuthMode(req.OpenAIAuthMode)
	requestedModel := strings.TrimSpace(req.Model)
	useConfiguredDefaultModel := requestedModel == "" && authMode == "opencode-config"

	modelID := ""
	providerID := ""
	fallbackNote := ""
	forwardCompatRegistryWarning := ""

	if useConfiguredDefaultModel {
		sendSSEData(w, flusher, map[string]interface{}{"output": "Using OpenCode configured default model."})
		log.Printf("[OPENCODE] Using OpenCode configured default model for refinement")
	} else {
		// Determine model
		modelID, providerID, fallbackNote = resolveSessionModelForRequest(req.ProjectPath, req.Model)
		sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("Using model: %s/%s", providerID, modelID)})
		log.Printf("[OPENCODE] Using model for refinement: %s/%s", providerID, modelID)
		if fallbackNote != "" {
			sendSSEData(w, flusher, map[string]interface{}{"output": "⚠️ " + fallbackNote})
		}
		if strings.EqualFold(providerID, "openai") && isOpenAICodexForwardCompatModelID(modelID) {
			probe, probeErr := probeOpenCodeProviderModel(req.ProjectPath, providerID, modelID)
			if probeErr != nil {
				log.Printf("[OPENCODE] Warning: failed probing provider registry for %s/%s: %v", providerID, modelID, probeErr)
			} else if probe.ProviderFound && !probe.ModelRegistered {
				forwardCompatRegistryWarning = fmt.Sprintf("⚠️  OpenCode registry does not list %s yet; using forward-compat template. If this run no-ops, update OpenCode or fall back to openai/gpt-5.3-codex.", modelID)
				sendSSEData(w, flusher, map[string]interface{}{"output": forwardCompatRegistryWarning})
				log.Printf("[OPENCODE] Forward-compat registry miss for %s/%s providerSample=[%s]", providerID, modelID, strings.Join(probe.ProviderModelSample, ", "))
			}
		}
	}

	// Capture a pre-run snapshot so we can recover changed files when stream file events are missing.
	preRunSnapshot, snapshotErr := captureProjectFileSnapshot(req.ProjectPath)
	if snapshotErr != nil {
		log.Printf("[OPENCODE] Warning: Failed to capture pre-run snapshot: %v", snapshotErr)
	}

	// Start event stream in background BEFORE sending prompt to avoid race condition
	log.Printf("[OPENCODE] Starting event stream listener...")
	type sessionStreamResult struct {
		completed    bool
		changedFiles []string
		hadActivity  bool
		errorMessage string
	}
	eventStreamDone := make(chan sessionStreamResult, 1)
	promptDispatched := make(chan struct{})

	go func() {
		completed, changedFiles, hadActivity, errorMessage := driver.streamEventsAndWaitForCompletion(ctx, w, flusher, req.ProjectPath, session.ID, promptDispatched)
		eventStreamDone <- sessionStreamResult{
			completed:    completed,
			changedFiles: changedFiles,
			hadActivity:  hadActivity,
			errorMessage: errorMessage,
		}
	}()

	// Small delay to ensure event stream connects first
	time.Sleep(100 * time.Millisecond)

	// Now send the prompt (this returns immediately, agent works asynchronously)
	log.Printf("[OPENCODE] Sending refine prompt")
	sendSSEData(w, flusher, map[string]interface{}{"output": "Agent is analyzing and refining the project..."})

	promptParams := opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Type: opencode.F(opencode.TextPartInputTypeText),
				Text: opencode.F(prompt),
			},
		}),
		Directory: opencode.F(req.ProjectPath),
	}
	if !useConfiguredDefaultModel {
		promptParams.Model = opencode.F(opencode.SessionPromptParamsModel{
			ModelID:    opencode.F(modelID),
			ProviderID: opencode.F(providerID),
		})
	}

	_, err = driver.sendSessionPrompt(ctx, session.ID, promptParams)

	if err != nil {
		log.Printf("[OPENCODE] Error: Refine failed: %v", err)
		historySummary = userFacingAgentErrorMessage(err.Error())
		if isUsageLimitErrorMessage(err.Error()) {
			go driver.abortSessionBestEffort(session.ID, req.ProjectPath, "usage limit while sending refine prompt")
		}
		sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": userFacingAgentErrorMessage(err.Error())})
		return
	}
	close(promptDispatched)

	log.Printf("[OPENCODE] Prompt sent, agent is now working...")

	// Wait for event stream goroutine to complete
	streamResult := <-eventStreamDone
	sessionCompleted := streamResult.completed
	changedFiles := streamResult.changedFiles
	sessionHadActivity := streamResult.hadActivity

	if !sessionCompleted {
		sessionErr := userFacingAgentErrorMessage(streamResult.errorMessage)
		if strings.TrimSpace(sessionErr) == "" || strings.EqualFold(sessionErr, "Unknown error occurred") {
			sessionErr = "Session did not complete"
		}
		log.Printf("[OPENCODE] Session did not complete successfully")
		historySummary = sessionErr
		sendSSEData(w, flusher, map[string]interface{}{
			"done":    true,
			"success": false,
			"error":   sessionErr,
		})
		return
	}

	if !sessionHadActivity {
		warn := "⚠️  Session reached idle without assistant/tool activity. The selected model may be only partially supported by this OpenCode server build."
		if forwardCompatRegistryWarning != "" {
			warn = "⚠️  Session reached idle with no assistant/tool activity while using forward-compat model mapping. Try openai/gpt-5.3-codex or update OpenCode."
		}
		if useConfiguredDefaultModel {
			log.Printf("[OPENCODE] Warning: %s session=%s model=(configured default)", warn, session.ID)
		} else {
			log.Printf("[OPENCODE] Warning: %s session=%s model=%s/%s", warn, session.ID, providerID, modelID)
		}
		sendSSEData(w, flusher, map[string]interface{}{"output": warn})
	}

	if len(changedFiles) == 0 && preRunSnapshot != nil {
		snapshotChanges, err := detectChangedFilesFromSnapshot(req.ProjectPath, preRunSnapshot)
		if err != nil {
			log.Printf("[OPENCODE] Warning: Failed snapshot fallback: %v", err)
		} else if len(snapshotChanges) > 0 {
			changedFiles = snapshotChanges
			log.Printf("[OPENCODE] Recovered changed files from snapshot fallback: %d", len(changedFiles))
			sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("📡 Recovered %d changed files via snapshot fallback", len(changedFiles))})
		}
	}

	if len(changedFiles) > 0 {
		log.Printf("[OPENCODE] Files changed from stream events: %d", len(changedFiles))
		for _, file := range changedFiles {
			sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("📄 Modified: %s", file)})
		}
	} else {
		log.Printf("[OPENCODE] Warning: No files were modified")
		sendSSEData(w, flusher, map[string]interface{}{"output": "⚠️  No direct file updates detected in primary pass; running post-pass reconciliation checks..."})
	}

	prototypeChanged := detectPrototypeChanged(changedFiles)
	shouldRunMediaPostPass := prototypeChanged || prototypeContainsMediaPlaceholders(req.ProjectPath)
	mediaPostPassSummary := map[string]interface{}{
		"generated": 0,
		"reused":    0,
		"warnings":  []string{},
	}
	assetPlacementSummary := map[string]interface{}{
		"ran":      false,
		"success":  false,
		"files":    []string{},
		"warnings": []string{},
	}
	platformAssetsSynced := false
	mediaGeneratedCount := 0
	mediaReusedCount := 0
	mediaPolicy := normalizeMediaGenerationPolicy(req.MediaGenerationPolicy)

	if shouldRunMediaPostPass && mediaPolicy != "auto" {
		approvalPlan, approvalPlanErr := buildOpenCodeMediaApproval(OpenCodeMediaPostPassRequest{
			ProjectPath:        req.ProjectPath,
			ImageSource:        req.ImageSource,
			ReferenceImagePath: req.ReferenceImagePath,
			ReferenceAssetID:   req.ReferenceAssetID,
			ScanTargets:        []string{"prototype/index.html"},
		})
		if approvalPlanErr != nil {
			warning := fmt.Sprintf("Could not inspect pending media safely: %s", sanitizeProviderError(approvalPlanErr))
			sendSSEData(w, flusher, map[string]interface{}{"output": "⚠️  " + warning})
			mediaPostPassSummary["warnings"] = []string{warning}
			if mediaPolicy != "auto" {
				shouldRunMediaPostPass = false
				mediaPostPassSummary["approval"] = "skipped"
			}
		} else if approvalPlan != nil {
			mediaPostPassSummary["pending"] = len(approvalPlan.Items)
			switch mediaPolicy {
			case "skip":
				shouldRunMediaPostPass = false
				mediaPostPassSummary["approval"] = "skipped"
				sendSSEData(w, flusher, map[string]interface{}{
					"output": fmt.Sprintf("⏭️  Skipped %d new media asset request(s). Existing assets were left unchanged.", len(approvalPlan.Items)),
				})
			case "ask":
				approvalID, approvalResponse := registerOpenCodeMediaApproval(req.ProjectPath)
				approvalPlan.ID = approvalID
				sendSSEData(w, flusher, map[string]interface{}{"mediaApproval": approvalPlan})
				decision, decisionErr := waitForOpenCodeMediaApproval(ctx, approvalID, approvalResponse)
				if decisionErr != nil {
					sendSSEData(w, flusher, map[string]interface{}{
						"output": "⚠️  Media approval was not received. New paid assets were skipped.",
					})
				}
				if decision != "generate" {
					shouldRunMediaPostPass = false
					mediaPostPassSummary["approval"] = "skipped"
					sendSSEData(w, flusher, map[string]interface{}{
						"output": fmt.Sprintf("⏭️  Kept %d new media asset request(s) pending without calling providers.", len(approvalPlan.Items)),
					})
				} else {
					mediaPostPassSummary["approval"] = "approved"
					sendSSEData(w, flusher, map[string]interface{}{"output": "✅ Media generation approved for this run."})
				}
			}
		}
	}

	if shouldRunMediaPostPass {
		sendSSEData(w, flusher, map[string]interface{}{"output": "🪄 Running media post-pass (image/video/audio materialization + platform sync)..."})

		openAIImageKey, geminiImageKey, xaiImageKey := resolveAgentImageProviderKeys(req)
		postPassResp, postPassErr := runOpenCodeMediaPostPass(ctx, OpenCodeMediaPostPassRequest{
			ProjectPath:          req.ProjectPath,
			ImageSource:          req.ImageSource,
			OpenAIKey:            openAIImageKey,
			GeminiKey:            geminiImageKey,
			XaiKey:               xaiImageKey,
			VeoGeminiKey:         req.VeoGeminiKey,
			ElevenLabsKey:        req.ElevenLabsKey,
			ElevenLabsVoiceID:    req.ElevenLabsVoiceID,
			ElevenLabsVoiceModel: req.ElevenLabsVoiceModel,
			ReferenceImagePath:   req.ReferenceImagePath,
			ReferenceAssetID:     req.ReferenceAssetID,
			ScanTargets:          []string{"prototype/index.html"},
		})
		if postPassErr != nil {
			warning := sanitizeProviderError(postPassErr)
			sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("⚠️  Media post-pass failed: %s", warning)})
			mediaPostPassSummary["warnings"] = []string{warning}
		} else {
			prototypeChanged = prototypeChanged || postPassResp.PrototypeChanged
			platformAssetsSynced = postPassResp.PlatformAssetsSynced
			mediaGeneratedCount = len(postPassResp.GeneratedAssets)
			mediaReusedCount = len(postPassResp.ReusedStudioAssets)
			mediaPostPassSummary = map[string]interface{}{
				"generated": mediaGeneratedCount,
				"reused":    mediaReusedCount,
				"warnings":  postPassResp.Warnings,
			}
			sendSSEData(w, flusher, map[string]interface{}{
				"output": fmt.Sprintf("🧩 Media post-pass: generated %d, reused %d, platform copies %d",
					mediaGeneratedCount,
					mediaReusedCount,
					len(postPassResp.PlatformCopies),
				),
			})
			for _, warning := range postPassResp.Warnings {
				sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("⚠️  %s", warning)})
			}
		}
	}

	if mediaGeneratedCount+mediaReusedCount > 0 {
		sendSSEData(w, flusher, map[string]interface{}{
			"output": "🧭 Running asset placement reconciliation across prototype and platform code...",
		})
		assetPlacementSummary["ran"] = true
		if useConfiguredDefaultModel {
			warning := "Asset placement reconciliation skipped when using OpenCode configured default model without explicit provider/model."
			sendSSEData(w, flusher, map[string]interface{}{"output": "⚠️  " + warning})
			assetPlacementSummary["warnings"] = []string{warning}
			assetPlacementSummary["success"] = true
			assetPlacementSummary["files"] = []string{}
		} else {
			reconciledFiles, reconcileWarnings, reconcileErr := driver.runAssetPlacementReconcilePass(
				ctx,
				req.ProjectPath,
				session.ID,
				modelID,
				providerID,
				w,
				flusher,
			)
			if reconcileErr != nil {
				warning := sanitizeProviderError(reconcileErr)
				sendSSEData(w, flusher, map[string]interface{}{
					"output": fmt.Sprintf("⚠️  Asset placement pass failed: %s", warning),
				})
				assetPlacementSummary["warnings"] = append(reconcileWarnings, warning)
			} else {
				assetPlacementSummary["success"] = true
				if len(reconciledFiles) > 0 {
					changedFiles = mergeChangedFiles(changedFiles, reconciledFiles)
					prototypeChanged = prototypeChanged || detectPrototypeChanged(reconciledFiles)
					for _, file := range reconciledFiles {
						sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("📌 Reconciled: %s", file)})
					}
				}

				if len(reconcileWarnings) > 0 {
					for _, warning := range reconcileWarnings {
						sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("⚠️  %s", warning)})
					}
				}
				assetPlacementSummary["warnings"] = reconcileWarnings
			}

			if files, ok := assetPlacementSummary["files"].([]string); ok {
				assetPlacementSummary["files"] = append(files, reconciledFiles...)
			} else {
				assetPlacementSummary["files"] = reconciledFiles
			}
		}
	}

	// Check for implementation report and display it
	reportPath := filepath.Join(req.ProjectPath, "IMPLEMENTATION_REPORT.md")
	if reportContent, err := os.ReadFile(reportPath); err == nil {
		log.Printf("[OPENCODE] Found implementation report at %s", reportPath)
		sendSSEData(w, flusher, map[string]interface{}{
			"output": "📋 Implementation Report:",
		})
		// Send report content as separate lines for better readability
		lines := strings.Split(string(reportContent), "\n")
		for _, line := range lines {
			if line != "" {
				sendSSEData(w, flusher, map[string]interface{}{"output": line})
			}
		}
	}

	// Send completion
	historyStatus = "completed"
	historySummary = fmt.Sprintf("✅ Refinement completed. %d file(s) changed.", len(changedFiles))
	sendSSEData(w, flusher, map[string]interface{}{
		"done":                 true,
		"success":              true,
		"output":               "✅ Refinement completed!",
		"files":                len(changedFiles),
		"changedFiles":         changedFiles,
		"noActivityObserved":   !sessionHadActivity,
		"forwardCompatWarning": forwardCompatRegistryWarning,
		"prototypeChanged":     prototypeChanged,
		"platformAssetsSynced": platformAssetsSynced,
		"mediaPostPass":        mediaPostPassSummary,
		"assetPlacementPass":   assetPlacementSummary,
		"restartPrototype":     prototypeChanged,
	})
}

// openCodeVerifyHandler handles verify requests - runs build and fixes issues
func openCodeVerifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.ProjectPath == "" {
		http.Error(w, "projectPath is required", http.StatusBadRequest)
		return
	}

	// Verify project exists
	paths := GetProjectPaths(req.ProjectPath)
	if _, err := os.Stat(paths.Manifest); os.IsNotExist(err) {
		http.Error(w, "Not a Glowbom project (missing glowbom.json)", http.StatusBadRequest)
		return
	}

	if err := ensureOpenCodeServerReady(req.ProjectPath, req.Model, req.OpenAIKey, req.AnthropicKey, req.GeminiKey, req.FireworksKey, req.OpenRouterKey, req.OpenCodeZenKey, req.XaiKey, req.OpenAIAuthMode, req.OpenAIRefreshToken, req.OpenAIExpiresAt); err != nil {
		log.Printf("[OPENCODE] Failed preparing OpenCode server: %v", err)
		http.Error(w, fmt.Sprintf("Failed to prepare OpenCode server: %v", err), http.StatusInternalServerError)
		return
	}

	driver := GetOpenCodeDriver()
	ctx := r.Context()

	// Set up SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	sendSSEData(w, flusher, map[string]interface{}{"output": "Starting build verification..."})

	// Create a new session
	session, err := driver.client.Session.New(ctx, opencode.SessionNewParams{
		Title:     opencode.F("Verify Glowbom Build"),
		Directory: opencode.F(req.ProjectPath),
	})
	if err != nil {
		sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": userFacingAgentErrorMessage(err.Error())})
		return
	}

	sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("Session created: %s", session.ID)})

	// Build verify prompt
	prompt := buildVerifyPrompt(req.ProjectPath)

	authMode := normalizeOpenAIAuthMode(req.OpenAIAuthMode)
	requestedModel := strings.TrimSpace(req.Model)
	useConfiguredDefaultModel := requestedModel == "" && authMode == "opencode-config"

	modelID := ""
	providerID := ""
	fallbackNote := ""
	forwardCompatRegistryWarning := ""

	if useConfiguredDefaultModel {
		sendSSEData(w, flusher, map[string]interface{}{"output": "Using OpenCode configured default model."})
		log.Printf("[OPENCODE] Using OpenCode configured default model for verify")
	} else {
		// Determine model
		modelID, providerID, fallbackNote = resolveSessionModelForRequest(req.ProjectPath, req.Model)
		if strings.EqualFold(providerID, "openai") && isOpenAICodexForwardCompatModelID(modelID) {
			probe, probeErr := probeOpenCodeProviderModel(req.ProjectPath, providerID, modelID)
			if probeErr != nil {
				log.Printf("[OPENCODE] Warning: failed probing provider registry for %s/%s: %v", providerID, modelID, probeErr)
			} else if probe.ProviderFound && !probe.ModelRegistered {
				forwardCompatRegistryWarning = fmt.Sprintf("⚠️  OpenCode registry does not list %s yet; using forward-compat template. If this run no-ops, update OpenCode or fall back to openai/gpt-5.3-codex.", modelID)
				sendSSEData(w, flusher, map[string]interface{}{"output": forwardCompatRegistryWarning})
				log.Printf("[OPENCODE] Forward-compat registry miss for %s/%s providerSample=[%s]", providerID, modelID, strings.Join(probe.ProviderModelSample, ", "))
			}
		}

		sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("Using model: %s/%s", providerID, modelID)})
		if fallbackNote != "" {
			sendSSEData(w, flusher, map[string]interface{}{"output": "⚠️ " + fallbackNote})
		}
	}

	// Start event stream in background BEFORE sending prompt to avoid race condition
	log.Printf("[OPENCODE] Starting event stream listener...")
	type sessionStreamResult struct {
		completed    bool
		changedFiles []string
		hadActivity  bool
		errorMessage string
	}
	eventStreamDone := make(chan sessionStreamResult, 1)
	promptDispatched := make(chan struct{})

	go func() {
		completed, changedFiles, hadActivity, errorMessage := driver.streamEventsAndWaitForCompletion(ctx, w, flusher, req.ProjectPath, session.ID, promptDispatched)
		eventStreamDone <- sessionStreamResult{
			completed:    completed,
			changedFiles: changedFiles,
			hadActivity:  hadActivity,
			errorMessage: errorMessage,
		}
	}()

	// Small delay to ensure event stream connects first
	time.Sleep(100 * time.Millisecond)

	// Now send the prompt
	log.Printf("[OPENCODE] Sending verify prompt")
	sendSSEData(w, flusher, map[string]interface{}{"output": "Agent is verifying and fixing build issues..."})

	promptParams := opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Type: opencode.F(opencode.TextPartInputTypeText),
				Text: opencode.F(prompt),
			},
		}),
		Directory: opencode.F(req.ProjectPath),
	}
	if !useConfiguredDefaultModel {
		promptParams.Model = opencode.F(opencode.SessionPromptParamsModel{
			ModelID:    opencode.F(modelID),
			ProviderID: opencode.F(providerID),
		})
	}

	_, err = driver.sendSessionPrompt(ctx, session.ID, promptParams)

	if err != nil {
		log.Printf("[OPENCODE] Error: Verify failed: %v", err)
		if isUsageLimitErrorMessage(err.Error()) {
			go driver.abortSessionBestEffort(session.ID, req.ProjectPath, "usage limit while sending verify prompt")
		}
		sendSSEData(w, flusher, map[string]interface{}{"done": true, "success": false, "error": userFacingAgentErrorMessage(err.Error())})
		return
	}
	close(promptDispatched)

	log.Printf("[OPENCODE] Prompt sent, agent is now working...")

	// Wait for event stream goroutine to complete
	streamResult := <-eventStreamDone
	sessionCompleted := streamResult.completed
	changedFiles := streamResult.changedFiles
	sessionHadActivity := streamResult.hadActivity

	if !sessionCompleted {
		sessionErr := userFacingAgentErrorMessage(streamResult.errorMessage)
		if strings.TrimSpace(sessionErr) == "" || strings.EqualFold(sessionErr, "Unknown error occurred") {
			sessionErr = "Session did not complete"
		}
		log.Printf("[OPENCODE] Session did not complete successfully")
		sendSSEData(w, flusher, map[string]interface{}{
			"done":    true,
			"success": false,
			"error":   sessionErr,
		})
		return
	}

	if !sessionHadActivity {
		warn := "⚠️  Session reached idle without assistant/tool activity. The selected model may be only partially supported by this OpenCode server build."
		if forwardCompatRegistryWarning != "" {
			warn = "⚠️  Session reached idle with no assistant/tool activity while using forward-compat model mapping. Try openai/gpt-5.3-codex or update OpenCode."
		}
		if useConfiguredDefaultModel {
			log.Printf("[OPENCODE] Warning: %s session=%s model=(configured default)", warn, session.ID)
		} else {
			log.Printf("[OPENCODE] Warning: %s session=%s model=%s/%s", warn, session.ID, providerID, modelID)
		}
		sendSSEData(w, flusher, map[string]interface{}{"output": warn})
	}

	// Fall back to file-walk when stream did not emit file.updated events.
	if len(changedFiles) == 0 {
		files, err := driver.collectGeneratedFiles(ctx, req.ProjectPath)
		if err != nil {
			log.Printf("[OPENCODE] Warning: Failed to collect files: %v", err)
		} else {
			changedFiles = files
		}
	}

	if len(changedFiles) > 0 {
		log.Printf("[OPENCODE] Files modified: %d", len(changedFiles))
		for _, file := range changedFiles {
			sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("📄 Modified: %s", file)})
		}
	}

	// Send completion
	sendSSEData(w, flusher, map[string]interface{}{
		"done":                 true,
		"success":              true,
		"output":               "✅ Build verification completed!",
		"files":                len(changedFiles),
		"changedFiles":         changedFiles,
		"noActivityObserved":   !sessionHadActivity,
		"forwardCompatWarning": forwardCompatRegistryWarning,
	})
}

// openCodeQuestionRespondHandler handles agent question responses
func openCodeQuestionRespondHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodeQuestionRespondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		http.Error(w, "sessionID is required", http.StatusBadRequest)
		return
	}

	driver := GetOpenCodeDriver()
	sessionID := req.SessionID
	questionID := req.QuestionID
	answer := req.Answer
	projectPath := req.ProjectPath

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := driver.respondToQuestion(ctx, sessionID, questionID, answer, projectPath, req.Answers, req.AnswerByQuestionID); err != nil {
		http.Error(w, fmt.Sprintf("Failed to respond to question: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]interface{}{"ok": true})
}

// openCodePermissionRespondHandler handles agent permission responses
func openCodePermissionRespondHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenCodePermissionRespondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" || req.PermissionID == "" || req.Response == "" {
		http.Error(w, "sessionID, permissionID, and response are required", http.StatusBadRequest)
		return
	}

	driver := GetOpenCodeDriver()
	if err := driver.respondToPermission(r.Context(), req.SessionID, req.PermissionID, req.Response, req.ProjectPath); err != nil {
		http.Error(w, fmt.Sprintf("Failed to respond to permission: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]interface{}{"ok": true})
}

const (
	maxInstructionAttachmentSizeBytes = int64(40 * 1024 * 1024)
	maxInstructionAttachmentCount     = 20
)

type stagedInstructionAttachment struct {
	AbsolutePath string
	RelativePath string
	SizeBytes    int64
}

func hasAnyInstructionAttachmentPath(requestedPaths []string) bool {
	for _, rawPath := range requestedPaths {
		if strings.TrimSpace(rawPath) != "" {
			return true
		}
	}
	return false
}

func resetCurrentInstructionsDirectory(projectPath string) error {
	currentInstructionsDir := filepath.Join(projectPath, "current_instructions")
	if err := os.RemoveAll(currentInstructionsDir); err != nil {
		return fmt.Errorf("failed to reset current_instructions folder: %w", err)
	}
	if err := os.MkdirAll(currentInstructionsDir, 0755); err != nil {
		return fmt.Errorf("failed to create current_instructions folder: %w", err)
	}
	return nil
}

func stageInstructionAttachments(projectPath string, requestedPaths []string) ([]stagedInstructionAttachment, error) {
	nonEmptyPaths := make([]string, 0, len(requestedPaths))
	for _, rawPath := range requestedPaths {
		if strings.TrimSpace(rawPath) == "" {
			continue
		}
		nonEmptyPaths = append(nonEmptyPaths, rawPath)
	}

	if len(nonEmptyPaths) == 0 {
		return nil, nil
	}
	if len(nonEmptyPaths) > maxInstructionAttachmentCount {
		return nil, fmt.Errorf("too many instruction attachments (max %d files)", maxInstructionAttachmentCount)
	}

	currentInstructionsDir := filepath.Join(projectPath, "current_instructions")
	if err := os.MkdirAll(currentInstructionsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create current_instructions folder: %w", err)
	}

	seenSourcePaths := map[string]struct{}{}
	usedNames := map[string]struct{}{}
	if existingEntries, err := os.ReadDir(currentInstructionsDir); err == nil {
		for _, entry := range existingEntries {
			usedNames[entry.Name()] = struct{}{}
		}
	}
	attachments := make([]stagedInstructionAttachment, 0, len(nonEmptyPaths))

	for _, rawPath := range nonEmptyPaths {
		trimmed := strings.TrimSpace(rawPath)
		cleanPath := filepath.Clean(trimmed)
		absolutePath, err := filepath.Abs(cleanPath)
		if err != nil {
			return nil, fmt.Errorf("failed resolving attachment path %q: %w", trimmed, err)
		}

		if _, seen := seenSourcePaths[absolutePath]; seen {
			continue
		}
		seenSourcePaths[absolutePath] = struct{}{}

		info, err := os.Stat(absolutePath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("attachment not found: %s", absolutePath)
			}
			return nil, fmt.Errorf("failed to access attachment %q: %w", absolutePath, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("attachment must be a file, not folder: %s", absolutePath)
		}
		if info.Size() > maxInstructionAttachmentSizeBytes {
			return nil, fmt.Errorf(
				"attachment %q exceeds the 40MB limit (%s)",
				filepath.Base(absolutePath),
				humanReadableBytes(info.Size()),
			)
		}

		baseName := sanitizeAttachmentFilename(filepath.Base(absolutePath))
		if baseName == "" {
			baseName = fmt.Sprintf("attachment-%d", len(attachments)+1)
		}
		targetName := uniqueAttachmentFilename(baseName, usedNames)
		targetPath := filepath.Join(currentInstructionsDir, targetName)

		if err := copyLocalAttachmentFile(absolutePath, targetPath); err != nil {
			return nil, fmt.Errorf("failed copying attachment %q: %w", absolutePath, err)
		}

		attachments = append(attachments, stagedInstructionAttachment{
			AbsolutePath: targetPath,
			RelativePath: filepath.ToSlash(filepath.Join("current_instructions", targetName)),
			SizeBytes:    info.Size(),
		})
	}

	if len(attachments) == 0 {
		return nil, nil
	}

	return attachments, nil
}

func stageInstructionTextFile(projectPath, instructions string) (string, error) {
	trimmed := strings.TrimSpace(instructions)
	if trimmed == "" {
		return "", nil
	}

	currentInstructionsDir := filepath.Join(projectPath, "current_instructions")
	if err := os.MkdirAll(currentInstructionsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create current_instructions folder: %w", err)
	}

	textPath := filepath.Join(currentInstructionsDir, "instructions.txt")
	if err := os.WriteFile(textPath, []byte(trimmed+"\n"), 0644); err != nil {
		return "", fmt.Errorf("failed writing instructions.txt: %w", err)
	}

	return filepath.ToSlash(filepath.Join("current_instructions", "instructions.txt")), nil
}

func mergeInstructionAttachmentContext(
	userInstructions string,
	instructionsFilePath string,
	attachments []stagedInstructionAttachment,
) string {
	if instructionsFilePath == "" && len(attachments) == 0 {
		return userInstructions
	}

	lines := []string{
		"[Local attachment context]",
		"PRIORITY: Start by reading files in current_instructions/ first. Treat them as the main instructions for this run.",
	}
	if instructionsFilePath != "" {
		lines = append(lines, "Primary file: "+instructionsFilePath)
	}
	for _, attachment := range attachments {
		lines = append(lines, "Primary file: "+attachment.RelativePath)
	}
	lines = append(lines, "Use these files as source-of-truth context when details conflict with defaults.")
	context := strings.Join(lines, "\n")

	trimmedInstructions := strings.TrimSpace(userInstructions)
	if trimmedInstructions == "" {
		return context
	}

	return trimmedInstructions + "\n\n" + context
}

func sanitizeAttachmentFilename(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(trimmed))
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-', r == '_', r == '.', r == '(', r == ')', r == '[', r == ']', r == '+':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}

	clean := strings.Trim(builder.String(), " .")
	if clean == "" {
		return ""
	}
	if strings.HasPrefix(clean, ".") {
		clean = "file" + clean
	}
	return clean
}

func uniqueAttachmentFilename(baseName string, used map[string]struct{}) string {
	if _, exists := used[baseName]; !exists {
		used[baseName] = struct{}{}
		return baseName
	}

	ext := filepath.Ext(baseName)
	stem := strings.TrimSuffix(baseName, ext)
	if stem == "" {
		stem = "attachment"
	}

	for idx := 2; ; idx++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, idx, ext)
		if _, exists := used[candidate]; exists {
			continue
		}
		used[candidate] = struct{}{}
		return candidate
	}
}

func copyLocalAttachmentFile(sourcePath, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	target, err := os.Create(targetPath)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func humanReadableBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}

	value := float64(size)
	units := []string{"KB", "MB", "GB", "TB"}
	unit := "B"
	for _, candidate := range units {
		value /= 1024
		unit = candidate
		if value < 1024 {
			break
		}
	}

	if unit == "KB" {
		return fmt.Sprintf("%.0f %s", value, unit)
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}

type agentHistoryAttachmentRecord struct {
	ID            string `json:"id"`
	MediaType     string `json:"mediaType"`
	Filename      string `json:"filename"`
	MimeType      string `json:"mimeType,omitempty"`
	FileSizeBytes int64  `json:"fileSizeBytes,omitempty"`
}

type agentHistoryEntryRecord struct {
	ID            string                         `json:"id"`
	Timestamp     string                         `json:"timestamp"`
	Instructions  string                         `json:"instructions"`
	TaskType      string                         `json:"taskType"`
	Status        string                         `json:"status,omitempty"`
	OutputSummary string                         `json:"outputSummary,omitempty"`
	Attachments   []agentHistoryAttachmentRecord `json:"attachments,omitempty"`
}

func persistCurrentInstructionsHistory(
	projectPath string,
	instructions string,
	status string,
	outputSummary string,
) error {
	currentInstructionsDir := filepath.Join(projectPath, "current_instructions")
	entries, err := os.ReadDir(currentInstructionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed reading current_instructions: %w", err)
	}

	regularFiles := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			regularFiles = append(regularFiles, entry)
			continue
		}
		info, infoErr := entry.Info()
		if infoErr == nil && info.Mode().IsRegular() {
			regularFiles = append(regularFiles, entry)
		}
	}
	if len(regularFiles) == 0 {
		return nil
	}

	sort.Slice(regularFiles, func(i, j int) bool {
		return regularFiles[i].Name() < regularFiles[j].Name()
	})

	historyRoot := filepath.Join(projectPath, "history")
	if err := os.MkdirAll(historyRoot, 0755); err != nil {
		return fmt.Errorf("failed creating history folder: %w", err)
	}

	now := time.Now().UTC()
	entryID := randomUUIDString()
	shortID := strings.Split(entryID, "-")[0]
	folderName := fmt.Sprintf("%s_%s", now.Format("2006-01-02_150405"), shortID)
	entryDir := filepath.Join(historyRoot, folderName)
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		return fmt.Errorf("failed creating history entry folder: %w", err)
	}

	attachments := make([]agentHistoryAttachmentRecord, 0, len(regularFiles))
	for _, entry := range regularFiles {
		name := entry.Name()
		sourcePath := filepath.Join(currentInstructionsDir, name)
		targetPath := filepath.Join(entryDir, name)
		if err := copyLocalAttachmentFile(sourcePath, targetPath); err != nil {
			return fmt.Errorf("failed copying %s to history: %w", name, err)
		}

		info, err := os.Stat(sourcePath)
		sizeBytes := int64(0)
		if err == nil {
			sizeBytes = info.Size()
		}
		mimeType := inferMimeTypeForFilename(name)

		attachments = append(attachments, agentHistoryAttachmentRecord{
			ID:            randomUUIDString(),
			MediaType:     inferHistoryMediaType(mimeType, name),
			Filename:      name,
			MimeType:      mimeType,
			FileSizeBytes: sizeBytes,
		})
	}

	historyEntry := agentHistoryEntryRecord{
		ID:            entryID,
		Timestamp:     now.Format(time.RFC3339),
		Instructions:  strings.TrimSpace(instructions),
		TaskType:      "refine",
		Status:        normalizeHistoryStatus(status),
		OutputSummary: truncateText(outputSummary, 500),
		Attachments:   attachments,
	}

	entryData, err := json.MarshalIndent(historyEntry, "", "  ")
	if err != nil {
		return fmt.Errorf("failed serializing history entry: %w", err)
	}

	entryPath := filepath.Join(entryDir, "entry.json")
	if err := os.WriteFile(entryPath, entryData, 0644); err != nil {
		return fmt.Errorf("failed writing history entry: %w", err)
	}

	log.Printf("[OPENCODE] Archived current instructions to history/%s", folderName)
	return nil
}

func inferMimeTypeForFilename(name string) string {
	extension := strings.ToLower(filepath.Ext(name))
	if extension == "" {
		return "application/octet-stream"
	}
	if mimeType := mime.TypeByExtension(extension); mimeType != "" {
		return mimeType
	}
	return "application/octet-stream"
}

func inferHistoryMediaType(mimeType, name string) string {
	lowerMime := strings.ToLower(strings.TrimSpace(mimeType))
	switch {
	case strings.HasPrefix(lowerMime, "image/"):
		return "screenshot"
	case strings.HasPrefix(lowerMime, "video/"):
		return "video"
	case strings.HasPrefix(lowerMime, "audio/"):
		return "audio"
	case strings.HasPrefix(lowerMime, "text/"):
		return "document"
	}

	extension := strings.ToLower(filepath.Ext(name))
	switch extension {
	case ".md", ".txt", ".json", ".yaml", ".yml", ".xml", ".csv", ".pdf":
		return "document"
	default:
		return "other"
	}
}

func normalizeHistoryStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return "running"
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return "failed"
	}
}

func truncateText(value string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= maxLen {
		return trimmed
	}
	return trimmed[:maxLen]
}

func randomUUIDString() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		fallback := fmt.Sprintf("%032x", time.Now().UnixNano())
		if len(fallback) < 32 {
			fallback = strings.Repeat("0", 32-len(fallback)) + fallback
		}
		return fmt.Sprintf("%s-%s-%s-%s-%s",
			fallback[0:8],
			fallback[8:12],
			fallback[12:16],
			fallback[16:20],
			fallback[20:32],
		)
	}

	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80

	hexValue := hex.EncodeToString(buffer)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexValue[0:8],
		hexValue[8:12],
		hexValue[12:16],
		hexValue[16:20],
		hexValue[20:32],
	)
}

// buildRefinePrompt creates the prompt for code refinement
func buildRefinePrompt(projectRoot, instructions string) string {
	basePrompt := fmt.Sprintf(`You are working in a Glowbom project at: %s

Your task is to make this project production-ready and improve code quality. Report your actions clearly.

IMPORTANT:
- If AGENTS.md exists in the project root, read it first and follow it as the source of truth.
- If AGENTS.md is missing but Agent.md exists, read and follow Agent.md.
- If user instructions conflict with AGENTS.md (or Agent.md), follow the user instructions.

## Project Structure
- prototype/ - HTML/Tailwind source prototype
- ios/ - SwiftUI code (if exists)
- android/ - Kotlin code (if exists)
- web/ - Next.js code (if exists)
- godot/ - Godot code (if exists)

## Instructions
1. First, explore the project structure to understand what exists - report what you find
2. Read the prototype HTML to understand the intended design - summarize key elements
3. Review the generated platform code for issues:
   - Code style and best practices - report improvements made
   - Missing functionality from the prototype - add what's missing
   - Error handling - enhance where needed
   - Performance concerns - optimize if possible
   - Accessibility issues - fix any found
4. Make improvements while preserving the original design intent
5. Verify any changes compile/build correctly - report build results and fixes
6. If you add or change media in prototype/index.html, use placeholders so post-pass can materialize assets:
   - Images: glowbomimages:<prompt>
   - Videos: glowbyvideo:<prompt>|from:<image_key>|aspect:<ratio>
   - Audio: glowbyaudio:<prompt>|type:<voice|sound|music>|voice:<voice_id>|model:<model_id>|duration:<seconds>
   - For sound effects, ALWAYS set |type:sound explicitly.
   - If you don't know a valid ElevenLabs model ID, OMIT model:<...>; never use model:standard.
   - If voice/model are omitted, backend applies the user's configured ElevenLabs defaults.
   - For voiceover/background music/sound effects requests, add glowbyaudio placeholders wherever those assets are needed so post-pass can generate and attach them.
   - If the request is personalization (for example "make this person the main character"), you MUST add/update at least one glowbomimages placeholder that explicitly describes that person so backend post-pass can generate personalized assets.
7. Report progress after each major step (e.g., "Analyzed ios/ code", "Fixed error handling", "Build successful")`, projectRoot)

	// Inject project metadata so the agent knows the app identity
	manifestPath := filepath.Join(projectRoot, "glowbom.json")
	if project, err := LoadProject(manifestPath); err == nil {
		var metaParts []string
		if project.DisplayName != "" {
			metaParts = append(metaParts, fmt.Sprintf("- Display Name: %s", project.DisplayName))
		}
		if project.BundleID != "" {
			metaParts = append(metaParts, fmt.Sprintf("- Bundle ID: %s", project.BundleID))
		}
		if project.Version != "" {
			metaParts = append(metaParts, fmt.Sprintf("- Version: %s", project.Version))
		}
		if project.BuildNumber != "" {
			metaParts = append(metaParts, fmt.Sprintf("- Build Number: %s", project.BuildNumber))
		}
		if len(metaParts) > 0 {
			basePrompt += fmt.Sprintf(`

## Project Identity (from glowbom.json)
Use these values when configuring platform targets (Info.plist, build.gradle, package.json, etc.):
%s`, strings.Join(metaParts, "\n"))
		}
	}

	agentContent, agentFile := loadAgentInstructions(projectRoot)
	if agentContent != "" {
		basePrompt += fmt.Sprintf(`

## Agent Instructions (%s)
%s`, agentFile, agentContent)
	}

	if instructions != "" {
		basePrompt += fmt.Sprintf(`

## User Instructions (override AGENTS.md if conflict)
%s`, instructions)
	}

	return basePrompt
}

func loadAgentInstructions(projectRoot string) (string, string) {
	for _, name := range []string{"AGENTS.md", "Agent.md"} {
		path := filepath.Join(projectRoot, name)
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(content))
		if text == "" {
			continue
		}
		return text, name
	}
	return "", ""
}

// buildAssetPlacementPrompt creates a strict post-media pass prompt to reconcile asset references.
func buildAssetPlacementPrompt(projectRoot string) string {
	return fmt.Sprintf(`You are running the final "asset placement reconciliation" pass for a Glowbom project at: %s

Goal:
Ensure newly materialized assets from prototype/assets are correctly referenced across prototype and all existing platform outputs (Apple/Android/Web/Godot) without changing core app logic.

Hard Rules:
- Do NOT generate new images, videos, or audio in this pass.
- Do NOT add new image placeholders (glowbomimages, glowbyimages, glowbomimage, or glowbyimage), glowbyvideo, or glowbyaudio placeholders.
- Do NOT rewrite unrelated game/app logic.
- Keep edits minimal and deterministic; only fix asset placement, references, and render-fit issues.

Read these files first:
1. AGENTS.md (if present)
2. prototype/index.html
3. prototype/assets.json
4. prototype/platform_assets_map.json (if present)

Tasks:
1. Prototype reference integrity
   - Verify every prototype image/video/audio reference points to an existing file under prototype/assets.
   - Replace stale legacy references with the best matching generated asset when intent clearly matches.
   - Preserve IDs/classes/layout and existing behavior.

2. Platform reference reconciliation (only for directories that exist)
   - Apple: ensure SwiftUI/extension code references the correct asset catalog names from Assets.xcassets.
   - Android: ensure resource names used in code map to existing drawable/raw files for image/video/audio.
   - Web: ensure references map to existing web/public/assets/images, web/public/assets/videos, and web/public/assets/audio files.
   - Godot: ensure resource paths map to existing godot/assets/sprites, godot/assets/videos, and godot/assets/audio files.

3. Rendering quality checks
   - Avatars/hero art: use cover/crop behavior, avoid distortion.
   - Logos/icons/UI symbols: use fit/contain behavior.
   - Keep responsive behavior intact.

4. Size/compression guardrails
   - Reuse existing generated assets by default.
   - Only create a downscaled derivative if a very large image is used in a tiny slot and would be wasteful.
   - If you create a derivative, update references deterministically and keep originals.

5. Validation
   - Ensure there are no broken asset references in modified files.
   - If platform folders exist, make sure code references only assets that actually exist in that platform output.

Output requirements:
- Report exactly which files were changed and why.
- Include any remaining warnings if a platform path could not be reconciled cleanly.`, projectRoot)
}

type openCodeSessionResult struct {
	completed    bool
	changedFiles []string
	hadActivity  bool
	errorMessage string
}

func (d *OpenCodeDriver) runAssetPlacementReconcilePass(
	ctx context.Context,
	projectPath string,
	existingSessionID string,
	modelID string,
	providerID string,
	w http.ResponseWriter,
	flusher http.Flusher,
) ([]string, []string, error) {
	warnings := []string{}

	var session *opencode.Session
	if existingSessionID != "" {
		session = &opencode.Session{ID: existingSessionID}
		sendSSEData(w, flusher, map[string]interface{}{
			"output": fmt.Sprintf("Asset placement using session: %s", session.ID),
		})
	} else {
		var err error
		session, err = d.client.Session.New(ctx, opencode.SessionNewParams{
			Title:     opencode.F("Reconcile Asset Placement"),
			Directory: opencode.F(projectPath),
		})
		if err != nil {
			return nil, warnings, err
		}
		sendSSEData(w, flusher, map[string]interface{}{
			"output": fmt.Sprintf("Asset placement session created: %s", session.ID),
		})
	}

	prompt := buildAssetPlacementPrompt(projectPath)
	preRunSnapshot, snapshotErr := captureProjectFileSnapshot(projectPath)
	if snapshotErr != nil {
		warnings = append(warnings, fmt.Sprintf("asset placement snapshot unavailable: %s", sanitizeProviderError(snapshotErr)))
	}

	eventStreamDone := make(chan openCodeSessionResult, 1)
	promptDispatched := make(chan struct{})
	go func() {
		completed, changedFiles, hadActivity, errorMessage := d.streamEventsAndWaitForCompletion(ctx, w, flusher, projectPath, session.ID, promptDispatched)
		eventStreamDone <- openCodeSessionResult{
			completed:    completed,
			changedFiles: changedFiles,
			hadActivity:  hadActivity,
			errorMessage: errorMessage,
		}
	}()

	time.Sleep(100 * time.Millisecond)
	sendSSEData(w, flusher, map[string]interface{}{"output": "Reconciling asset references and render fit across targets..."})

	_, err := d.sendSessionPrompt(ctx, session.ID, opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Type: opencode.F(opencode.TextPartInputTypeText),
				Text: opencode.F(prompt),
			},
		}),
		Model: opencode.F(opencode.SessionPromptParamsModel{
			ModelID:    opencode.F(modelID),
			ProviderID: opencode.F(providerID),
		}),
		Directory: opencode.F(projectPath),
	})
	if err != nil {
		if isUsageLimitErrorMessage(err.Error()) {
			go d.abortSessionBestEffort(session.ID, projectPath, "usage limit while sending asset placement prompt")
		}
		return nil, warnings, err
	}
	close(promptDispatched)

	streamResult := <-eventStreamDone
	if !streamResult.completed {
		sessionErr := userFacingAgentErrorMessage(streamResult.errorMessage)
		if strings.TrimSpace(sessionErr) == "" || strings.EqualFold(sessionErr, "Unknown error occurred") {
			sessionErr = "asset placement session did not complete"
		}
		return nil, warnings, fmt.Errorf("%s", sessionErr)
	}
	if !streamResult.hadActivity {
		warnings = append(warnings, "asset placement session reported idle without assistant activity")
	}

	changedFiles := streamResult.changedFiles
	if len(changedFiles) == 0 && preRunSnapshot != nil {
		snapshotChanges, err := detectChangedFilesFromSnapshot(projectPath, preRunSnapshot)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("asset placement fallback diff failed: %s", sanitizeProviderError(err)))
		} else if len(snapshotChanges) > 0 {
			changedFiles = snapshotChanges
			sendSSEData(w, flusher, map[string]interface{}{
				"output": fmt.Sprintf("📡 Recovered %d asset-placement file changes via snapshot fallback", len(changedFiles)),
			})
		}
	}

	if len(changedFiles) == 0 {
		warnings = append(warnings, "asset placement pass completed with no file changes")
	}

	return changedFiles, dedupeWarnings(warnings), nil
}

// buildVerifyPrompt creates the prompt for build verification
func buildVerifyPrompt(projectRoot string) string {
	return fmt.Sprintf(`You are working in a Glowbom project at: %s

Your task is to verify the project builds successfully and fix any issues. Report all actions and results clearly.

## Project Structure
- prototype/ - HTML/Tailwind source prototype
- ios/ - SwiftUI code (if exists)
- android/ - Kotlin code (if exists)
- web/ - Next.js code (if exists)
- godot/ - Godot code (if exists)

## Instructions
1. First, explore the project to see what target platforms exist - report findings
2. For each platform that has code:

   **iOS (SwiftUI)**:
   - Navigate to ios/ directory
   - Report current state of files
   - Run 'swift build' to verify compilation - report output
   - Fix any Swift compiler errors - report each fix made
   - Re-run build to verify - report success/failure

   **Android (Kotlin)**:
   - Navigate to android/ directory
   - Report current state of files
   - Run './gradlew build' to verify compilation - report output
   - Fix any Kotlin/Gradle errors - report each fix made
   - Re-run build to verify - report success/failure

   **Web (Next.js)**:
   - Navigate to web/ directory
   - Report current state of files
   - Run 'npm install && npm run build' to verify - report output
   - Fix any TypeScript/build errors - report each fix made
   - Re-run build to verify - report success/failure

   **Godot**:
   - Navigate to godot/ directory
   - Report current state of files
   - Verify scene files are valid - report issues
   - Fix any GDScript errors - report each fix made
   - Re-verify - report success/failure

3. For each error encountered:
   - Analyze the root cause - explain what went wrong
   - Make the minimal fix required - describe the change
   - Re-run the build to verify the fix - report results

4. Continue until all platforms build successfully - provide final status report
5. Report progress after each major action (e.g., "Checking ios/ build", "Fixed Swift error in ViewModel", "iOS build successful")`, projectRoot)
}

// getPropertyValue searches for a property value in a map or struct, supporting nested paths like "Part.Text"
func getPropertyValue(p interface{}, path string) interface{} {
	if p == nil {
		return nil
	}

	parts := strings.Split(path, ".")
	current := p

	for _, part := range parts {
		if current == nil {
			return nil
		}

		// Handle map
		if m, ok := current.(map[string]interface{}); ok {
			found := false
			// Case-insensitive search in map
			for k, v := range m {
				if strings.EqualFold(k, part) {
					current = v
					found = true
					break
				}
			}
			if !found {
				return nil
			}
			continue
		}

		// Handle struct via reflection
		v := reflect.ValueOf(current)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		if v.Kind() == reflect.Struct {
			f := v.FieldByName(strings.Title(part))
			if !f.IsValid() {
				// Try as-is
				f = v.FieldByName(part)
			}
			if f.IsValid() {
				current = f.Interface()
				continue
			}
		}

		return nil
	}

	return current
}

// getPropertyString returns a string value from an interface, handling multiple potential paths
func getPropertyString(p interface{}, paths ...string) string {
	for _, path := range paths {
		val := getPropertyValue(p, path)
		if val != nil {
			if s, ok := val.(string); ok && s != "" {
				return s
			}
			// Fallback to string representation if not a string but exists
			if valStr := fmt.Sprintf("%v", val); valStr != "" && valStr != "<nil>" {
				return valStr
			}
		}
	}
	return ""
}

// getPropertyMap returns a map representation of a property
func getPropertyMap(p interface{}, path string) map[string]interface{} {
	val := getPropertyValue(p, path)
	if val == nil {
		return nil
	}

	if m, ok := val.(map[string]interface{}); ok {
		return m
	}

	// Try to convert struct to map via reflection if needed
	v := reflect.ValueOf(val)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		res := make(map[string]interface{})
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if f.CanInterface() {
				res[t.Field(i).Name] = f.Interface()
			}
		}
		return res
	}

	return nil
}

func getStringSlice(p interface{}, path string) []string {
	val := getPropertyValue(p, path)
	if val == nil {
		return nil
	}

	switch v := val.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			} else if item != nil {
				out = append(out, fmt.Sprintf("%v", item))
			}
		}
		return out
	default:
		return nil
	}
}

// extractAssistantMessageText pulls assistant text from message.updated payloads.
// OpenCode events often nest text under properties.info.parts[].text.
func extractAssistantMessageText(props interface{}) string {
	if props == nil {
		return ""
	}

	// Legacy/simple paths.
	if direct := getPropertyString(props, "Content", "Text", "Part.Text", "Part.Content"); direct != "" {
		return direct
	}

	// Canonical path for message.updated: properties.info.parts[].text
	if info := getPropertyValue(props, "Info"); info != nil {
		if text := joinPartTexts(getPropertyValue(info, "Parts")); text != "" {
			return text
		}
	}

	// Raw map fallback if structure differs by SDK/runtime.
	if info := getPropertyValue(props, "info"); info != nil {
		if text := joinPartTexts(getPropertyValue(info, "parts")); text != "" {
			return text
		}
	}

	return ""
}

func extractMessageRole(props interface{}) string {
	role := strings.ToLower(strings.TrimSpace(getPropertyString(
		props,
		"Info.Role",
		"info.role",
		"Role",
		"role",
		"Message.Role",
		"message.role",
	)))
	return role
}

func looksLikeDriverPromptEcho(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	if lower == "" {
		return false
	}
	// Guardrail: when a user message.update is surfaced without role metadata,
	// avoid replaying the entire backend prompt template into the UI stream.
	return strings.Contains(lower, "you are working in a glowbom project at:") &&
		strings.Contains(lower, "your task is to make this project production-ready") &&
		strings.Contains(lower, "project structure")
}

func getMapValueCaseInsensitive(m map[string]interface{}, key string) interface{} {
	for candidate, value := range m {
		if strings.EqualFold(candidate, key) {
			return value
		}
	}
	return nil
}

func getMapStringCaseInsensitive(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		val := getMapValueCaseInsensitive(m, key)
		if val == nil {
			continue
		}
		if s, ok := val.(string); ok {
			trimmed := strings.TrimSpace(s)
			if trimmed != "" {
				return trimmed
			}
		} else {
			rendered := strings.TrimSpace(fmt.Sprintf("%v", val))
			if rendered != "" && rendered != "<nil>" {
				return rendered
			}
		}
	}
	return ""
}

func summarizeToolInput(input map[string]interface{}) string {
	if len(input) == 0 {
		return ""
	}
	if path := getMapStringCaseInsensitive(input, "Path", "path", "File", "file"); path != "" {
		return fmt.Sprintf("(%s)", filepath.Base(path))
	}
	if cmd := getMapStringCaseInsensitive(input, "Command", "command", "Cmd", "cmd"); cmd != "" {
		if len(cmd) > 50 {
			cmd = cmd[:47] + "..."
		}
		return fmt.Sprintf("(%s)", cmd)
	}
	if pattern := getMapStringCaseInsensitive(input, "Pattern", "pattern"); pattern != "" {
		if len(pattern) > 42 {
			pattern = pattern[:39] + "..."
		}
		return fmt.Sprintf("(%s)", pattern)
	}
	return ""
}

type toolPartProgress struct {
	key      string
	status   string
	label    string
	errorMsg string
}

func extractToolPartProgress(props interface{}) (toolPartProgress, bool) {
	if props == nil {
		return toolPartProgress{}, false
	}

	partVal := getPropertyValue(props, "Part")
	if partVal == nil {
		partVal = getPropertyValue(props, "part")
	}
	if partVal == nil {
		return toolPartProgress{}, false
	}

	partType := strings.ToLower(strings.TrimSpace(getPropertyString(partVal, "Type", "type")))
	if partType != "tool" {
		return toolPartProgress{}, false
	}

	partID := strings.TrimSpace(getPropertyString(partVal, "ID", "Id", "id"))
	callID := strings.TrimSpace(getPropertyString(partVal, "CallID", "CallId", "callID", "callId"))
	toolName := strings.TrimSpace(getPropertyString(partVal, "Tool", "tool", "Name", "name"))

	stateVal := getPropertyValue(partVal, "State")
	if stateVal == nil {
		stateVal = getPropertyValue(partVal, "state")
	}
	status := strings.ToLower(strings.TrimSpace(getPropertyString(stateVal, "Status", "status")))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(getPropertyString(partVal, "State.Status", "state.status")))
	}
	if status == "" {
		status = "running"
	}

	title := strings.TrimSpace(getPropertyString(stateVal, "Title", "title"))
	if title == "" {
		title = strings.TrimSpace(getPropertyString(partVal, "Name", "name"))
	}

	var input map[string]interface{}
	if stateVal != nil {
		input = getPropertyMap(stateVal, "Input")
		if input == nil {
			input = getPropertyMap(stateVal, "input")
		}
	}
	if input == nil {
		if stateMap, ok := stateVal.(map[string]interface{}); ok {
			if nested, ok := getMapValueCaseInsensitive(stateMap, "input").(map[string]interface{}); ok {
				input = nested
			}
		}
	}

	label := title
	if label == "" {
		label = toolName
	}
	if label == "" {
		label = "tool"
	}
	if suffix := summarizeToolInput(input); suffix != "" {
		label = label + " " + suffix
	}

	errMsg := strings.TrimSpace(getPropertyString(stateVal, "Error", "error"))

	key := callID
	if key == "" {
		key = partID
	}
	if key == "" {
		key = label
	}

	return toolPartProgress{
		key:      key,
		status:   status,
		label:    label,
		errorMsg: errMsg,
	}, true
}

func formatToolPartProgressLine(progress toolPartProgress) string {
	switch progress.status {
	case "pending", "running":
		return fmt.Sprintf("🔧 Running: %s", progress.label)
	case "completed":
		return fmt.Sprintf("✅ Completed: %s", progress.label)
	case "error":
		if strings.TrimSpace(progress.errorMsg) != "" {
			return fmt.Sprintf("❌ Tool failed: %s (%s)", progress.label, progress.errorMsg)
		}
		return fmt.Sprintf("❌ Tool failed: %s", progress.label)
	default:
		return ""
	}
}

func joinPartTexts(parts interface{}) string {
	if parts == nil {
		return ""
	}

	var chunks []string
	appendText := func(part interface{}) {
		if part == nil {
			return
		}
		if text := getPropertyString(part, "Text", "text", "Content", "content", "Snapshot", "snapshot"); text != "" {
			chunks = append(chunks, text)
		}
	}

	switch list := parts.(type) {
	case []interface{}:
		for _, part := range list {
			appendText(part)
		}
	case []map[string]interface{}:
		for _, part := range list {
			appendText(part)
		}
	default:
		v := reflect.ValueOf(parts)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
			for i := 0; i < v.Len(); i++ {
				if v.Index(i).CanInterface() {
					appendText(v.Index(i).Interface())
				}
			}
		}
	}

	if len(chunks) == 0 {
		return ""
	}
	return strings.Join(chunks, "")
}

func extractPartID(props interface{}) string {
	return strings.TrimSpace(getPropertyString(
		props,
		"Part.ID",
		"Part.Id",
		"part.id",
		"PartID",
		"partID",
	))
}

func extractPartMessageID(props interface{}) string {
	return strings.TrimSpace(getPropertyString(
		props,
		"Part.MessageID",
		"Part.MessageId",
		"part.messageID",
		"part.messageId",
		"MessageID",
		"messageID",
	))
}

func extractPartDelta(props interface{}) string {
	return getPropertyString(
		props,
		"Delta",
		"delta",
		"Part.Delta",
		"part.delta",
	)
}

func extractPartSnapshotText(props interface{}) string {
	return getPropertyString(
		props,
		"Part.Text",
		"Part.Content",
		"Part.Snapshot",
		"part.text",
		"part.content",
		"part.snapshot",
		"Text",
		"text",
		"Content",
		"content",
		"Snapshot",
		"snapshot",
	)
}

func longestCommonPrefix(left, right string) string {
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	n := len(leftRunes)
	if len(rightRunes) < n {
		n = len(rightRunes)
	}

	i := 0
	for i < n && leftRunes[i] == rightRunes[i] {
		i++
	}
	if i == 0 {
		return ""
	}
	return string(leftRunes[:i])
}

// computePartChunk deduplicates mixed part.updated/delta streams by tracking
// the latest known snapshot for each part and returning only incremental text.
func computePartChunk(partID, explicitDelta, snapshotText string, snapshots map[string]string) string {
	if partID == "" {
		if explicitDelta != "" {
			return explicitDelta
		}
		return snapshotText
	}

	prev := snapshots[partID]
	if snapshotText != "" {
		if snapshotText == prev {
			return ""
		}
		if prev == "" {
			snapshots[partID] = snapshotText
			return snapshotText
		}
		if strings.HasPrefix(snapshotText, prev) {
			chunk := strings.TrimPrefix(snapshotText, prev)
			snapshots[partID] = snapshotText
			if chunk != "" {
				return chunk
			}
			if explicitDelta != "" {
				return explicitDelta
			}
			return ""
		}

		// Snapshot was rewritten; emit only changed suffix to avoid replaying whole text.
		common := longestCommonPrefix(prev, snapshotText)
		chunk := strings.TrimPrefix(snapshotText, common)
		snapshots[partID] = snapshotText
		return chunk
	}

	if explicitDelta != "" {
		snapshots[partID] = prev + explicitDelta
		return explicitDelta
	}

	return ""
}

func extractPartChunk(props interface{}, snapshots map[string]string) (chunk string, partID string, partMessageID string) {
	if props == nil {
		return "", "", ""
	}
	partID = extractPartID(props)
	partMessageID = extractPartMessageID(props)
	explicitDelta := extractPartDelta(props)
	snapshotText := extractPartSnapshotText(props)
	// Some providers emit snapshot-only part updates without a stable part ID.
	// Use message ID as a synthetic key so we can still compute deltas.
	if partID == "" && partMessageID != "" && (snapshotText != "" || explicitDelta != "") {
		partID = "message:" + partMessageID
	}
	chunk = computePartChunk(partID, explicitDelta, snapshotText, snapshots)
	return chunk, partID, partMessageID
}

func parseEventPropertiesFromRaw(raw string) map[string]interface{} {
	if raw == "" {
		return nil
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil
	}
	if props, ok := payload["properties"].(map[string]interface{}); ok {
		return props
	}
	return payload
}

func extractQuestionItems(props interface{}) []map[string]interface{} {
	if props == nil {
		return nil
	}

	questionsVal := getPropertyValue(props, "Questions")
	if questionsVal == nil {
		questionsVal = getPropertyValue(props, "questions")
	}
	if questionsVal == nil {
		if questionMap := getPropertyMap(props, "Question"); questionMap != nil {
			return []map[string]interface{}{
				questionMap,
			}
		}
		return nil
	}

	switch v := questionsVal.(type) {
	case []map[string]interface{}:
		return v
	case []interface{}:
		items := make([]map[string]interface{}, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				items = append(items, m)
			} else if s, ok := item.(string); ok {
				items = append(items, map[string]interface{}{"prompt": s})
			}
		}
		return items
	default:
		return nil
	}
}

func extractQuestionFields(props interface{}) (string, string, string, []string) {
	if props == nil {
		return "", "", "", nil
	}

	questionID := getPropertyString(props, "ID", "Id", "QuestionID", "questionID", "RequestID", "requestID", "Question.Id", "Question.ID", "QuestionID.Id", "Question.RequestID")
	sessionID := getPropertyString(props, "SessionID", "sessionID", "Session.Id", "Session.ID", "Question.SessionID")
	prompt := getPropertyString(props, "Prompt", "Question", "Text", "Message", "Content", "question", "Question.Text", "Question.Prompt")
	choices := getStringSlice(props, "Choices")

	if questionMap := getPropertyMap(props, "Question"); questionMap != nil {
		if questionID == "" {
			if val, ok := questionMap["id"].(string); ok {
				questionID = val
			} else if val, ok := questionMap["ID"].(string); ok {
				questionID = val
			} else if val, ok := questionMap["requestID"].(string); ok {
				questionID = val
			} else if val, ok := questionMap["RequestID"].(string); ok {
				questionID = val
			}
		}
		if sessionID == "" {
			if val, ok := questionMap["sessionID"].(string); ok {
				sessionID = val
			} else if val, ok := questionMap["SessionID"].(string); ok {
				sessionID = val
			}
		}
		if prompt == "" {
			if val, ok := questionMap["prompt"].(string); ok {
				prompt = val
			} else if val, ok := questionMap["text"].(string); ok {
				prompt = val
			}
		}
		if len(choices) == 0 {
			if val, ok := questionMap["choices"].([]interface{}); ok {
				for _, item := range val {
					choices = append(choices, fmt.Sprintf("%v", item))
				}
			}
		}
	}

	if prompt == "" {
		prompt = "Agent asked a question."
	}

	return questionID, sessionID, prompt, choices
}

func extractPermissionFields(props interface{}) (string, string, string, string, string, string) {
	if props == nil {
		return "", "", "", "", "", ""
	}

	permissionID := getPropertyString(props, "ID", "Id", "PermissionID", "permissionID", "RequestID", "requestID")
	sessionID := getPropertyString(props, "SessionID", "sessionID")
	title := getPropertyString(props, "Title", "title")
	permType := getPropertyString(props, "Type", "type", "Permission", "permission")
	message := getPropertyString(props, "Message", "message", "Metadata.Message")
	patternVal := getPropertyValue(props, "Pattern")
	if patternVal == nil {
		patternVal = getPropertyValue(props, "Patterns")
	}
	pattern := ""
	switch v := patternVal.(type) {
	case string:
		pattern = v
	case []string:
		pattern = strings.Join(v, ", ")
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, fmt.Sprintf("%v", item))
		}
		pattern = strings.Join(parts, ", ")
	}

	return permissionID, sessionID, title, permType, message, pattern
}

func eventSessionID(props interface{}) string {
	return getPropertyString(
		props,
		"SessionID",
		"sessionID",
		"Info.SessionID",
		"info.sessionID",
		"Part.SessionID",
		"part.sessionID",
		"Message.SessionID",
		"message.sessionID",
		"Question.SessionID",
		"question.sessionID",
		"Permission.SessionID",
		"permission.sessionID",
	)
}

func extractEventSessionID(event opencode.EventListResponse) string {
	if sid := strings.TrimSpace(eventSessionID(event.Properties)); sid != "" {
		return sid
	}
	if rawProps := parseEventPropertiesFromRaw(event.JSON.RawJSON()); rawProps != nil {
		return strings.TrimSpace(eventSessionID(rawProps))
	}
	return ""
}

var usageLimitResetAtPattern = regexp.MustCompile(`(?i)(?:resets[_-]?at|reset[_-]?at|x-codex-primary-reset-at)["']?\s*[:=]\s*["']?(\d{9,})`)
var usageLimitResetInSecondsPattern = regexp.MustCompile(`(?i)(?:resets[_-]?in[_-]?seconds|reset[_-]?after[_-]?seconds|x-codex-primary-reset-after-seconds)["']?\s*[:=]\s*["']?(\d{1,10})`)
var usageLimitWindowMinutesPattern = regexp.MustCompile(`(?i)(?:x-codex-primary-window-minutes|window[_-]?minutes)["']?\s*[:=]\s*["']?(\d{1,10})`)
var usageLimitUsedPercentPattern = regexp.MustCompile(`(?i)(?:x-codex-primary-used-percent|used[_-]?percent)["']?\s*[:=]\s*["']?(\d{1,3})`)
var simpleStatusMapPattern = regexp.MustCompile(`(?i)^map\[(?:type|status):([^\]]+)\]$`)

func isUsageLimitErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "usage_limit_reached") {
		return true
	}
	if strings.Contains(lower, "usage limit has been reached") {
		return true
	}
	if strings.Contains(lower, "too many requests") {
		return true
	}
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "quota") {
		return true
	}
	return false
}

func latestUsageLimitHint() (usageLimitHint, bool) {
	usageLimitHintState.mu.RLock()
	hint := usageLimitHintState.hint
	usageLimitHintState.mu.RUnlock()

	if hint.UpdatedAt.IsZero() {
		return usageLimitHint{}, false
	}
	// Avoid reusing stale quota metadata.
	if time.Since(hint.UpdatedAt) > 8*24*time.Hour {
		return usageLimitHint{}, false
	}

	hasAny := hint.HasResetAt || hint.HasResetInSeconds || hint.HasWindowMinutes || hint.HasUsedPercent
	if !hasAny {
		return usageLimitHint{}, false
	}
	return hint, true
}

func extractUsageLimitInt(raw string, pattern *regexp.Regexp) (int64, bool) {
	if raw == "" || pattern == nil {
		return 0, false
	}
	if matches := pattern.FindStringSubmatch(raw); len(matches) == 2 {
		if value, err := strconv.ParseInt(matches[1], 10, 64); err == nil && value >= 0 {
			return value, true
		}
	}
	return 0, false
}

func extractUsageLimitResetInSeconds(raw string) (int64, bool) {
	return extractUsageLimitInt(raw, usageLimitResetInSecondsPattern)
}

func extractUsageLimitWindowMinutes(raw string) (int64, bool) {
	return extractUsageLimitInt(raw, usageLimitWindowMinutesPattern)
}

func extractUsageLimitUsedPercent(raw string) (int64, bool) {
	return extractUsageLimitInt(raw, usageLimitUsedPercentPattern)
}

func recordUsageLimitHintFromText(raw string) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return
	}
	if !isUsageLimitErrorMessage(text) &&
		!usageLimitResetAtPattern.MatchString(text) &&
		!usageLimitResetInSecondsPattern.MatchString(text) &&
		!usageLimitWindowMinutesPattern.MatchString(text) &&
		!usageLimitUsedPercentPattern.MatchString(text) {
		return
	}

	usageLimitHintState.mu.Lock()
	defer usageLimitHintState.mu.Unlock()

	hint := usageLimitHintState.hint
	updated := false

	if epoch, ok := extractUsageLimitInt(text, usageLimitResetAtPattern); ok && epoch > 0 {
		hint.ResetAt = time.Unix(epoch, 0)
		hint.HasResetAt = true
		updated = true
	}
	if seconds, ok := extractUsageLimitResetInSeconds(text); ok && seconds > 0 {
		hint.ResetInSeconds = seconds
		hint.HasResetInSeconds = true
		if !hint.HasResetAt {
			hint.ResetAt = time.Now().Add(time.Duration(seconds) * time.Second)
			hint.HasResetAt = true
		}
		updated = true
	}
	if windowMinutes, ok := extractUsageLimitWindowMinutes(text); ok && windowMinutes > 0 {
		hint.WindowMinutes = windowMinutes
		hint.HasWindowMinutes = true
		updated = true
	}
	if usedPercent, ok := extractUsageLimitUsedPercent(text); ok && usedPercent >= 0 {
		hint.UsedPercent = usedPercent
		hint.HasUsedPercent = true
		updated = true
	}

	if updated {
		hint.UpdatedAt = time.Now()
		usageLimitHintState.hint = hint
	}
}

func extractUsageLimitResetTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}

	if epoch, ok := extractUsageLimitInt(raw, usageLimitResetAtPattern); ok && epoch > 0 {
		return time.Unix(epoch, 0), true
	}

	if seconds, ok := extractUsageLimitResetInSeconds(raw); ok && seconds > 0 {
		return time.Now().Add(time.Duration(seconds) * time.Second), true
	}

	return time.Time{}, false
}

func formatApproxDuration(d time.Duration) string {
	if d <= 0 {
		return "<1m"
	}

	totalMinutes := int64(d.Round(time.Minute) / time.Minute)
	if totalMinutes <= 0 {
		return "<1m"
	}

	days := totalMinutes / (24 * 60)
	totalMinutes %= 24 * 60
	hours := totalMinutes / 60
	minutes := totalMinutes % 60

	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 && len(parts) < 2 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if len(parts) == 0 {
		return "<1m"
	}
	return strings.Join(parts, " ")
}

func formatQuotaWindow(windowMinutes int64) string {
	if windowMinutes <= 0 {
		return "quota"
	}
	if windowMinutes%(24*60) == 0 {
		days := windowMinutes / (24 * 60)
		return fmt.Sprintf("%d-day", days)
	}
	if windowMinutes%60 == 0 {
		hours := windowMinutes / 60
		return fmt.Sprintf("%d-hour", hours)
	}
	return fmt.Sprintf("%d-minute", windowMinutes)
}

func isTransientSessionStatus(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	switch lower {
	case "", "busy", "running", "working", "started", "pending", "processing", "in_progress", "in-progress":
		return true
	default:
		return false
	}
}

func isIgnorableSessionStatus(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return true
	}
	if isTransientSessionStatus(trimmed) {
		return true
	}

	if matches := simpleStatusMapPattern.FindStringSubmatch(trimmed); len(matches) == 2 {
		return isTransientSessionStatus(matches[1])
	}

	var statusMap map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &statusMap); err == nil && len(statusMap) > 0 {
		// Ignore pure {"type":"busy"} / {"status":"running"} style payloads.
		if len(statusMap) <= 2 {
			nonTerminal := true
			seenStatusField := false
			for key, rawVal := range statusMap {
				lowerKey := strings.ToLower(strings.TrimSpace(key))
				if lowerKey == "type" || lowerKey == "status" {
					seenStatusField = true
					if !isTransientSessionStatus(fmt.Sprintf("%v", rawVal)) {
						nonTerminal = false
					}
				} else {
					nonTerminal = false
				}
			}
			if seenStatusField && nonTerminal {
				return true
			}
		}
	}

	return false
}

func usageLimitContext(message, rawEvent string) string {
	msg := strings.TrimSpace(message)
	raw := strings.TrimSpace(rawEvent)
	if msg == "" {
		if isUsageLimitErrorMessage(raw) {
			return raw
		}
		return ""
	}
	if raw == "" {
		return msg
	}
	if !isUsageLimitErrorMessage(msg) && !isUsageLimitErrorMessage(raw) {
		return msg
	}
	if strings.Contains(raw, msg) {
		return raw
	}
	if strings.Contains(msg, raw) {
		return msg
	}
	return msg + "\n" + raw
}

func userFacingAgentErrorMessage(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "Unknown error occurred"
	}
	recordUsageLimitHintFromText(raw)

	clean := sanitizeProviderError(errors.New(raw))
	if clean == "" {
		clean = "Unknown error occurred"
	}

	if !isUsageLimitErrorMessage(raw) && !isUsageLimitErrorMessage(clean) {
		return clean
	}

	usedPercent, hasUsedPercent := extractUsageLimitUsedPercent(raw)
	windowMinutes, hasWindowMinutes := extractUsageLimitWindowMinutes(raw)
	resetAt, hasResetAt := extractUsageLimitResetTime(raw)
	resetInSeconds, hasResetInSeconds := extractUsageLimitResetInSeconds(raw)

	if hint, ok := latestUsageLimitHint(); ok {
		if !hasUsedPercent && hint.HasUsedPercent {
			usedPercent = hint.UsedPercent
			hasUsedPercent = true
		}
		if !hasWindowMinutes && hint.HasWindowMinutes {
			windowMinutes = hint.WindowMinutes
			hasWindowMinutes = true
		}
		if !hasResetAt && hint.HasResetAt {
			resetAt = hint.ResetAt
			hasResetAt = true
		}
		if !hasResetInSeconds && hint.HasResetInSeconds {
			resetInSeconds = hint.ResetInSeconds
			hasResetInSeconds = true
		}
	}

	msg := "Usage limit reached for the selected model on the current plan. Try another model, or retry after your quota resets."
	if hasUsedPercent {
		if hasWindowMinutes && windowMinutes > 0 {
			msg = fmt.Sprintf("%s Current usage: %d%% of your %s quota window.", msg, usedPercent, formatQuotaWindow(windowMinutes))
		} else {
			msg = fmt.Sprintf("%s Current usage: %d%% of your quota window.", msg, usedPercent)
		}
	}

	if hasResetAt {
		relative := formatApproxDuration(time.Until(resetAt))
		msg = fmt.Sprintf("%s Quota resets at %s (in about %s).", msg, resetAt.Local().Format("2006-01-02 15:04 MST"), relative)
	} else if hasResetInSeconds && resetInSeconds > 0 {
		msg = fmt.Sprintf("%s Retry in about %s.", msg, formatApproxDuration(time.Duration(resetInSeconds)*time.Second))
	}
	return msg
}

func extractSessionErrorMessage(props interface{}) string {
	msg := getPropertyString(
		props,
		"Error.Data.Message",
		"error.data.message",
		"Error.Message",
		"error.message",
		"Message",
		"message",
		"Details",
		"details",
		"Status",
		"status",
		"Error",
	)
	return strings.TrimSpace(msg)
}

func extractSessionStatusMessage(props interface{}) string {
	paths := []string{
		"Details.Message",
		"details.message",
		"Details.Description",
		"details.description",
		"Details.Detail",
		"details.detail",
		"Status.Message",
		"status.message",
		"Status.Description",
		"status.description",
		"Message.Text",
		"message.text",
		"Details",
		"details",
		"Status",
		"status",
		"Message",
		"message",
		"Error.Data.Message",
		"error.data.message",
		"Error.Message",
		"error.message",
		"Error",
		"error",
	}

	for _, path := range paths {
		val := getPropertyValue(props, path)
		if val == nil {
			continue
		}
		switch v := val.(type) {
		case string:
			trimmed := strings.TrimSpace(v)
			if trimmed == "" || isIgnorableSessionStatus(trimmed) {
				continue
			}
			return trimmed
		case map[string]interface{}:
			rawJSON, err := json.Marshal(v)
			if err != nil {
				continue
			}
			trimmed := strings.TrimSpace(string(rawJSON))
			if trimmed == "" || isIgnorableSessionStatus(trimmed) {
				continue
			}
			return trimmed
		default:
			trimmed := strings.TrimSpace(fmt.Sprintf("%v", val))
			if trimmed == "" || trimmed == "<nil>" || isIgnorableSessionStatus(trimmed) {
				continue
			}
			return trimmed
		}
	}

	return ""
}

func (d *OpenCodeDriver) abortSessionBestEffort(sessionID, projectDir, reason string) {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return
	}

	abortCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	params := opencode.SessionAbortParams{}
	if strings.TrimSpace(projectDir) != "" {
		params.Directory = opencode.F(projectDir)
	}

	if _, err := d.client.Session.Abort(abortCtx, sid, params); err != nil {
		log.Printf("[OPENCODE] Failed to abort session %s (%s): %v", sid, reason, err)
		return
	}

	log.Printf("[OPENCODE] Aborted session %s (%s)", sid, reason)
}

// streamEventsToSSE streams OpenCode events to the HTTP response as SSE
func (d *OpenCodeDriver) streamEventsToSSE(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, projectDir string) {
	log.Printf("[OPENCODE] Starting SSE event streaming for directory: %s", projectDir)
	stream := d.client.Event.ListStreaming(ctx, opencode.EventListParams{
		Directory: opencode.F(projectDir),
	})

	lastToolName := ""
	lastAssistantMessageID := ""
	totalTextSent := ""
	partAccumulated := ""
	lastSessionStatus := ""
	partSnapshots := make(map[string]string)
	toolProgressByID := make(map[string]string)
	for stream.Next() {
		event := stream.Current()
		eventSID := extractEventSessionID(event)
		rawProps := parseEventPropertiesFromRaw(event.JSON.RawJSON())

		// Stream relevant events to client
		switch event.Type {
		case "message.updated":
			role := extractMessageRole(event.Properties)
			if role == "" && rawProps != nil {
				role = extractMessageRole(rawProps)
			}
			// Skip non-assistant updates so we don't replay the original user prompt.
			if role != "" && role != "assistant" {
				break
			}
			messageID := getPropertyString(event.Properties, "Info.ID", "Info.Id", "ID", "id")
			if messageID != "" && messageID != lastAssistantMessageID {
				lastAssistantMessageID = messageID
				totalTextSent = ""
				partAccumulated = ""
				partSnapshots = make(map[string]string)
			}

			content := extractAssistantMessageText(event.Properties)
			if content == "" {
				if rawProps != nil {
					content = extractAssistantMessageText(rawProps)
				}
			}
			if content != "" {
				if role == "" && looksLikeDriverPromptEcho(content) {
					break
				}
				if len(content) > len(totalTextSent) && strings.HasPrefix(content, totalTextSent) {
					delta := content[len(totalTextSent):]
					sendSSEData(w, flusher, map[string]interface{}{"outputChunk": delta})
					totalTextSent = content
				} else if len(content) > len(totalTextSent) {
					// Snapshot diverged (edit/rewrite); send full content as discrete output.
					sendSSEData(w, flusher, map[string]interface{}{"output": content})
					totalTextSent = content
				}
				// else: stale snapshot (len <= totalTextSent), skip
			}
		case "message.part.updated", "message.part.delta":
			progress, hasProgress := extractToolPartProgress(event.Properties)
			if !hasProgress && rawProps != nil {
				progress, hasProgress = extractToolPartProgress(rawProps)
			}
			if hasProgress {
				if lastStatus, exists := toolProgressByID[progress.key]; !exists || lastStatus != progress.status {
					toolProgressByID[progress.key] = progress.status
					if line := formatToolPartProgressLine(progress); line != "" {
						sendSSEData(w, flusher, map[string]interface{}{"output": line})
					}
				}
			}

			// Extract incremental thought/output text from part events (common in OpenAI/ChatGPT)
			chunk, _, partMessageID := extractPartChunk(event.Properties, partSnapshots)
			if rawProps != nil {
				if chunk == "" {
					rawChunk, _, rawPartMessageID := extractPartChunk(rawProps, partSnapshots)
					chunk = rawChunk
					if partMessageID == "" {
						partMessageID = rawPartMessageID
					}
				} else if partMessageID == "" {
					partMessageID = extractPartMessageID(rawProps)
				}
			}
			if chunk != "" {
				if looksLikeDriverPromptEcho(chunk) {
					break
				}
				if partMessageID != "" && partMessageID != lastAssistantMessageID {
					lastAssistantMessageID = partMessageID
					totalTextSent = ""
					partAccumulated = ""
					partSnapshots = make(map[string]string)
				}
				partAccumulated += chunk
				if len(partAccumulated) > len(totalTextSent) && strings.HasPrefix(partAccumulated, totalTextSent) {
					delta := partAccumulated[len(totalTextSent):]
					sendSSEData(w, flusher, map[string]interface{}{"outputChunk": delta})
					totalTextSent = partAccumulated
				}
			}
		case "session.status":
			details := extractSessionStatusMessage(event.Properties)
			if details == "" {
				if rawProps != nil {
					details = extractSessionStatusMessage(rawProps)
				}
			}
			rawDetails := strings.TrimSpace(string(event.JSON.RawJSON()))
			details = usageLimitContext(details, rawDetails)
			if details != "" {
				if isUsageLimitErrorMessage(details) {
					friendly := userFacingAgentErrorMessage(details)
					log.Printf("[OPENCODE] Session status indicates usage limit: %s", friendly)
					d.abortSessionBestEffort(eventSID, projectDir, "usage limit in streamEventsToSSE session.status")
					sendSSEData(w, flusher, map[string]interface{}{
						"output":  fmt.Sprintf("❌ Error: %s", friendly),
						"done":    true,
						"success": false,
						"error":   friendly,
					})
					break
				}
				normalizedStatus := strings.TrimSpace(details)
				if normalizedStatus != "" && normalizedStatus != lastSessionStatus {
					lastSessionStatus = normalizedStatus
					sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("ℹ️ Status: %s", normalizedStatus)})
				}
			}
		case "session.error":
			errorMsg := extractSessionErrorMessage(event.Properties)
			if errorMsg == "" {
				if rawProps := parseEventPropertiesFromRaw(event.JSON.RawJSON()); rawProps != nil {
					errorMsg = extractSessionErrorMessage(rawProps)
				}
			}
			rawError := strings.TrimSpace(string(event.JSON.RawJSON()))
			errorMsg = usageLimitContext(errorMsg, rawError)
			if errorMsg == "" {
				errorMsg = "Unknown error occurred"
			}
			friendly := userFacingAgentErrorMessage(errorMsg)
			log.Printf("[OPENCODE] Session error: %s", friendly)
			if isUsageLimitErrorMessage(errorMsg) || isUsageLimitErrorMessage(friendly) {
				d.abortSessionBestEffort(eventSID, projectDir, "usage limit in streamEventsToSSE session.error")
			}
			sendSSEData(w, flusher, map[string]interface{}{
				"output":  fmt.Sprintf("❌ Error: %s", friendly),
				"done":    true,
				"success": false,
				"error":   friendly,
			})
		case "question.asked":
			questionID, sessionID, prompt, choices := extractQuestionFields(event.Properties)
			items := extractQuestionItems(event.Properties)
			if questionID == "" || sessionID == "" || prompt == "Agent asked a question." || len(items) == 0 {
				if rawProps := parseEventPropertiesFromRaw(event.JSON.RawJSON()); rawProps != nil {
					questionID, sessionID, prompt, choices = extractQuestionFields(rawProps)
					items = extractQuestionItems(rawProps)
				}
			}
			if prompt == "Agent asked a question." && len(items) > 0 {
				if val, ok := items[0]["prompt"].(string); ok && val != "" {
					prompt = val
				} else if val, ok := items[0]["question"].(string); ok && val != "" {
					prompt = val
				}
			}
			sendSSEData(w, flusher, map[string]interface{}{
				"question": map[string]interface{}{
					"id":        questionID,
					"sessionID": sessionID,
					"prompt":    prompt,
					"choices":   choices,
					"questions": items,
				},
			})
			sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("❓ Question: %s", prompt)})
		case "permission.updated", "permission.asked":
			permissionID, sessionID, title, permType, message, pattern := extractPermissionFields(event.Properties)
			if permissionID == "" || sessionID == "" {
				if rawProps := parseEventPropertiesFromRaw(event.JSON.RawJSON()); rawProps != nil {
					permissionID, sessionID, title, permType, message, pattern = extractPermissionFields(rawProps)
				}
			}
			sendSSEData(w, flusher, map[string]interface{}{
				"permission": map[string]interface{}{
					"id":        permissionID,
					"sessionID": sessionID,
					"title":     title,
					"type":      permType,
					"pattern":   pattern,
					"message":   message,
				},
			})
			sendSSEData(w, flusher, map[string]interface{}{"output": "🔐 Permission requested by agent..."})
		case "tool.start":
			toolName := getPropertyString(event.Properties, "Name")
			if toolName != "" {
				lastToolName = toolName
				var argsStr string
				input := getPropertyMap(event.Properties, "Input")
				if input != nil {
					// Handle common tool inputs
					if path, ok := input["Path"].(string); ok {
						argsStr = fmt.Sprintf(" (%s)", filepath.Base(path))
					} else if path, ok := input["path"].(string); ok {
						argsStr = fmt.Sprintf(" (%s)", filepath.Base(path))
					} else if cmd, ok := input["Command"].(string); ok {
						if len(cmd) > 30 {
							cmd = cmd[:27] + "..."
						}
						argsStr = fmt.Sprintf(" (%s)", cmd)
					} else if cmd, ok := input["command"].(string); ok {
						if len(cmd) > 30 {
							cmd = cmd[:27] + "..."
						}
						argsStr = fmt.Sprintf(" (%s)", cmd)
					}
				}
				sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("🔧 Running: %s%s", toolName, argsStr)})
			}
		case "tool.end":
			toolName := getPropertyString(event.Properties, "Name")
			if toolName != "" {
				if lastToolName == toolName {
					lastToolName = ""
				}
				sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("✅ Completed: %s", toolName)})
			}
		case "file.updated", "file.edited":
			filePath := getPropertyString(event.Properties, "Path", "path", "File", "file")
			if filePath != "" {
				sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("📝 Updated: %s", filePath)})
			}
		case "session.idle", "server.connected", "session.updated", "session.diff", "lsp.client.diagnostics", "server.heartbeat", "lsp.updated", "file.watcher.updated":
			// Silence these frequent/internal events
		default:
			log.Printf("[OPENCODE] Unhandled SSE event: %s", event.Type)
		}
	}

	if err := stream.Err(); err != nil {
		log.Printf("[OPENCODE] SSE stream error: %v", err)
	} else {
		log.Printf("[OPENCODE] SSE streaming ended successfully")
	}
}

// streamEventsAndWaitForCompletion streams events and waits for the session to complete.
// Returns:
//   - completed: true if session completed successfully (idle), false otherwise
//   - changedFiles: paths detected from file.updated/file.edited events
//   - hadActivity: true if assistant/tool/question/permission/todo activity was observed
//   - errorMessage: normalized session error when available
func (d *OpenCodeDriver) streamEventsAndWaitForCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	projectDir,
	sessionID string,
	promptDispatched <-chan struct{},
) (bool, []string, bool, string) {
	// Closing the browser request does not stop an independently running OpenCode session.
	// Abort with a fresh, bounded context because the request context is already canceled.
	stopCancellation := context.AfterFunc(ctx, func() {
		d.abortSessionBestEffort(sessionID, projectDir, "build request canceled")
	})
	defer stopCancellation()
	log.Printf("[OPENCODE] Starting SSE event streaming and waiting for completion (session: %s)", sessionID)
	stream := d.client.Event.ListStreaming(ctx, opencode.EventListParams{
		Directory: opencode.F(projectDir),
	})
	defer stream.Close()

	sessionIdle := false
	hadError := false
	sawSessionActivity := false
	promptWasDispatched := false
	sessionEventCount := 0
	lastEventTime := time.Now()
	lastToolName := ""
	lastAssistantMessageID := ""
	totalTextSent := ""
	partAccumulated := ""
	lastSessionStatus := ""
	partSnapshots := make(map[string]string)
	toolProgressByID := make(map[string]string)
	changedFilesSet := make(map[string]struct{})
	lastSessionError := ""
	abortRequested := false

	requestUsageLimitAbort := func(source string) {
		if abortRequested {
			return
		}
		abortRequested = true
		go d.abortSessionBestEffort(sessionID, projectDir, source)
	}

	heartbeatTicker := time.NewTicker(10 * time.Second)
	defer heartbeatTicker.Stop()

	go func() {
		for {
			select {
			case <-heartbeatTicker.C:
				if !sessionIdle {
					elapsed := time.Since(lastEventTime)
					if elapsed.Seconds() > 10 {
						statusMsg := fmt.Sprintf("⏳ Agent still working... (%.0fs since last update)", elapsed.Seconds())
						if lastToolName != "" {
							statusMsg += fmt.Sprintf(" [Current task: %s]", lastToolName)
						}
						sendSSEData(w, flusher, map[string]interface{}{"output": statusMsg})
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	for stream.Next() {
		event := stream.Current()
		if !promptWasDispatched && promptDispatched != nil {
			select {
			case <-promptDispatched:
				promptWasDispatched = true
			default:
			}
		}
		eventSID := extractEventSessionID(event)
		if eventSID != "" && eventSID != sessionID {
			// Directory-wide stream includes events from other sessions; ignore them.
			continue
		}
		rawProps := parseEventPropertiesFromRaw(event.JSON.RawJSON())
		sessionEventCount++
		lastEventTime = time.Now()

		// Stream relevant events to client
		log.Printf("[OPENCODE-STREAM-DEBUG] event.Type=%q totalSent=%d partAcc=%d lastMsgID=%q", event.Type, len(totalTextSent), len(partAccumulated), lastAssistantMessageID)
		switch event.Type {
		case "message.updated":
			role := extractMessageRole(event.Properties)
			if role == "" && rawProps != nil {
				role = extractMessageRole(rawProps)
			}
			if role != "" && role != "assistant" {
				log.Printf("[OPENCODE-STREAM-DEBUG] message.updated SKIPPED: role=%q (not assistant)", role)
				break
			}
			messageID := getPropertyString(event.Properties, "Info.ID", "Info.Id", "ID", "id")
			if messageID == "" && rawProps != nil {
				messageID = getPropertyString(rawProps, "Info.ID", "Info.Id", "ID", "id")
			}
			log.Printf("[OPENCODE-STREAM-DEBUG] message.updated: role=%q messageID=%q lastMsgID=%q totalSent=%d", role, messageID, lastAssistantMessageID, len(totalTextSent))
			if messageID != "" && messageID != lastAssistantMessageID {
				log.Printf("[OPENCODE-STREAM-DEBUG] message.updated: NEW message ID detected")
				lastAssistantMessageID = messageID
				totalTextSent = ""
				partAccumulated = ""
				partSnapshots = make(map[string]string)
			}

			content := extractAssistantMessageText(event.Properties)
			if content == "" {
				if rawProps != nil {
					content = extractAssistantMessageText(rawProps)
				}
			}
			contentPreview := content
			if len(contentPreview) > 120 {
				contentPreview = contentPreview[:120] + "..."
			}
			log.Printf("[OPENCODE-STREAM-DEBUG] message.updated: contentLen=%d preview=%q", len(content), contentPreview)
			if content != "" {
				if role == "" && looksLikeDriverPromptEcho(content) {
					log.Printf("[OPENCODE-STREAM-DEBUG] message.updated SUPPRESSED: looks like driver prompt echo")
					break
				}
				sawSessionActivity = true
				if len(content) > len(totalTextSent) && strings.HasPrefix(content, totalTextSent) {
					delta := content[len(totalTextSent):]
					log.Printf("[OPENCODE-STREAM-DEBUG] message.updated FORWARDED delta (%d chars)", len(delta))
					sendSSEData(w, flusher, map[string]interface{}{"outputChunk": delta})
					totalTextSent = content
				} else if len(content) > len(totalTextSent) {
					log.Printf("[OPENCODE-STREAM-DEBUG] message.updated FORWARDED rewrite (%d chars)", len(content))
					sendSSEData(w, flusher, map[string]interface{}{"output": content})
					totalTextSent = content
				} else {
					log.Printf("[OPENCODE-STREAM-DEBUG] message.updated SKIPPED: stale (contentLen=%d <= totalSentLen=%d)", len(content), len(totalTextSent))
				}
			}
		case "message.part.updated", "message.part.delta":
			progress, hasProgress := extractToolPartProgress(event.Properties)
			if !hasProgress && rawProps != nil {
				progress, hasProgress = extractToolPartProgress(rawProps)
			}
			if hasProgress {
				if lastStatus, exists := toolProgressByID[progress.key]; !exists || lastStatus != progress.status {
					toolProgressByID[progress.key] = progress.status
					if line := formatToolPartProgressLine(progress); line != "" {
						sawSessionActivity = true
						sendSSEData(w, flusher, map[string]interface{}{"output": line})
					}
				}
			}

			// Extract incremental thought/output text from part events (common in OpenAI/ChatGPT)
			chunk, _, partMessageID := extractPartChunk(event.Properties, partSnapshots)
			if rawProps != nil {
				if chunk == "" {
					rawChunk, _, rawPartMessageID := extractPartChunk(rawProps, partSnapshots)
					chunk = rawChunk
					if partMessageID == "" {
						partMessageID = rawPartMessageID
					}
				} else if partMessageID == "" {
					partMessageID = extractPartMessageID(rawProps)
				}
			}
			chunkPreview := chunk
			if len(chunkPreview) > 120 {
				chunkPreview = chunkPreview[:120] + "..."
			}
			log.Printf("[OPENCODE-STREAM-DEBUG] part event: type=%q hasProgress=%t chunkLen=%d partMsgID=%q preview=%q", event.Type, hasProgress, len(chunk), partMessageID, chunkPreview)
			if chunk != "" {
				if looksLikeDriverPromptEcho(chunk) {
					log.Printf("[OPENCODE-STREAM-DEBUG] part chunk SUPPRESSED: looks like driver prompt echo (%d chars)", len(chunk))
					break
				}
				if partMessageID != "" && partMessageID != lastAssistantMessageID {
					lastAssistantMessageID = partMessageID
					totalTextSent = ""
					partAccumulated = ""
					partSnapshots = make(map[string]string)
				}
				partAccumulated += chunk
				if len(partAccumulated) > len(totalTextSent) && strings.HasPrefix(partAccumulated, totalTextSent) {
					delta := partAccumulated[len(totalTextSent):]
					log.Printf("[OPENCODE-STREAM-DEBUG] part chunk FORWARDED delta (%d chars, totalSent now %d)", len(delta), len(partAccumulated))
					sawSessionActivity = true
					sendSSEData(w, flusher, map[string]interface{}{"outputChunk": delta})
					totalTextSent = partAccumulated
				} else {
					log.Printf("[OPENCODE-STREAM-DEBUG] part chunk SKIPPED: already covered (partAcc=%d, totalSent=%d)", len(partAccumulated), len(totalTextSent))
				}
			}
		case "session.status":
			details := extractSessionStatusMessage(event.Properties)
			if details == "" {
				if rawProps != nil {
					details = extractSessionStatusMessage(rawProps)
				}
			}
			rawDetails := strings.TrimSpace(string(event.JSON.RawJSON()))
			details = usageLimitContext(details, rawDetails)
			if details != "" {
				friendlyStatus := userFacingAgentErrorMessage(details)
				if isUsageLimitErrorMessage(details) || isUsageLimitErrorMessage(friendlyStatus) {
					sawSessionActivity = true
					hadError = true
					lastSessionError = details
					requestUsageLimitAbort("usage limit in streamEventsAndWaitForCompletion session.status")
					sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("❌ Error: %s", friendlyStatus)})
					// End stream early; caller will emit done=false with this error.
					sessionIdle = true
				} else {
					normalizedStatus := strings.TrimSpace(details)
					if normalizedStatus != "" && normalizedStatus != lastSessionStatus {
						lastSessionStatus = normalizedStatus
						sawSessionActivity = true
						sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("ℹ️ Status: %s", normalizedStatus)})
					}
				}
			}
		case "session.error":
			sawSessionActivity = true
			hadError = true
			errorMsg := extractSessionErrorMessage(event.Properties)
			if errorMsg == "" {
				if rawProps != nil {
					errorMsg = extractSessionErrorMessage(rawProps)
				}
			}
			rawError := strings.TrimSpace(string(event.JSON.RawJSON()))
			errorMsg = usageLimitContext(errorMsg, rawError)
			if errorMsg == "" {
				errorMsg = "Unknown error occurred"
			}
			friendly := userFacingAgentErrorMessage(errorMsg)
			if isUsageLimitErrorMessage(errorMsg) || isUsageLimitErrorMessage(friendly) {
				requestUsageLimitAbort("usage limit in streamEventsAndWaitForCompletion session.error")
			}
			lastSessionError = errorMsg
			log.Printf("[OPENCODE] Session error: %s", friendly)
			sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("❌ Error: %s", friendly)})
		case "question.asked":
			sawSessionActivity = true
			questionID, questionSessionID, prompt, choices := extractQuestionFields(event.Properties)
			items := extractQuestionItems(event.Properties)
			if questionID == "" || questionSessionID == "" || prompt == "Agent asked a question." || len(items) == 0 {
				if rawProps != nil {
					questionID, questionSessionID, prompt, choices = extractQuestionFields(rawProps)
					items = extractQuestionItems(rawProps)
				}
			}
			if questionSessionID == "" {
				questionSessionID = eventSID
			}
			if questionSessionID == "" {
				questionSessionID = sessionID
			}
			if questionSessionID != sessionID {
				continue
			}
			if prompt == "Agent asked a question." && len(items) > 0 {
				if val, ok := items[0]["prompt"].(string); ok && val != "" {
					prompt = val
				} else if val, ok := items[0]["question"].(string); ok && val != "" {
					prompt = val
				}
			}
			sendSSEData(w, flusher, map[string]interface{}{
				"question": map[string]interface{}{
					"id":        questionID,
					"sessionID": questionSessionID,
					"prompt":    prompt,
					"choices":   choices,
					"questions": items,
				},
			})
			sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("❓ Question: %s", prompt)})
		case "permission.updated", "permission.asked":
			sawSessionActivity = true
			permissionID, permissionSessionID, title, permType, message, pattern := extractPermissionFields(event.Properties)
			if permissionID == "" || permissionSessionID == "" {
				if rawProps != nil {
					permissionID, permissionSessionID, title, permType, message, pattern = extractPermissionFields(rawProps)
				}
			}
			if permissionSessionID == "" {
				permissionSessionID = eventSID
			}
			if permissionSessionID == "" {
				permissionSessionID = sessionID
			}
			if permissionSessionID != sessionID {
				continue
			}
			sendSSEData(w, flusher, map[string]interface{}{
				"permission": map[string]interface{}{
					"id":        permissionID,
					"sessionID": permissionSessionID,
					"title":     title,
					"type":      permType,
					"pattern":   pattern,
					"message":   message,
				},
			})
			sendSSEData(w, flusher, map[string]interface{}{"output": "🔐 Permission requested by agent..."})
		case "session.idle":
			if !sawSessionActivity && !promptWasDispatched {
				log.Printf("[OPENCODE] Ignoring pre-work session.idle for session %s", sessionID)
				continue
			}
			sessionIdle = true
		case "tool.start":
			toolName := getPropertyString(event.Properties, "Name")
			if toolName != "" {
				sawSessionActivity = true
				lastToolName = toolName
				var argsStr string
				input := getPropertyMap(event.Properties, "Input")
				if input != nil {
					if path, ok := input["Path"].(string); ok {
						argsStr = fmt.Sprintf(" (%s)", filepath.Base(path))
					} else if path, ok := input["path"].(string); ok {
						argsStr = fmt.Sprintf(" (%s)", filepath.Base(path))
					} else if cmd, ok := input["Command"].(string); ok {
						if len(cmd) > 30 {
							cmd = cmd[:27] + "..."
						}
						argsStr = fmt.Sprintf(" (%s)", cmd)
					} else if cmd, ok := input["command"].(string); ok {
						if len(cmd) > 30 {
							cmd = cmd[:27] + "..."
						}
						argsStr = fmt.Sprintf(" (%s)", cmd)
					}
				}
				sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("🔧 Running: %s%s", toolName, argsStr)})
			}
		case "tool.end":
			toolName := getPropertyString(event.Properties, "Name")
			if toolName != "" {
				sawSessionActivity = true
				if lastToolName == toolName {
					lastToolName = ""
				}
				sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("✅ Completed: %s", toolName)})
			}
		case "file.updated", "file.edited":
			filePath := getPropertyString(event.Properties, "Path", "path", "File", "file")
			if filePath != "" {
				sawSessionActivity = true
				normalizedPath := normalizeChangedFilePath(projectDir, filePath)
				if normalizedPath == "" {
					normalizedPath = filepath.ToSlash(filePath)
				}
				changedFilesSet[normalizedPath] = struct{}{}
				sendSSEData(w, flusher, map[string]interface{}{"output": fmt.Sprintf("📝 Updated: %s", normalizedPath)})
			}
		case "todo.updated":
			val := reflect.ValueOf(event.Properties)
			if val.Kind() == reflect.Struct {
				todosField := val.FieldByName("Todos")
				if todosField.IsValid() && todosField.Kind() == reflect.Slice {
					sawSessionActivity = true
					completedCount := 0
					totalCount := todosField.Len()
					var currentTask string
					for i := 0; i < todosField.Len(); i++ {
						todoItem := todosField.Index(i)
						status := fmt.Sprintf("%v", todoItem.FieldByName("Status").Interface())
						content := fmt.Sprintf("%v", todoItem.FieldByName("Content").Interface())
						if status == "completed" {
							completedCount++
						} else if status == "in_progress" {
							currentTask = content
						}
					}
					if totalCount > 0 {
						progressMsg := fmt.Sprintf("📊 Progress: %d/%d tasks completed", completedCount, totalCount)
						if currentTask != "" {
							progressMsg += fmt.Sprintf(" - Working on: %s", currentTask)
						}
						sendSSEData(w, flusher, map[string]interface{}{"output": progressMsg})
					}
				}
			}
		case "session.diff", "lsp.client.diagnostics", "server.connected", "session.updated", "server.heartbeat", "lsp.updated", "file.watcher.updated":
			// Skip
		default:
			log.Printf("[OPENCODE] Unhandled SSE event: %s", event.Type)
		}

		if sessionIdle {
			break
		}
	}

	if err := stream.Err(); err != nil {
		if lastSessionError == "" {
			lastSessionError = userFacingAgentErrorMessage(err.Error())
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			log.Printf(
				"[OPENCODE] SSE stream canceled for session %s (client request canceled) events=%d promptDispatched=%t sawActivity=%t idle=%t",
				sessionID,
				sessionEventCount,
				promptWasDispatched,
				sawSessionActivity,
				sessionIdle,
			)
		} else {
			log.Printf("[OPENCODE] SSE stream error: %v", err)
		}
		return false, nil, sawSessionActivity, lastSessionError
	}

	changedFiles := make([]string, 0, len(changedFilesSet))
	for path := range changedFilesSet {
		changedFiles = append(changedFiles, path)
	}
	sort.Strings(changedFiles)
	log.Printf(
		"[OPENCODE] Session stream summary session=%s completed=%t hadError=%t events=%d activity=%t changedFiles=%d",
		sessionID,
		!hadError && sessionIdle,
		hadError,
		sessionEventCount,
		sawSessionActivity,
		len(changedFiles),
	)

	return !hadError && sessionIdle, changedFiles, sawSessionActivity, lastSessionError
}

func normalizeChangedFilePath(projectDir, filePath string) string {
	trimmed := strings.TrimSpace(filePath)
	if trimmed == "" {
		return ""
	}

	if filepath.IsAbs(trimmed) {
		if rel, err := filepath.Rel(projectDir, trimmed); err == nil {
			return filepath.ToSlash(filepath.Clean(rel))
		}
	}

	return filepath.ToSlash(filepath.Clean(trimmed))
}

type projectFileSnapshotEntry struct {
	Size            int64
	ModTimeUnixNano int64
}

func captureProjectFileSnapshot(projectDir string) (map[string]projectFileSnapshotEntry, error) {
	snapshot := make(map[string]projectFileSnapshotEntry)

	err := filepath.Walk(projectDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if info.IsDir() {
			if shouldSkipSnapshotDirectory(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(projectDir, path)
		if err != nil {
			return nil
		}
		cleanRel := filepath.ToSlash(filepath.Clean(relPath))
		if cleanRel == "" || cleanRel == "." {
			return nil
		}

		snapshot[cleanRel] = projectFileSnapshotEntry{
			Size:            info.Size(),
			ModTimeUnixNano: info.ModTime().UnixNano(),
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func detectChangedFilesFromSnapshot(projectDir string, before map[string]projectFileSnapshotEntry) ([]string, error) {
	after, err := captureProjectFileSnapshot(projectDir)
	if err != nil {
		return nil, err
	}

	changedSet := make(map[string]struct{})
	for path, afterEntry := range after {
		beforeEntry, exists := before[path]
		if !exists || beforeEntry.Size != afterEntry.Size || beforeEntry.ModTimeUnixNano != afterEntry.ModTimeUnixNano {
			changedSet[path] = struct{}{}
		}
	}

	for path := range before {
		if _, exists := after[path]; !exists {
			changedSet[path] = struct{}{}
		}
	}

	changedFiles := make([]string, 0, len(changedSet))
	for path := range changedSet {
		changedFiles = append(changedFiles, path)
	}
	sort.Strings(changedFiles)
	return changedFiles, nil
}

func shouldSkipSnapshotDirectory(name string) bool {
	switch name {
	case ".git", "history", "current_instructions", "node_modules", ".gradle", ".next", "build", "dist", ".idea", ".vscode":
		return true
	default:
		return false
	}
}

func mergeChangedFiles(existing []string, incoming []string) []string {
	mergedSet := make(map[string]struct{}, len(existing)+len(incoming))
	for _, path := range existing {
		clean := filepath.ToSlash(filepath.Clean(path))
		if clean == "" || clean == "." {
			continue
		}
		mergedSet[clean] = struct{}{}
	}
	for _, path := range incoming {
		clean := filepath.ToSlash(filepath.Clean(path))
		if clean == "" || clean == "." {
			continue
		}
		mergedSet[clean] = struct{}{}
	}

	merged := make([]string, 0, len(mergedSet))
	for path := range mergedSet {
		merged = append(merged, path)
	}
	sort.Strings(merged)
	return merged
}

func detectPrototypeChanged(changedFiles []string) bool {
	for _, path := range changedFiles {
		clean := filepath.ToSlash(filepath.Clean(path))
		if clean == "prototype/index.html" || strings.HasPrefix(clean, "prototype/assets/") {
			return true
		}
	}
	return false
}

func prototypeContainsMediaPlaceholders(projectPath string) bool {
	indexPath := filepath.Join(projectPath, "prototype", "index.html")
	content, err := os.ReadFile(indexPath)
	if err != nil {
		return false
	}

	html := string(content)
	return len(extractImagePlaceholders(html)) > 0 ||
		strings.Contains(html, "glowbyvideo:") ||
		strings.Contains(html, "glowbyaudio:")
}

// sendSSEData sends a JSON object as SSE data.
// Recovers from panics caused by writing to a closed connection.
func sendSSEData(w http.ResponseWriter, flusher http.Flusher, data map[string]interface{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[OPENCODE] sendSSEData: recovered from write panic (client disconnected): %v", r)
		}
	}()
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", string(jsonData))
	flusher.Flush()
}

func (d *OpenCodeDriver) resolveQuestionEndpoint(_ context.Context) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.questionReplyPath == "" {
		d.questionReplyPath = "/question/{requestID}/reply"
	}
	return d.questionReplyPath
}

func (d *OpenCodeDriver) hasRecentQuestionReply(questionID string) bool {
	if questionID == "" {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Keep map bounded in long-running sessions.
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, at := range d.repliedQuestions {
		if at.Before(cutoff) {
			delete(d.repliedQuestions, id)
		}
	}

	_, exists := d.repliedQuestions[questionID]
	return exists
}

func (d *OpenCodeDriver) beginQuestionReply(questionID string) bool {
	if questionID == "" {
		return true
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	cutoff := time.Now().Add(-10 * time.Minute)
	for id, at := range d.repliedQuestions {
		if at.Before(cutoff) {
			delete(d.repliedQuestions, id)
		}
	}

	if _, exists := d.repliedQuestions[questionID]; exists {
		return false
	}

	// Mark as in-flight immediately so concurrent duplicate submissions are ignored.
	d.repliedQuestions[questionID] = time.Now()
	return true
}

func (d *OpenCodeDriver) markQuestionReplied(questionID string) {
	if questionID == "" {
		return
	}
	d.mu.Lock()
	d.repliedQuestions[questionID] = time.Now()
	d.mu.Unlock()
}

func (d *OpenCodeDriver) clearQuestionReply(questionID string) {
	if questionID == "" {
		return
	}
	d.mu.Lock()
	delete(d.repliedQuestions, questionID)
	d.mu.Unlock()
}

func (d *OpenCodeDriver) respondToQuestion(ctx context.Context, sessionID, questionID, answer, projectDir string, answers [][]string, answersByID map[string][]string) error {
	if d.hasRecentQuestionReply(questionID) {
		log.Printf("[OPENCODE] Skipping duplicate reply for question %s", questionID)
		return nil
	}
	if !d.beginQuestionReply(questionID) {
		log.Printf("[OPENCODE] Skipping in-flight duplicate reply for question %s", questionID)
		return nil
	}
	success := false
	defer func() {
		// If we fail, clear so a legitimate retry can be attempted.
		if !success {
			d.clearQuestionReply(questionID)
		}
	}()

	endpoint := ""
	if questionID != "" {
		endpoint = d.resolveQuestionEndpoint(ctx)
	}
	if endpoint != "" {
		path := endpoint
		path = strings.ReplaceAll(path, "{questionID}", questionID)
		path = strings.ReplaceAll(path, "{id}", questionID)
		path = strings.ReplaceAll(path, "{requestID}", questionID)
		path = strings.ReplaceAll(path, ":questionID", questionID)
		path = strings.ReplaceAll(path, ":id", questionID)
		path = strings.ReplaceAll(path, ":requestID", questionID)
		if strings.Contains(path, "{") {
			path = strings.TrimRight(path, "/") + "/" + questionID
		}
		url := fmt.Sprintf("%s%s", d.serverURL, path)
		if projectDir != "" {
			if parsedURL, err := neturl.Parse(url); err == nil {
				query := parsedURL.Query()
				query.Set("directory", projectDir)
				parsedURL.RawQuery = query.Encode()
				url = parsedURL.String()
			}
		}
		payloads := buildQuestionReplyPayloads(sessionID, questionID, answer, answers, answersByID)
		if len(payloads) == 0 {
			return fmt.Errorf("question reply payload is empty")
		}

		payload := payloads[0]
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		applyOpenCodeServerAuthorization(req)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		respBlob, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		respBody := strings.TrimSpace(string(respBlob))

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			d.markQuestionReplied(questionID)
			success = true
			return nil
		}
		log.Printf("[OPENCODE] Question reply payload keys: %v", mapKeys(payload))
		if respBody != "" {
			log.Printf("[OPENCODE] Question reply returned status %d for %s: %s", resp.StatusCode, path, respBody)
		} else {
			log.Printf("[OPENCODE] Question reply returned status %d for %s", resp.StatusCode, path)
		}
		return fmt.Errorf("question reply rejected (status %d)", resp.StatusCode)
	}

	// Legacy fallback for cases where questionID isn't available.
	params := opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Type: opencode.F(opencode.TextPartInputTypeText),
				Text: opencode.F(answer),
			},
		}),
	}
	if projectDir != "" {
		params.Directory = opencode.F(projectDir)
	}
	_, err := d.sendSessionPrompt(ctx, sessionID, params)
	if err == nil {
		d.markQuestionReplied(questionID)
		success = true
		return nil
	}
	return err
}

func buildQuestionReplyPayloads(sessionID, questionID, answer string, answers [][]string, answersByID map[string][]string) []map[string]interface{} {
	normalizedAnswers := answers
	if len(normalizedAnswers) == 0 && len(answersByID) > 0 {
		if questionID != "" {
			if selected, ok := answersByID[questionID]; ok {
				normalizedAnswers = append(normalizedAnswers, selected)
			}
		}
		if len(normalizedAnswers) == 0 {
			keys := make([]string, 0, len(answersByID))
			for key := range answersByID {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				normalizedAnswers = append(normalizedAnswers, answersByID[key])
			}
		}
	}
	if len(normalizedAnswers) == 0 && answer != "" {
		normalizedAnswers = [][]string{{answer}}
	}

	// OpenCode expects: { "answers": [["label-1"], ["label-2","label-3"]] }
	// (array of answer arrays, in order of asked questions).
	payload := map[string]interface{}{"answers": normalizedAnswers}
	if sessionID != "" {
		payload["sessionID"] = sessionID
		payload["sessionId"] = sessionID
	}
	if questionID != "" {
		payload["requestID"] = questionID
		payload["questionID"] = questionID
		payload["id"] = questionID
	}
	return []map[string]interface{}{payload}
}

func copyStringMap(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func mapKeys(src map[string]interface{}) []string {
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (d *OpenCodeDriver) resolvePermissionEndpoint(ctx context.Context) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.permissionReplyPath == "" {
		d.permissionReplyPath = "/permission/{requestID}/reply"
	}
	return d.permissionReplyPath
}

func (d *OpenCodeDriver) respondToPermission(ctx context.Context, sessionID, permissionID, response, projectDir string) error {
	endpoint := d.resolvePermissionEndpoint(ctx)
	if endpoint != "" {
		path := endpoint
		path = strings.ReplaceAll(path, "{requestID}", permissionID)
		path = strings.ReplaceAll(path, "{permissionID}", permissionID)
		path = strings.ReplaceAll(path, ":requestID", permissionID)
		path = strings.ReplaceAll(path, ":permissionID", permissionID)
		if strings.Contains(path, "{") {
			// Fallback to append if token replacement failed
			path = strings.TrimRight(path, "/") + "/" + permissionID
		}

		url := fmt.Sprintf("%s%s", d.serverURL, path)
		if projectDir != "" {
			if parsedURL, err := neturl.Parse(url); err == nil {
				query := parsedURL.Query()
				query.Set("directory", projectDir)
				parsedURL.RawQuery = query.Encode()
				url = parsedURL.String()
			}
		}
		payload := map[string]string{
			"reply":    response,
			"response": response,
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			applyOpenCodeServerAuthorization(req)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
			}
		}
	}

	params := opencode.SessionPermissionRespondParams{
		Response: opencode.F(opencode.SessionPermissionRespondParamsResponse(response)),
	}
	if projectDir != "" {
		params.Directory = opencode.F(projectDir)
	}
	_, err := d.client.Session.Permissions.Respond(ctx, sessionID, permissionID, params)
	return err
}
