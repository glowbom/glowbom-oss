package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
)

func TestV2CatalogWaitsForProvidersAndModels(t *testing.T) {
	providerCalls, modelCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("location[directory]") != "/fixture" {
			t.Errorf("catalog request lost its project location: %s", r.URL.String())
		}
		switch r.URL.Path {
		case "/api/provider":
			providerCalls++
			if providerCalls < 3 {
				io.WriteString(w, `{"data":[]}`)
				return
			}
			io.WriteString(w, `{"data":[{"id":"opencode","name":"OpenCode","activation":"auto"}]}`)
		case "/api/model":
			modelCalls++
			if modelCalls < 4 {
				io.WriteString(w, `{"data":[]}`)
				return
			}
			io.WriteString(w, `{"data":[{"id":"big-pickle","providerID":"opencode","name":"Big Pickle","enabled":true}]}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	providers, err := NewOpenCodeDriver(server.URL).client.App.Providers(ctx, opencode.AppProvidersParams{Directory: opencode.F("/fixture")})
	if err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 1 || providers.Providers[0].ID != "opencode" || providers.Providers[0].Models["big-pickle"].Name != "Big Pickle" {
		t.Fatalf("initial catalog was returned before it loaded: %#v", providers)
	}
	if providerCalls != 4 || modelCalls != 4 {
		t.Fatalf("catalog reads = %d providers, %d models; want four of each", providerCalls, modelCalls)
	}
}

func TestV2CatalogWaitHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	setOpenCodeProtocol(server.URL, "v2")
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err := NewOpenCodeDriver(server.URL).client.App.Providers(ctx, opencode.AppProvidersParams{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("catalog wait ignored cancellation: %v", err)
	}
}

func TestV2CatalogDoesNotRetryAuthFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":"catalog authentication rejected"}`)
			}))
			defer server.Close()
			setOpenCodeProtocol(server.URL, "v2")
			_, err := NewOpenCodeDriver(server.URL).client.App.Providers(context.Background(), opencode.AppProvidersParams{})
			if err == nil || calls != 1 {
				t.Fatalf("authentication failure: err=%v, requests=%d", err, calls)
			}
		})
	}
}
