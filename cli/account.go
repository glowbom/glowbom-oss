package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"
)

type accountConfig struct {
	APIURL   string
	LoginURL string
}

type accountCredentials struct {
	IDToken      string `json:"idToken"`
	RefreshToken string `json:"refreshToken"`
	UID          string `json:"uid"`
	Email        string `json:"email"`
	ExpiresIn    int    `json:"expiresIn"`
	ExpiresAt    int64  `json:"expiresAt"`
}

type accountStore interface {
	Load() (accountCredentials, error)
	Save(accountCredentials) error
	Delete() error
}

type accountClient struct {
	config accountConfig
	http   *http.Client
	store  accountStore
	out    io.Writer
	open   func(context.Context, string) error
}

type accountAPIError struct {
	status int
	code   string
}

func (e *accountAPIError) Error() string {
	switch e.code {
	case "access_denied":
		return "connection canceled in the browser"
	case "expired_session", "expired_code", "session_used", "unknown_session", "session_expired", "unauthorized":
		return "sign-in expired or was already used; run glowbom login again"
	case "account_not_found":
		return "your account is connected, but its allowance is not ready yet"
	case "login_unavailable":
		return "CLI sign-in is unavailable; please try again later"
	}
	if e.status == 429 {
		return "too many requests; wait a minute and try again"
	}
	return fmt.Sprintf("Glowbom could not complete the request (HTTP %d)", e.status)
}

func loopbackURL(u *url.URL) bool {
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}

func loadAccountConfig() (accountConfig, error) {
	cfg := accountConfig{
		APIURL:   strings.TrimRight(os.Getenv("GLOWBOM_ACCOUNT_API_URL"), "/"),
		LoginURL: os.Getenv("GLOWBOM_LOGIN_URL"),
	}
	local := os.Getenv("GLOWBOM_AUTH_LOCAL") == "1"
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.glowbom.com"
	}
	if cfg.LoginURL == "" {
		cfg.LoginURL = "https://glowbom.com/draw/"
	}
	for _, raw := range []string{cfg.APIURL, cfg.LoginURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return cfg, errors.New("account URLs must be absolute URLs without credentials, queries, or fragments")
		}
		if local {
			if u.Scheme != "http" || !loopbackURL(u) {
				return cfg, errors.New("local authentication requires both account URLs to use HTTP on loopback")
			}
		} else if u.Scheme != "https" {
			return cfg, errors.New("account URLs must use HTTPS; local emulator testing requires GLOWBOM_AUTH_LOCAL=1")
		}
	}
	return cfg, nil
}

func newAccountClient() (*accountClient, error) {
	cfg, err := loadAccountConfig()
	if err != nil {
		return nil, err
	}
	store, err := newCredentialStore(cfg.APIURL)
	if err != nil {
		return nil, err
	}
	return &accountClient{config: cfg, store: store, out: os.Stdout, open: openLoginBrowser,
		http: &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}}}, nil
}

func (c *accountClient) request(ctx context.Context, path, token string, body any, result any) error {
	method := http.MethodGet
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return errors.New("could not prepare the sign-in request")
		}
		method = http.MethodPost
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.config.APIURL+path, input)
	if err != nil {
		return errors.New("invalid account API address")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return errors.New("could not reach Glowbom; check your connection and retry the command")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return errors.New("Glowbom returned an unreadable response; retry the command")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &failure)
		return &accountAPIError{status: response.StatusCode, code: failure.Code}
	}
	if result != nil && json.Unmarshal(data, result) != nil {
		return errors.New("Glowbom returned an unexpected response")
	}
	return nil
}

func (c *accountClient) login(ctx context.Context, noBrowser, deviceAuth bool) error {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return errors.New("could not create a secure login request")
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challenge := sha256.Sum256([]byte(verifier))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	start := map[string]string{
		"action": "start", "mode": "device", "codeChallenge": base64.RawURLEncoding.EncodeToString(challenge[:]),
	}
	var callback *loginCallback
	if !deviceAuth {
		stateBytes := make([]byte, 32)
		if _, err := rand.Read(stateBytes); err != nil {
			return errors.New("could not create a secure login request")
		}
		var err error
		callback, err = newLoginCallback(base64.RawURLEncoding.EncodeToString(stateBytes))
		if err != nil {
			return err
		}
		defer callback.close()
		start["mode"] = "callback"
		start["state"] = callback.state
		start["redirectUri"] = callback.redirectURI()
	}
	var session struct {
		Mode       string `json:"mode"`
		SessionID  string `json:"sessionId"`
		DeviceCode string `json:"deviceCode"`
		UserCode   string `json:"userCode"`
		ExpiresIn  int    `json:"expiresIn"`
		Interval   int    `json:"interval"`
	}
	if err := c.request(ctx, "/cliAuth", "", start, &session); err != nil {
		return err
	}
	if len(session.SessionID) != 32 || len(session.DeviceCode) != 43 || len(session.UserCode) != 9 ||
		session.ExpiresIn <= 0 || session.ExpiresIn > 600 || session.Interval < 1 || session.Interval > 30 {
		return errors.New("Glowbom returned an invalid login session")
	}
	if (!deviceAuth && session.Mode != "callback") || (deviceAuth && session.Mode != "" && session.Mode != "device") {
		return errors.New("Glowbom does not support this login mode; update the login service before trying again")
	}
	loginURL, _ := url.Parse(c.config.LoginURL)
	query := loginURL.Query()
	query.Set("flow", "cli")
	query.Set("session", session.SessionID)
	loginURL.RawQuery = query.Encode()
	ctx, expire := context.WithTimeout(ctx, time.Duration(session.ExpiresIn)*time.Second)
	defer expire()
	proof := map[string]string{"action": "exchange", "sessionId": session.SessionID,
		"deviceCode": session.DeviceCode, "codeVerifier": verifier}
	if callback != nil {
		callback.serve(ctx, func(code string) error {
			proof["authorizationCode"] = code
			proof["redirectUri"] = callback.redirectURI()
			var result struct {
				Status string `json:"status"`
				accountCredentials
			}
			if err := c.request(ctx, "/cliAuth", "", proof, &result); err != nil {
				return err
			}
			if result.Status != "authorized" || !result.accountCredentials.valid() {
				return errors.New("sign-in did not complete; run glowbom login again")
			}
			return c.saveLogin(ctx, result.accountCredentials, proof)
		})
	}
	fmt.Fprintf(c.out, "Open this link to connect your Glowbom account:\n%s\n\n", loginURL.String())
	if deviceAuth {
		fmt.Fprintf(c.out, "Enter this code in the browser: %s\n", session.UserCode)
	} else {
		fmt.Fprintln(c.out, "Use a browser on this computer. For a remote terminal, use glowbom login --device-auth.")
	}
	fmt.Fprintln(c.out, "Only approve a login you started. Waiting for approval...")
	var browserOpened <-chan error
	if !noBrowser {
		opened := make(chan error, 1)
		browserOpened = opened
		go func() { opened <- c.open(ctx, loginURL.String()) }()
	}
	if callback != nil {
		for {
			select {
			case err := <-callback.result:
				return err
			case err := <-browserOpened:
				browserOpened = nil
				if err != nil {
					fmt.Fprintln(c.out, "The browser could not open automatically. Open the link above to continue.")
				}
			case <-ctx.Done():
				select {
				case err := <-callback.result:
					return err
				default:
					return errors.New("login canceled or expired; run glowbom login again")
				}
			}
		}
	}
	interval := time.Duration(session.Interval) * time.Second
	for {
		select {
		case <-ctx.Done():
			return errors.New("login canceled or expired; run glowbom login again")
		case err := <-browserOpened:
			browserOpened = nil
			if err != nil {
				fmt.Fprintln(c.out, "The browser could not open automatically. Open the link above to continue.")
			}
			continue
		case <-time.After(interval):
		}
		var result struct {
			Status string `json:"status"`
			accountCredentials
		}
		err := c.request(ctx, "/cliAuth", "", proof, &result)
		if err != nil {
			var apiError *accountAPIError
			if errors.As(err, &apiError) && apiError.status == 429 {
				// Dashboard blocks can last a minute. Never hammer a throttled endpoint.
				interval = time.Minute
				continue
			}
			return err
		}
		if result.Status == "pending" {
			interval = time.Duration(session.Interval) * time.Second
			continue
		}
		if result.Status != "authorized" || !result.accountCredentials.valid() {
			return errors.New("sign-in did not complete; run glowbom login again")
		}
		return c.saveLogin(ctx, result.accountCredentials, proof)
	}
}

func (c *accountClient) saveLogin(ctx context.Context, credentials accountCredentials, proof map[string]string) error {
	credentials.ExpiresAt = time.Now().Add(time.Duration(credentials.ExpiresIn) * time.Second).Unix()
	if err := c.store.Save(credentials); err != nil {
		return err
	}
	proof["action"] = "complete"
	delete(proof, "authorizationCode")
	if err := c.request(ctx, "/cliAuth", credentials.IDToken, proof, nil); err != nil {
		fmt.Fprintln(c.out, "Credentials saved. The server confirmation could not be updated; you can close the browser.")
	}
	name := credentials.Email
	if name == "" {
		name = credentials.UID
	}
	fmt.Fprintf(c.out, "Connected to Glowbom as %s.\nRun glowbom account to see your remaining allowance.\n", name)
	return nil
}

func (credentials accountCredentials) valid() bool {
	return credentials.IDToken != "" && credentials.RefreshToken != "" && credentials.UID != "" &&
		credentials.ExpiresIn > 0 && credentials.ExpiresIn <= 86400
}

func (c *accountClient) refresh(ctx context.Context, credentials accountCredentials) (accountCredentials, error) {
	var updated accountCredentials
	if err := c.request(ctx, "/cliAuth", "", map[string]string{
		"action": "refresh", "refreshToken": credentials.RefreshToken,
	}, &updated); err != nil {
		return updated, err
	}
	if !updated.valid() || updated.UID != credentials.UID {
		return updated, errors.New("account verification failed; run glowbom login again")
	}
	updated.ExpiresAt = time.Now().Add(time.Duration(updated.ExpiresIn) * time.Second).Unix()
	return updated, c.store.Save(updated)
}

func (c *accountClient) account(ctx context.Context, forceRefresh bool) error {
	credentials, err := c.store.Load()
	if err != nil {
		return err
	}
	if !credentials.valid() {
		return errors.New("saved sign-in is invalid; run glowbom login again")
	}
	refreshed := forceRefresh || credentials.ExpiresAt <= time.Now().Add(time.Minute).Unix()
	if refreshed {
		credentials, err = c.refresh(ctx, credentials)
		if err != nil {
			return err
		}
	}
	var result struct {
		SubscriptionStatus string  `json:"subscriptionStatus"`
		RemainingUSD       float64 `json:"remainingUsd"`
		AllowanceUSD       float64 `json:"allowanceUsd"`
	}
	err = c.request(ctx, "/account", credentials.IDToken, nil, &result)
	var apiError *accountAPIError
	if !refreshed && errors.As(err, &apiError) && apiError.status == 401 {
		credentials, err = c.refresh(ctx, credentials)
		if err == nil {
			err = c.request(ctx, "/account", credentials.IDToken, nil, &result)
		}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Account: %s\nSubscription: %s\nRemaining allowance: $%.4f of $%.2f\n",
		credentials.Email, result.SubscriptionStatus, result.RemainingUSD, result.AllowanceUSD)
	return nil
}

func runAccountCommand(command string, args []string) int {
	flags := flag.NewFlagSet("glowbom "+command, flag.ContinueOnError)
	noBrowser, deviceAuth, forceRefresh := false, false, false
	if command == "login" {
		flags.BoolVar(&noBrowser, "no-browser", false, "Print the login link without opening a browser")
		flags.BoolVar(&deviceAuth, "device-auth", false, "Use a terminal code and polling for a remote machine")
	}
	if command == "account" {
		flags.BoolVar(&forceRefresh, "refresh", false, "Refresh the session before reading the allowance")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "This command takes no positional arguments.")
		return 2
	}
	c, err := newAccountClient()
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		switch command {
		case "login":
			err = c.login(ctx, noBrowser, deviceAuth)
		case "account":
			err = c.account(ctx, forceRefresh)
		case "logout":
			err = c.store.Delete()
			if err == nil {
				fmt.Fprintln(c.out, "Signed out of Glowbom on this computer. Your browser remains signed in.")
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", err)
		return 1
	}
	return 0
}

func openLoginBrowser(ctx context.Context, address string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", address)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", address)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", address)
	}
	return cmd.Run()
}
