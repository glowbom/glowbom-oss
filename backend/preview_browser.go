package main

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

func validPreviewBrowserURL(value string) bool {
	address, err := url.Parse(value)
	if err != nil || address.Scheme != "http" || address.User != nil {
		return false
	}
	switch address.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return false
	}
	port, err := strconv.Atoi(address.Port())
	return err == nil && port > 0 && port <= 65535
}

func previewBrowserCommand(platform, address string) (string, []string, error) {
	if !validPreviewBrowserURL(address) {
		return "", nil, errors.New("The preview address must be a local running server.")
	}
	switch platform {
	case "darwin":
		return "open", []string{address}, nil
	case "windows":
		return "rundll32.exe", []string{"url.dll,FileProtocolHandler", address}, nil
	case "linux":
		return "xdg-open", []string{address}, nil
	default:
		return "", nil, errors.New("Opening a browser is not supported on this platform.")
	}
}

func openPreviewBrowser(ctx context.Context, address string) error {
	name, args, err := previewBrowserCommand(runtime.GOOS, address)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Use a fixed executable and arguments, without a shell or a caller-provided URL.
	return exec.CommandContext(ctx, name, args...).Run()
}
