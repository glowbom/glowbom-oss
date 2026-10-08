package opencodecompat

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func fixtureBinary(t *testing.T, dir, version, help string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo '" + version + "';;\nserve) echo '" + help + "';;\nesac\n"
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDetectAndServeArgs(t *testing.T) {
	for _, tc := range []struct {
		version, help, protocol, level string
		logs                           bool
	}{
		{"1.18.34", "--print-logs --log-level DEBUG INFO WARN ERROR", "v1", "WARN", true},
		{"opencode v2.0.21", "--log-level <all|trace|debug|info|warn|warning|error|fatal|none>", "v2", "warn", false},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			path := fixtureBinary(t, t.TempDir(), tc.version, tc.help)
			value, err := Detect(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if value.Protocol != tc.protocol {
				t.Fatalf("protocol = %s", value.Protocol)
			}
			want := []string{"serve", "--port", "4096", "--hostname", "127.0.0.1", "--log-level", tc.level}
			if tc.logs {
				want = append(want, "--print-logs")
			}
			if got := value.ServeArgs("4096", "127.0.0.1", "WaRn"); !reflect.DeepEqual(got, want) {
				t.Fatalf("args = %v, want %v", got, want)
			}
		})
	}
}

func TestResolvePreferenceAndOverride(t *testing.T) {
	root := t.TempDir()
	first := fixtureBinary(t, filepath.Join(root, "first"), "2.0.21", "--log-level <all|trace|debug|info|warn>")
	second := fixtureBinary(t, filepath.Join(root, "second"), "1.18.34", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GLOWBOM_OPENCODE_VERSION", "auto")
	t.Setenv("GLOWBOM_OPENCODE_BIN", "")
	t.Setenv("PATH", filepath.Dir(first)+string(os.PathListSeparator)+filepath.Dir(second))
	value, err := Resolve(context.Background())
	if err != nil || value.Executable != first {
		t.Fatalf("automatic = %+v, %v", value, err)
	}
	t.Setenv("GLOWBOM_OPENCODE_VERSION", "v1")
	value, err = Resolve(context.Background())
	if err != nil || value.Executable != second {
		t.Fatalf("v1 = %+v, %v", value, err)
	}
	t.Setenv("GLOWBOM_OPENCODE_BIN", first)
	if _, err := Resolve(context.Background()); err == nil {
		t.Fatal("mismatched override accepted")
	}
}

func TestSelectionPersistsAndRejectsInvalid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GLOWBOM_OPENCODE_VERSION", "")
	if err := SaveSelection("v2"); err != nil {
		t.Fatal(err)
	}
	if got, err := Selection(); got != "v2" || err != nil {
		t.Fatalf("selection = %s, %v", got, err)
	}
	if err := SaveSelection("v3"); err == nil {
		t.Fatal("invalid preference saved")
	}
	t.Setenv("GLOWBOM_OPENCODE_VERSION", "v1")
	if err := SaveSelection("auto"); err == nil {
		t.Fatal("environment preference replaced")
	}
}

func TestUnknownMajorIsNotGuessed(t *testing.T) {
	path := fixtureBinary(t, t.TempDir(), "3.0.0", "--log-level <all|trace|debug|info|warn>")
	if _, err := Detect(context.Background(), path); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error = %v", err)
	}
}
