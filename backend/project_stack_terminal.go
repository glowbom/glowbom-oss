package main

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
)

// Open a folder, never interpolate project paths into shell commands.
func stackTerminalCommand(directory string) (*exec.Cmd, bool) {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"open", "-a", "Terminal", directory}}
	case "windows":
		candidates = [][]string{{"wt.exe", "-d", directory}}
	case "linux":
		candidates = [][]string{{"gnome-terminal", "--working-directory=" + directory}, {"konsole", "--workdir", directory}, {"xfce4-terminal", "--working-directory=" + directory}}
	}
	for _, args := range candidates {
		if path, err := exec.LookPath(args[0]); err == nil {
			cmd := exec.Command(path, args[1:]...)
			cmd.Dir = directory
			cmd.Env = previewProcessEnv()
			for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "TERM", "COLORTERM"} {
				if value, ok := os.LookupEnv(key); ok {
					cmd.Env = append(cmd.Env, key+"="+value)
				}
			}
			return cmd, true
		}
	}
	return nil, false
}

func openStackTerminal(directory string) error {
	cmd, ok := stackTerminalCommand(directory)
	if !ok {
		return errors.New("No supported terminal was found.")
	}
	if err := cmd.Start(); err != nil {
		return errors.New("Could not open the terminal.")
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
