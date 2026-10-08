package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type experientialTransport func(*http.Request) (*http.Response, error)

func (f experientialTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const experientialModelFixture = `{"model":{"slug":"coding-model","display_name":"Coding model","status":"active","context_window":128000,"max_output_tokens":8192,"input_modalities":["text","image"],"output_modalities":["text"],"supported_params":{"tools":true}}}`

func TestExperientialCatalogPaginationAndCapabilities(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: experientialTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.experientiallabs.ai" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatal("incorrect catalog destination or authentication")
		}
		body := `{"total":2,"models":[` + experientialModelFixture + `]}`
		if calls == 2 {
			if r.URL.Query().Get("offset") != "1" {
				t.Fatal("pagination did not advance")
			}
			body = `{"total":2,"models":[{"model":{"slug":"embedding","status":"active"}}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	config, err := experientialProviderConfig(context.Background(), client, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config)
	text := string(data)
	if calls != 2 || strings.Contains(text, "embedding") || strings.Contains(text, "test-key") || !strings.Contains(text, `"context":128000`) || !strings.Contains(text, `"image"`) || !strings.Contains(text, experientialBaseURL) {
		t.Fatal("incorrect provider configuration", text)
	}
}

func TestExperientialCatalogRejectsFailures(t *testing.T) {
	for _, body := range []string{`{"error":"test-key"}`, `{"total":0,"models":[]}`, `{"total":5,"models":[]}`, `not json`} {
		client := &http.Client{Transport: experientialTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		_, err := experientialProviderConfig(context.Background(), client, "test-key")
		if err == nil || strings.Contains(err.Error(), "test-key") {
			t.Fatal("failed catalog must produce a safe error")
		}
	}
}

func TestExperientialConnectionConfiguresBeforeSavingKey(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "local-token")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	t.Setenv("OPENCODE_URL", "")
	previous := experientialCatalogClient
	defer func() { experientialCatalogClient = previous }()
	experientialCatalogClient = &http.Client{Transport: experientialTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"total":1,"models":[` + experientialModelFixture + `]}`)), Header: make(http.Header)}, nil
	})}
	for _, failConfig := range []bool{false, true} {
		calls := []string{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method+" "+r.URL.Path)
			switch r.URL.Path {
			case "/session/status":
				io.WriteString(w, `{}`)
			case "/global/config":
				data, _ := io.ReadAll(r.Body)
				if strings.Contains(string(data), "test-key") || !strings.Contains(string(data), "@ai-sdk/openai-compatible") {
					t.Error("unsafe or invalid config")
				}
				if failConfig {
					http.Error(w, "test-key", 500)
					return
				}
				io.WriteString(w, `{}`)
			case "/auth/explabs":
				var auth map[string]string
				json.NewDecoder(r.Body).Decode(&auth)
				if auth["key"] != "test-key" {
					t.Error("key not sent to OpenCode credential store")
				}
				io.WriteString(w, `true`)
			case "/instance/dispose":
				io.WriteString(w, `true`)
			default:
				t.Error("unexpected request", r.URL.Path)
			}
		}))
		chat := &chatService{directory: t.TempDir(), serverURL: server.URL, client: server.Client(), prepare: func() error { return nil }}
		r := httptest.NewRequest("POST", "/settings/providers/connect", strings.NewReader(`{"provider":"explabs","apiKey":"test-key"}`))
		r.Header.Set("Authorization", "Bearer local-token")
		w := httptest.NewRecorder()
		providerConnectionHandler(chat)(w, r)
		server.Close()
		want, count := 200, 4
		if failConfig {
			want, count = 502, 2
		}
		if w.Code != want || len(calls) != count || strings.Contains(w.Body.String(), "test-key") {
			t.Fatal("incorrect connection result", w.Code, calls, w.Body.String())
		}
		if strings.Join(calls[:2], ",") != "GET /session/status,PATCH /global/config" {
			t.Fatal("incorrect setup order", calls)
		}
	}
}

func TestExperientialModelPassesThroughBuild(t *testing.T) {
	for _, provider := range []string{"explabs", "opencode-go"} {
		model, gotProvider := parseModel(provider + "/coding-model")
		if model != "coding-model" || gotProvider != provider {
			t.Fatal("provider was rewritten", model, gotProvider)
		}
	}
}
