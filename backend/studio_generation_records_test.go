package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func studioCheckpointFixture() studioGenerationCheckpoint {
	return studioGenerationCheckpoint{
		State: studioProgressState{
			Found: true, ID: "fixture-video-generation", Kind: "video", Stage: "downloading",
			Prompt: "A quiet performance", AspectRatio: "16:9", DurationSeconds: 8,
			ReferenceID: "fixture-reference", SourceID: "xai-subscription", StartedAt: "2026-09-30T15:40:00Z",
			CanResume: true, FirstFrame: &studioImageSummary{ID: "fixture-frame", Prompt: "A quiet performance"},
		},
		OperationID: "fixture-provider-operation",
		VideoURL:    "https://media.example.test/result.mp4?signature=private-fixture",
	}
}

func TestStudioGenerationCheckpointRoundTripAndAtomicReplacement(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", root)
	checkpoint := studioCheckpointFixture()
	if _, found, err := loadStudioGenerationCheckpoint(checkpoint.State.ID); err != nil || found {
		t.Fatal("a new store should have no saved generation")
	}
	if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := loadStudioGenerationCheckpoint(checkpoint.State.ID)
	if err != nil || !found || !reflect.DeepEqual(loaded, checkpoint) {
		t.Fatal("saved generation did not survive a new disk read")
	}
	checkpoint.State.Stage = "complete"
	checkpoint.State.Asset = &studioImageSummary{ID: "fixture-video", AssetType: "generated"}
	if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	loaded, found, err = loadStudioGenerationCheckpoint(checkpoint.State.ID)
	if err != nil || !found || !reflect.DeepEqual(loaded, checkpoint) {
		t.Fatal("replacement did not retain the completed generation")
	}
	files, err := os.ReadDir(filepath.Join(root, "Generations"))
	if err != nil || len(files) != 1 || files[0].Name() != checkpoint.State.ID+".json" {
		t.Fatal("temporary generation files were left behind")
	}
	if _, err := os.Stat(filepath.Join(root, "Assets")); !os.IsNotExist(err) {
		t.Fatal("saving generation progress touched the asset catalog")
	}
}

func TestStudioGenerationCheckpointPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Studio")
	t.Setenv("GLOWBOM_STUDIO_DIR", root)
	checkpoint := studioCheckpointFixture()
	if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]os.FileMode{
		root: 0700, filepath.Join(root, "Generations"): 0700,
		filepath.Join(root, "Generations", checkpoint.State.ID+".json"): 0600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != expected {
			t.Fatalf("private generation storage has incorrect permissions at %s", filepath.Base(path))
		}
	}
}

func TestStudioGenerationCheckpointRejectsInvalidIdentifiers(t *testing.T) {
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	for _, id := range []string{"", "short", "../fixture-video-generation", "fixture/video-generation", strings.Repeat("a", 129)} {
		checkpoint := studioCheckpointFixture()
		checkpoint.State.ID = id
		if err := saveStudioGenerationCheckpoint(checkpoint); err == nil {
			t.Fatal("invalid identifier was saved")
		}
		if _, found, err := loadStudioGenerationCheckpoint(id); err == nil || found {
			t.Fatal("invalid identifier was loaded")
		}
	}
	checkpoint := studioCheckpointFixture()
	checkpoint.State.Found = false
	if err := saveStudioGenerationCheckpoint(checkpoint); err == nil {
		t.Fatal("missing generation state was saved")
	}
}

func TestStudioGenerationCheckpointRejectsMalformedAndOversizedRecords(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", root)
	checkpoint := studioCheckpointFixture()
	if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Generations", checkpoint.State.ID+".json")
	wrongID := studioCheckpointFixture()
	wrongID.State.ID = "another-video-generation"
	mismatched, _ := json.Marshal(wrongID)
	for _, data := range [][]byte{
		[]byte(`{"state":`), []byte(`null`), []byte(`{}`), mismatched,
		[]byte(strings.Repeat(" ", studioGenerationCheckpointLimit+1)),
		[]byte(`{"state":{"found":true,"id":"fixture-video-generation"}} trailing data`),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, found, err := loadStudioGenerationCheckpoint(checkpoint.State.ID); err == nil || found {
			t.Fatal("malformed or oversized record was loaded")
		}
	}
	checkpoint.State.Prompt = strings.Repeat("a", studioGenerationCheckpointLimit)
	if err := saveStudioGenerationCheckpoint(checkpoint); err == nil {
		t.Fatal("oversized record was saved")
	}
}

func TestStudioGenerationCheckpointRejectsLinksAndNonregularFiles(t *testing.T) {
	for _, name := range []string{"record link", "directory link", "root link", "record directory"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("GLOWBOM_STUDIO_DIR", root)
			checkpoint := studioCheckpointFixture()
			if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "Generations")
			path := filepath.Join(dir, checkpoint.State.ID+".json")
			outside := filepath.Join(t.TempDir(), "private.json")
			if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "record link":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "directory link":
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), dir); err != nil {
					t.Fatal(err)
				}
			case "root link":
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), root); err != nil {
					t.Fatal(err)
				}
			case "record directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveStudioGenerationCheckpoint(checkpoint); err == nil {
				t.Fatal("generation storage followed a link or replaced a nonregular file")
			}
			if _, found, err := loadStudioGenerationCheckpoint(checkpoint.State.ID); err == nil || found {
				t.Fatal("generation storage loaded a link or nonregular file")
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "untouched" {
				t.Fatal("generation storage changed data outside its directory")
			}
		})
	}
}

func TestStudioGenerationCheckpointErrorsDoNotExposePrivateData(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GLOWBOM_STUDIO_DIR", root)
	checkpoint := studioCheckpointFixture()
	checkpoint.State.Prompt = strings.Repeat("a", studioGenerationCheckpointLimit)
	check := func(err error) {
		t.Helper()
		if err == nil || strings.Contains(err.Error(), checkpoint.VideoURL) || strings.Contains(err.Error(), checkpoint.OperationID) || strings.Contains(err.Error(), root) {
			t.Fatal("generation record error exposed private data or was missing")
		}
	}
	check(saveStudioGenerationCheckpoint(checkpoint))
	checkpoint = studioCheckpointFixture()
	if err := saveStudioGenerationCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Generations", checkpoint.State.ID+".json")
	if err := os.WriteFile(path, []byte(`{"state": `+checkpoint.VideoURL), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadStudioGenerationCheckpoint(checkpoint.State.ID)
	check(err)
}
