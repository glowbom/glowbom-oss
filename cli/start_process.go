package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

type managedProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	err      error
	stopOnce sync.Once
}

func startManagedProcess(cmd *exec.Cmd) (*managedProcess, error) {
	configureManagedProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := &managedProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		process.err = cmd.Wait()
		close(process.done)
	}()
	return process, nil
}

func (p *managedProcess) stop() {
	p.stopOnce.Do(func() {
		_ = stopManagedProcessTree(p.cmd)
		<-p.done
	})
}

func runManagedCommand(cmd *exec.Cmd) error {
	process, err := startManagedProcess(cmd)
	if err != nil {
		return err
	}
	defer process.stop()
	<-process.done
	return process.err
}

func acquireStartupLock(ctx context.Context, instance string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !launchInstanceName.MatchString(instance) {
		return nil, fmt.Errorf("invalid launch instance")
	}
	stateDir := launchStateDir(instance)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(stateDir, "start.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := tryStartupFileLock(file)
	if err != nil || !locked {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("could not lock local launch: %w", err)
		}
		return nil, fmt.Errorf("instance %q is already running. Stop its existing launch first; no existing process was stopped", instance)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockStartupFile(file)
			_ = file.Close()
		})
	}, nil
}
