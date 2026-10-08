package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// The helper creates an actual child listener, like go run starting the backend.
func TestManagedProcessHelper(t *testing.T) {
	mode := os.Getenv("GLOWBOM_TEST_PROCESS_HELPER")
	if mode == "" {
		return
	}
	if mode == "parent" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestManagedProcessHelper$")
		cmd.Env = append(os.Environ(), "GLOWBOM_TEST_PROCESS_HELPER=listener")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		// Keep the parent alive after its listener closes to test late cleanup.
		for {
			time.Sleep(time.Hour)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("GLOWBOM_TEST_PROCESS_PORT"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]int{"pid": os.Getpid(), "port": listener.Addr().(*net.TCPAddr).Port})
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

func startTestManagedListener(t *testing.T, ctx context.Context, mode string, port int) (*managedProcess, int, int) {
	t.Helper()
	binary := os.Args[0]
	if testBinary := os.Getenv("GLOWBOM_TEST_PROCESS_BINARY"); testBinary != "" {
		binary = testBinary
	}
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestManagedProcessHelper$")
	cmd.Env = append(os.Environ(), "GLOWBOM_TEST_PROCESS_HELPER="+mode, "GLOWBOM_TEST_PROCESS_PORT="+strconv.Itoa(port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	process, err := startManagedProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(process.stop)
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("helper listener did not start")
	}
	var address struct {
		PID  int `json:"pid"`
		Port int `json:"port"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &address); err != nil {
		t.Fatal(err)
	}
	return process, address.PID, address.Port
}

func awaitTestPortClosed(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("owned listener survived cleanup")
}

func TestManagedProcessCancellationStopsGrandchild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, _, port := startTestManagedListener(t, ctx, "parent", 0)
	cancel()
	<-process.done
	awaitTestPortClosed(t, port)
}

func TestManagedProcessCleanupLeavesReplacementOnSamePort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process group replacement check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	old, childPID, port := startTestManagedListener(t, ctx, "parent", 0)
	child, err := os.FindProcess(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Kill(); err != nil {
		t.Fatal(err)
	}
	awaitTestPortClosed(t, port)
	replacement, _, _ := startTestManagedListener(t, ctx, "listener", port)
	old.stop()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal("old launcher cleanup stopped replacement:", err)
	}
	_ = conn.Close()
	select {
	case <-replacement.done:
		t.Fatal("replacement exited")
	default:
	}
}

func TestStartupLockSeparatesInstancesAndRefusesDuplicate(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", os.Getenv("TMPDIR"))
	first, err := acquireStartupLock(context.Background(), "oss")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	started := time.Now()
	if unlock, err := acquireStartupLock(context.Background(), "oss"); err == nil || unlock != nil {
		t.Fatal("duplicate instance acquired the lock")
	}
	if time.Since(started) > time.Second {
		t.Fatal("duplicate launch did not fail promptly")
	}
	other, err := acquireStartupLock(context.Background(), "desktop-dev")
	if err != nil {
		t.Fatal("different instance was blocked:", err)
	}
	other()
	first()
	first()
	restarted, err := acquireStartupLock(context.Background(), "oss")
	if err != nil {
		t.Fatal(err)
	}
	restarted()
}

func TestStartupLockRejectsCanceledContextBeforeAcquiring(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", os.Getenv("TMPDIR"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unlock, err := acquireStartupLock(ctx, "oss")
	if unlock != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup acquired lock: unlockPresent=%t err=%v", unlock != nil, err)
	}
}

func TestWaitForServerRequiresSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if waitForServer(context.Background(), server.URL, 650*time.Millisecond) {
		t.Fatal("unhealthy server was accepted as ready")
	}
}

func TestLaunchRefusesOccupiedPortWithoutStoppingListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	options := startOptions{BackendPort: port, WebPort: 1, AgentPort: 2}
	if err := requireLaunchPortsFree(options); err == nil {
		t.Fatal("occupied port was accepted")
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal("unrelated listener was stopped:", err)
	}
	_ = conn.Close()
}

func TestManagedCommandFailureCleansItsChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, _, port := startTestManagedListener(t, ctx, "parent", 0)
	// A later startup phase fails. Deferred cleanup affects only this process tree.
	process.stop()
	awaitTestPortClosed(t, port)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer other.Close()
	process.stop()
	if !waitForServer(ctx, other.URL, time.Second) {
		t.Fatal("late cleanup affected another server")
	}
}
