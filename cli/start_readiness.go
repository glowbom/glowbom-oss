package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// A cold source build includes Go dependencies and compilation. It does not
// consume the separate 30-second budget for the running backend to respond.
const backendBuildTimeout = 10 * time.Minute

var backendSourceBuildCommand = exec.CommandContext

func buildBackendExecutable(ctx context.Context, directory string, environment []string, timeout time.Duration) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	temporary, err := os.MkdirTemp("", "glowbom-backend-build-")
	if err != nil {
		return "", nil, fmt.Errorf("could not prepare the source build: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	completed := false
	defer func() {
		if !completed {
			cleanup()
		}
	}()
	name := "glowbom-backend"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(temporary, name)
	buildContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := backendSourceBuildCommand(buildContext, "go", "build", "-buildvcs=false", "-o", executable, ".")
	command.Dir, command.Env = directory, environment
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	build, err := startManagedProcess(command)
	if err != nil {
		return "", nil, fmt.Errorf("could not start the source build: %w", err)
	}
	defer build.stop()
	<-build.done
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err := buildContext.Err(); err != nil {
		return "", nil, fmt.Errorf("source build did not finish within %s: %w", timeout, err)
	}
	if build.err != nil {
		return "", nil, fmt.Errorf("source build failed: %w", build.err)
	}
	completed = true
	return executable, cleanup, nil
}

func waitForManagedServer(ctx context.Context, url string, timeout time.Duration, process *managedProcess) error {
	return waitForManagedServerIdentity(ctx, url, timeout, process, nil)
}

func waitForManagedServerIdentity(ctx context.Context, url string, timeout time.Duration, process *managedProcess, identity *launchIdentity) error {
	waiting, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if process != nil {
		// Cancel an in-flight HTTP request as soon as its process exits.
		go func() {
			select {
			case <-process.done:
				cancel()
			case <-waiting.Done():
			}
		}()
	}
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := managedReadinessError(ctx, waiting, process, timeout); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(waiting, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("could not check readiness: %w", err)
		}
		response, err := client.Do(request)
		if err == nil {
			if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
				var actual launchIdentity
				if identity != nil {
					err = json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&actual)
				}
				_ = response.Body.Close()
				if err != nil || (identity != nil && actual != *identity) {
					return fmt.Errorf("readiness endpoint belongs to a different launch or returned an invalid identity")
				}
				return managedReadinessError(ctx, waiting, process, timeout)
			}
			_ = response.Body.Close()
		}
		select {
		case <-waiting.Done():
			return managedReadinessError(ctx, waiting, process, timeout)
		case <-ticker.C:
		}
	}
}

func managedReadinessError(parent, waiting context.Context, process *managedProcess, timeout time.Duration) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if process != nil {
		select {
		case <-process.done:
			if process.err != nil {
				return fmt.Errorf("exited before becoming ready: %w", process.err)
			}
			return fmt.Errorf("exited before becoming ready")
		default:
		}
	}
	if err := waiting.Err(); err != nil {
		return fmt.Errorf("did not become ready within %s: %w", timeout, err)
	}
	return nil
}
