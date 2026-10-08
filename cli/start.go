package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	backendPort = 4569
	webPort     = 4572
)

func runStart(args []string) int {
	options, err := parseStartOptions(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	if options.Help {
		fmt.Print(usage)
		return 0
	}
	launchID, err := resolveLaunchID()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating launch identity: %v\n", err)
		return 2
	}
	isolatedCredentials := options.isolatedCredentials()

	glowbomRoot, err := findGlowbomRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	backendDir := filepath.Join(glowbomRoot, "backend")
	webDir := options.WebDir
	if webDir == "" {
		webDir = filepath.Join(glowbomRoot, "web")
	}
	options.WebDir = webDir

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
	if options.ProjectPath != "" {
		p, err := filepath.Abs(options.ProjectPath)
		if err == nil && isDir(p) {
			projectPath = p
		} else if isDir(options.ProjectPath) {
			projectPath = options.ProjectPath
		} else {
			fmt.Fprintf(os.Stderr, "error: %s is not a directory\n", options.ProjectPath)
			return 1
		}
	}

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			fmt.Println("\nShutting down...")
			cancel()
		case <-ctx.Done():
		}
	}()

	// Hold ownership for the entire launch. Another instance never replaces it.
	unlock, err := acquireStartupLock(ctx, options.Instance)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error preparing local services: %v\n", err)
		return 1
	}
	defer unlock()
	if err := requireLaunchPortsFree(options); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if err := prepareFolderPicker(ctx); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	// Compile before starting services. The readiness budget starts
	// only after the compiled backend process has been launched.
	fmt.Println("Building backend from source...")
	backendExecutable, cleanupBackendBuild, err := buildBackendExecutable(ctx, backendDir, os.Environ(), backendBuildTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error building backend: %v\n", err)
		return 1
	}
	defer cleanupBackendBuild()

	if err := requireLaunchPortsFree(options); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	serverToken, err := launchSecret(isolatedCredentials, "GLOWBOM_SERVER_TOKEN", "GLOWBY_SERVER_TOKEN")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating backend security token: %v\n", err)
		return 1
	}
	opencodePassword, err := launchSecret(isolatedCredentials, "OPENCODE_SERVER_PASSWORD")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating OpenCode server password: %v\n", err)
		return 1
	}

	identity := launchIdentity{Instance: options.Instance, LaunchID: launchID}
	cliPath, _ := os.Executable()
	backendEnv, webEnv := launchEnvironments(options, identity, serverToken, opencodePassword, cliPath)
	state := launchState{launchIdentity: identity, LauncherPID: os.Getpid(), BackendPort: options.BackendPort, WebPort: options.WebPort, AgentPort: options.AgentPort}
	defer clearLaunchState(identity)

	// Start backend
	fmt.Println("Starting backend...")
	backendCmd := exec.CommandContext(ctx, backendExecutable)
	backendCmd.Dir = backendDir
	backendCmd.Env = backendEnv
	backendCmd.Stdout = os.Stdout
	backendCmd.Stderr = os.Stderr
	backend, err := startManagedProcess(backendCmd)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error starting backend: %v\n", err)
		return 1
	}
	defer backend.stop()

	if err := waitForManagedServerIdentity(ctx, fmt.Sprintf("http://127.0.0.1:%d/healthz", options.BackendPort), 30*time.Second, backend, &identity); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error: backend %v\n", err)
		cancel()
		return 1
	}
	if options.ShowLocalAuth {
		printLocalAuth(options, serverToken, opencodePassword)
	}

	// Install web dependencies if needed
	nodeModules := filepath.Join(webDir, "node_modules")
	if !isDir(nodeModules) {
		fmt.Println("Installing web dependencies...")
		installCmd := exec.CommandContext(ctx, "bun", "install")
		installCmd.Dir = webDir
		installCmd.Env = webEnv
		installCmd.Stdout = os.Stdout
		installCmd.Stderr = os.Stderr
		if err := runManagedCommand(installCmd); err != nil {
			if ctx.Err() != nil {
				return 0
			}
			fmt.Fprintf(os.Stderr, "error installing web dependencies: %v\n", err)
			cancel()
			return 1
		}
	}

	state.BackendPID = backend.cmd.Process.Pid
	if err := writeLaunchState(state); err != nil {
		fmt.Fprintf(os.Stderr, "error recording launch ownership: %v\n", err)
		return 1
	}

	// Start web dev server
	fmt.Println("Starting web UI...")
	webCmd := exec.CommandContext(ctx, "bun", "run", "dev")
	webCmd.Dir = webDir
	webCmd.Env = webEnv
	webCmd.Stdout = os.Stdout
	webCmd.Stderr = os.Stderr
	web, err := startManagedProcess(webCmd)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error starting web UI: %v\n", err)
		cancel()
		return 1
	}
	defer web.stop()

	url := fmt.Sprintf("http://127.0.0.1:%d", options.WebPort)
	if err := waitForManagedServerIdentity(ctx, url+"/__glowbom/launch", 30*time.Second, web, &identity); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error: web UI %v\n", err)
		cancel()
		return 1
	}
	state.WebPID = web.cmd.Process.Pid
	if err := writeLaunchState(state); err != nil {
		fmt.Fprintf(os.Stderr, "error recording launch ownership: %v\n", err)
		return 1
	}

	if projectPath != "" {
		fmt.Printf("\nProject path hint: %s\n", projectPath)
		fmt.Println("Paste or choose this folder in the Glowbom OSS UI to load your project.")
	}
	if options.NoBrowser {
		fmt.Printf("\nGlowbom is ready at %s\n\n", url)
	} else {
		fmt.Printf("\nOpening %s\n\n", url)
		openBrowser(url)
	}

	// Only stop this launcher's process trees. A replacement may already own the ports.
	select {
	case <-ctx.Done():
		return 0
	case <-backend.done:
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "backend exited: %v\n", backend.err)
			cancel()
			return 1
		}
	case <-web.done:
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "web UI exited: %v\n", web.err)
			cancel()
			return 1
		}
	}
	return 0
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
	return waitForManagedServer(ctx, url, timeout, nil) == nil
}

func launchSecret(isolated bool, envKeys ...string) (string, error) {
	if isolated {
		return randomHexSecret(32)
	}
	return resolveOrGenerateSecret(envKeys...)
}

func resolveOrGenerateSecret(envKeys ...string) (string, error) {
	for _, key := range envKeys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value, nil
		}
	}
	return randomHexSecret(32)
}

func printLocalAuth(options startOptions, serverToken, opencodePassword string) {
	fmt.Println()
	fmt.Println("Local auth credentials:")
	fmt.Printf("  Glowbom OSS backend http://127.0.0.1:%d\n", options.BackendPort)
	fmt.Printf("    Authorization: Bearer %s\n", serverToken)
	fmt.Printf("  OpenCode http://127.0.0.1:%d\n", options.AgentPort)
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
