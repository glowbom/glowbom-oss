package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type studioClipToolTransport func(*http.Request) (*http.Response, error)

func (f studioClipToolTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStudioClipToolDownloadVerification(t *testing.T) {
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	zip.Write([]byte("test executable"))
	zip.Close()
	hash := sha256.Sum256(compressed.Bytes())
	download := studioClipToolDownload{name: "ffmpeg-darwin-arm64.gz", digest: hex.EncodeToString(hash[:])}
	client := &http.Client{Transport: studioClipToolTransport(func(r *http.Request) (*http.Response, error) {
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		if r.URL.Host != "github.com" || r.URL.Scheme != "https" {
			t.Fatal("unexpected download host", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(compressed.Bytes())), Header: make(http.Header)}, nil
	})}
	dir := t.TempDir()
	if err := downloadStudioClipTool(context.Background(), client, dir, download); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ffmpeg"))
	if err != nil || string(data) != "test executable" {
		t.Fatal("download not decompressed", err)
	}
	info, _ := os.Stat(filepath.Join(dir, "ffmpeg"))
	if info.Mode().Perm() != 0700 {
		t.Fatal("download permissions", info.Mode())
	}
	download.digest = "wrong"
	dir = t.TempDir()
	if err := downloadStudioClipTool(context.Background(), client, dir, download); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("unverified download retained")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := downloadStudioClipTool(ctx, client, dir, download); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled setup continued", err)
	}
}
