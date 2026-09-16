package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	backendPort = 4569
	webPort     = 4572
)

func runStart(args []string) int {
	showLocalAuth := false
	var positionalArgs []string
	for _, arg := range args {
		switch arg {
		case "--show-local-auth":
			showLocalAuth = true
		case "-h", "--help":
			fmt.Print(usage)
			return 0
		default:
			positionalArgs = append(positionalArgs, arg)
		}
	}
	if len(positionalArgs) > 1 {
		fmt.Fprintln(os.Stderr, "error: glowbom start accepts at most one project path")
		return 2
	}

	glowbomRoot, err := findGlowbomRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	backendDir := filepath.Join(glowbomRoot, "backend")
	webDir := filepath.Join(glowbomRoot, "web")

	// Validate directories exist
	if !isDir(backendDir) {
		fmt.Fprintf(os.Stderr, "error: backend directory not found at %s\n", backendDir)
		return 1
	}
	if !isDir(webDir) {
		fmt.Fprintf(os.Stderr, "error: web directory not found at %s\n", webDir)
		return 1
	}

	// Parse optional project path
	projectPath := ""
	if len(positionalArgs) > 0 {
		p, err := filepath.Abs(positionalArgs[0])
		if err == nil && isDir(p) {
			projectPath = p
		} else if isDir(positionalArgs[0]) {
			projectPath = positionalArgs[0]
		} else {
			fmt.Fprintf(os.Stderr, "error: %s is not a directory\n", positionalArgs[0])
			return 1
		}
	}

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		cancel()
	}()

	if err := reclaimManagedService(glowbomRoot, "backend", backendPort); err != nil {
		fmt.Fprintf(os.Stderr, "error preparing backend port: %v\n", err)
		return 1
	}
	if err := reclaimManagedService(glowbomRoot, "web", webPort); err != nil {
		fmt.Fprintf(os.Stderr, "error preparing web port: %v\n", err)
		return 1
	}
	agentPort, err := strconv.Atoi(localAgentPort())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid OpenCode agent port %q\n", localAgentPort())
		return 1
	}
	if err := reclaimManagedAgentService(agentPort); err != nil {
		fmt.Fprintf(os.Stderr, "error preparing OpenCode agent port: %v\n", err)
		return 1
	}

	serverToken, err := resolveOrGenerateSecret("GLOWBOM_SERVER_TOKEN", "GLOWBY_SERVER_TOKEN")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating backend security token: %v\n", err)
		return 1
	}
	opencodePassword, err := resolveOrGenerateSecret("OPENCODE_SERVER_PASSWORD")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating OpenCode server password: %v\n", err)
		return 1
	}

	backendEnv := append(
		os.Environ(),
		"GLOWBOM_BIND_HOST=127.0.0.1",
		"GLOWBOM_SERVER_TOKEN="+serverToken,
		"GLOWBY_BIND_HOST=127.0.0.1",
		"GLOWBY_SERVER_TOKEN="+serverToken,
		"OPENCODE_SERVER_PASSWORD="+opencodePassword,
	)
	webEnv := append(
		os.Environ(),
		"GLOWBOM_BIND_HOST=127.0.0.1",
		"GLOWBY_BIND_HOST=127.0.0.1",
		fmt.Sprintf("VITE_BACKEND_TARGET=http://127.0.0.1:%d", backendPort),
		"VITE_GLOWBOM_SERVER_TOKEN="+serverToken,
		"VITE_GLOWBY_SERVER_TOKEN="+serverToken,
	)

	// Start backend
	fmt.Println("Starting backend...")
	backendCmd := exec.CommandContext(ctx, "go", "run", ".")
	backendCmd.Dir = backendDir
	backendCmd.Env = backendEnv
	backendCmd.Stdout = os.Stdout
	backendCmd.Stderr = os.Stderr
	if err := backendCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error starting backend: %v\n", err)
		return 1
	}

	if !waitForServer(ctx, fmt.Sprintf("http://127.0.0.1:%d/healthz", backendPort), 30*time.Second) {
		fmt.Fprintln(os.Stderr, "error: backend did not become ready on time")
		cancel()
		_ = backendCmd.Wait()
		return 1
	}
	if showLocalAuth {
		printLocalAuth(serverToken, opencodePassword)
	}

	// Install web dependencies if needed
	nodeModules := filepath.Join(webDir, "node_modules")
	if !isDir(nodeModules) {
		fmt.Println("Installing web dependencies...")
		installCmd := exec.CommandContext(ctx, "bun", "install")
		installCmd.Dir = webDir
		installCmd.Stdout = os.Stdout
		installCmd.Stderr = os.Stderr
		if err := installCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error installing web dependencies: %v\n", err)
			cancel()
			return 1
		}
	}

	if err := recordManagedService(glowbomRoot, "backend", backendPort); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record backend process info: %v\n", err)
	}

	// Start web dev server
	fmt.Println("Starting web UI...")
	webCmd := exec.CommandContext(ctx, "bun", "run", "dev")
	webCmd.Dir = webDir
	webCmd.Env = webEnv
	webCmd.Stdout = os.Stdout
	webCmd.Stderr = os.Stderr
	if err := webCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error starting web UI: %v\n", err)
		cancel()
		return 1
	}

	// Wait for web server to be ready, then open browser
	go func() {
		url := fmt.Sprintf("http://127.0.0.1:%d", webPort)
		if waitForServer(ctx, url, 30*time.Second) {
			if err := recordManagedService(glowbomRoot, "web", webPort); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not record web process info: %v\n", err)
			}
			if projectPath != "" {
				fmt.Printf("\nProject path hint: %s\n", projectPath)
				fmt.Println("Paste or choose this folder in the Glowbom OSS UI to load your project.")
			}
			fmt.Printf("\nOpening %s\n\n", url)
			openBrowser(url)
		}
	}()

	// Wait for both processes
	var wg sync.WaitGroup
	exitCode := 0

	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := backendCmd.Wait(); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "backend exited: %v\n", err)
			exitCode = 1
			cancel()
		}
	}()
	go func() {
		defer wg.Done()
		if err := webCmd.Wait(); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "web UI exited: %v\n", err)
			exitCode = 1
			cancel()
		}
	}()

	wg.Wait()
	if err := stopManagedAgentService(agentPort); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not stop OpenCode agent server on port %d: %v\n", agentPort, err)
	}
	clearManagedService(glowbomRoot, "backend")
	clearManagedService(glowbomRoot, "web")
	return exitCode
}

func findGlowbomRoot() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		if root, ok := searchGlowbomRoot(filepath.Dir(exe)); ok {
			return root, nil
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine working directory: %w", err)
	}
	if root, ok := searchGlowbomRoot(cwd); ok {
		return root, nil
	}

	return "", fmt.Errorf("cannot find the Glowbom OSS checkout root (expected sibling backend/ and web/ directories). Run from the glowbom-oss repo root or one of its subdirectories")
}

func searchGlowbomRoot(start string) (string, bool) {
	dir := start
	for {
		if isDir(filepath.Join(dir, "backend")) && isDir(filepath.Join(dir, "web")) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func waitForServer(ctx context.Context, url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return false
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				return true
			}
		}
	}
}

func reclaimManagedService(glowbomRoot, service string, port int) error {
	pids, err := listPortPIDs(port)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		clearManagedService(glowbomRoot, service)
		return nil
	}

	recorded, err := readManagedService(glowbomRoot, service)
	if err != nil {
		return err
	}
	if len(recorded) == 0 || !isSubset(pids, recorded) {
		if service == "backend" && isExpectedGlowbomService(service, port) {
			// Backend auth is per-run, so pid files can drift across rebuilds/restarts.
			// If /healthz still identifies Glowbom OSS on this port, reclaim it safely.
		} else if !isExpectedGlowbomService(service, port) {
			return fmt.Errorf("port %d is already in use by another app. Stop it and retry", port)
		}
	}

	fmt.Printf("Stopping existing Glowbom OSS %s on port %d...\n", service, port)
	for _, pid := range pids {
		proc, findErr := os.FindProcess(pid)
		if findErr != nil {
			return fmt.Errorf("could not find existing %s process %d: %w", service, pid, findErr)
		}
		if killErr := proc.Kill(); killErr != nil {
			return fmt.Errorf("could not stop existing %s process %d: %w", service, pid, killErr)
		}
	}
	if !waitForPortFree(port, 5*time.Second) {
		return fmt.Errorf("port %d did not become available after stopping the existing %s", port, service)
	}

	clearManagedService(glowbomRoot, service)
	return nil
}

func reclaimManagedAgentService(port int) error {
	pids, err := listPortPIDs(port)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}

	fmt.Printf("Stopping existing agent server on port %d...\n", port)
	return killProcessesOnPort(port, pids, "agent server")
}

func recordManagedService(glowbomRoot, service string, port int) error {
	pids, err := listPortPIDs(port)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return fmt.Errorf("no process found listening on port %d", port)
	}

	stateDir := managedStateDir(glowbomRoot)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}

	lines := make([]string, 0, len(pids))
	for _, pid := range pids {
		lines = append(lines, strconv.Itoa(pid))
	}
	return os.WriteFile(managedStatePath(glowbomRoot, service), []byte(strings.Join(lines, "\n")), 0o644)
}

func readManagedService(glowbomRoot, service string) ([]int, error) {
	data, err := os.ReadFile(managedStatePath(glowbomRoot, service))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var pids []int
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		pid, convErr := strconv.Atoi(line)
		if convErr != nil {
			return nil, fmt.Errorf("invalid pid %q in %s state: %w", line, service, convErr)
		}
		pids = append(pids, pid)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return uniqueSortedInts(pids), nil
}

func clearManagedService(glowbomRoot, service string) {
	_ = os.Remove(managedStatePath(glowbomRoot, service))
}

func stopManagedAgentService(port int) error {
	pids, err := listPortPIDs(port)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}
	return killProcessesOnPort(port, pids, "agent server")
}

func managedStateDir(glowbomRoot string) string {
	sum := sha1.Sum([]byte(glowbomRoot))
	// Keep the legacy directory name so upgraded CLIs can reclaim earlier managed processes.
	return filepath.Join(os.TempDir(), "glowby", fmt.Sprintf("%x", sum[:8]))
}

func managedStatePath(glowbomRoot, service string) string {
	return filepath.Join(managedStateDir(glowbomRoot), fmt.Sprintf("%s.pid", service))
}

func waitForPortFree(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pids, err := listPortPIDs(port)
		if err == nil && len(pids) == 0 {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func killProcessesOnPort(port int, pids []int, label string) error {
	for _, pid := range pids {
		proc, findErr := os.FindProcess(pid)
		if findErr != nil {
			return fmt.Errorf("could not find existing %s process %d: %w", label, pid, findErr)
		}
		if killErr := proc.Kill(); killErr != nil {
			return fmt.Errorf("could not stop existing %s process %d: %w", label, pid, killErr)
		}
	}
	if !waitForPortFree(port, 5*time.Second) {
		return fmt.Errorf("port %d did not become available after stopping the existing %s", port, label)
	}
	return nil
}

func isExpectedGlowbomService(service string, port int) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	url := ""
	expectedValues := []string{}

	switch service {
	case "backend":
		url = fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
		expectedValues = []string{`"name":"Glowbom OSS"`, `"name":"Glowby"`}
	case "web":
		url = fmt.Sprintf("http://127.0.0.1:%d", port)
		expectedValues = []string{"<title>Glowbom OSS</title>", "<title>Glowby</title>"}
	default:
		return false
	}

	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false
	}
	for _, expected := range expectedValues {
		if strings.Contains(string(body), expected) {
			return true
		}
	}
	return false
}

func areExpectedOpenCodeProcesses(pids []int) bool {
	if len(pids) == 0 {
		return true
	}
	for _, pid := range pids {
		command, err := processCommandSummary(pid)
		if err != nil {
			return false
		}
		if !strings.Contains(strings.ToLower(command), "opencode") {
			return false
		}
	}
	return true
}

func resolveOrGenerateSecret(envKeys ...string) (string, error) {
	for _, key := range envKeys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value, nil
		}
	}
	return randomHexSecret(32)
}

func printLocalAuth(serverToken, opencodePassword string) {
	fmt.Println()
	fmt.Println("Local auth credentials:")
	fmt.Printf("  Glowbom OSS backend http://127.0.0.1:%d\n", backendPort)
	fmt.Printf("    Authorization: Bearer %s\n", serverToken)
	fmt.Printf("  OpenCode http://127.0.0.1:%s\n", localAgentPort())
	fmt.Printf("    Username: %s\n", localOpenCodeUsername())
	fmt.Printf("    Password: %s\n", opencodePassword)
	fmt.Println()
}

func localAgentPort() string {
	if port := strings.TrimSpace(os.Getenv("GLOWBOM_AGENT_PORT")); port != "" {
		return port
	}
	return "4571"
}

func localOpenCodeUsername() string {
	if username := strings.TrimSpace(os.Getenv("OPENCODE_SERVER_USERNAME")); username != "" {
		return username
	}
	return "opencode"
}

func randomHexSecret(byteLen int) (string, error) {
	if byteLen <= 0 {
		byteLen = 32
	}
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func listPortPIDs(port int) ([]int, error) {
	switch runtime.GOOS {
	case "windows":
		return listPortPIDsWindows(port)
	default:
		return listPortPIDsUnix(port)
	}
}

func processCommandSummary(pid int) (string, error) {
	switch runtime.GOOS {
	case "windows":
		return processCommandSummaryWindows(pid)
	default:
		return processCommandSummaryUnix(pid)
	}
}

func processCommandSummaryUnix(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("could not inspect process %d with ps: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func processCommandSummaryWindows(pid int) (string, error) {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return "", fmt.Errorf("could not inspect process %d with tasklist: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func listPortPIDsUnix(port int) ([]int, error) {
	// Client connections can outlive the server. Only listeners own the port.
	out, err := exec.Command("lsof", "-nP", "-t", "-a", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(exitErr.Stderr) == 0 && len(out) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("could not inspect port %d with lsof: %w", port, err)
	}
	return parsePIDLines(string(out))
}

func listPortPIDsWindows(port int) ([]int, error) {
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return nil, fmt.Errorf("could not inspect port %d with netstat: %w", port, err)
	}

	target := fmt.Sprintf(":%d", port)
	var pids []int
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		if !strings.EqualFold(fields[0], "TCP") {
			continue
		}
		if !strings.HasSuffix(fields[1], target) || !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		pid, convErr := strconv.Atoi(fields[4])
		if convErr != nil {
			continue
		}
		pids = append(pids, pid)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return uniqueSortedInts(pids), nil
}

func parsePIDLines(text string) ([]int, error) {
	var pids []int
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	return uniqueSortedInts(pids), nil
}

func uniqueSortedInts(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	set := make(map[int]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	out := make([]int, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

func isSubset(values, allowed []int) bool {
	if len(values) == 0 {
		return true
	}
	allowedSet := make(map[int]struct{}, len(allowed))
	for _, value := range allowed {
		allowedSet[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := allowedSet[value]; !ok {
			return false
		}
	}
	return true
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Run()
}
