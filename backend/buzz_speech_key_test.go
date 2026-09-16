package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSpeechKeyPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "key")
	l := newBuzzLive()
	defer l.close()
	l.keyPath = path
	s := &buzzSession{connected: true, live: l}
	if w := liveRequest(s, `{"key":"fixture-secret","remember":true}`); w.Code != 204 {
		t.Fatal(w.Code)
	}
	restored := &buzzLive{keyPath: path, key: "environment-fallback"}
	restored.restoreSpeechKey()
	if restored.key != "fixture-secret" {
		t.Fatal("new listener did not restore key")
	}
	key, err := readSpeechKey(path)
	if err != nil || key != "fixture-secret" {
		t.Fatal("key not restored")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatal("file permissions")
		}
		info, _ = os.Stat(filepath.Dir(path))
		if info.Mode().Perm() != 0700 {
			t.Fatal("directory permissions")
		}
	}
	if w := liveRequest(s, `{"key":"","remember":true}`); w.Code != 204 {
		t.Fatal(w.Code)
	}
	restored.key = "environment-fallback"
	restored.restoreSpeechKey()
	if restored.key != "" {
		t.Fatal("clear did not suppress fallback")
	}
	key, err = readSpeechKey(path)
	if err != nil || key != "" {
		t.Fatal("clear left key behind")
	}
	l.key = "existing"
	l.keyPath = filepath.Join(path, "invalid")
	if w := liveRequest(s, `{"key":"replacement","remember":true}`); w.Code != 500 {
		t.Fatal(w.Code)
	}
	if l.key != "existing" {
		t.Fatal("failed save replaced active key")
	}
}
