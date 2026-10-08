package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func pickerTestLookPath(available map[string]bool) func(string) (string, error) {
	return func(name string) (string, error) {
		if available[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestFolderPickerPreservesExistingPicker(t *testing.T) {
	for _, name := range []string{"zenity", "qarma", "yad", "kdialog"} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := ensureLinuxFolderPicker(context.Background(), true, false, true, pickerTestLookPath(map[string]bool{name: true}), func(context.Context, string, ...string) error {
				t.Fatal("attempted installation with an existing picker")
				return nil
			}, &out)
			if err != nil || out.Len() != 0 {
				t.Fatalf("existing picker: err=%v, output=%q", err, out.String())
			}
		})
	}
}

func TestFolderPickerSkipsHeadlessInstallation(t *testing.T) {
	var out bytes.Buffer
	err := ensureLinuxFolderPicker(context.Background(), false, false, true, pickerTestLookPath(map[string]bool{"pacman": true}), func(context.Context, string, ...string) error {
		t.Fatal("attempted installation without a desktop session")
		return nil
	}, &out)
	if err != nil || !strings.Contains(out.String(), "sudo /usr/bin/pacman") {
		t.Fatalf("headless setup: err=%v, output=%q", err, out.String())
	}
}

func TestFolderPickerUsesSystemInstaller(t *testing.T) {
	tests := []struct {
		name       string
		tools      []string
		root       bool
		terminal   bool
		executable string
		args       []string
	}{
		{"omarchy terminal", []string{"omarchy", "pacman"}, false, true, "omarchy", []string{"pkg", "add", "zenity"}},
		{"omarchy root", []string{"omarchy", "pacman"}, true, false, "omarchy", []string{"pkg", "add", "zenity"}},
		{"arch terminal", []string{"pacman", "sudo"}, false, true, "sudo", []string{"/usr/bin/pacman", "-S", "--needed", "--noconfirm", "zenity"}},
		{"omarchy graphical", []string{"omarchy", "pacman", "sudo", "pkexec"}, false, false, "pkexec", []string{"/usr/bin/pacman", "-S", "--needed", "--noconfirm", "zenity"}},
		{"ubuntu terminal", []string{"apt-get", "sudo"}, false, true, "sudo", []string{"/usr/bin/apt-get", "install", "-y", "zenity"}},
		{"fedora graphical", []string{"dnf", "pkexec"}, false, false, "pkexec", []string{"/usr/bin/dnf", "install", "-y", "zenity"}},
		{"suse root", []string{"zypper"}, true, false, "zypper", []string{"--non-interactive", "install", "zenity"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			available := make(map[string]bool)
			for _, tool := range test.tools {
				available[tool] = true
			}
			var out bytes.Buffer
			calls := 0
			err := ensureLinuxFolderPicker(context.Background(), true, test.root, test.terminal, pickerTestLookPath(available), func(_ context.Context, executable string, args ...string) error {
				calls++
				if executable != "/usr/bin/"+test.executable || !reflect.DeepEqual(args, test.args) {
					t.Fatalf("install command = %s %v, want %s %v", executable, args, test.executable, test.args)
				}
				available["zenity"] = true
				return nil
			}, &out)
			if err != nil || calls != 1 || !strings.Contains(out.String(), "picker is ready") {
				t.Fatalf("setup: err=%v, calls=%d, output=%q", err, calls, out.String())
			}
		})
	}
}

func TestFolderPickerInstallationFailures(t *testing.T) {
	tests := []struct {
		name    string
		tools   map[string]bool
		runErr  error
		wantErr string
		calls   int
	}{
		{"unknown package manager", nil, nil, "no supported package manager", 0},
		{"missing elevation", map[string]bool{"pacman": true}, nil, "sudo is unavailable", 0},
		{"installation denied", map[string]bool{"pacman": true, "sudo": true}, errors.New("denied"), "could not install Zenity: denied", 1},
		{"success without picker", map[string]bool{"pacman": true, "sudo": true}, nil, "no Linux folder picker is available on PATH", 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			err := ensureLinuxFolderPicker(context.Background(), true, false, true, pickerTestLookPath(test.tools), func(context.Context, string, ...string) error {
				calls++
				return test.runErr
			}, &out)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) || calls != test.calls || strings.Contains(out.String(), "picker is ready") {
				t.Fatalf("setup: err=%v, calls=%d, output=%q", err, calls, out.String())
			}
		})
	}
}

func TestFolderPickerCancellation(t *testing.T) {
	for _, duringInstall := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if !duringInstall {
			cancel()
		}
		var out bytes.Buffer
		err := ensureLinuxFolderPicker(ctx, true, false, true, pickerTestLookPath(map[string]bool{"pacman": true, "sudo": true}), func(ctx context.Context, _ string, _ ...string) error {
			if !duringInstall {
				t.Fatal("ran installer after cancellation")
			}
			cancel()
			return ctx.Err()
		}, &out)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation during install=%v: %v", duringInstall, err)
		}
	}
}

func TestDoctorReportsFolderPicker(t *testing.T) {
	for _, installed := range []bool{false, true} {
		var out bytes.Buffer
		ok := reportLinuxFolderPicker(&out, pickerTestLookPath(map[string]bool{"kdialog": installed, "omarchy": true}))
		if ok != installed {
			t.Fatalf("doctor result=%v, installed=%v", ok, installed)
		}
		if installed {
			if !strings.Contains(out.String(), "[ok]") || !strings.Contains(out.String(), "/usr/bin/kdialog") {
				t.Fatalf("installed picker output=%q", out.String())
			}
		} else if !strings.Contains(out.String(), "[MISSING]") || !strings.Contains(out.String(), "omarchy pkg add zenity") || strings.Contains(out.String(), "sudo /usr/bin/omarchy") {
			t.Fatalf("missing picker output=%q", out.String())
		}
	}
}
