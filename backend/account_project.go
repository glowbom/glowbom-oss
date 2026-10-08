// Created by Codex under Jacob's direction, Glowbom Labs.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	accountProjectTimeout = 4*time.Minute + 20*time.Second
	accountPromptLimit    = 64 * 1024
	accountDrawingLimit   = 10 * 1024 * 1024
)

var (
	errInvalidOutput = errors.New("invalid output")
	errOutputExists  = errors.New("output exists")
)

type accountExportResult struct {
	Version int    `json:"version"`
	Path    string `json:"path"`
	Files   int    `json:"files"`
	Code    string `json:"code"`
}

type accountProjectResponse struct {
	Version     int    `json:"version"`
	Path        string `json:"path"`
	Files       int    `json:"files"`
	Prompt      string `json:"prompt,omitempty"`
	Drawing     string `json:"drawing,omitempty"`
	DrawingType string `json:"drawingType,omitempty"`
}

func (b *accountBridge) downloadProject(w http.ResponseWriter, r *http.Request) {
	if !accountMethod(w, r, http.MethodPost) {
		return
	}
	if !b.begin() {
		accountBridgeReply(w, http.StatusConflict, accountUnavailable("account_busy"))
		return
	}
	defer b.end()

	var req struct {
		Parent string `json:"parent"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := decoder.Decode(&req); err != nil {
		accountBridgeReply(w, http.StatusBadRequest, accountUnavailable("invalid_output"))
		return
	}
	parent, err := accountProjectParent(req.Parent)
	if err != nil {
		accountBridgeReply(w, http.StatusBadRequest, accountUnavailable("invalid_output"))
		return
	}
	destination, err := newAccountProjectDir(parent)
	if err != nil {
		code := "invalid_output"
		status := http.StatusBadRequest
		if errors.Is(err, errOutputExists) {
			code = "output_exists"
			status = http.StatusConflict
		}
		accountBridgeReply(w, status, accountUnavailable(code))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), accountProjectTimeout)
	defer cancel()
	output, runErr := b.run(ctx, "export", "--output", destination)
	result, code := accountProjectResult(output, runErr, ctx.Err(), destination)
	if code != "" {
		accountBridgeReply(w, accountProjectHTTPStatus(code), accountUnavailable(code))
		return
	}
	path, code := acceptExportedProject(parent, destination, result)
	if code != "" {
		accountBridgeReply(w, accountProjectHTTPStatus(code), accountUnavailable(code))
		return
	}
	prompt, drawing, drawingType := savedProjectInputs(path)
	accountBridgeReply(w, http.StatusOK, accountProjectResponse{Version: 1, Path: path, Files: result.Files, Prompt: prompt, Drawing: drawing, DrawingType: drawingType})
}

func accountProjectParent(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 || strings.ContainsRune(raw, 0) || !filepath.IsAbs(raw) {
		return "", errInvalidOutput
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(raw))
	if err != nil {
		return "", errInvalidOutput
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errInvalidOutput
	}
	return resolved, nil
}

func newAccountProjectDir(parent string) (string, error) {
	for n := 1; n <= 40; n++ {
		name := "Glowbom"
		if n > 1 {
			name = fmt.Sprintf("Glowbom %d", n)
		}
		destination, ok := accountProjectChild(parent, name)
		if !ok {
			return "", errInvalidOutput
		}
		_, err := os.Lstat(destination)
		if errors.Is(err, os.ErrNotExist) {
			return destination, nil
		}
		if err != nil {
			return "", errInvalidOutput
		}
	}
	return "", errOutputExists
}

func accountProjectChild(parent, name string) (string, bool) {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	return filepath.Join(parent, name), true
}

func accountProjectResult(output []byte, runErr, ctxErr error, destination string) (accountExportResult, string) {
	if errors.Is(runErr, errAccountCLIMissing) {
		return accountExportResult{}, "cli_unavailable"
	}
	var failure *accountCommandFailure
	if errors.As(runErr, &failure) {
		if !knownProjectDownloadCode(failure.code) {
			return accountExportResult{}, "export_failed"
		}
		return accountExportResult{}, failure.code
	}
	if ctxErr == context.DeadlineExceeded {
		return accountExportResult{}, "export_timeout"
	}
	if runErr != nil || ctxErr != nil {
		if ctxErr != nil {
			return accountExportResult{}, "canceled"
		}
		return accountExportResult{}, "export_failed"
	}
	path, files := exportedProjectLine(output)
	if path == "" {
		path = destination
	}
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) || files < 1 || files > 20000 {
		return accountExportResult{}, "export_failed"
	}
	return accountExportResult{Version: 1, Path: path, Files: files}, ""
}

func exportedProjectLine(output []byte) (string, int) {
	path := ""
	files := 1
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Exported project: "); ok {
			path = strings.TrimSpace(rest)
		}
		if rest, ok := strings.CutPrefix(line, "Saved "); ok {
			count, _, found := strings.Cut(rest, " ")
			parsed, err := strconv.Atoi(count)
			if found && err == nil && parsed > 0 && parsed <= 20000 {
				files = parsed
			}
		}
	}
	return path, files
}

func knownProjectDownloadCode(code string) bool {
	switch code {
	case "sign_in_required", "project_not_found", "project_incomplete", "project_too_large",
		"invalid_project", "rate_limited", "download_denied", "download_failed",
		"output_exists", "invalid_output", "starter_unavailable", "export_failed",
		"export_timeout", "canceled":
		return true
	default:
		return false
	}
}

func accountProjectHTTPStatus(code string) int {
	switch code {
	case "sign_in_required", "download_denied":
		return http.StatusForbidden
	case "project_not_found":
		return http.StatusNotFound
	case "project_incomplete", "output_exists", "account_busy":
		return http.StatusConflict
	case "project_too_large":
		return http.StatusRequestEntityTooLarge
	case "invalid_project":
		return http.StatusUnprocessableEntity
	case "rate_limited":
		return http.StatusTooManyRequests
	case "invalid_output", "canceled":
		return http.StatusBadRequest
	default:
		return http.StatusServiceUnavailable
	}
}

func acceptExportedProject(parent, destination string, result accountExportResult) (string, string) {
	cleaned := filepath.Clean(result.Path)
	if cleaned != destination {
		resolved, err := filepath.EvalSymlinks(cleaned)
		expected, expectedErr := filepath.EvalSymlinks(destination)
		if err != nil || expectedErr != nil || resolved != expected {
			return "", "export_failed"
		}
		cleaned = resolved
	}
	info, err := os.Lstat(cleaned)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "export_failed"
	}
	relative, err := filepath.Rel(parent, cleaned)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") || strings.ContainsRune(relative, os.PathSeparator) {
		return "", "export_failed"
	}
	manifestInfo, err := os.Lstat(filepath.Join(cleaned, "glowbom.json"))
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Size() == 0 || manifestInfo.Size() > 64*1024 {
		return "", "export_failed"
	}
	return renameExportedProject(parent, cleaned), ""
}

func renameExportedProject(parent, current string) string {
	name := savedProjectFolderName(current)
	if name == "" || name == filepath.Base(current) {
		return current
	}
	for n := 1; n <= 40; n++ {
		candidate := name
		if n > 1 {
			candidate = fmt.Sprintf("%s %d", name, n)
		}
		target, ok := accountProjectChild(parent, candidate)
		if !ok {
			return current
		}
		_, err := os.Lstat(target)
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return current
		}
		if err := os.Rename(current, target); err != nil {
			return current
		}
		info, statErr := os.Lstat(target)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return current
		}
		return target
	}
	return current
}

func savedProjectInputs(dir string) (string, string, string) {
	data, ok := readAccountProjectFile(dir, "glowbom.json", 64*1024)
	if !ok {
		return "", "", ""
	}
	var manifest struct {
		PromptPath         string  `json:"promptPath"`
		InitialDrawingPath *string `json:"initialDrawingPath"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return "", "", ""
	}
	drawingName := ""
	if manifest.InitialDrawingPath != nil {
		drawingName = *manifest.InitialDrawingPath
	}
	drawing, drawingType := savedProjectDrawing(dir, drawingName)
	return savedProjectPrompt(dir, manifest.PromptPath), drawing, drawingType
}

func savedProjectPrompt(dir, name string) string {
	if name == "glowbom.json" {
		return ""
	}
	data, ok := readAccountProjectFile(dir, name, accountPromptLimit)
	if !ok || !utf8.Valid(data) || bytes.Contains(data, []byte{0}) {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func savedProjectDrawing(dir, name string) (string, string) {
	if name == "glowbom.json" {
		return "", ""
	}
	data, ok := readAccountProjectFile(dir, name, accountDrawingLimit)
	if !ok {
		return "", ""
	}
	kind := accountImageType(data)
	if kind == "" {
		return "", ""
	}
	return base64.StdEncoding.EncodeToString(data), kind
}

func accountImageType(data []byte) string {
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg"
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return "image/webp"
	}
	return ""
}

func readAccountProjectFile(dir, name string, limit int64) ([]byte, bool) {
	if !accountProjectInputPath(name) {
		return nil, false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, false
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) == 0 || int64(len(data)) > limit {
		return nil, false
	}
	return data, true
}

func accountProjectInputPath(name string) bool {
	if name == "" || len(name) > 512 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return false
	}
	if strings.ContainsAny(name, "\\:%<>\"|?*") || filepath.IsAbs(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || part != filepath.Base(part) {
			return false
		}
	}
	return true
}

func savedProjectFolderName(dir string) string {
	info, err := os.Lstat(filepath.Join(dir, "glowbom.json"))
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 64*1024 {
		return ""
	}
	file, err := os.Open(filepath.Join(dir, "glowbom.json"))
	if err != nil {
		return ""
	}
	defer file.Close()
	var manifest struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	if json.NewDecoder(io.LimitReader(file, 64*1024)).Decode(&manifest) != nil {
		return ""
	}
	if name := projectFolderName(manifest.Name); name != "" {
		return name
	}
	return projectFolderName(manifest.DisplayName)
}

func projectFolderName(raw string) string {
	if !utf8.ValidString(raw) {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	name := strings.Trim(strings.Join(strings.Fields(b.String()), " "), ". ")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	runes := []rune(name)
	if len(runes) > 60 {
		name = strings.Trim(string(runes[:60]), ". ")
	}
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}
