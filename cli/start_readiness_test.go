package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type startupReadinessTransport func(*http.Request) (*http.Response, error)

func (transport startupReadinessTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func mockStartupReadiness(t *testing.T, transport startupReadinessTransport) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func TestManagedReadinessReportsProcessExitBeforeFirstProbe(t *testing.T) {
	for _, exitError := range []error{nil, errors.New("fixture exit status 17")} {
		process := &managedProcess{done: make(chan struct{}), err: exitError}
		close(process.done)
		mockStartupReadiness(t, func(*http.Request) (*http.Response, error) {
			t.Fatal("readiness probed an exited process")
			return nil, nil
		})
		err := waitForManagedServer(context.Background(), "http://127.0.0.1/healthz", time.Second, process)
		if err == nil || !strings.Contains(err.Error(), "exited before becoming ready") || (exitError != nil && !errors.Is(err, exitError)) {
			t.Fatalf("process exit was hidden: %v", err)
		}
	}
}

func TestManagedReadinessProcessExitCancelsBlockedProbe(t *testing.T) {
	started := make(chan struct{})
	probeCanceled := make(chan struct{})
	mockStartupReadiness(t, func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		close(probeCanceled)
		return nil, request.Context().Err()
	})
	exitError := errors.New("fixture process failed")
	process := &managedProcess{done: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- waitForManagedServer(context.Background(), "http://127.0.0.1/healthz", 10*time.Second, process)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("readiness probe did not begin")
	}
	process.err = exitError
	close(process.done)
	select {
	case err := <-result:
		if !errors.Is(err, exitError) {
			t.Fatalf("blocked probe concealed process exit: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("process exit waited for the HTTP timeout")
	}
	select {
	case <-probeCanceled:
	default:
		t.Fatal("process exit left its HTTP probe running")
	}
}

func TestManagedReadinessCancellationAndTimeoutCancelBlockedProbe(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel bool
	}{
		{"canceled", true},
		{"readiness timeout", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			mockStartupReadiness(t, func(request *http.Request) (*http.Response, error) {
				close(started)
				<-request.Context().Done()
				return nil, request.Context().Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- waitForManagedServer(ctx, "http://127.0.0.1/healthz", 50*time.Millisecond, nil) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("readiness probe did not begin")
			}
			wanted := context.DeadlineExceeded
			if test.cancel {
				wanted = context.Canceled
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, wanted) {
					t.Fatalf("wrong startup cancellation reason: %v", err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("canceled startup left the HTTP probe running")
			}
		})
	}
}

func TestManagedReadinessRejectsSuccessFromExitedProcess(t *testing.T) {
	process := &managedProcess{done: make(chan struct{})}
	mockStartupReadiness(t, func(*http.Request) (*http.Response, error) {
		close(process.done)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ready"))}, nil
	})
	if err := waitForManagedServer(context.Background(), "http://127.0.0.1/healthz", time.Second, process); err == nil || !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Fatalf("a response concealed the owned process exit: %v", err)
	}
}

func TestBackendSourceBuildHelper(t *testing.T) {
	mode := os.Getenv("GLOWBOM_TEST_BACKEND_BUILD_HELPER")
	if mode == "" {
		return
	}
	output := os.Args[len(os.Args)-1]
	if err := os.WriteFile(output+".started", []byte("started"), 0600); err != nil {
		t.Fatal(err)
	}
	if mode == "waiting" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "failed" {
		fmt.Fprintln(os.Stderr, "fixture compilation failed")
		os.Exit(17)
	}
	if delay := os.Getenv("GLOWBOM_TEST_BACKEND_BUILD_DELAY"); delay != "" {
		duration, err := time.ParseDuration(delay)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(duration)
	}
	if err := os.WriteFile(output, []byte(os.Getenv("GLOWBOM_ENABLE_GROK_SUBSCRIPTION_MEDIA")), 0700); err != nil {
		t.Fatal(err)
	}
}

func mockBackendSourceBuild(t *testing.T, commands chan<- *exec.Cmd) {
	t.Helper()
	previous := backendSourceBuildCommand
	backendSourceBuildCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "go" || len(args) != 5 || args[0] != "build" || args[1] != "-buildvcs=false" || args[2] != "-o" || args[4] != "." {
			t.Errorf("unexpected source build command: %s %v", name, args)
		}
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackendSourceBuildHelper$", "--", args[3])
		if commands != nil {
			commands <- command
		}
		return command
	}
	t.Cleanup(func() { backendSourceBuildCommand = previous })
}

func TestBackendSourceBuildDoesNotConsumeReadinessBudget(t *testing.T) {
	mockBackendSourceBuild(t, nil)
	environment := append(os.Environ(), "GLOWBOM_TEST_BACKEND_BUILD_HELPER=success", "GLOWBOM_TEST_BACKEND_BUILD_DELAY=75ms", "GLOWBOM_ENABLE_GROK_SUBSCRIPTION_MEDIA=1")
	executable, cleanup, err := buildBackendExecutable(context.Background(), t.TempDir(), environment, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if runtime.GOOS == "windows" && !strings.HasSuffix(executable, ".exe") {
		t.Fatal("Windows backend executable has no .exe suffix")
	}
	data, err := os.ReadFile(executable)
	if err != nil || string(data) != "1" {
		t.Fatalf("build lost the supplied environment: %v", err)
	}
	mockStartupReadiness(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ready"))}, nil
	})
	if err := waitForManagedServer(context.Background(), "http://127.0.0.1/healthz", 25*time.Millisecond, nil); err != nil {
		t.Fatalf("source compilation consumed server readiness time: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(executable)); !os.IsNotExist(err) {
		t.Fatal("temporary backend executable was not removed")
	}
}

func TestBackendSourceBuildFailureAndCancellationCleanArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   string
		cancel bool
	}{
		{"compile failure", "failed", false},
		{"canceled build", "waiting", true},
		{"build timeout", "waiting", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := make(chan *exec.Cmd, 1)
			mockBackendSourceBuild(t, commands)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			environment := append(os.Environ(), "GLOWBOM_TEST_BACKEND_BUILD_HELPER="+test.mode)
			result := make(chan error, 1)
			timeout := 2 * time.Second
			if test.name == "build timeout" {
				timeout = 200 * time.Millisecond
			}
			go func() {
				executable, cleanup, err := buildBackendExecutable(ctx, t.TempDir(), environment, timeout)
				if executable != "" || cleanup != nil {
					result <- errors.New("failed build returned an executable")
					return
				}
				result <- err
			}()
			var command *exec.Cmd
			select {
			case command = <-commands:
			case <-time.After(time.Second):
				t.Fatal("source build did not start")
			}
			output := command.Args[len(command.Args)-1]
			if test.cancel {
				deadline := time.Now().Add(time.Second)
				for {
					if _, err := os.Stat(output + ".started"); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("build helper did not start")
					}
					time.Sleep(5 * time.Millisecond)
				}
				cancel()
			}
			select {
			case err := <-result:
				if err == nil || (test.cancel && !errors.Is(err, context.Canceled)) || (test.name == "build timeout" && !errors.Is(err, context.DeadlineExceeded)) || (test.mode == "failed" && !strings.Contains(err.Error(), "source build failed")) {
					t.Fatalf("wrong source build failure: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("source build did not stop promptly")
			}
			if command.ProcessState == nil {
				t.Fatal("failed source build was not reaped")
			}
			if _, err := os.Stat(filepath.Dir(output)); !os.IsNotExist(err) {
				t.Fatal("failed source build left temporary artifacts")
			}
		})
	}
}

func TestManagedReadinessRequiresExactLaunchIdentity(t *testing.T) {
	identity := launchIdentity{Instance: "desktop-dev", LaunchID: strings.Repeat("a", 32)}
	for _, test := range []struct {
		name, body string
		pass       bool
	}{
		{"matching", `{"instance":"desktop-dev","launchId":"` + identity.LaunchID + `"}`, true},
		{"other instance", `{"instance":"oss","launchId":"` + identity.LaunchID + `"}`, false},
		{"previous launch", `{"instance":"desktop-dev","launchId":"` + strings.Repeat("b", 32) + `"}`, false},
		{"legacy health", `{"name":"Glowbom OSS","ok":true}`, false},
		{"ordinary HTML", `<title>Glowbom OSS</title>`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			mockStartupReadiness(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})
			err := waitForManagedServerIdentity(context.Background(), "http://127.0.0.1/__glowbom/launch", time.Second, nil, &identity)
			if (err == nil) != test.pass {
				t.Fatalf("identity check: %v", err)
			}
		})
	}
}
