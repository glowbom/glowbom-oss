// Package opencodecompat identifies the local OpenCode runtime used by Glowbom.
package opencodecompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Runtime struct {
	Executable string `json:"executable"`
	Version    string `json:"version"`
	Protocol   string `json:"protocol"`
	PrintLogs  bool   `json:"-"`
}

var cache struct {
	sync.Mutex
	values map[string]Runtime
}

var versionPattern = regexp.MustCompile(`\b(?:v)?(\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?)\b`)

func Selection() (string, error) {
	selection := strings.TrimSpace(os.Getenv("GLOWBOM_OPENCODE_VERSION"))
	if selection == "" {
		path, err := settingsPath()
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("Could not read the OpenCode version preference.")
		}
		if len(data) > 0 {
			var value struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &value) != nil {
				return "", errors.New("Could not read the OpenCode version preference.")
			}
			selection = value.Version
		}
	}
	if selection == "" {
		selection = "auto"
	}
	if selection != "auto" && selection != "v1" && selection != "v2" {
		return "", errors.New("Choose auto, v1, or v2 for the OpenCode version.")
	}
	return selection, nil
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "glowbom", "opencode-runtime.json"), nil
}

func SaveSelection(selection string) error {
	if selection != "auto" && selection != "v1" && selection != "v2" {
		return errors.New("Choose auto, v1, or v2 for the OpenCode version.")
	}
	if os.Getenv("GLOWBOM_OPENCODE_VERSION") != "" {
		return errors.New("The OpenCode version is set by GLOWBOM_OPENCODE_VERSION. Change that variable and restart Glowbom.")
	}
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".opencode-runtime-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(struct {
		Version string `json:"version"`
	}{selection})
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

// Resolve selects a real executable once, so diagnostics and startup agree.
func Resolve(ctx context.Context) (Runtime, error) {
	selection, err := Selection()
	if err != nil {
		return Runtime{}, err
	}
	if configured := strings.TrimSpace(os.Getenv("GLOWBOM_OPENCODE_BIN")); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return Runtime{}, errors.New("The configured OpenCode executable was not found.")
		}
		value, err := Detect(ctx, path)
		if err == nil && selection != "auto" && value.Protocol != selection {
			err = errors.New("The configured OpenCode executable does not match the selected version.")
		}
		return value, err
	}
	names := []string{"opencode", "opencode2"}
	if runtime.GOOS == "windows" {
		names = []string{"opencode.exe", "opencode2.exe"}
	}
	var directories []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" {
			directories = append(directories, dir)
		}
	}
	directories = append(directories, "/opt/homebrew/bin", "/usr/local/bin")
	if home, err := os.UserHomeDir(); err == nil {
		directories = append(directories, filepath.Join(home, ".opencode", "bin"), filepath.Join(home, ".local", "share", "mise", "installs", "opencode", "latest"), filepath.Join(home, ".local", "bin"))
	}
	seen := map[string]bool{}
	found := false
	for _, dir := range directories {
		for _, name := range names {
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode()&0111 == 0) {
				continue
			}
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				path = resolved
			}
			// Some launchers install or change the global runtime even for --version.
			if updatingMiseWrapper(path) {
				continue
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			found = true
			value, err := Detect(ctx, path)
			if err != nil {
				if selection == "auto" {
					return value, err
				}
				continue
			}
			if selection == "auto" || value.Protocol == selection {
				return value, nil
			}
		}
	}
	if !found {
		return Runtime{}, fmt.Errorf("opencode CLI not found: %w", exec.ErrNotFound)
	}
	return Runtime{}, fmt.Errorf("OpenCode %s was not found. Install that version or choose Automatic in Tools.", strings.TrimPrefix(selection, "v"))
}

func updatingMiseWrapper(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	data, _ := io.ReadAll(io.LimitReader(file, 8192))
	return bytes.HasPrefix(data, []byte("#!")) && bytes.Contains(data, []byte("mise use -g"))
}

func Detect(ctx context.Context, executable string) (Runtime, error) {
	value := Runtime{Executable: executable}
	info, err := os.Stat(executable)
	if err != nil {
		return value, err
	}
	key := fmt.Sprintf("%s:%d:%d", executable, info.Size(), info.ModTime().UnixNano())
	cache.Lock()
	if cached, ok := cache.values[key]; ok {
		cache.Unlock()
		return cached, nil
	}
	cache.Unlock()
	version, versionErr := command(ctx, executable, "--version")
	if match := versionPattern.FindStringSubmatch(version); len(match) > 1 {
		value.Version = match[1]
	}
	switch {
	case strings.HasPrefix(value.Version, "1."):
		value.Protocol, value.PrintLogs = "v1", true
	case strings.HasPrefix(value.Version, "2."):
		value.Protocol = "v2"
	case value.Version != "":
		return value, errors.New("This OpenCode version is not supported. Choose OpenCode 1 or 2.")
	}
	if value.Protocol != "v1" {
		help, helpErr := command(ctx, executable, "serve", "--help")
		lower := strings.ToLower(help)
		if strings.Contains(lower, "v2 api") || strings.Contains(lower, "<all|trace|debug|info|warn") || strings.Contains(lower, "\"warning\"") {
			value.Protocol = "v2"
		}
		if value.Protocol == "" && strings.Contains(help, `"WARN"`) {
			value.Protocol = "v1"
		}
		value.PrintLogs = strings.Contains(help, "--print-logs")
		if helpErr != nil && value.Protocol == "" && versionErr != nil {
			return value, errors.New("Could not inspect the OpenCode executable. Run its --version and serve --help commands in a terminal.")
		}
	}
	if value.Protocol == "" {
		return value, errors.New("This OpenCode executable has an unrecognized server protocol.")
	}
	cache.Lock()
	if cache.values == nil {
		cache.values = map[string]Runtime{}
	}
	cache.values[key] = value
	cache.Unlock()
	return value, nil
}

func command(ctx context.Context, executable string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = append(os.Environ(), "OPENCODE_DISABLE_AUTOUPDATE=true")
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(data []byte) (int, error) {
	n := len(data)
	if available := (64 << 10) - b.Len(); available > 0 {
		if len(data) > available {
			data = data[:available]
		}
		_, _ = b.Buffer.Write(data)
	}
	return n, nil
}

func (value Runtime) ServeArgs(port, hostname, level string) []string {
	if level == "" {
		level = "warn"
	}
	if value.Protocol == "v1" {
		level = strings.ToUpper(level)
	} else {
		level = strings.ToLower(level)
	}
	args := []string{"serve", "--port", port, "--hostname", hostname, "--log-level", level}
	if value.PrintLogs {
		args = append(args, "--print-logs")
	}
	return args
}
