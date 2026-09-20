package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

type stackEditor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var stackEditors = []struct {
	stackEditor
	app string
	cli string
}{
	{stackEditor{"vscode", "VS Code"}, "Visual Studio Code", "code"},
	{stackEditor{"cursor", "Cursor"}, "Cursor", "cursor"},
	{stackEditor{"zed", "Zed"}, "Zed", "zed"},
	{stackEditor{"antigravity", "Antigravity"}, "Antigravity", "antigravity"},
	{stackEditor{"xcode", "Xcode"}, "Xcode", ""},
	{stackEditor{"android-studio", "Android Studio"}, "Android Studio", "studio"},
}

func stackEditorCommand(id string) (openCodeLaunchCommand, bool) {
	for _, editor := range stackEditors {
		if editor.ID != id {
			continue
		}
		if runtime.GOOS == "darwin" {
			home, _ := os.UserHomeDir()
			for _, base := range []string{"/Applications", "/System/Applications", filepath.Join(home, "Applications")} {
				app := filepath.Join(base, editor.app+".app")
				if applicationExists(app) {
					if bin := resolveLaunchExecutable("open"); bin != "" {
						return openCodeLaunchCommand{Executable: bin, Args: []string{"-a", app}}, true
					}
				}
			}
		}
		if editor.ID == "vscode" {
			return vsCodeLaunchCommand()
		}
		if editor.ID == "android-studio" {
			return androidStudioLaunchCommand()
		}
		if editor.cli != "" {
			if bin := resolveLaunchExecutable(editor.cli); bin != "" {
				return openCodeLaunchCommand{Executable: bin}, true
			}
		}
	}
	return openCodeLaunchCommand{}, false
}

func installedStackEditors() []stackEditor {
	result := []stackEditor{}
	for _, editor := range stackEditors {
		if _, ok := stackEditorCommand(editor.ID); ok {
			result = append(result, editor.stackEditor)
		}
	}
	return result
}

func stackFileManagerLabel() string {
	if runtime.GOOS == "darwin" {
		return "Show in Finder"
	}
	if runtime.GOOS == "windows" {
		return "Open in File Explorer"
	}
	return "Open in File Manager"
}

func launchStackTool(directory, editor string) error {
	var command openCodeLaunchCommand
	var ok bool
	if editor == "folder" {
		command, ok = folderOpenLaunchCommand()
		if runtime.GOOS == "darwin" {
			command.Args = append(command.Args, "-R")
		}
	} else {
		command, ok = stackEditorCommand(editor)
	}
	if !ok {
		return errors.New("This application is not available on this computer.")
	}
	args := append(append([]string{}, command.Args...), directory)
	cmd := exec.Command(command.Executable, args...)
	cmd.Env = previewProcessEnv()
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, exists := os.LookupEnv(key); exists {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	if err := cmd.Start(); err != nil {
		return errors.New("Could not launch the selected application.")
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
