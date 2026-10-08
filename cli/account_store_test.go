package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func fileCredentialEnvironment(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file credentials are supported on macOS/Linux")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GLOWBOM_CREDENTIAL_STORE", "file")
	t.Setenv("GLOWBOM_CONFIG_DIR", "")
	t.Chdir(home)
	return base
}

func credentialTestDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func credentialTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFileCredentialLocationsRejectProjectAncestors(t *testing.T) {
	for _, kind := range []string{"repository", "worktree", "source-export", "glowbom-project", "studio-record", "chat-record", "project-book", "node-project", "python-project"} {
		t.Run(kind, func(t *testing.T) {
			base := fileCredentialEnvironment(t)
			project := filepath.Join(base, "project")
			cwd := filepath.Join(project, "src", "deep")
			credentialTestDirectory(t, cwd)
			switch kind {
			case "repository":
				credentialTestDirectory(t, filepath.Join(project, ".git"))
			case "worktree":
				credentialTestFile(t, filepath.Join(project, ".git"))
			case "source-export":
				credentialTestDirectory(t, filepath.Join(project, "backend"))
				credentialTestDirectory(t, filepath.Join(project, "web"))
			case "glowbom-project":
				credentialTestFile(t, filepath.Join(project, "glowbom.json"))
			case "studio-record", "chat-record":
				credentialTestDirectory(t, filepath.Join(project, ".glowbom"))
				name := "studio.json"
				if kind == "chat-record" {
					name = "chat.json"
				}
				credentialTestFile(t, filepath.Join(project, ".glowbom", name))
			case "project-book":
				credentialTestDirectory(t, filepath.Join(project, "project-book"))
				credentialTestFile(t, filepath.Join(project, "project-book", "book.json"))
			case "node-project":
				credentialTestFile(t, filepath.Join(project, "package.json"))
			case "python-project":
				credentialTestFile(t, filepath.Join(project, "pyproject.toml"))
			}
			t.Chdir(cwd)
			for _, dir := range []string{project, filepath.Join(project, "private", "config"), filepath.Join(cwd, "config")} {
				t.Setenv("GLOWBOM_CONFIG_DIR", dir)
				if _, err := newCredentialStore("https://api.example.test"); !errors.Is(err, errCredentialInProject) {
					t.Fatalf("accepted project location or returned wrong error: %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "accounts")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("rejected configuration created a credential directory")
				}
			}
		})
	}
}

func TestFileCredentialsRejectOtherProjectWhenRunFromHome(t *testing.T) {
	base := fileCredentialEnvironment(t)
	project := filepath.Join(base, "other-project")
	credentialTestDirectory(t, project)
	credentialTestFile(t, filepath.Join(project, "go.mod"))
	t.Setenv("GLOWBOM_CONFIG_DIR", filepath.Join(project, "config"))
	if _, err := newCredentialStore("https://api.example.test"); !errors.Is(err, errCredentialInProject) {
		t.Fatalf("accepted another source checkout: %v", err)
	}
}

func TestFileCredentialsRejectUnmarkedWorkingProject(t *testing.T) {
	base := fileCredentialEnvironment(t)
	project := filepath.Join(base, "project")
	credentialTestDirectory(t, project)
	t.Chdir(project)
	t.Setenv("GLOWBOM_CONFIG_DIR", filepath.Join(project, "new", "config"))
	if _, err := newCredentialStore("https://api.example.test"); !errors.Is(err, errCredentialInProject) {
		t.Fatalf("accepted the current project: %v", err)
	}
	// A similarly named external directory is not a descendant of the project.
	t.Setenv("GLOWBOM_CONFIG_DIR", filepath.Join(base, "project-private", "config"))
	if _, err := newCredentialStore("https://api.example.test"); err != nil {
		t.Fatalf("rejected an external directory: %v", err)
	}
}

func TestFileCredentialsRejectSymlinkProjectPaths(t *testing.T) {
	for _, kind := range []string{"outside-to-project", "project-to-outside", "accounts-to-project", "unmarked-project-target", "dangling-parent"} {
		t.Run(kind, func(t *testing.T) {
			base := fileCredentialEnvironment(t)
			project := filepath.Join(base, "project")
			external := filepath.Join(base, "external")
			credentialTestDirectory(t, project)
			credentialTestDirectory(t, external)
			if kind == "unmarked-project-target" {
				t.Chdir(project)
			} else {
				credentialTestDirectory(t, filepath.Join(project, ".git"))
			}
			link, target := filepath.Join(external, "alias"), project
			config := filepath.Join(link, "not-created", "config")
			switch kind {
			case "project-to-outside":
				link, target = filepath.Join(project, "alias"), external
				config = filepath.Join(link, "config")
			case "accounts-to-project":
				link = filepath.Join(external, "accounts")
				config = external
			case "dangling-parent":
				target = filepath.Join(base, "does-not-exist")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GLOWBOM_CONFIG_DIR", config)
			if _, err := newCredentialStore("https://api.example.test"); err == nil {
				t.Fatal("accepted a symlink bypass or unresolvable credential location")
			}
		})
	}
}

func TestFileCredentialOperationsRecheckChangedSymlinkTarget(t *testing.T) {
	base := fileCredentialEnvironment(t)
	external := filepath.Join(base, "external")
	project := filepath.Join(base, "project")
	credentialTestDirectory(t, external)
	credentialTestDirectory(t, filepath.Join(project, "accounts"))
	credentialTestDirectory(t, filepath.Join(project, ".git"))
	link := filepath.Join(base, "config")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLOWBOM_CONFIG_DIR", link)
	store, err := newCredentialStore("https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testCredentials()); err != nil {
		t.Fatal(err)
	}
	projectFile := filepath.Join(project, "accounts", filepath.Base(store.(*credentialStore).file))
	credentialTestFile(t, projectFile)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, errCredentialInProject) {
		t.Fatalf("load did not reject the changed target: %v", err)
	}
	if err := store.Save(testCredentials()); !errors.Is(err, errCredentialInProject) {
		t.Fatalf("save did not reject the changed target: %v", err)
	}
	if err := store.Delete(); !errors.Is(err, errCredentialInProject) {
		t.Fatalf("delete did not reject the changed target: %v", err)
	}
	data, err := os.ReadFile(projectFile)
	if err != nil || string(data) != "fixture" {
		t.Fatal("a rejected credential operation changed the project file")
	}
}

func TestFileCredentialSaveRejectsNewRepositoryBeforeCreatingDirectories(t *testing.T) {
	base := fileCredentialEnvironment(t)
	project := filepath.Join(base, "new-project")
	credentialTestDirectory(t, project)
	t.Setenv("GLOWBOM_CONFIG_DIR", filepath.Join(project, "config"))
	store, err := newCredentialStore("https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	credentialTestDirectory(t, filepath.Join(project, ".git"))
	if err := store.Save(testCredentials()); !errors.Is(err, errCredentialInProject) {
		t.Fatalf("save accepted a new repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("save created directories before checking the new repository boundary")
	}
}

func TestFileCredentialsWorkFromHomeAndFilesystemRoot(t *testing.T) {
	for _, location := range []string{"home-default", "root-explicit", "project-external"} {
		t.Run(location, func(t *testing.T) {
			base := fileCredentialEnvironment(t)
			switch location {
			case "root-explicit":
				t.Chdir(string(filepath.Separator))
				t.Setenv("GLOWBOM_CONFIG_DIR", filepath.Join(base, "external"))
			case "project-external":
				project := filepath.Join(base, "project")
				credentialTestDirectory(t, filepath.Join(project, ".git"))
				t.Chdir(project)
				t.Setenv("GLOWBOM_CONFIG_DIR", filepath.Join(base, "external"))
			}
			store, err := newCredentialStore("https://api.example.test")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(testCredentials()); err != nil {
				t.Fatal(err)
			}
			// Reopen an existing external file store, as an installed CLI does.
			store, err = newCredentialStore("https://api.example.test")
			if err != nil {
				t.Fatal(err)
			}
			if saved, err := store.Load(); err != nil || saved.UID != "owner" {
				t.Fatal("existing external credentials could not be loaded")
			}
			if err := store.Delete(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKeyringIgnoresFileCredentialLocation(t *testing.T) {
	t.Setenv("GLOWBOM_CONFIG_DIR", "invalid-relative-project-path")
	for _, mode := range []string{"", "keyring"} {
		t.Setenv("GLOWBOM_CREDENTIAL_STORE", mode)
		store, err := newCredentialStore("https://api.example.test")
		if err != nil || store.(*credentialStore).file != "" {
			t.Fatal("keyring selection was changed by the file-store checks")
		}
	}
}

func TestFileCredentialsDoNotTreatBroadManifestFoldersAsProjects(t *testing.T) {
	base := fileCredentialEnvironment(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	credentialTestFile(t, filepath.Join(home, "package.json"))
	credentialTestDirectory(t, filepath.Join(home, ".glowbom"))
	credentialTestFile(t, filepath.Join(home, ".glowbom", "project-book-writing.json"))
	if _, err := newCredentialStore("https://api.example.test"); err != nil {
		t.Fatalf("home manifest blocked user configuration: %v", err)
	}
	sharedTemp := filepath.Join(base, "shared-temp")
	credentialTestDirectory(t, sharedTemp)
	t.Setenv("TMPDIR", sharedTemp)
	credentialTestFile(t, filepath.Join(sharedTemp, "package.json"))
	t.Setenv("GLOWBOM_CONFIG_DIR", filepath.Join(sharedTemp, "private-config"))
	if _, err := newCredentialStore("https://api.example.test"); err != nil {
		t.Fatalf("shared temp manifest blocked an external directory: %v", err)
	}
	// An actual repository remains unsafe, even if it encompasses home.
	credentialTestDirectory(t, filepath.Join(home, ".git"))
	t.Setenv("GLOWBOM_CONFIG_DIR", filepath.Join(home, "private-config"))
	if _, err := newCredentialStore("https://api.example.test"); !errors.Is(err, errCredentialInProject) {
		t.Fatalf("accepted credentials within a home repository: %v", err)
	}
}

func TestFileCredentialsRejectExplicitGlowbomProjectAtHome(t *testing.T) {
	for _, kind := range []string{"manifest", "source-export"} {
		t.Run(kind, func(t *testing.T) {
			fileCredentialEnvironment(t)
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			if kind == "manifest" {
				credentialTestFile(t, filepath.Join(home, "glowbom.json"))
			} else {
				credentialTestDirectory(t, filepath.Join(home, "backend"))
				credentialTestDirectory(t, filepath.Join(home, "web"))
			}
			if _, err := newCredentialStore("https://api.example.test"); !errors.Is(err, errCredentialInProject) {
				t.Fatalf("accepted credentials inside a Glowbom project at home: %v", err)
			}
		})
	}
}
