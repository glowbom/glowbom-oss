package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestCompanionStartupDoesNotReadStudioMedia(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		restored bool
	}{
		{name: "desktop", restored: true},
		{name: "native", restored: true},
		{name: "changed identity"},
		{name: "outside metadata"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			studio := t.TempDir()
			t.Setenv("GLOWBOM_STUDIO_DIR", studio)
			manager := newCompanionManager(http.NotFoundHandler())
			t.Cleanup(manager.Shutdown)
			project := sharedCompanionProject(t, testCompanion(t, manager.api))
			record, err := registerStudioProject(project.path)
			if err != nil {
				t.Fatal(err)
			}
			job := manager.newRun(project, "desktop", OpenCodeAgentRequest{Instructions: "Continue the saved build", AgentDriver: "cursor", Model: "auto"})
			switch scenario.name {
			case "native":
				if err := os.Remove(filepath.Join(studio, "DesktopProjects", record.ID+".json")); err != nil {
					t.Fatal(err)
				}
				directory := filepath.Join(studio, "Projects")
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(map[string]string{"id": record.ID, "projectName": record.Name, "exportedProjectPath": project.path})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, record.ID+".json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "changed identity":
				data := []byte(`{"projectID":"AAAAAAAA-1111-2222-3333-444444444444"}`)
				if err := os.WriteFile(filepath.Join(project.path, ".glowbom", "studio.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "outside metadata":
				metadata := filepath.Join(project.path, ".glowbom")
				outside := filepath.Join(t.TempDir(), "metadata")
				if err := os.Rename(metadata, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, metadata); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			// An invalid media directory must not prevent saved build recovery.
			if err := os.WriteFile(filepath.Join(studio, "Assets"), []byte("unavailable media directory"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := listStudioProjects(); err == nil {
				t.Fatal("media catalog unexpectedly readable")
			}
			shared, err := companionProjects(nil)
			if err != nil {
				t.Fatal(err)
			}
			_, found := shared[companionProjectID(project.path)]
			if found != scenario.restored {
				t.Fatalf("pairing project discovery: found=%v, want=%v", found, scenario.restored)
			}
			restored := newCompanionManager(http.NotFoundHandler())
			t.Cleanup(restored.Shutdown)
			restored.restoreRecentRuns()
			recovered := restored.run(job.id)
			if scenario.restored {
				if recovered == nil || recovered.status != "failed" || recovered.instructions != job.instructions || recovered.agentDriver != "cursor" {
					t.Fatalf("saved build was not recovered: %+v", recovered)
				}
			} else if recovered != nil {
				t.Fatal("recovered a build from an unavailable project")
			}
		})
	}
}

func TestCompanionLANInterfaceExcludesTunnels(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		flags net.Flags
		want  bool
	}{
		{"Ethernet", net.FlagUp | net.FlagBroadcast | net.FlagMulticast, true},
		{"Wi-Fi", net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast, true},
		{"inactive", net.FlagBroadcast | net.FlagMulticast, false},
		{"loopback", net.FlagUp | net.FlagLoopback, false},
		{"VPN tunnel", net.FlagUp | net.FlagPointToPoint | net.FlagMulticast, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if got := companionLANInterface(scenario.flags); got != scenario.want {
				t.Fatalf("LAN candidate=%v, want=%v", got, scenario.want)
			}
		})
	}
}
