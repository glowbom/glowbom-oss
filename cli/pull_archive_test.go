package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type archiveTestEntry struct {
	name   string
	data   []byte
	mode   os.FileMode
	method uint16
}

func archiveTestZIP(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: entry.method}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(header.Name, "/") {
			if _, err := file.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func archiveTestCloudEntries() []archiveTestEntry {
	return []archiveTestEntry{
		{name: "glowbom.json", data: []byte(`{"syncFormatVersion":"1.0","name":"My saved project","promptPath":"prompt.txt","initialDrawingPath":"drawing.png","iconPath":"assets/icon.png","exports":[{"platform":"web","path":"web/index.html"},{"platform":"ios","path":"ios/ContentView.swift"}],"unknownMetadata":{"keep":true}}`)},
		{name: "prompt.txt", data: []byte("Build a small garden journal.")},
		{name: "drawing.png", data: []byte("drawing bytes")},
		{name: "assets/icon.png", data: []byte("icon bytes")},
		{name: "web/index.html", data: []byte("<h1>My garden</h1>"), mode: 0755},
		{name: "ios/ContentView.swift", data: []byte("import SwiftUI")},
	}
}

func TestProjectArchiveExtractPreservesCloudBundle(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		t.Run(fmt.Sprint(method), func(t *testing.T) {
			entries := archiveTestCloudEntries()
			for index := range entries {
				entries[index].method = method
			}
			parent := t.TempDir()
			target := filepath.Join(parent, "saved-project")
			output, err := prepareProjectOutput(target)
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("preflight published the destination")
			}
			result, count, err := output.Extract(context.Background(), archiveTestZIP(t, entries))
			if err != nil {
				t.Fatal(err)
			}
			canonical, _ := filepath.EvalSymlinks(target)
			if result != canonical || count != len(entries) {
				t.Fatalf("unexpected result %q, %d", result, count)
			}
			for _, entry := range entries {
				path := filepath.Join(result, filepath.FromSlash(entry.name))
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, entry.data) {
					t.Fatalf("entry %s changed: %v", entry.name, err)
				}
				if runtime.GOOS != "windows" {
					info, _ := os.Stat(path)
					if info.Mode().Perm() != 0600 {
						t.Fatalf("unsafe permissions for %s: %v", entry.name, info.Mode())
					}
				}
			}
			children, _ := os.ReadDir(parent)
			if len(children) != 1 {
				t.Fatal("preflight left temporary directories")
			}
		})
	}
}

func TestProjectArchiveDefaultOutputIsUnique(t *testing.T) {
	parent := t.TempDir()
	t.Chdir(parent)
	first, err := prepareProjectOutput("")
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareProjectOutput("")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	defer second.Close()
	archive := archiveTestZIP(t, archiveTestCloudEntries())
	one, _, err := first.Extract(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := second.Extract(context.Background(), archive)
	if err != nil || one == two || !strings.HasPrefix(filepath.Base(one), "glowbom-project-") {
		t.Fatalf("default output did not use unique names: %q %q %v", one, two, err)
	}
}

func TestProjectOutputRejectsExistingPathsAndMissingParent(t *testing.T) {
	for _, kind := range []string{"empty directory", "directory with work", "file", "symlink", "broken symlink"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "project")
			switch kind {
			case "empty directory", "directory with work":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "directory with work" {
					_ = os.WriteFile(filepath.Join(target, "keep.txt"), []byte("local work"), 0600)
				}
			case "file":
				_ = os.WriteFile(target, []byte("local work"), 0600)
			case "symlink", "broken symlink":
				destination := parent
				if kind == "broken symlink" {
					destination = filepath.Join(parent, "missing")
				}
				if err := os.Symlink(destination, target); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			if output, err := prepareProjectOutput(target); err == nil {
				output.Close()
				t.Fatal("accepted an existing output path")
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatal("removed an existing output path")
			}
		})
	}
	if output, err := prepareProjectOutput(filepath.Join(t.TempDir(), "missing", "project")); err == nil {
		output.Close()
		t.Fatal("created a missing parent")
	}
}

func TestProjectOutputPreservesDestinationCreatedDuringDownload(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "project")
	output, err := prepareProjectOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(target)
	if _, _, err := output.Extract(context.Background(), archiveTestZIP(t, archiveTestCloudEntries())); err == nil {
		t.Fatal("replaced an empty directory created during download")
	}
	after, err := os.Stat(target)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("destination directory was replaced")
	}
	children, _ := os.ReadDir(target)
	if len(children) != 0 {
		t.Fatal("files were merged into an existing directory")
	}
}

func TestProjectOutputParentSymlinkIsResolvedOnce(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	_ = os.Mkdir(first, 0700)
	_ = os.Mkdir(second, 0700)
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	output, err := prepareProjectOutput(filepath.Join(alias, "project"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	_ = os.Remove(alias)
	_ = os.Symlink(second, alias)
	result, _, err := output.Extract(context.Background(), archiveTestZIP(t, archiveTestCloudEntries()))
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(filepath.Join(first, "project"))
	if result != canonical {
		t.Fatalf("output followed a changed parent alias: %s", result)
	}
	children, _ := os.ReadDir(second)
	if len(children) != 0 {
		t.Fatal("wrote into the changed parent alias")
	}
}

func TestProjectArchiveRejectsUnsafePathsAndEntryTypes(t *testing.T) {
	paths := []string{"../escape", "/absolute", "a/../../escape", "a//b", "./a", "a/./b", "a\\b", "C:drive", "a%2fb", "nul\x00name", "new\nline", "DEL\x7f", "a/CON.txt", "NUL", "com1.js", "LPT9", "COM¹", "trailing.", "trailing ", "a?b", "a:b", "a*b", "a|b", "a<", "a>", "a\"b", "folder/", strings.Repeat("x", 513)}
	for _, name := range paths {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			entries := append(archiveTestCloudEntries(), archiveTestEntry{name: name, data: []byte("unsafe")})
			if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
				t.Fatal("accepted an unsafe entry")
			}
		})
	}
	for _, mode := range []os.FileMode{os.ModeSymlink | 0777, os.ModeNamedPipe | 0600, os.ModeDevice | 0600, os.ModeDir | 0700} {
		t.Run(mode.String(), func(t *testing.T) {
			entries := archiveTestCloudEntries()
			entries[1].mode = mode
			if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
				t.Fatal("accepted an entry that is not a regular file")
			}
		})
	}
}

func TestProjectArchiveRejectsCollisions(t *testing.T) {
	for _, name := range []string{"prompt.txt", "PROMPT.TXT", "web", "Web/another.html", "glowbom.json/nested", "web/index.html/nested"} {
		t.Run(name, func(t *testing.T) {
			entries := append(archiveTestCloudEntries(), archiveTestEntry{name: name})
			if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
				t.Fatal("accepted a duplicate, case collision, or file/directory collision")
			}
		})
	}
}

func TestProjectArchiveRejectsBadManifestAndUnrelatedFiles(t *testing.T) {
	manifests := []string{
		`{`, `null`, `{}`, `{"syncFormatVersion":"2.0","promptPath":"prompt.txt","exports":[{"path":"web/index.html"}]}`,
		`{"syncFormatVersion":"1.0","promptPath":"prompt.txt","exports":[]}`,
		`{"syncFormatVersion":"1.0","promptPath":"glowbom.json","exports":[{"path":"web/index.html"}]}`,
		`{"syncFormatVersion":"1.0","promptPath":"missing.txt","exports":[{"path":"web/index.html"}]}`,
		`{"syncFormatVersion":"1.0","promptPath":"../outside","exports":[{"path":"web/index.html"}]}`,
		`{"syncFormatVersion":"1.0","promptPath":"prompt.txt","exports":[null]}`,
		`{"syncFormatVersion":"1.0","promptPath":"prompt.txt","iconPath":12,"exports":[{"path":"web/index.html"}]}`,
		`{"syncFormatVersion":"1.0","promptPath":"prompt.txt","exports":[{"path":"web/index.html"}]}`,
	}
	for index, manifest := range manifests {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			entries := archiveTestCloudEntries()
			entries[0].data = []byte(manifest)
			if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
				t.Fatal("accepted invalid manifest or unrelated entries")
			}
		})
	}
	entries := archiveTestCloudEntries()
	for _, invalid := range [][]archiveTestEntry{entries[1:], entries[:len(entries)-1], append(entries, archiveTestEntry{name: "unrelated.txt"})} {
		if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, invalid)); err == nil {
			t.Fatal("accepted missing required files or an unrelated entry")
		}
	}
}

func TestProjectArchiveAcceptsOptionalPathsAndDuplicateManifestReferences(t *testing.T) {
	entries := []archiveTestEntry{
		{name: "glowbom.json", data: []byte(`{"syncFormatVersion":"1.0","promptPath":"prompt.txt","initialDrawingPath":null,"iconPath":"","exports":[{"path":"web/index.html"},{"path":"web/index.html"}]}`)},
		{name: "prompt.txt", data: []byte("prompt")},
		{name: "web/index.html", data: []byte("web")},
	}
	if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err != nil {
		t.Fatal(err)
	}
}

func TestProjectArchiveRejectsCorruptionEncryptionAndCompression(t *testing.T) {
	valid := archiveTestZIP(t, archiveTestCloudEntries())
	for _, kind := range []string{"CRC", "encrypted", "strong encryption", "compression", "truncated", "declared size"} {
		t.Run(kind, func(t *testing.T) {
			data := bytes.Clone(valid)
			central := bytes.Index(data, []byte{'P', 'K', 1, 2})
			switch kind {
			case "CRC":
				position := bytes.Index(data, []byte("Build a small garden journal."))
				data[position] ^= 0xff
			case "encrypted":
				data[central+8] |= 1
			case "strong encryption":
				data[central+8] |= 64
			case "compression":
				binary.LittleEndian.PutUint16(data[central+10:], 99)
			case "truncated":
				data = data[:len(data)-30]
			case "declared size":
				binary.LittleEndian.PutUint32(data[central+24:], 1)
			}
			parent := t.TempDir()
			output, err := prepareProjectOutput(filepath.Join(parent, "project"))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := output.Extract(context.Background(), data); err == nil {
				t.Fatal("accepted a corrupt or unsupported ZIP")
			}
			children, _ := os.ReadDir(parent)
			if len(children) != 0 {
				t.Fatal("invalid archive left output or temporary files")
			}
		})
	}
}

func TestProjectArchiveEnforcesSizeAndFileLimits(t *testing.T) {
	if _, err := validateProjectArchive(context.Background(), make([]byte, maxProjectArchiveBytes+1)); err == nil {
		t.Fatal("accepted an oversized ZIP")
	}
	entries := archiveTestCloudEntries()
	entries[0].data = bytes.Repeat([]byte(" "), maxProjectManifestBytes+1)
	if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
		t.Fatal("accepted an oversized manifest")
	}
	entries = archiveTestCloudEntries()
	entries[1].data = bytes.Repeat([]byte("x"), maxProjectUncompressedBytes)
	entries[1].method = zip.Deflate
	if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
		t.Fatal("accepted an oversized uncompressed project")
	}
	entries = archiveTestCloudEntries()
	for len(entries) <= maxProjectFiles {
		entries = append(entries, archiveTestEntry{name: fmt.Sprintf("extra%d.txt", len(entries))})
	}
	if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err == nil {
		t.Fatal("accepted too many files")
	}
	// The exact uncompressed limit remains valid, including the manifest.
	manifest, _ := json.Marshal(map[string]any{"syncFormatVersion": "1.0", "promptPath": "prompt.txt", "exports": []map[string]string{{"path": "web.html"}}})
	entries = []archiveTestEntry{{name: "glowbom.json", data: manifest}, {name: "prompt.txt"}, {name: "web.html", data: bytes.Repeat([]byte("x"), maxProjectUncompressedBytes-len(manifest)), method: zip.Deflate}}
	if _, err := validateProjectArchive(context.Background(), archiveTestZIP(t, entries)); err != nil {
		t.Fatalf("rejected exact size limit: %v", err)
	}
}

type archiveTestContext struct {
	context.Context
	check func() error
}

func (ctx archiveTestContext) Err() error { return ctx.check() }

func TestProjectArchiveCancellationCleansOnlyCreatedFiles(t *testing.T) {
	for _, preserveConcurrentFile := range []bool{false, true} {
		t.Run(fmt.Sprint(preserveConcurrentFile), func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "project")
			output, err := prepareProjectOutput(target)
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			ctx := archiveTestContext{Context: context.Background(), check: func() error {
				if _, err := os.Stat(filepath.Join(target, "glowbom.json")); err == nil {
					if preserveConcurrentFile {
						_ = os.WriteFile(filepath.Join(target, "keep.txt"), []byte("local work"), 0600)
					}
					return context.Canceled
				}
				return nil
			}}
			if _, _, err := output.Extract(ctx, archiveTestZIP(t, archiveTestCloudEntries())); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation: %v", err)
			}
			children, _ := os.ReadDir(parent)
			if !preserveConcurrentFile {
				if len(children) != 0 {
					t.Fatal("failed extraction was not cleaned up")
				}
			} else {
				data, err := os.ReadFile(filepath.Join(target, "keep.txt"))
				if err != nil || string(data) != "local work" {
					t.Fatal("concurrent local work was removed")
				}
				children, _ = os.ReadDir(target)
				if len(children) != 1 || children[0].Name() != "keep.txt" {
					t.Fatal("failed extraction left owned files")
				}
			}
		})
	}
}

func TestProjectArchiveCanceledBeforeExtractionAndCloseCleanup(t *testing.T) {
	parent := t.TempDir()
	output, err := prepareProjectOutput(filepath.Join(parent, "project"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	cancel()
	if _, _, err := output.Extract(ctx, archiveTestZIP(t, archiveTestCloudEntries())); err == nil {
		t.Fatal("accepted a canceled pull")
	}
	output.Close()
	children, _ := os.ReadDir(parent)
	if len(children) != 0 {
		t.Fatal("cancellation left output or temporary files")
	}
	output, err = prepareProjectOutput(filepath.Join(parent, "project"))
	if err != nil {
		t.Fatal(err)
	}
	output.Close()
	output.Close()
	children, _ = os.ReadDir(parent)
	if len(children) != 0 {
		t.Fatal("Close did not clean preflight resources")
	}
	if _, _, err := output.Extract(context.Background(), archiveTestZIP(t, archiveTestCloudEntries())); err == nil {
		t.Fatal("accepted a closed output")
	}
}
