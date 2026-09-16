package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

func speechKeyPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Glowbom", "live-private", "elevenlabs-key")
}

// Restore only during real session initialization, not when constructing test listeners.
// A saved empty value explicitly disables an environment-provided fallback.
func (l *buzzLive) restoreSpeechKey() {
	if l.keyPath == "" {
		l.keyPath = speechKeyPath()
	}
	if key, err := readSpeechKey(l.keyPath); err == nil {
		l.key = key
	}
}
func readSpeechKey(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", os.ErrInvalid
	}
	return strings.TrimSpace(string(data)), nil
}
func saveSpeechKey(path, key string) error {
	if path == "" || len(key) > 4096 {
		return os.ErrInvalid
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".speech-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString(key)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
