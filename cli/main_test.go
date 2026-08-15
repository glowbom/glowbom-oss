package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsagePresentsGlowbomStart(t *testing.T) {
	if !strings.Contains(usage, "glowbom start") {
		t.Fatalf("usage does not present glowbom start: %q", usage)
	}
	if !strings.Contains(usage, "Deprecated alias for glowbom start") {
		t.Fatalf("usage does not document the code compatibility alias: %q", usage)
	}
}

func TestCodeCommandRemainsACompatibilityAlias(t *testing.T) {
	if exitCode := run([]string{"code", "first", "second"}); exitCode != 2 {
		t.Fatalf("run(code) exit code = %d, want 2 for invalid arguments", exitCode)
	}
}

func TestSearchGlowbomRootFindsParentCheckout(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"backend", "web", filepath.Join("nested", "child")} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	got, ok := searchGlowbomRoot(filepath.Join(root, "nested", "child"))
	if !ok {
		t.Fatal("searchGlowbomRoot did not find the checkout")
	}
	if got != root {
		t.Fatalf("searchGlowbomRoot = %q, want %q", got, root)
	}
}

func TestResolveOrGenerateSecretPrefersNewVariable(t *testing.T) {
	t.Setenv("GLOWBOM_TEST_SECRET", "new-value")
	t.Setenv("GLOWBY_TEST_SECRET", "legacy-value")

	got, err := resolveOrGenerateSecret("GLOWBOM_TEST_SECRET", "GLOWBY_TEST_SECRET")
	if err != nil {
		t.Fatalf("resolveOrGenerateSecret returned error: %v", err)
	}
	if got != "new-value" {
		t.Fatalf("resolveOrGenerateSecret = %q, want new-value", got)
	}
}
