package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendSessionPromptAsyncUsesRunModelAndReturnsOnAcceptance(t *testing.T) {
	project := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session/ses_build/prompt_async" || r.URL.Query().Get("directory") != project {
			t.Errorf("unexpected prompt request: %s %s", r.Method, r.URL.String())
		}
		var payload struct {
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
			Model map[string]string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode prompt: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(payload.Parts) != 1 || payload.Parts[0].Type != "text" || payload.Parts[0].Text != "Build the welcome screen" {
			t.Errorf("wrong prompt parts: %#v", payload.Parts)
		}
		if payload.Model["providerID"] != "xai" || payload.Model["modelID"] != "grok-4.7" {
			t.Errorf("wrong run model: %#v", payload.Model)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	driver := &OpenCodeDriver{serverURL: server.URL}
	if err := driver.sendSessionPromptAsync(context.Background(), project, "ses_build", "Build the welcome screen", "xai", "grok-4.7"); err != nil {
		t.Fatal(err)
	}
}
