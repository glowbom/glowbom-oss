package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const starterProjectLimit = 16 * 1024 * 1024

var errStarterProject = errors.New("Could not prepare the starter project. Check your connection and update the Glowbom CLI, then try again.")
var errStarterDestination = errors.New("That folder already exists. Choose another name or open the existing project.")

type starterProjectFile struct {
	name string
	data []byte
	mode os.FileMode
}

// The CLI owns downloading and ZIP validation. Only a completed private copy
// reaches the destination; no downloaded code or dependency installer runs.
func createStarterProject(ctx context.Context, parent, name string, run accountCLIRunner) (string, error) {
	destination, err := sketchProjectDestination(parent, name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", starterProjectContextError(err)
	}
	parent = filepath.Dir(destination)
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return "", errors.New("Could not open the project folder. Check its permissions.")
	}
	defer parentRoot.Close()
	parentInfo, err := parentRoot.Stat(".")
	if err != nil {
		return "", errStarterProject
	}
	if _, err := parentRoot.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", errStarterDestination
		}
		return "", errors.New("Could not check the project folder. Check its permissions.")
	}
	files, err := loadStarterProject(ctx, name, run)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(parent); err != nil || !os.SameFile(info, parentInfo) {
		return "", errors.New("The parent folder changed. Choose it again.")
	}
	if err := installStarterProject(ctx, parentRoot, name, files); err != nil {
		if ctx.Err() != nil {
			return "", starterProjectContextError(ctx.Err())
		}
		return "", err
	}
	return destination, nil
}

// Load validated files without creating a destination, so project import can
// use the same starter before adding its saved prototype and original inputs.
func loadStarterProject(ctx context.Context, name string, run accountCLIRunner) ([]starterProjectFile, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, starterProjectContextError(err)
	}
	stage, err := os.MkdirTemp("", "glowbom-new-project-")
	if err != nil {
		return nil, errStarterProject
	}
	defer os.RemoveAll(stage)
	stagedProject := filepath.Join(stage, "starter")
	_, err = run(ctx, "template", "--output", stagedProject)
	if ctx.Err() != nil {
		return nil, starterProjectContextError(ctx.Err())
	}
	if errors.Is(err, errAccountCLIMissing) {
		return nil, errors.New("Install or update the Glowbom CLI to create a project from the starter.")
	}
	if err != nil {
		return nil, errStarterProject
	}
	files, err := readStarterProject(ctx, stagedProject, name)
	if err != nil {
		if ctx.Err() != nil {
			return nil, starterProjectContextError(ctx.Err())
		}
		return nil, errStarterProject
	}
	return files, nil
}

func starterProjectContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("The starter download took too long. Check your connection and try again.")
	}
	return errors.New("Project creation was canceled.")
}

func omitStarterPath(name string) bool {
	if name == "prototype" || strings.HasPrefix(name, "prototype/") || name == "icon.png" || name == "android/local.properties" {
		return true
	}
	for _, part := range strings.Split(name, "/") {
		if part == "__MACOSX" || part == ".DS_Store" || part == ".idea" || part == "xcuserdata" {
			return true
		}
	}
	return false
}

func readStarterProject(ctx context.Context, directory, name string) ([]starterProjectFile, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errStarterProject
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errStarterProject
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errStarterProject
	}
	var files []starterProjectFile
	count, total := 0, 0
	manifestFound, instructionsFound := false, false
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 513 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return errStarterProject
		}
		if omitStarterPath(path) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			file.Close()
			return errStarterProject
		}
		data, readErr := io.ReadAll(io.LimitReader(file, int64(starterProjectLimit-total)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > starterProjectLimit-total {
			return errStarterProject
		}
		total += len(data)
		if path == "AGENTS.md" {
			instructionsFound = len(bytes.TrimSpace(data)) > 0
		}
		if path == "glowbom.json" {
			data, err = prepareStarterManifest(data, name)
			if err != nil {
				return err
			}
			manifestFound = true
		}
		mode := os.FileMode(0644)
		if path == "android/gradlew" && info.Mode()&0111 != 0 {
			mode = 0755
		}
		files = append(files, starterProjectFile{name: filepath.FromSlash(path), data: data, mode: mode})
		return nil
	})
	if err != nil || !manifestFound || !instructionsFound {
		return nil, errStarterProject
	}
	for _, target := range []string{"apple", "android", "web"} {
		info, err := root.Lstat(target)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errStarterProject
		}
	}
	return files, nil
}

func prepareStarterManifest(data []byte, name string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if len(data) > 512*1024 || json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, errStarterProject
	}
	var targets map[string]struct {
		OutputDir string `json:"outputDir"`
	}
	if json.Unmarshal(fields["targets"], &targets) != nil || targets["ios"].OutputDir != "apple" || targets["android"].OutputDir != "android" || targets["web"].OutputDir != "web" {
		return nil, errStarterProject
	}
	// Patch the raw object so nested target settings and future metadata survive.
	stamp := time.Now().UTC().Format(time.RFC3339)
	for key, value := range map[string]string{"name": name, "displayName": name, "createdAt": stamp, "updatedAt": stamp} {
		fields[key], _ = json.Marshal(value)
	}
	return json.MarshalIndent(fields, "", "  ")
}

type starterCreatedPath struct {
	name string
	info os.FileInfo
	hash [32]byte
	size int64
}

// Mkdir and exclusive file creation preserve any destination that appeared
// during download. Rollback never removes replacement files or local edits.
func installStarterProject(ctx context.Context, parent *os.Root, name string, files []starterProjectFile, installed ...func(os.FileInfo, []starterCreatedPath)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := parent.Mkdir(name, 0755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errStarterDestination
		}
		return errors.New("Could not create the project folder. Check its permissions.")
	}
	info, err := parent.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errStarterProject
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return errStarterProject
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errStarterProject
	}
	var created []starterCreatedPath
	committed := false
	defer func() {
		if committed {
			return
		}
		for i := len(created) - 1; i >= 0; i-- {
			entry := created[i]
			current, err := root.Lstat(entry.name)
			if err != nil || !os.SameFile(current, entry.info) {
				continue
			}
			if !entry.info.IsDir() {
				if current.Size() != entry.size {
					continue
				}
				file, err := root.Open(entry.name)
				if err != nil {
					continue
				}
				digest := sha256.New()
				_, readErr := io.Copy(digest, io.LimitReader(file, entry.size+1))
				file.Close()
				if readErr != nil || !bytes.Equal(digest.Sum(nil), entry.hash[:]) {
					continue
				}
			}
			_ = root.Remove(entry.name)
		}
		_ = root.Close()
		if current, err := parent.Lstat(name); err == nil && os.SameFile(current, info) {
			_ = parent.Remove(name)
		}
	}()
	directories := map[string]bool{}
	makeDirectory := func(path string) error {
		parts := strings.Split(path, string(filepath.Separator))
		for i := 1; i <= len(parts); i++ {
			dir := filepath.Join(parts[:i]...)
			if dir == "." || directories[dir] {
				continue
			}
			if err := root.Mkdir(dir, 0755); err != nil {
				return errStarterProject
			}
			info, err := root.Lstat(dir)
			if err != nil || !info.IsDir() {
				return errStarterProject
			}
			created = append(created, starterCreatedPath{name: dir, info: info})
			directories[dir] = true
		}
		return nil
	}
	for _, entry := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := makeDirectory(filepath.Dir(entry.name)); err != nil {
			return err
		}
		file, err := root.OpenFile(entry.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, entry.mode)
		if err != nil {
			return errStarterProject
		}
		written, writeErr := file.Write(entry.data)
		fileInfo, statErr := file.Stat()
		closeErr := file.Close()
		if statErr == nil {
			created = append(created, starterCreatedPath{name: entry.name, info: fileInfo, size: int64(written), hash: sha256.Sum256(entry.data[:written])})
		}
		if writeErr != nil || statErr != nil || closeErr != nil {
			return errStarterProject
		}
	}
	if err := makeDirectory(filepath.Join("prototype", "assets")); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(current, info) {
		return errStarterProject
	}
	parentInfo, err := parent.Stat(".")
	currentParent, pathErr := os.Stat(parent.Name())
	if err != nil || pathErr != nil || !os.SameFile(parentInfo, currentParent) {
		return errors.New("The parent folder changed. Choose it again.")
	}
	committed = true
	if len(installed) > 0 && installed[0] != nil {
		installed[0](info, created)
	}
	return nil
}
