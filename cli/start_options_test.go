package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartOptionsDefaultsAndOverrides(t *testing.T) {
	t.Setenv("GLOWBOM_AGENT_PORT", "4571")
	options, err := parseStartOptions(nil)
	if err != nil || options.BackendPort != 4569 || options.WebPort != 4572 || options.AgentPort != 4571 || options.Instance != "oss" {
		t.Fatalf("defaults = %+v, %v", options, err)
	}
	web := filepath.Join(t.TempDir(), "web with spaces")
	options, err = parseStartOptions([]string{"project", "--web-dir", web, "--backend-port=4591", "--web-port", "4592", "--agent-port", "4593", "--instance=desktop-dev", "--no-browser", "--show-local-auth"})
	if err != nil || options.WebDir != web || options.BackendPort != 4591 || options.WebPort != 4592 || options.AgentPort != 4593 || options.Instance != "desktop-dev" || options.ProjectPath != "project" || !options.NoBrowser || !options.ShowLocalAuth || !options.isolatedCredentials() {
		t.Fatalf("overrides = %+v, %v", options, err)
	}
	options, err = parseStartOptions([]string{"--", "-project"})
	if err != nil || options.ProjectPath != "-project" {
		t.Fatalf("positional delimiter: %+v %v", options, err)
	}
}

func TestStartOptionsRejectAmbiguousOrUnsafeSettings(t *testing.T) {
	t.Setenv("GLOWBOM_AGENT_PORT", "4571")
	for _, args := range [][]string{
		{"--web-dir", "relative"}, {"--instance", "../other"}, {"--instance", ""}, {"--instance", "Desktop"},
		{"--backend-port", "0"}, {"--web-port", "65536"}, {"--agent-port", "text"},
		{"--web-port", "4569"}, {"--agent-port", "4572"}, {"--web-port"}, {"--web-port", "--no-browser"},
		{"--unknown"}, {"one", "two"},
	} {
		if options, err := parseStartOptions(args); err == nil {
			t.Fatalf("accepted %v as %+v", args, options)
		}
	}
	t.Setenv("GLOWBOM_AGENT_PORT", "broken")
	if _, err := parseStartOptions(nil); err == nil {
		t.Fatal("invalid inherited port accepted")
	}
	if _, err := parseStartOptions([]string{"--agent-port", "4593"}); err != nil {
		t.Fatal("explicit port did not override environment:", err)
	}
}

func TestSourceLaunchEnvironmentOverridesRoutingAndKeepsProviderSettings(t *testing.T) {
	for _, key := range []string{"GLOWBOM_DESKTOP", "GLOWBOM_WEB_DIR", "GLOWBOM_RESOURCE_DIR", "OPENCODE_URL", "GLOWBY_OPENCODE_URL", "GLOWBOM_PORT", "GLOWBOM_SERVER_TOKEN", "GLOWBY_SERVER_TOKEN", "GLOWBOM_ALLOWED_ORIGINS", "OPENCODE_SERVER_HOSTNAME", "OPENCODE_PASSWORD", "VITE_BACKEND_TARGET", "VITE_GLOWBOM_SERVER_TOKEN"} {
		t.Setenv(key, "inherited-setting")
	}
	t.Setenv("GLOWBOM_OPENCODE_BIN", "/fixture/opencode")
	t.Setenv("OPENAI_API_KEY", "fixture-provider-value")
	options := startOptions{Instance: "desktop-dev", BackendPort: 4591, WebPort: 4592, AgentPort: 4593}
	identity := launchIdentity{Instance: options.Instance, LaunchID: strings.Repeat("a", 32)}
	backend, web := launchEnvironments(options, identity, "fixture-token", "fixture-password", "/fixture/glowbom")
	asMap := func(environment []string) map[string]string {
		values := map[string]string{}
		for _, entry := range environment {
			key, value, _ := strings.Cut(entry, "=")
			if _, exists := values[key]; exists {
				t.Fatalf("duplicate environment key %s", key)
			}
			values[key] = value
		}
		return values
	}
	backValues, webValues := asMap(backend), asMap(web)
	for _, values := range []map[string]string{backValues, webValues} {
		for _, key := range []string{"GLOWBOM_DESKTOP", "GLOWBOM_WEB_DIR", "GLOWBOM_RESOURCE_DIR", "OPENCODE_URL", "GLOWBY_OPENCODE_URL"} {
			if _, present := values[key]; present {
				t.Fatalf("inherited %s survived", key)
			}
		}
		if values["GLOWBOM_INSTANCE"] != "desktop-dev" || values["GLOWBOM_LAUNCH_ID"] != identity.LaunchID || values["GLOWBOM_AGENT_PORT"] != "4593" || values["GLOWBOM_BIND_HOST"] != "127.0.0.1" || values["GLOWBOM_OPENCODE_BIN"] != "/fixture/opencode" || values["OPENAI_API_KEY"] != "fixture-provider-value" {
			t.Fatal("launch environment lost expected settings")
		}
	}
	if backValues["GLOWBOM_SERVER_TOKEN"] != "fixture-token" || backValues["OPENCODE_SERVER_PASSWORD"] != "fixture-password" || backValues["OPENCODE_SERVER_HOSTNAME"] != "127.0.0.1" || backValues["GLOWBOM_CLI_BIN"] != "/fixture/glowbom" {
		t.Fatal("backend settings mismatch")
	}
	if backValues["GLOWBOM_ALLOWED_ORIGINS"] != "http://127.0.0.1:4592,http://localhost:4592,http://127.0.0.1:4591,http://localhost:4591" {
		t.Fatal("origin allowlist is not scoped to this launch")
	}
	if webValues["GLOWBOM_WEB_PORT"] != "4592" || webValues["VITE_BACKEND_TARGET"] != "http://127.0.0.1:4591" || webValues["VITE_GLOWBOM_SERVER_TOKEN"] != "fixture-token" {
		t.Fatal("web routing/auth mismatch")
	}
	if _, exists := webValues["OPENCODE_SERVER_PASSWORD"]; exists {
		t.Fatal("web inherited agent password")
	}
}

func TestIsolatedLaunchIgnoresInheritedSecrets(t *testing.T) {
	t.Setenv("GLOWBOM_SERVER_TOKEN", "inherited-secret")
	one, err := launchSecret(true, "GLOWBOM_SERVER_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	two, err := launchSecret(true, "GLOWBOM_SERVER_TOKEN")
	if err != nil || one == two || one == "inherited-secret" {
		t.Fatal("isolated launches reused a credential")
	}
	legacy, err := launchSecret(false, "GLOWBOM_SERVER_TOKEN")
	if err != nil || legacy != "inherited-secret" {
		t.Fatal("default explicit token compatibility changed")
	}
}

func TestLaunchIDAcceptsParentIdentityAndRejectsInvalidValues(t *testing.T) {
	t.Setenv("GLOWBOM_LAUNCH_ID", strings.Repeat("a", 32))
	id, err := resolveLaunchID()
	if err != nil || id != strings.Repeat("a", 32) {
		t.Fatal("parent identity was not preserved")
	}
	t.Setenv("GLOWBOM_LAUNCH_ID", "not-valid")
	if _, err := resolveLaunchID(); err == nil {
		t.Fatal("invalid parent identity accepted")
	}
	t.Setenv("GLOWBOM_LAUNCH_ID", "")
	id, err = resolveLaunchID()
	if err != nil || !launchIDFormat.MatchString(id) {
		t.Fatal("fresh launch identity missing")
	}
}

func TestLaunchStateCleanupPreservesOtherInstanceAndReplacement(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", os.Getenv("TMPDIR"))
	one := launchIdentity{Instance: "oss", LaunchID: strings.Repeat("a", 32)}
	other := launchIdentity{Instance: "desktop-dev", LaunchID: strings.Repeat("b", 32)}
	for _, identity := range []launchIdentity{one, other} {
		if err := writeLaunchState(launchState{launchIdentity: identity, LauncherPID: 10, BackendPID: 20}); err != nil {
			t.Fatal(err)
		}
	}
	clearLaunchState(one)
	if _, err := os.Stat(filepath.Join(launchStateDir(one.Instance), "launch.json")); !os.IsNotExist(err) {
		t.Fatal("owned state survived cleanup")
	}
	path := filepath.Join(launchStateDir(other.Instance), "launch.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cleanup removed another instance:", err)
	}
	var read launchState
	if json.Unmarshal(data, &read) != nil || read.launchIdentity != other || read.BackendPID != 20 {
		t.Fatal("ownership metadata did not round trip")
	}
	replacement := launchIdentity{Instance: other.Instance, LaunchID: strings.Repeat("c", 32)}
	if err := writeLaunchState(launchState{launchIdentity: replacement}); err != nil {
		t.Fatal(err)
	}
	clearLaunchState(other)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("old cleanup removed replacement state")
	}
	clearLaunchState(replacement)
}
