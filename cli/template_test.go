package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func templateTestEntries() []archiveTestEntry {
	return []archiveTestEntry{
		{name: "glowbom.json", data: []byte(`{"name":"Glowbom starter"}`)},
		{name: "AGENTS.md", data: []byte("Project instructions")},
		{name: "android/", mode: os.ModeDir | 0755},
		{name: "android/gradlew", data: []byte("#!/bin/sh\necho starter\n"), mode: 0755},
		{name: "web/app/page.tsx", data: []byte("export default function Page() {}")},
		{name: "__MACOSX/", mode: os.ModeDir | 0755},
		{name: "__MACOSX/._glowbom.json", data: []byte("metadata")},
		{name: "web/.DS_Store", data: []byte("metadata")},
	}
}

type templateRoundTripper func(*http.Request) (*http.Response, error)

func (transport templateRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

// Keep the production starter URL and redirect checks, but send test requests
// only to this test's local HTTPS server.
func templateTestClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	transport := client.Transport
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = templateRoundTripper(func(request *http.Request) (*http.Response, error) {
		local := request.Clone(request.Context())
		copyURL := *request.URL
		copyURL.Scheme = address.Scheme
		copyURL.Host = address.Host
		local.URL = &copyURL
		return transport.RoundTrip(local)
	})
	return client
}

func TestTemplateCommandParsesOutputAndHelpWithoutAccount(t *testing.T) {
	for _, args := range [][]string{nil, {"--output", "new folder"}, {"--output=new folder"}} {
		opts, err := parseTemplateOptions(args)
		if err != nil || (len(args) > 0 && opts.Output != "new folder") {
			t.Fatalf("could not parse options %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"unexpected"}, {"--output"}, {"--output="}, {"--output=a", "--output=b"}, {"--url=https://example.com"}} {
		if _, err := parseTemplateOptions(args); err == nil {
			t.Fatalf("accepted unsupported options %v", args)
		}
	}
	t.Setenv("GLOWBOM_ACCOUNT_API_URL", "invalid and should not be used")
	t.Setenv("GLOWBOM_CREDENTIAL_STORE", "invalid and should not be used")
	if code := run([]string{"template", "--help"}); code != 0 {
		t.Fatalf("template help returned %d", code)
	}
	if !strings.Contains(usage, "glowbom template") {
		t.Fatal("template missing from main help")
	}
}

func TestTemplateDownloadsCurrentStarterWithoutCredentials(t *testing.T) {
	data := archiveTestZIP(t, templateTestEntries())
	requests := 0
	client := templateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("starter download sent credentials")
		}
		if r.Header.Get("Cache-Control") != "no-cache" {
			t.Error("starter download did not request fresh content")
		}
		if r.URL.RawQuery == "" {
			http.Redirect(w, r, templateURL+"?version=current", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	})
	jar, _ := cookiejar.New(nil)
	address, _ := url.Parse(templateURL)
	jar.SetCookies(address, []*http.Cookie{{Name: "private", Value: "must-not-send"}})
	client.Jar = jar
	client.Timeout = time.Second
	var terminal bytes.Buffer
	parent := t.TempDir()
	target := filepath.Join(parent, "starter")
	if err := createTemplate(context.Background(), pullOptions{Output: target}, client, &terminal); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || client.Jar != jar || client.Timeout != time.Second || client.CheckRedirect != nil {
		t.Fatal("wrong request count or input HTTP client was modified")
	}
	for _, name := range []string{"glowbom.json", "AGENTS.md", "android/gradlew", "web/app/page.tsx"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(name))); err != nil {
			t.Fatalf("missing starter file %s: %v", name, err)
		}
	}
	for _, name := range []string{"__MACOSX", "web/.DS_Store"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("metadata was not removed: %s", name)
		}
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(target, "android", "gradlew"))
		if info.Mode().Perm() != 0755 {
			t.Fatalf("Gradle launcher mode = %o", info.Mode().Perm())
		}
	}
	children, _ := os.ReadDir(parent)
	if len(children) != 1 || !strings.Contains(terminal.String(), "Downloaded 4 files") {
		t.Fatalf("unexpected output or leftovers: %q", terminal.String())
	}
}

func TestTemplateAcceptsUpdatedStarterAtTheSameURL(t *testing.T) {
	first := templateTestEntries()
	second := templateTestEntries()
	second[0].data = []byte(`{"name":"Updated Glowbom starter","version":"next"}`)
	second = append(second, archiveTestEntry{name: "README.md", data: []byte("New starter instructions")})
	versions := [][]byte{archiveTestZIP(t, first), archiveTestZIP(t, second)}
	var requests atomic.Int32
	client := templateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/starter.zip" || r.URL.RawQuery != "" {
			t.Error("did not fetch the same shared starter URL")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("sent account credentials")
		}
		index := int(requests.Add(1)) - 1
		if index >= len(versions) {
			http.Error(w, "unexpected retry", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(versions[index])
	})
	parent := t.TempDir()
	for index, manifest := range [][]byte{first[0].data, second[0].data} {
		target := filepath.Join(parent, fmt.Sprintf("starter-%d", index))
		if err := createTemplate(context.Background(), pullOptions{Output: target}, client, io.Discard); err != nil {
			t.Fatalf("starter version %d failed: %v", index, err)
		}
		actual, err := os.ReadFile(filepath.Join(target, "glowbom.json"))
		if err != nil || !bytes.Equal(actual, manifest) {
			t.Fatalf("starter version %d did not contain its own manifest", index)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("got %d requests, want two fresh downloads", requests.Load())
	}
	if _, err := os.Stat(filepath.Join(parent, "starter-1", "README.md")); err != nil {
		t.Fatal("updated starter did not include its new file")
	}
}

func TestTemplateRejectsCorruptionAndTransportFailuresWithoutOutput(t *testing.T) {
	data := archiveTestZIP(t, templateTestEntries())
	for _, failure := range []string{"CRC", "truncated", "oversized", "streamed oversized", "status", "content type", "unsafe redirect", "unsafe path redirect", "redirect loop"} {
		t.Run(failure, func(t *testing.T) {
			requests := 0
			client := templateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/zip")
				switch failure {
				case "CRC":
					modified := bytes.Clone(data)
					modified[bytes.Index(modified, []byte("Project instructions"))] ^= 1
					_, _ = w.Write(modified)
				case "truncated":
					_, _ = w.Write(data[:len(data)-1])
				case "oversized":
					w.Header().Set("Content-Length", fmt.Sprint(maxTemplateArchiveBytes+1))
					w.WriteHeader(http.StatusOK)
				case "streamed oversized":
					w.(http.Flusher).Flush()
					_, _ = w.Write(make([]byte, maxTemplateArchiveBytes+1))
				case "status":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "content type":
					w.Header().Set("Content-Type", "text/html")
					_, _ = w.Write(data)
				case "unsafe redirect":
					http.Redirect(w, r, "https://unexpected.example/private-value", http.StatusFound)
				case "unsafe path redirect":
					http.Redirect(w, r, "https://audio.glowbom.com/private-value", http.StatusFound)
				case "redirect loop":
					http.Redirect(w, r, templateURL, http.StatusFound)
				}
			})
			parent := t.TempDir()
			err := createTemplate(context.Background(), pullOptions{Output: filepath.Join(parent, "starter")}, client, io.Discard)
			if err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("expected safe failure, got %v", err)
			}
			if strings.HasPrefix(failure, "unsafe") && requests != 1 {
				t.Fatal("followed an unsupported redirect")
			}
			children, _ := os.ReadDir(parent)
			if len(children) != 0 {
				t.Fatal("failed download left output or temporary paths")
			}
		})
	}
}

func TestTemplateDoesNotReplaceExistingOutputOrDownloadWhenCanceled(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "starter")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "keep.txt")
	_ = os.WriteFile(marker, []byte("local work"), 0600)
	client := &http.Client{Transport: templateRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("download started for unusable output")
		return nil, nil
	})}
	if err := createTemplate(context.Background(), pullOptions{Output: target}, client, io.Discard); err == nil {
		t.Fatal("accepted existing output")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := createTemplate(ctx, pullOptions{Output: filepath.Join(parent, "new")}, client, io.Discard); err == nil {
		t.Fatal("ignored canceled download")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "local work" {
		t.Fatal("changed existing work")
	}
}

func TestTemplateURLPolicy(t *testing.T) {
	for _, address := range []string{templateURL, "https://audio.glowbom.com/starter.zip?version=2", "https://audio.glowbom.com:443/starter.zip"} {
		parsed, _ := url.Parse(address)
		if !validTemplateURL(parsed) {
			t.Fatalf("rejected supported address %s", address)
		}
	}
	for _, address := range []string{"http://audio.glowbom.com/starter.zip", "https://audio.glowbom.com.evil.example/starter.zip", "https://user:password@audio.glowbom.com/starter.zip", "https://audio.glowbom.com:444/starter.zip", "https://127.0.0.1/starter.zip", "file:///starter.zip", "https://audio.glowbom.com/starter.zip#fragment", "https://audio.glowbom.com/another.zip", "https://audio.glowbom.com/%73tarter.zip", "https://github.com/glowbom/glowby/releases/download/3.0/project.zip"} {
		parsed, _ := url.Parse(address)
		if validTemplateURL(parsed) {
			t.Fatalf("accepted unsupported address %s", address)
		}
	}
}

func TestTemplateArchiveRejectsUnsafeEntriesAndLimits(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "android/../../escape", "android\\escape", "web/app/PAGE.tsx", "web/app", "glowbom.json", "CON"} {
		t.Run(name, func(t *testing.T) {
			entries := append(templateTestEntries(), archiveTestEntry{name: name, data: []byte("unsafe")})
			if _, err := validateTemplateArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
				t.Fatal("accepted an unsafe path or collision")
			}
		})
	}
	for _, mode := range []os.FileMode{os.ModeSymlink | 0777, os.ModeNamedPipe | 0600, os.ModeDevice | 0600} {
		entries := append(templateTestEntries(), archiveTestEntry{name: "bad", data: []byte("unsafe"), mode: mode})
		if _, err := validateTemplateArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
			t.Fatal("accepted an unsupported file type")
		}
	}
	entries := append(templateTestEntries(), archiveTestEntry{name: "large", data: bytes.Repeat([]byte("x"), maxTemplateUncompressedBytes+1), method: zip.Deflate})
	if _, err := validateTemplateArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
		t.Fatal("accepted an oversized unpacked starter")
	}
	entries = templateTestEntries()
	for len(entries) <= maxTemplateEntries {
		entries = append(entries, archiveTestEntry{name: fmt.Sprintf("file%d", len(entries))})
	}
	if _, err := validateTemplateArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
		t.Fatal("accepted too many starter entries")
	}
}
