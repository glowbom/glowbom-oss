package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// A separate process distinguishes an editor client from the listening server.
func TestPortClientHelper(t *testing.T) {
	address := os.Getenv("GLOWBOM_TEST_PORT_CLIENT")
	if address == "" {
		return
	}
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Println("connected")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestListPortPIDsUnixExcludesClientConnections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix lsof check")
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestPortClientHelper$")
	cmd.Env = append(os.Environ(), "GLOWBOM_TEST_PORT_CLIENT="+listener.Addr().String())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "connected" {
		t.Fatal("client did not connect")
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pids, err := listPortPIDsUnix(port)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 1 || pids[0] != os.Getpid() {
		t.Fatalf("listeners = %v, want server %d only (client %d)", pids, os.Getpid(), cmd.Process.Pid)
	}
	// Existing connections do not reserve the port after the listener closes.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	pids, err = listPortPIDsUnix(port)
	if err != nil || len(pids) != 0 {
		t.Fatalf("port %s with clients but no listener: %v, %v", strconv.Itoa(port), pids, err)
	}
}
