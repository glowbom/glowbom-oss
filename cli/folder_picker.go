package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

type pickerInstaller struct {
	executable string
	args       []string
	elevates   bool
}

func linuxFolderPicker(lookPath func(string) (string, error)) string {
	for _, name := range []string{"zenity", "qarma", "yad", "kdialog"} {
		if path, err := lookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func linuxPickerInstaller(lookPath func(string) (string, error), allowOmarchy bool) (pickerInstaller, error) {
	if allowOmarchy {
		if path, err := lookPath("omarchy"); err == nil {
			return pickerInstaller{path, []string{"pkg", "add", "zenity"}, true}, nil
		}
	}
	for _, installer := range []pickerInstaller{
		{"pacman", []string{"-S", "--needed", "--noconfirm", "zenity"}, false},
		{"apt-get", []string{"install", "-y", "zenity"}, false},
		{"dnf", []string{"install", "-y", "zenity"}, false},
		{"zypper", []string{"--non-interactive", "install", "zenity"}, false},
	} {
		if path, err := lookPath(installer.executable); err == nil {
			installer.executable = path
			return installer, nil
		}
	}
	return pickerInstaller{}, fmt.Errorf("no supported package manager found; install zenity, kdialog, yad, or qarma and retry")
}

func linuxPickerInstallHint(lookPath func(string) (string, error)) string {
	installer, err := linuxPickerInstaller(lookPath, true)
	if err != nil {
		return "Install zenity, kdialog, yad, or qarma using your system package manager."
	}
	prefix := "sudo "
	if installer.elevates {
		prefix = ""
	}
	return "Install: " + prefix + installer.executable + " " + strings.Join(installer.args, " ")
}

func reportLinuxFolderPicker(out io.Writer, lookPath func(string) (string, error)) bool {
	if path := linuxFolderPicker(lookPath); path != "" {
		fmt.Fprintf(out, "  [ok]    Linux folder picker (%s)\n", path)
		return true
	}
	fmt.Fprintln(out, "  [MISSING] Linux folder picker for opening and saving projects")
	fmt.Fprintf(out, "            %s\n", linuxPickerInstallHint(lookPath))
	fmt.Fprintln(out, "            glowbom start installs Zenity automatically in a desktop session.")
	return false
}

func ensureLinuxFolderPicker(ctx context.Context, desktop, root, terminal bool, lookPath func(string) (string, error), run func(context.Context, string, ...string) error, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if linuxFolderPicker(lookPath) != "" {
		return nil
	}
	if !desktop {
		fmt.Fprintln(out, "No Linux folder picker found. Automatic installation requires a desktop session.")
		fmt.Fprintln(out, linuxPickerInstallHint(lookPath))
		return nil
	}
	// Omarchy manages its own sudo prompt and needs a terminal unless already root.
	installer, err := linuxPickerInstaller(lookPath, terminal || root)
	if err != nil {
		return err
	}
	executable, args := installer.executable, installer.args
	if !root && !installer.elevates {
		elevation := "pkexec"
		if terminal {
			elevation = "sudo"
		}
		executable, err = lookPath(elevation)
		if err != nil {
			return fmt.Errorf("%s is unavailable for installation. %s", elevation, linuxPickerInstallHint(lookPath))
		}
		args = append([]string{installer.executable}, args...)
	}
	fmt.Fprintln(out, "Installing Zenity for opening and saving project folders...")
	if err := run(ctx, executable, args...); err != nil {
		return fmt.Errorf("could not install Zenity: %w. %s", err, linuxPickerInstallHint(lookPath))
	}
	if linuxFolderPicker(lookPath) == "" {
		return fmt.Errorf("installation finished but no Linux folder picker is available on PATH. %s", linuxPickerInstallHint(lookPath))
	}
	fmt.Fprintln(out, "Linux folder picker is ready.")
	return nil
}

func prepareFolderPicker(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	desktop := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	return ensureLinuxFolderPicker(ctx, desktop, os.Geteuid() == 0, pickerHasTerminal(), exec.LookPath, func(ctx context.Context, executable string, args ...string) error {
		cmd := exec.CommandContext(ctx, executable, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}, os.Stdout)
}
