package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxProjectArchiveBytes = 10_000_000
const maxProjectUncompressedBytes = 9 * 1024 * 1024
const maxProjectManifestBytes = 512 * 1024
const maxProjectFiles = 100

type projectCreatedPath struct {
	name string
	info os.FileInfo
}

type projectOutput struct {
	parent        *os.Root
	parentPath    string
	temporary     string
	temporaryInfo os.FileInfo
	destination   string
	root          *os.Root
	rootInfo      os.FileInfo
	created       []projectCreatedPath
	committed     bool
}

// prepareProjectOutput checks access before downloading. The final directory
// is claimed only after every archive entry has been checked in memory.
func prepareProjectOutput(output string) (*projectOutput, error) {
	return prepareDirectoryOutput(output, "glowbom-project")
}

func prepareDirectoryOutput(output, prefix string) (*projectOutput, error) {
	automatic := output == ""
	if automatic {
		output = "."
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return nil, errors.New("could not resolve the project output path")
	}
	parentPath := filepath.Dir(absolute)
	if automatic {
		parentPath = absolute
	}
	parentPath, err = filepath.EvalSymlinks(parentPath)
	if err != nil {
		return nil, errors.New("the project output parent must be an existing directory")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, errors.New("could not open the project output parent directory")
	}
	outputState := &projectOutput{parent: parent, parentPath: parentPath}
	if !automatic {
		outputState.destination = filepath.Base(absolute)
		if _, err := parent.Lstat(outputState.destination); !errors.Is(err, os.ErrNotExist) {
			outputState.Close()
			return nil, errors.New("project output already exists or cannot be inspected; choose a new directory")
		}
	}
	for attempts := 0; attempts < 10; attempts++ {
		suffix := rand.Text()
		temporary := "." + prefix + "-download-" + suffix
		if err := parent.Mkdir(temporary, 0700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			outputState.Close()
			return nil, errors.New("the project output parent directory is not writable")
		}
		outputState.temporary = temporary
		outputState.temporaryInfo, err = parent.Lstat(temporary)
		if err != nil {
			outputState.Close()
			return nil, errors.New("could not inspect the project output preflight directory")
		}
		if automatic {
			outputState.destination = prefix + "-" + suffix
		}
		return outputState, nil
	}
	outputState.Close()
	return nil, errors.New("could not prepare a unique project output directory")
}

func (output *projectOutput) Close() {
	if output == nil {
		return
	}
	if output.root != nil {
		if !output.committed {
			// Remove only entries we created, and only if their identity is still
			// unchanged. A concurrent local file must never be removed recursively.
			for index := len(output.created) - 1; index >= 0; index-- {
				entry := output.created[index]
				if info, err := output.root.Lstat(entry.name); err == nil && os.SameFile(info, entry.info) {
					_ = output.root.Remove(entry.name)
				}
			}
		}
		_ = output.root.Close()
		output.root = nil
	}
	if output.parent != nil {
		if !output.committed && output.rootInfo != nil {
			if info, err := output.parent.Lstat(output.destination); err == nil && os.SameFile(info, output.rootInfo) {
				_ = output.parent.Remove(output.destination)
			}
		}
		if output.temporary != "" && output.temporaryInfo != nil {
			// The preflight directory is empty; Remove preserves unexpected files.
			if info, err := output.parent.Lstat(output.temporary); err == nil && os.SameFile(info, output.temporaryInfo) {
				_ = output.parent.Remove(output.temporary)
			}
		}
		_ = output.parent.Close()
		output.parent = nil
	}
}

type projectArchiveEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func (output *projectOutput) Extract(ctx context.Context, archive []byte) (string, int, error) {
	defer output.Close()
	entries, err := validateProjectArchive(ctx, archive)
	if err != nil {
		return "", 0, err
	}
	return output.extractEntries(ctx, entries)
}

func (output *projectOutput) extractEntries(ctx context.Context, entries []projectArchiveEntry) (string, int, error) {
	if output == nil || output.parent == nil {
		return "", 0, errors.New("project output is no longer available")
	}
	defer output.Close()
	var err error
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	// Mkdir fails if any file, directory, or symlink appeared during download.
	// A directory rename could replace an existing empty directory on Unix.
	if err := output.parent.Mkdir(output.destination, 0700); err != nil {
		return "", 0, errors.New("could not create the project directory without replacing an existing path; choose a new directory")
	}
	output.rootInfo, err = output.parent.Lstat(output.destination)
	if err != nil || !output.rootInfo.IsDir() || output.rootInfo.Mode()&os.ModeSymlink != 0 {
		output.rootInfo = nil
		return "", 0, errors.New("the project output directory changed while saving")
	}
	output.root, err = output.parent.OpenRoot(output.destination)
	if err != nil {
		return "", 0, errors.New("could not open the new project output directory")
	}
	info, err := output.root.Stat(".")
	if err != nil || !os.SameFile(info, output.rootInfo) {
		return "", 0, errors.New("the project output directory changed while saving")
	}
	directories := map[string]bool{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		parts := strings.Split(entry.name, "/")
		for index := 1; index < len(parts); index++ {
			directory := filepath.Join(parts[:index]...)
			if directories[directory] {
				continue
			}
			if err := output.root.Mkdir(directory, 0700); err != nil {
				return "", 0, errors.New("could not create a project subdirectory safely")
			}
			info, err := output.root.Lstat(directory)
			if err != nil || !info.IsDir() {
				return "", 0, errors.New("could not inspect a new project subdirectory")
			}
			output.created = append(output.created, projectCreatedPath{name: directory, info: info})
			directories[directory] = true
		}
		name := filepath.FromSlash(entry.name)
		file, err := output.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", 0, errors.New("could not create a project file without replacing another file")
		}
		info, statErr := file.Stat()
		if statErr == nil {
			output.created = append(output.created, projectCreatedPath{name: name, info: info})
		}
		_, writeErr := io.Copy(file, &projectContextReader{ctx: ctx, reader: bytes.NewReader(entry.data)})
		if writeErr == nil && entry.mode != 0 {
			writeErr = file.Chmod(entry.mode)
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if statErr != nil || writeErr != nil || closeErr != nil {
			if ctx.Err() != nil {
				return "", 0, ctx.Err()
			}
			return "", 0, errors.New("could not finish writing the project files")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	info, err = output.parent.Lstat(output.destination)
	if err != nil || !os.SameFile(info, output.rootInfo) {
		return "", 0, errors.New("the project output directory changed while saving")
	}
	output.committed = true
	return filepath.Join(output.parentPath, output.destination), len(entries), nil
}

func validateProjectArchive(ctx context.Context, archive []byte) ([]projectArchiveEntry, error) {
	invalid := errors.New("the downloaded project is not a valid supported Glowbom ZIP bundle")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(archive) > maxProjectArchiveBytes {
		return nil, errors.New("the project exceeds the 10 MB ZIP download limit")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxProjectFiles {
		return nil, invalid
	}
	entries := make([]projectArchiveEntry, 0, len(reader.File))
	files := make(map[string][]byte, len(reader.File))
	pathSpellings := map[string]string{}
	pathKinds := map[string]bool{}
	total := 0
	for _, file := range reader.File {
		if !validProjectArchivePath(file.Name) || !file.Mode().IsRegular() || file.Flags&(1|64) != 0 ||
			(file.Method != zip.Store && file.Method != zip.Deflate) {
			return nil, invalid
		}
		parts := strings.Split(file.Name, "/")
		for index := 1; index <= len(parts); index++ {
			prefix := strings.Join(parts[:index], "/")
			folded := strings.ToLower(prefix)
			isFile := index == len(parts)
			if previous, exists := pathSpellings[folded]; exists {
				if previous != prefix || pathKinds[folded] || isFile {
					return nil, invalid
				}
			}
			pathSpellings[folded] = prefix
			pathKinds[folded] = isFile
		}
		limit := maxProjectUncompressedBytes - total
		if file.Name == "glowbom.json" && limit > maxProjectManifestBytes {
			limit = maxProjectManifestBytes
		}
		if file.UncompressedSize64 > uint64(limit) {
			return nil, errors.New("the project exceeds the supported unpacked size limits")
		}
		stream, err := file.Open()
		if err != nil {
			return nil, invalid
		}
		// Reading through EOF verifies the ZIP checksum, even for empty files.
		data, readErr := io.ReadAll(io.LimitReader(&projectContextReader{ctx: ctx, reader: stream}, int64(limit)+1))
		closeErr := stream.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil || closeErr != nil || len(data) > limit {
			return nil, invalid
		}
		total += len(data)
		files[file.Name] = data
		entries = append(entries, projectArchiveEntry{name: file.Name, data: data})
	}
	manifest, ok := files["glowbom.json"]
	if !ok || !validProjectManifest(manifest, files) {
		return nil, invalid
	}
	return entries, nil
}

func validProjectManifest(data []byte, files map[string][]byte) bool {
	var manifest struct {
		SyncFormatVersion  string  `json:"syncFormatVersion"`
		PromptPath         string  `json:"promptPath"`
		InitialDrawingPath *string `json:"initialDrawingPath"`
		IconPath           *string `json:"iconPath"`
		Exports            []struct {
			Path string `json:"path"`
		} `json:"exports"`
	}
	if !utf8.Valid(data) || json.Unmarshal(data, &manifest) != nil || manifest.SyncFormatVersion != "1.0" ||
		len(manifest.Exports) == 0 || len(manifest.Exports) > maxProjectFiles {
		return false
	}
	paths := []string{manifest.PromptPath}
	for _, optional := range []*string{manifest.InitialDrawingPath, manifest.IconPath} {
		if optional != nil && *optional != "" {
			paths = append(paths, *optional)
		}
	}
	for _, export := range manifest.Exports {
		paths = append(paths, export.Path)
	}
	required := map[string]bool{"glowbom.json": true}
	for _, name := range paths {
		if name == "glowbom.json" || !validProjectArchivePath(name) {
			return false
		}
		if _, exists := files[name]; !exists {
			return false
		}
		required[name] = true
	}
	return len(required) == len(files)
}

func validProjectArchivePath(name string) bool {
	if name == "" || len(name) > 512 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:%<>\"|?*") {
		return false
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return false
		}
		if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
			suffix := strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT")
			if strings.Contains("123456789¹²³", suffix) && utf8.RuneCountInString(suffix) == 1 {
				return false
			}
		}
	}
	return true
}

type projectContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *projectContextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, fmt.Errorf("project pull stopped: %w", err)
	}
	return reader.reader.Read(data)
}
