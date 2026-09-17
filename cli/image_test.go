package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func generationTestPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImageGenerationSavesReferencesAndReportsCost(t *testing.T) {
	pngData := generationTestPNG(t)
	var calls, downloads int
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/generateImage":
			calls++
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer private-id-token" {
				t.Error("missing authenticated generation")
			}
			var body struct {
				Prompt       string   `json:"prompt"`
				ImageRefs    []string `json:"imageRefs"`
				OutputFormat string   `json:"outputFormat"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Prompt != "a friendly robot" || body.OutputFormat != "png" || len(body.ImageRefs) != 2 {
				t.Errorf("wrong request: prompt=%q format=%q refs=%d", body.Prompt, body.OutputFormat, len(body.ImageRefs))
			}
			for _, ref := range body.ImageRefs {
				if !strings.HasPrefix(ref, "data:image/png;base64,") {
					t.Error("reference was not encoded")
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"image": server.URL + "/result?private-signed-query", "usage": map[string]any{"status": "settled", "cost": 0.0123}})
		case "/result":
			downloads++
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("credentials leaked to image host")
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngData)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	ref := filepath.Join(dir, "reference.png")
	if err := os.WriteFile(ref, pngData, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "result.png")
	opts, err := parseImageOptions([]string{"a friendly robot", "--ref", ref, "--ref", ref, "--output", output})
	if err != nil {
		t.Fatal(err)
	}
	var terminal bytes.Buffer
	client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
	if err := client.generateImage(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, pngData) || calls != 1 || downloads != 1 {
		t.Fatalf("wrong saved result: err=%v generation=%d downloads=%d", err, calls, downloads)
	}
	if text := terminal.String(); !strings.Contains(text, output) || !strings.Contains(text, "$0.0123") || strings.Contains(text, "private-") {
		t.Fatalf("bad terminal output: %s", text)
	}
}

func TestImageGenerationRefreshesBeforeSpendingAndRetriesOnlyAuthentication(t *testing.T) {
	for _, expired := range []bool{true, false} {
		t.Run(map[bool]string{true: "expired", false: "server-rejects-token"}[expired], func(t *testing.T) {
			pngData := generationTestPNG(t)
			calls, refreshes := 0, 0
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/cliAuth":
					refreshes++
					updated := testCredentials()
					updated.IDToken = "fresh-token"
					json.NewEncoder(w).Encode(updated)
				case "/generateImage":
					calls++
					if r.Header.Get("Authorization") != "Bearer fresh-token" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"image": server.URL + "/result", "usage": map[string]any{"status": "pending_reconciliation"}})
				case "/result":
					w.Write(pngData)
				}
			}))
			defer server.Close()
			creds := testCredentials()
			if expired {
				creds.ExpiresAt = time.Now().Add(-time.Minute).Unix()
			}
			store := &memoryAccountStore{value: creds}
			var terminal bytes.Buffer
			client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: store, out: &terminal}
			opts, _ := parseImageOptions([]string{"robot", "--output", t.TempDir()})
			if err := client.generateImage(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			expectedCalls := 2
			if expired {
				expectedCalls = 1
			}
			if calls != expectedCalls || refreshes != 1 || store.saves != 1 || !strings.Contains(terminal.String(), "pending confirmation") {
				t.Fatalf("unexpected refresh/cost handling: calls=%d refreshes=%d saves=%d", calls, refreshes, store.saves)
			}
		})
	}
}

func TestImageGenerationDoesNotRetryUncertainResultsOrLeakErrors(t *testing.T) {
	for _, response := range []struct {
		name   string
		status int
		body   string
	}{
		{"server-error", 500, `{"error":"private-id-token"}`},
		{"gateway-timeout", 504, "private-id-token"},
		{"invalid-json", 200, "private-id-token"},
		{"missing-image", 200, `{}`},
		{"rate-limit", 429, "private-id-token"},
		{"allowance", 403, `{"message":"private-id-token"}`},
		{"redirect", 302, ""},
	} {
		t.Run(response.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/should-not-follow")
				w.WriteHeader(response.status)
				w.Write([]byte(response.body))
			}))
			defer server.Close()
			var terminal bytes.Buffer
			client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
			dir := t.TempDir()
			opts, _ := parseImageOptions([]string{"robot", "--output", dir})
			err := client.generateImage(context.Background(), opts)
			files, _ := os.ReadDir(dir)
			if err == nil || calls != 1 || len(files) != 0 || strings.Contains(err.Error()+terminal.String(), "private-") {
				t.Fatalf("unsafe failure handling: err=%v calls=%d files=%d", err, calls, len(files))
			}
		})
	}
}

func TestImageGenerationValidatesLocalInputsBeforeAPIRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.png")
	os.WriteFile(existing, []byte("keep me"), 0600)
	for _, args := range [][]string{
		{"robot", "--output", existing},
		{"robot", "--output", filepath.Join(dir, "missing", "out.png")},
		{"robot", "--output", dir, "--ref", filepath.Join(dir, "absent.png")},
	} {
		opts, _ := parseImageOptions(args)
		var terminal bytes.Buffer
		client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
		if err := client.generateImage(context.Background(), opts); err == nil {
			t.Fatal("accepted invalid local inputs")
		}
	}
	if calls != 0 {
		t.Fatalf("made %d requests for invalid local inputs", calls)
	}
	got, _ := os.ReadFile(existing)
	files, _ := os.ReadDir(dir)
	if string(got) != "keep me" || len(files) != 1 {
		t.Fatal("changed existing file or left temporary files")
	}
}

func TestImageCommandHelpAndInvalidOptionsDoNotNeedLogin(t *testing.T) {
	if run([]string{"generate-image", "--help"}) != 0 {
		t.Fatal("image help failed")
	}
	if run([]string{"generate-image"}) != 2 || run([]string{"generate-image", "robot", "--unknown"}) != 2 {
		t.Fatal("invalid arguments were accepted")
	}
}

func TestImageGenerationDoesNotReplayAfterConnectionLoss(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	dir := t.TempDir()
	opts, _ := parseImageOptions([]string{"robot", "--output", dir})
	var terminal bytes.Buffer
	client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
	err := client.generateImage(context.Background(), opts)
	files, _ := os.ReadDir(dir)
	if err == nil || calls.Load() != 1 || len(files) != 0 || !strings.Contains(err.Error(), "allowance may have been used") {
		t.Fatalf("bad disconnect handling: err=%v calls=%d files=%d", err, calls.Load(), len(files))
	}
}

func TestImageGenerationCancellationDoesNotReplay(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before-request", false: "during-request"}[before], func(t *testing.T) {
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				cancel()
				w.WriteHeader(504)
			}))
			defer server.Close()
			if before {
				cancel()
			}
			dir := t.TempDir()
			opts, _ := parseImageOptions([]string{"robot", "--output", dir})
			var terminal bytes.Buffer
			client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
			err := client.generateImage(ctx, opts)
			files, _ := os.ReadDir(dir)
			wantCalls := int32(1)
			if before {
				wantCalls = 0
			}
			if err == nil || calls.Load() != wantCalls || len(files) != 0 {
				t.Fatalf("bad cancel handling: err=%v calls=%d files=%d", err, calls.Load(), len(files))
			}
		})
	}
}

func TestImageGenerationRefreshesAtMostOnce(t *testing.T) {
	calls, refreshes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cliAuth" {
			refreshes++
			json.NewEncoder(w).Encode(testCredentials())
			return
		}
		calls++
		w.WriteHeader(401)
	}))
	defer server.Close()
	dir := t.TempDir()
	opts, _ := parseImageOptions([]string{"robot", "--output", dir})
	var terminal bytes.Buffer
	client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
	err := client.generateImage(context.Background(), opts)
	if err == nil || calls != 2 || refreshes != 1 {
		t.Fatalf("bad auth retry behavior: err=%v calls=%d refreshes=%d", err, calls, refreshes)
	}
}

func TestImageGenerationReportsCostWithoutRegeneratingOnDownloadFailure(t *testing.T) {
	calls := 0
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/generateImage" {
			calls++
			json.NewEncoder(w).Encode(map[string]any{"image": server.URL + "/result?private-query", "usage": map[string]any{"status": "settled", "cost": 0.03}})
			return
		}
		w.WriteHeader(500)
		w.Write([]byte("private-error"))
	}))
	defer server.Close()
	dir := t.TempDir()
	opts, _ := parseImageOptions([]string{"robot", "--output", dir})
	var terminal bytes.Buffer
	client := &accountClient{config: accountConfig{APIURL: server.URL}, http: server.Client(), store: &memoryAccountStore{value: testCredentials()}, out: &terminal}
	err := client.generateImage(context.Background(), opts)
	files, _ := os.ReadDir(dir)
	if err == nil || calls != 1 || len(files) != 0 || !strings.Contains(err.Error(), "saving failed") || !strings.Contains(terminal.String(), "$0.0300") {
		t.Fatalf("bad download failure: err=%v calls=%d files=%d output=%s", err, calls, len(files), terminal.String())
	}
	if strings.Contains(err.Error()+terminal.String(), "private-") {
		t.Fatal("download error leaked server details")
	}
}
