package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImagePrefixCompatibility(t *testing.T) {
	for _, prefix := range []string{"glowbyimage:", "glowbyimages:", "glowbomimage:", "glowbomimages:"} {
		t.Run(prefix, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			root := t.TempDir()
			prototype := filepath.Join(root, "prototype")
			if err := os.MkdirAll(prototype, 0755); err != nil {
				t.Fatal(err)
			}
			token := prefix + "Snowy city"
			html := `<script>const hero = "` + token + `"; const duplicate = "` + token + `";</script>`
			path := filepath.Join(prototype, "index.html")
			if err := os.WriteFile(path, []byte(html), 0644); err != nil {
				t.Fatal(err)
			}
			refs := extractImagePlaceholders(html)
			if len(refs) != 1 || refs[0].Token != token || refs[0].Prompt != "Snowy city" {
				t.Fatalf("extracted: %+v", refs)
			}
			if !prototypeContainsMediaPlaceholders(root) {
				t.Fatal("placeholder not detected")
			}
			req := OpenCodeMediaPostPassRequest{ProjectPath: root, ImageSource: "Test Images"}
			approval, err := buildOpenCodeMediaApproval(req)
			if err != nil || approval == nil || len(approval.Items) != 1 {
				t.Fatalf("approval: %+v, %v", approval, err)
			}
			// Reuse a local image to exercise replacement without a paid provider call.
			config, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			assets := filepath.Join(config, "Glowbom", "Studio", "Assets")
			if err := os.MkdirAll(assets, 0755); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(studioAssetRecord{ID: "test-image", MediaType: "image", Prompt: "Snowy city", SourceService: "Test Images", DataBase64: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a3ioAAAAASUVORK5CYII="})
			if err := os.WriteFile(filepath.Join(assets, "image.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := runOpenCodeMediaPostPass(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(updated), token) || !result.PrototypeChanged || len(result.ReusedStudioAssets) != 1 {
				t.Fatalf("replacement failed: %s, %+v", updated, result)
			}
			if prototypeContainsMediaPlaceholders(root) {
				t.Fatal("resolved image still detected as placeholder")
			}
		})
	}
}
