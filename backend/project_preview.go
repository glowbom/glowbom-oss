package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type previewTarget struct {
	Target          string   `json:"target"`
	Name            string   `json:"name"`
	Directory       string   `json:"directory"`
	Command         []string `json:"command,omitempty"`
	Description     string   `json:"description,omitempty"`
	Preset          string   `json:"preset,omitempty"`
	PreviewMode     string   `json:"previewMode,omitempty"`
	PreviewNotes    string   `json:"previewNotes,omitempty"`
	CanOpenFolder   bool     `json:"canOpenFolder"`
	CanOpenTerminal bool     `json:"canOpenTerminal"`
	Revision        string   `json:"revision,omitempty"`
	Kind            string   `json:"kind"`
	Available       bool     `json:"available"`
	NeedsInstall    bool     `json:"needsInstall"`
	Reason          string   `json:"reason,omitempty"`
	Status          string   `json:"status"`
	ID              string   `json:"id,omitempty"`
	URL             string   `json:"url,omitempty"`
	Error           string   `json:"error,omitempty"`
	Logs            []string `json:"logs,omitempty"`
	directory       string
	cli             string
}

type previewRequest struct {
	ProjectRoot bool               `json:"projectRoot"`
	Editor      string             `json:"editor"`
	Path        string             `json:"path"`
	Target      string             `json:"target"`
	Action      string             `json:"action"`
	Install     bool               `json:"install"`
	ID          string             `json:"id"`
	Config      *previewDefinition `json:"config"`
}

type previewDefinition struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Directory    string   `json:"directory"`
	Command      []string `json:"command,omitempty"`
	Description  string   `json:"description,omitempty"`
	Preset       string   `json:"preset,omitempty"`
	PreviewMode  string   `json:"previewMode,omitempty"`
	PreviewNotes string   `json:"previewNotes,omitempty"`
}

func readPreviewDefinitions(project string) ([]previewDefinition, error) {
	defs := []previewDefinition{{ID: "prototype", Name: "Prototype", Directory: "prototype"}, {ID: "web", Name: "Web", Directory: "web"}}
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(".glowbom/previews.json")
	if errors.Is(err, os.ErrNotExist) {
		return defs, nil
	}
	if err != nil {
		return nil, errors.New("Could not read the project's preview settings.")
	}
	defer f.Close()
	var custom []previewDefinition
	if err := json.NewDecoder(io.LimitReader(f, 64*1024)).Decode(&custom); err != nil || len(custom) > 20 {
		return nil, errors.New("Fix .glowbom/previews.json before using previews.")
	}
	seen := map[string]bool{"prototype": true, "web": true}
	for _, d := range custom {
		if err := validatePreviewDefinition(d); err != nil || seen[d.ID] {
			return nil, errors.New("Invalid custom preview settings.")
		}
		seen[d.ID] = true
		defs = append(defs, d)
	}
	return defs, nil
}

func validatePreviewDefinition(d previewDefinition) error {
	if d.PreviewMode != "" && d.PreviewMode != "auto" && d.PreviewMode != "command" && d.PreviewMode != "none" {
		return errors.New("Choose automatic, command, or no browser preview.")
	}
	if len(d.PreviewNotes) > 2000 || strings.ContainsRune(d.PreviewNotes, 0) {
		return errors.New("Keep preview notes under 2,000 bytes.")
	}
	if d.PreviewMode == "command" && len(d.Command) == 0 {
		return errors.New("Enter a preview command using {host} and {port}.")
	}
	if (d.PreviewMode == "auto" || d.PreviewMode == "none") && len(d.Command) != 0 {
		return errors.New("Use command preview mode to save a launch command.")
	}
	if len(d.Description) > 8000 || strings.ContainsRune(d.Description, 0) || len(d.Preset) > 80 {
		return errors.New("Keep the stack description under 8,000 bytes.")
	}
	if !strings.HasPrefix(d.ID, "custom-") || strings.ContainsAny(d.ID, "/\\") || len(d.ID) > 80 || strings.TrimSpace(d.Name) == "" || len(d.Name) > 80 {
		return errors.New("Give the custom preview a name.")
	}
	if d.Directory == "" || filepath.IsAbs(d.Directory) || !filepath.IsLocal(d.Directory) {
		return errors.New("Choose a folder inside the project.")
	}
	if len(d.Command) > 0 {
		joined := strings.Join(d.Command, " ")
		if len(d.Command) > 64 || len(joined) > 4096 || strings.ContainsRune(joined, 0) || strings.TrimSpace(d.Command[0]) == "" {
			return errors.New("Invalid preview command.")
		}
		arguments := strings.Join(d.Command[1:], " ")
		if !strings.Contains(arguments, "{host}") || !strings.Contains(arguments, "{port}") {
			return errors.New("The command must include {host} and {port} so Glowbom can select a local address.")
		}
	}
	return nil
}

func savePreviewDefinitions(project string, defs []previewDefinition) error {
	data, err := json.MarshalIndent(defs[2:], "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > 64*1024 {
		return errors.New("Preview settings are too large.")
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir(".glowbom", 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	// A rooted write cannot follow a settings symlink outside the project.
	f, err := root.OpenFile(".glowbom/previews.json", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

type projectPreviewManager struct {
	mu       sync.Mutex
	sessions map[string]*previewSession
	closed   bool
}

type previewSession struct {
	mu         sync.Mutex
	view       previewTarget
	cancel     context.CancelFunc
	server     *http.Server
	project    string
	pendingLog string
	done       chan struct{}
}

func newProjectPreviewManager() *projectPreviewManager {
	return &projectPreviewManager{sessions: make(map[string]*previewSession)}
}

func previewProjectPath(raw string) (string, error) {
	p, err := normalizeExistingDirectoryPath(raw)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func inspectPreviewTarget(project, target string) previewTarget {
	return inspectPreviewDefinition(project, previewDefinition{ID: target, Name: target, Directory: target})
}

func inspectPreviewDefinition(project string, d previewDefinition) previewTarget {
	v := previewTarget{Target: d.ID, Name: d.Name, Directory: d.Directory, Command: d.Command, Description: d.Description, Preset: d.Preset, PreviewMode: d.PreviewMode, PreviewNotes: d.PreviewNotes, Status: "stopped"}
	dir, err := filepath.EvalSymlinks(filepath.Join(project, d.Directory))
	if err != nil || !isPathWithin(project, dir) || !directoryExists(dir) {
		v.Reason = "No " + d.Directory + " folder is available inside this project."
		return v
	}
	v.directory = dir
	_, v.CanOpenTerminal = stackTerminalCommand(dir)
	_, v.CanOpenFolder = folderOpenLaunchCommand()
	if d.PreviewMode == "none" {
		v.Reason = "This stack runs in native tools or a terminal. It has no browser preview."
		return v
	}
	if len(d.Command) > 0 {
		v.Kind, v.Available = "custom", true
		return v
	}
	packagePath := filepath.Join(dir, "package.json")
	if info, err := os.Stat(packagePath); err == nil {
		if info.Size() > 1024*1024 {
			v.Reason = "package.json is too large to inspect."
			return v
		}
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
			Scripts         map[string]string `json:"scripts"`
		}
		data, err := os.ReadFile(packagePath)
		if err != nil || json.Unmarshal(data, &pkg) != nil {
			v.Reason = "Fix package.json before starting a preview."
			return v
		}
		for _, kind := range []string{"next", "vite"} {
			if pkg.Dependencies[kind] != "" || pkg.DevDependencies[kind] != "" {
				v.Kind, v.Available = kind, true
				v.cli = previewFrameworkCLI(project, dir, kind)
				v.NeedsInstall = v.cli == ""
				return v
			}
		}
		if pkg.Scripts["dev"] != "" || pkg.Scripts["start"] != "" {
			v.Reason = "This app uses a different server. Add a custom stack preview with its launch command."
			return v
		}
	}
	if index, err := filepath.EvalSymlinks(filepath.Join(dir, "index.html")); err == nil && isPathWithin(dir, index) {
		if info, err := os.Stat(index); err == nil && info.Mode().IsRegular() {
			v.Kind, v.Available = "static", true
			return v
		}
	}
	v.Reason = "Add index.html or a Next.js or Vite app to this folder to preview it."
	return v
}

func previewFrameworkCLI(project, dir, kind string) string {
	entry := filepath.Join("node_modules", "vite", "bin", "vite.js")
	if kind == "next" {
		entry = filepath.Join("node_modules", "next", "dist", "bin", "next")
	}
	for parent := dir; isPathWithin(project, parent); parent = filepath.Dir(parent) {
		candidate := filepath.Join(parent, entry)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
		if parent == project {
			break
		}
	}
	return ""
}

func (m *projectPreviewManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req previewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&req); err != nil {
		http.Error(w, "Invalid preview request", http.StatusBadRequest)
		return
	}
	project, err := previewProjectPath(req.Path)
	if err != nil {
		http.Error(w, "Project folder is unavailable", http.StatusBadRequest)
		return
	}
	if req.ProjectRoot {
		switch req.Action {
		case "tools":
			_, terminal := stackTerminalCommand(project)
			_, folder := folderOpenLaunchCommand()
			writeJSON(w, map[string]interface{}{"editors": installedStackEditors(), "path": project, "fileManagerLabel": stackFileManagerLabel(), "canOpenTerminal": terminal, "canOpenFolder": folder})
		case "open", "terminal":
			var err error
			if req.Action == "terminal" {
				err = openStackTerminal(project)
			} else {
				err = launchStackTool(project, req.Editor)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]bool{"success": true})
		default:
			http.Error(w, "Unsupported project folder action", http.StatusBadRequest)
		}
		return
	}
	if req.Action != "inspect" && req.Action != "start" && req.Action != "stop" && req.Action != "save" && req.Action != "remove" && req.Action != "terminal" && req.Action != "discover" && req.Action != "folder" && req.Action != "tools" && req.Action != "open" {
		http.Error(w, "Unknown preview action", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		http.Error(w, "Preview service is shutting down", http.StatusServiceUnavailable)
		return
	}
	key := project + "/" + req.Target
	// A settings edit must not prevent an existing server from being stopped.
	if req.Action == "stop" {
		if s := m.sessions[key]; s != nil && (req.ID == "" || s.snapshot().ID == req.ID) {
			s.stop()
			delete(m.sessions, key)
		}
	}
	defs, err := readPreviewDefinitions(project)
	if err != nil {
		if req.Action == "stop" {
			writeJSON(w, map[string]interface{}{"targets": []previewTarget{}})
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Action == "discover" {
		apps, limited, err := discoverProjectStacks(project, defs)
		if err != nil {
			http.Error(w, "Could not inspect project folders.", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]interface{}{"apps": apps, "limited": limited})
		return
	}
	if req.Action == "save" {
		if req.Config == nil {
			http.Error(w, "Missing preview settings", http.StatusBadRequest)
			return
		}
		d := *req.Config
		if d.ID == "" {
			d.ID = "custom-" + randomUUIDString()
		}
		if err := validatePreviewDefinition(d); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		found := false
		for i := range defs {
			if defs[i].ID == d.ID {
				defs[i] = d
				found = true
			}
		}
		if !found {
			if len(defs) >= 22 {
				http.Error(w, "Use at most 20 custom previews", http.StatusBadRequest)
				return
			}
			defs = append(defs, d)
		}
		if err := savePreviewDefinitions(project, defs); err != nil {
			http.Error(w, "Could not save preview settings", http.StatusBadRequest)
			return
		}
		if s := m.sessions[project+"/"+d.ID]; s != nil {
			s.stop()
			delete(m.sessions, project+"/"+d.ID)
		}
	}
	var selected *previewDefinition
	for i := range defs {
		if defs[i].ID == req.Target {
			selected = &defs[i]
		}
	}
	if req.Action != "inspect" && req.Action != "save" && req.Action != "stop" && selected == nil {
		http.Error(w, "Preview target not found", http.StatusBadRequest)
		return
	}
	if req.Action == "remove" && !strings.HasPrefix(req.Target, "custom-") {
		http.Error(w, "Built-in previews cannot be removed", http.StatusBadRequest)
		return
	}
	if req.Action == "tools" || req.Action == "open" {
		v := inspectPreviewDefinition(project, *selected)
		if v.directory == "" {
			http.Error(w, "The stack folder is unavailable inside this project.", http.StatusBadRequest)
			return
		}
		if req.Action == "tools" {
			writeJSON(w, map[string]interface{}{"editors": installedStackEditors(), "path": v.directory, "fileManagerLabel": stackFileManagerLabel()})
			return
		}
		if err := launchStackTool(v.directory, req.Editor); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Action == "folder" {
		v := inspectPreviewDefinition(project, *selected)
		if !v.CanOpenFolder {
			http.Error(w, "The stack folder or a supported file manager is not available.", http.StatusBadRequest)
			return
		}
		if _, err := openProjectFolder(v.directory); err != nil {
			http.Error(w, "Could not open the stack folder.", http.StatusInternalServerError)
			return
		}
	}
	if req.Action == "terminal" {
		v := inspectPreviewDefinition(project, *selected)
		if !v.CanOpenTerminal {
			http.Error(w, "The stack folder or a supported terminal is not available.", http.StatusBadRequest)
			return
		}
		if err := openStackTerminal(v.directory); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.Action == "remove" {
		if s := m.sessions[key]; s != nil && (req.ID == "" || s.snapshot().ID == req.ID) {
			s.stop()
			delete(m.sessions, key)
		}
	}
	if req.Action == "remove" {
		var remaining []previewDefinition
		for _, d := range defs {
			if d.ID != req.Target {
				remaining = append(remaining, d)
			}
		}
		if err := savePreviewDefinitions(project, remaining); err != nil {
			http.Error(w, "Could not save preview settings", http.StatusBadRequest)
			return
		}
		defs = remaining
	}
	if req.Action == "start" {
		v := inspectPreviewDefinition(project, *selected)
		if !v.Available || (v.NeedsInstall && !req.Install) {
			message := v.Reason
			if v.NeedsInstall {
				message = "Install this app's dependencies before starting it."
			}
			http.Error(w, message, http.StatusBadRequest)
			return
		}
		if s := m.sessions[key]; s == nil || s.snapshot().Status == "failed" {
			if s != nil {
				s.stop()
			}
			// Keep the active project's targets and release previous projects.
			for oldKey, old := range m.sessions {
				if old.project != project {
					old.stop()
					delete(m.sessions, oldKey)
				}
			}
			s, err := startProjectPreview(project, v)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			m.sessions[key] = s
		}
	}
	views := make([]previewTarget, 0, len(defs))
	for _, d := range defs {
		v := inspectPreviewDefinition(project, d)
		if s := m.sessions[project+"/"+d.ID]; s != nil {
			running := s.snapshot()
			v.Status, v.ID, v.URL, v.Error, v.Logs = running.Status, running.ID, running.URL, running.Error, running.Logs
			if running.Status == "running" && (v.Kind == "static" || v.Kind == "custom") {
				v.Revision = previewRevision(v.directory)
			}
		}
		// Match the browser's loopback hostname so preview cookies work in frames.
		if strings.HasPrefix(r.Header.Get("Origin"), "http://localhost:") {
			v.URL = strings.Replace(v.URL, "http://127.0.0.1:", "http://localhost:", 1)
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]interface{}{"targets": views, "stackInstructionsSupported": true, "stackCatalogSupported": true})
}

func (m *projectPreviewManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for key, s := range m.sessions {
		s.stop()
		delete(m.sessions, key)
	}
}

func (s *previewSession) snapshot() previewTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.view
	v.Logs = append([]string(nil), v.Logs...)
	if s.pendingLog != "" {
		v.Logs = append(v.Logs, cleanPreviewLog(s.pendingLog))
	}
	return v
}

func (s *previewSession) stop() {
	s.cancel()
	if s.server != nil {
		_ = s.server.Close()
	}
	// Wait for process-group cleanup before the backend can exit.
	if s.done != nil {
		select {
		case <-s.done:
		case <-time.After(4 * time.Second):
		}
	}
}

func (s *previewSession) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.view.Status, s.view.URL = "failed", ""
	s.view.Error = sanitizeProviderError(err)
}

var previewANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
var previewSecret = regexp.MustCompile(`(?i)(token|password|secret|api[_-]?key|authorization|bearer)(["']?\s*[:=]\s*|\s+)(bearer\s+)?[^\s,;]+`)

func (s *previewSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingLog += string(p)
	for {
		end := strings.IndexAny(s.pendingLog, "\n\r")
		if end < 0 {
			break
		}
		line := s.pendingLog[:end]
		s.pendingLog = s.pendingLog[end+1:]
		if line != "" {
			s.view.Logs = append(s.view.Logs, cleanPreviewLog(line))
		}
	}
	// Bound output even when a program never writes a newline.
	if len(s.pendingLog) > 64*1024 {
		s.pendingLog = "[long output omitted]"
	}
	if len(s.view.Logs) > 80 {
		s.view.Logs = s.view.Logs[len(s.view.Logs)-80:]
	}
	return len(p), nil
}

func cleanPreviewLog(line string) string {
	line = previewANSI.ReplaceAllString(line, "")
	line = previewSecret.ReplaceAllString(line, "$1=[redacted]")
	if len(line) > 1500 {
		line = line[:1500] + "..."
	}
	return line
}

func startProjectPreview(project string, v previewTarget) (*previewSession, error) {
	var secret [24]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.ID, v.Status = hex.EncodeToString(secret[:]), "starting"
	s := &previewSession{view: v, cancel: cancel, project: project, done: make(chan struct{})}
	if v.Kind == "static" {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			cancel()
			return nil, fmt.Errorf("could not open a preview port: %w", err)
		}
		root, err := os.OpenRoot(v.directory)
		if err != nil {
			cancel()
			_ = listener.Close()
			return nil, errors.New("could not open the preview folder")
		}
		port := listener.Addr().(*net.TCPAddr).Port
		s.view.URL = fmt.Sprintf("http://127.0.0.1:%d/?glowbom_preview=%s", port, v.ID)
		s.view.Status = "running"
		s.server = &http.Server{Handler: staticPreviewHandler(root, v.ID, port), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			defer close(s.done)
			defer root.Close()
			if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.fail(err)
			}
		}()
		return s, nil
	}
	executable := "bun"
	if v.Kind == "custom" {
		executable = v.Command[0]
	}
	if strings.HasPrefix(executable, ".") {
		executable = filepath.Join(v.directory, executable)
	}
	bun, err := exec.LookPath(executable)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s was not found. Install it and restart Glowbom OSS", filepath.Base(executable))
	}
	go func() {
		defer close(s.done)
		s.runFramework(ctx, project, bun)
	}()
	return s, nil
}

func previewProcessEnv() []string {
	// Project frameworks load their own .env files. Never inherit Glowbom credentials.
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true, "TMP": true, "TEMP": true, "SYSTEMROOT": true, "WINDIR": true, "APPDATA": true, "LOCALAPPDATA": true, "LANG": true}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[strings.ToUpper(key)] {
			env = append(env, entry)
		}
	}
	return append(env, "HOST=127.0.0.1", "BROWSER=none", "NO_COLOR=1", "NEXT_TELEMETRY_DISABLED=1")
}

func (s *previewSession) command(ctx context.Context, bun string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bun, args...)
	cmd.Dir = s.view.directory
	cmd.Env = previewProcessEnv()
	cmd.Stdout, cmd.Stderr = s, s
	cmd.WaitDelay = 2 * time.Second
	configurePreviewProcess(cmd)
	return cmd
}

func (s *previewSession) runFramework(ctx context.Context, project, bun string) {
	v := s.snapshot()
	if v.NeedsInstall {
		s.mu.Lock()
		s.view.Status = "installing"
		s.mu.Unlock()
		installCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		cmd := s.command(installCtx, bun, "install")
		err := cmd.Run()
		stopPreviewProcess(cmd)
		cancel()
		if err != nil {
			s.fail(errors.New("Dependency installation failed. See the preview logs and try again."))
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	v.cli = previewFrameworkCLI(project, v.directory, v.Kind)
	if v.Kind != "custom" && v.cli == "" {
		s.fail(errors.New("The framework is still missing after installation. Check package.json and the preview logs."))
		return
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		s.fail(err)
		return
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	args := []string{"--bun", v.cli}
	if v.Kind == "next" {
		args = append(args, "dev", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	} else if v.Kind == "vite" {
		args = append(args, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--strictPort")
	} else {
		args = make([]string, 0, len(v.Command)-1)
		for _, arg := range v.Command[1:] {
			args = append(args, strings.NewReplacer("{host}", "127.0.0.1", "{port}", strconv.Itoa(port)).Replace(arg))
		}
	}
	cmd := s.command(ctx, bun, args...)
	cmd.Env = append(cmd.Env, "PORT="+strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		s.fail(err)
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer stopPreviewProcess(cmd)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	s.mu.Lock()
	s.view.Status, s.view.NeedsInstall = "starting", false
	s.mu.Unlock()
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	startupTimeout := 90 * time.Second
	if v.Kind == "custom" {
		// SDK startup and game exports can take longer than a frontend dev server.
		startupTimeout = 5 * time.Minute
	}
	deadline := time.NewTimer(startupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-done:
			if err == nil {
				err = errors.New("The preview server stopped.")
			}
			s.fail(fmt.Errorf("preview server exited: %w. See the preview logs", err))
			return
		case <-deadline.C:
			if !ready {
				s.fail(fmt.Errorf("The app did not respond within %s. See the preview logs and retry.", startupTimeout))
				return
			}
		case <-ticker.C:
			if ready {
				continue
			}
			url := respondingPreviewURL(ctx, client, port)
			if url == "" {
				continue
			}
			baseURL = url
			// Error responses can be the framework's useful compile-error screen.
			ready = true
			deadline.Stop()
			s.mu.Lock()
			s.view.Status, s.view.URL = "running", baseURL
			s.mu.Unlock()
		}
	}
}

// Some SDKs bind localhost only on IPv6. Probe explicit loopback addresses
// without following redirects, and publish the address that actually responds.
func respondingPreviewURL(ctx context.Context, client *http.Client, port int) string {
	for _, host := range []string{"127.0.0.1", "::1"} {
		address := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return ""
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ""
			}
			continue
		}
		_ = resp.Body.Close()
		return address
	}
	return ""
}

func staticPreviewHandler(root *os.Root, token string, port int) http.Handler {
	cookieName := "glowbom_preview_" + token[:12]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		host, rawPort, err := net.SplitHostPort(r.Host)
		if err != nil || !isLoopbackHost(host) || rawPort != strconv.Itoa(port) {
			http.Error(w, "Host not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if supplied := r.URL.Query().Get("glowbom_preview"); supplied != "" {
			if subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
				http.Error(w, "Preview access expired", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			u := *r.URL
			q := u.Query()
			q.Del("glowbom_preview")
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
			http.Error(w, "Open this preview from Glowbom OSS", http.StatusUnauthorized)
			return
		}
		// Static previews also reload when opened in a separate browser tab.
		if r.URL.Path == "/__glowbom_preview__/revision" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, previewRevision(root.Name()))
			return
		}
		if r.URL.Path == "/__glowbom_preview__/reload.js" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			fmt.Fprintf(w, `(() => {
  const revision = %q;
  let checking = false;
  setInterval(async () => {
    if (checking) return;
    checking = true;
    try {
      const response = await fetch('/__glowbom_preview__/revision', { cache: 'no-store' });
      if (response.ok && (await response.text()) !== revision) location.reload();
    } catch {} finally { checking = false; }
  }, 1500);
})();`, previewRevision(root.Name()))
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			name += "index.html"
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(root.Name(), filepath.FromSlash(name)))
		rel, relErr := filepath.Rel(root.Name(), resolved)
		if err != nil || relErr != nil || !isPathWithin(root.Name(), resolved) || previewHiddenPath(name) || previewHiddenPath(filepath.ToSlash(rel)) {
			http.NotFound(w, r)
			return
		}
		file, err := root.Open(rel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		if strings.EqualFold(filepath.Ext(info.Name()), ".html") && info.Size() <= 16*1024*1024 {
			data, err := io.ReadAll(io.LimitReader(file, 16*1024*1024+1))
			if err != nil || len(data) > 16*1024*1024 {
				http.Error(w, "Could not read this page", http.StatusInternalServerError)
				return
			}
			data = append(data, []byte(`<script src="/__glowbom_preview__/reload.js"></script>`)...)
			http.ServeContent(w, r, info.Name(), info.ModTime(), bytes.NewReader(data))
			return
		}
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	})
}

func previewHiddenPath(name string) bool {
	for _, part := range strings.Split(strings.ReplaceAll(name, "\\", "/"), "/") {
		if strings.HasPrefix(part, ".") || part == "node_modules" {
			return true
		}
	}
	return false
}

var _ io.Writer = (*previewSession)(nil)

func previewRevision(dir string) string {
	h := sha256.New()
	count := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		if count > 5000 {
			return filepath.SkipAll
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			fmt.Fprintf(h, "%s:%d:%d\n", path, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}
