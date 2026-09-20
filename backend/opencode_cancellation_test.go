package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

func TestOpenCodeStreamCancellationAbortsWorker(t *testing.T) {
	connected := make(chan struct{})
	aborted := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(connected)
			<-r.Context().Done()
		case "/session/session-test/abort":
			if r.Method != "POST" {
				t.Errorf("abort method = %s", r.Method)
			}
			aborted <- r.URL.Query().Get("directory")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "true")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	driver := &OpenCodeDriver{client: opencode.NewClient(option.WithBaseURL(server.URL))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		w := httptest.NewRecorder()
		completed, _, _, _ := driver.streamEventsAndWaitForCompletion(ctx, w, w, "/test-project", "session-test", nil)
		done <- completed
	}()
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not connect")
	}
	cancel()
	select {
	case dir := <-aborted:
		if dir != "/test-project" {
			t.Fatalf("wrong abort directory %q", dir)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not abort the OpenCode worker")
	}
	select {
	case completed := <-done:
		if completed {
			t.Fatal("cancel reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not stop")
	}
}

func TestOpenCodeCompletedStreamDoesNotAbortWorker(t *testing.T) {
	aborted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"session-test\"}}\n\n")
			return
		}
		if r.URL.Path == "/session/session-test/abort" {
			aborted <- struct{}{}
			fmt.Fprint(w, "true")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	driver := &OpenCodeDriver{client: opencode.NewClient(option.WithBaseURL(server.URL))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatched := make(chan struct{})
	close(dispatched)
	w := httptest.NewRecorder()
	completed, _, _, _ := driver.streamEventsAndWaitForCompletion(ctx, w, w, "/test-project", "session-test", dispatched)
	if !completed {
		t.Fatal("idle session should complete")
	}
	cancel()
	select {
	case <-aborted:
		t.Fatal("completed session aborted when HTTP request ended")
	case <-time.After(100 * time.Millisecond):
	}
}
