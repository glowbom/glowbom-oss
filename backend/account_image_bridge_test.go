package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClassifyImageCLIDiagnosticsWithoutLeakingDetails(t *testing.T) {
	for _, test := range []struct{ diagnostic, code string }{
		{"Glowbom: you are not signed in; run glowbom login first", "sign_in_required"},
		{"Glowbom: your sign-in was not accepted; run glowbom login again", "sign_in_required"},
		{"Glowbom: saved sign-in is invalid; run glowbom login again", "sign_in_required"},
		{"Glowbom: image generation was denied; check your account allowance with glowbom account", "allowance_required"},
		{"Glowbom: image generation was rate limited; wait before trying again", "rate_limited"},
		{"Glowbom: too many requests; wait a minute and try again", "rate_limited"},
		{"Glowbom: reference image 1 exceeds the Flux limit of 4,194,304 pixels; resize it first", "invalid_image_request"},
		{"Glowbom: reference images together exceed the 7,000,000 character upload limit", "invalid_image_request"},
		{"Glowbom: the image request was rejected; check the prompt, reference images, and selected options", "invalid_image_request"},
		{"Glowbom: this image model supports at most 5 reference images", "invalid_image_request"},
		{"unknown command: generate-image\nRun glowbom login to sign in", "cli_update_required"},
		{"Glowbom: unknown generate-image option; use --help for usage\nSign in with glowbom login first.", "cli_update_required"},
		{"Glowbom: flag provided but not defined: -source", "cli_update_required"},
		{"Glowbom: image generated, but saving failed: private path. Generation was not retried", "image_generation_failed"},
		{"Glowbom: could not confirm image generation; allowance may have been used. Check glowbom account before trying again", "image_generation_failed"},
		{"unexpected private-token https://signed.example/image?secret=private\nRun glowbom login", "image_generation_failed"},
		{"", "image_generation_failed"},
	} {
		if code := classifyImageCLI(test.diagnostic); code != test.code {
			t.Errorf("diagnostic %q returned %q, expected %q", test.diagnostic, code, test.code)
		}
	}
}

func imageBridgeTestCLI(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture requires a Unix host")
	}
	path := filepath.Join(t.TempDir(), "glowbom")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_CLI_BIN", path)
}

func TestImageCLIRunnerDiscardsOutputAndDoesNotRetry(t *testing.T) {
	imageBridgeTestCLI(t, `printf 'attempt\n' >> "$GLOWBOM_TEST_IMAGE_ATTEMPTS"
printf 'private stdout token and signed URL\n'
printf '%s\n' "$GLOWBOM_TEST_IMAGE_STDERR" >&2
exit 1
`)
	attempts := filepath.Join(t.TempDir(), "attempts")
	t.Setenv("GLOWBOM_TEST_IMAGE_ATTEMPTS", attempts)
	t.Setenv("GLOWBOM_TEST_IMAGE_STDERR", "Glowbom: image generation was denied; private diagnostic")
	output, err := runAccountCLI(context.Background(), "generate-image", "--format", "png", "private prompt")
	var failure *accountCommandFailure
	if len(output) != 0 || !errors.As(err, &failure) || failure.code != "allowance_required" || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe result: %q %v", output, err)
	}
	data, err := os.ReadFile(attempts)
	if err != nil || string(data) != "attempt\n" {
		t.Fatal("generation was replayed")
	}
}

func TestImageCLIRunnerBoundsStderrAndPreservesCancellation(t *testing.T) {
	t.Run("bounded stderr", func(t *testing.T) {
		imageBridgeTestCLI(t, "printf '%s' \"$GLOWBOM_TEST_IMAGE_STDERR\" >&2\nexit 1\n")
		t.Setenv("GLOWBOM_TEST_IMAGE_STDERR", strings.Repeat("private token ", 4000))
		output, err := runAccountCLI(context.Background(), "generate-image", "test")
		var failure *accountCommandFailure
		if len(output) != 0 || !errors.As(err, &failure) || failure.code != "image_generation_failed" {
			t.Fatalf("unexpected bounded result: %q %v", output, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		imageBridgeTestCLI(t, "exec sleep 10\n")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		output, err := runAccountCLI(ctx, "generate-image", "test")
		if len(output) != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancellation was hidden: %q %v", output, err)
		}
	})
}
