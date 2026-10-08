package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const glowbomImageSourceLabel = "Glowbom (Flux)"
const glowbomImageTimeout = 10 * time.Minute

// The same bridge serializes generation with account login, logout, and refresh.
var glowbomImageAccount *accountBridge

func authorizeGlowbomImage(w http.ResponseWriter, r *http.Request) bool {
	bridge := glowbomImageAccount
	if bridge == nil || bridge.token == "" || !hasValidGlowbomServerToken(r, bridge.token) {
		http.Error(w, "Restart Glowbom with its secure local connection to use account images.", http.StatusUnauthorized)
		return false
	}
	return true
}

type glowbomImageFailure struct{ message string }

func (e *glowbomImageFailure) Error() string { return e.message }

func glowbomImageError(err error) error {
	message := "Glowbom could not confirm image generation. Account allowance may have been used. Check Account before trying again."
	var failure *accountCommandFailure
	if errors.Is(err, errAccountCLIMissing) {
		message = "Update or install the Glowbom CLI to use Glowbom images."
	} else if errors.As(err, &failure) {
		switch failure.code {
		case "sign_in_required":
			message = "Sign in again from Account to use Glowbom images."
		case "allowance_required":
			message = "Glowbom image generation was denied. Check your account allowance."
		case "rate_limited":
			message = "Glowbom image generation is busy. Wait before trying again."
		case "invalid_image_request":
			message = "Glowbom could not use this image request. Check the prompt and reference photo."
		case "cli_update_required":
			message = "Update the Glowbom CLI to use Glowbom images."
		}
	}
	return &glowbomImageFailure{message}
}

func glowbomImageAccountCode(ctx context.Context, bridge *accountBridge) string {
	probe, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	output, err := bridge.run(probe, "account", "--json")
	if errors.Is(err, errAccountCLIMissing) {
		return "cli_unavailable"
	}
	var status localAccountStatus
	if len(output) > 16384 || json.Unmarshal(output, &status) != nil || !validAccountStatus(status) {
		if err != nil || probe.Err() != nil {
			return "account_unavailable"
		}
		return "cli_update_required"
	}
	if err != nil || probe.Err() != nil {
		return "account_unavailable"
	}
	if status.Status == "signed_out" {
		return "sign_in_required"
	}
	if status.Status != "signed_in" {
		return "account_unavailable"
	}
	return ""
}

func readGlowbomImageAccount(ctx context.Context, bridge *accountBridge) error {
	switch glowbomImageAccountCode(ctx, bridge) {
	case "":
		return nil
	case "cli_unavailable":
		return &glowbomImageFailure{"Update or install the Glowbom CLI to use Glowbom images."}
	case "cli_update_required":
		return &glowbomImageFailure{"Could not check your Glowbom account. Update the Glowbom CLI and try again."}
	case "sign_in_required":
		return &glowbomImageFailure{"Sign in from Account to use Glowbom images."}
	default:
		return &glowbomImageFailure{"Could not check your Glowbom account. Open Account and try again."}
	}
}

func glowbomImagesAvailability(ctx context.Context) (bool, string) {
	bridge := glowbomImageAccount
	if bridge == nil {
		return false, "cli_unavailable"
	}
	if bridge.token == "" {
		return false, "backend_auth_required"
	}
	if ctx.Err() != nil {
		return false, "account_unavailable"
	}
	if !bridge.begin() {
		return false, "account_busy"
	}
	defer bridge.end()
	code := glowbomImageAccountCode(ctx, bridge)
	return code == "", code
}

func validateGlowbomImageReference(reference string) error {
	if reference == "" {
		return nil
	}
	if len(reference)+len("data:image/png;base64,") > 7_000_000 {
		return &glowbomImageFailure{"The Glowbom reference photo is too large. Use a smaller photo."}
	}
	data, err := base64.StdEncoding.DecodeString(reference)
	if err != nil {
		return &glowbomImageFailure{"Choose a valid PNG or JPEG reference photo."}
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" || config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 4_194_304 {
		return &glowbomImageFailure{"Glowbom reference photos must be PNG or JPEG images up to 4 megapixels. Use a smaller photo."}
	}
	return nil
}

// Use the CLI's existing credential store, request validation, and safe download.
// The backend never reads account tokens or retries a generation request.
func callGlowbomImageGeneration(ctx context.Context, prompt, reference string) (string, error) {
	if err := validateGlowbomImageReference(reference); err != nil {
		return "", err
	}
	bridge := glowbomImageAccount
	if bridge == nil {
		return "", glowbomImageError(errAccountCLIMissing)
	}
	if bridge.token == "" {
		return "", &glowbomImageFailure{"Restart Glowbom with its secure local connection to use account images."}
	}
	if !bridge.begin() {
		return "", &glowbomImageFailure{"Your Glowbom account is busy. Wait for its current operation to finish."}
	}
	defer bridge.end()
	if err := readGlowbomImageAccount(ctx, bridge); err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", &glowbomImageFailure{"Glowbom image generation stopped before it could start."}
	}
	ctx, cancel := context.WithTimeout(ctx, glowbomImageTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "glowbom-image-")
	if err != nil {
		return "", &glowbomImageFailure{"Could not prepare a temporary image folder."}
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "image.png")
	args := []string{"generate-image", "--format", "png", "--source", "flux", "--quality", "fast", "--output", output}
	if reference != "" {
		data, _ := base64.StdEncoding.DecodeString(reference)
		ref := filepath.Join(dir, "reference.png")
		if err := os.WriteFile(ref, data, 0600); err != nil {
			return "", &glowbomImageFailure{"Could not prepare the reference photo."}
		}
		args = append(args, "--ref", ref)
	}
	args = append(args, "--", prompt)
	if _, err := bridge.run(ctx, args...); err != nil {
		if ctx.Err() != nil {
			return "", &glowbomImageFailure{"Glowbom image generation stopped or timed out. Account allowance may have been used. No retry was made."}
		}
		return "", glowbomImageError(err)
	}
	if ctx.Err() != nil {
		return "", &glowbomImageFailure{"Glowbom image generation stopped. No retry was made."}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", glowbomImageError(err)
	}
	defer root.Close()
	info, err := root.Lstat("image.png")
	if err != nil || !info.Mode().IsRegular() || info.Size() > projectIconMaxBytes {
		return "", &glowbomImageFailure{"Glowbom generated an image, but its saved file could not be opened. No retry was made."}
	}
	file, err := root.Open("image.png")
	if err != nil {
		return "", glowbomImageError(err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, projectIconMaxBytes+1))
	if err == nil {
		data, err = normalizeProjectIcon(data)
	}
	if err != nil {
		return "", &glowbomImageFailure{"Glowbom returned an unsupported image. No retry was made."}
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), nil
}
