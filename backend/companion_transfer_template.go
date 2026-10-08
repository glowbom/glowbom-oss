package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"time"
)

// Keep the starter's platform files and target settings. The phone supplies a
// browser prototype, not translated Apple, Android, or Next.js implementations.
func prepareCompanionStarterProject(files []starterProjectFile, request companionPrototypeImport, assets []companionPreparedImportAsset) ([]starterProjectFile, error) {
	var snapshot struct {
		CustomIconAssetID string `json:"customIconAssetID"`
	}
	if request.SourceSnapshot != "" && json.Unmarshal([]byte(request.SourceSnapshot), &snapshot) != nil {
		return nil, errors.New("The saved project details could not be read.")
	}
	var icon []byte
	if snapshot.CustomIconAssetID != "" {
		for _, asset := range assets {
			if asset.ID != normalizedStudioUUID(snapshot.CustomIconAssetID) {
				continue
			}
			var err error
			icon, err = normalizeProjectIcon(asset.data)
			if err != nil {
				return nil, errors.New("The saved app icon could not be prepared. Choose a supported icon on the phone and send again.")
			}
			break
		}
		if len(icon) == 0 {
			return nil, errors.New("The saved app icon is missing. Restore it on the phone and send again.")
		}
	}

	metadata := make([]prototypeAssetsManifestItem, 0, len(assets))
	for _, asset := range assets {
		item := prototypeAssetsManifestItem{Filename: asset.Filename, Prompt: asset.Prompt,
			SourceService: asset.SourceService, MediaType: asset.media}
		if asset.dimensions != nil {
			item.Dimensions = map[string]int{"width": asset.dimensions.Width, "height": asset.dimensions.Height}
		}
		metadata = append(metadata, item)
	}
	catalog, err := json.MarshalIndent(prototypeAssetsManifest{
		Version: "1.0", ExportedAt: time.Now().UTC().Format(time.RFC3339), Assets: metadata,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	overlays := map[string][]byte{"prototype/assets.json": catalog}
	if len(icon) > 0 {
		for _, path := range []string{"icon.png", "web/src/app/icon.png", "web/public/icon.png"} {
			overlays[path] = icon
		}
	}

	manifestFound := false
	for index := range files {
		path := filepath.ToSlash(files[index].name)
		if data, ok := overlays[path]; ok {
			files[index].data = data
			delete(overlays, path)
		}
		if path != "glowbom.json" {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(files[index].data, &fields) != nil || fields == nil {
			return nil, errStarterProject
		}
		for key, value := range map[string]string{
			"description": request.Prompt, "prototypePath": "prototype/index.html",
			"prototypeAssetsManifestPath": "prototype/assets.json", "agentsPath": "AGENTS.md",
		} {
			fields[key], _ = json.Marshal(value)
		}
		delete(fields, "iconPath")
		if len(icon) > 0 {
			fields["iconPath"], _ = json.Marshal("icon.png")
		}
		files[index].data, err = json.MarshalIndent(fields, "", "  ")
		if err != nil {
			return nil, err
		}
		manifestFound = true
	}
	if !manifestFound {
		return nil, errStarterProject
	}
	for path, data := range overlays {
		files = append(files, starterProjectFile{name: filepath.FromSlash(path), data: data, mode: 0644})
	}
	return files, nil
}
