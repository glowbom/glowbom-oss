package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type launchIdentity struct {
	Instance string `json:"instance"`
	LaunchID string `json:"launchId"`
}

type launchState struct {
	launchIdentity
	LauncherPID int `json:"launcherPid"`
	BackendPID  int `json:"backendPid"`
	WebPID      int `json:"webPid"`
	BackendPort int `json:"backendPort"`
	WebPort     int `json:"webPort"`
	AgentPort   int `json:"agentPort"`
}

var launchIDFormat = regexp.MustCompile(`^[a-f0-9]{32}$`)

func resolveLaunchID() (string, error) {
	if value := strings.TrimSpace(os.Getenv("GLOWBOM_LAUNCH_ID")); value != "" {
		if !launchIDFormat.MatchString(value) {
			return "", fmt.Errorf("GLOWBOM_LAUNCH_ID must contain 32 lowercase hexadecimal characters")
		}
		return value, nil
	}
	return randomHexSecret(16)
}

func launchStateDir(instance string) string {
	return filepath.Join(os.TempDir(), "glowbom-launcher", "instances", instance)
}

// These records are diagnostic ownership metadata, never permission to kill a PID.
func writeLaunchState(state launchState) error {
	directory := launchStateDir(state.Instance)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".launch-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(append(data, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), filepath.Join(directory, "launch.json"))
}

func clearLaunchState(identity launchIdentity) {
	path := filepath.Join(launchStateDir(identity.Instance), "launch.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state launchState
	if json.Unmarshal(data, &state) != nil || state.launchIdentity != identity {
		return
	}
	_ = os.Remove(path)
}
