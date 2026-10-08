package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var studioProjectMu sync.Mutex
var studioUUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type studioProjectSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path,omitempty"`
	Timestamp  string `json:"timestamp"`
	AssetCount int    `json:"assetCount"`
	Available  bool   `json:"available"`
}

func normalizedStudioUUID(id string) string {
	id = strings.TrimSpace(id)
	if !studioUUIDPattern.MatchString(id) {
		return ""
	}
	return strings.ToUpper(id)
}

func normalizedStudioUUIDs(ids []string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if id = normalizedStudioUUID(id); id != "" && !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result
}

func studioHasProject(ids []string, id string) bool {
	for _, candidate := range ids {
		if strings.EqualFold(candidate, id) {
			return true
		}
	}
	return false
}

func studioAssetProjectIDs(asset studioImageSummary) []string {
	ids := append([]string{}, asset.UsedInProjects...)
	return normalizedStudioUUIDs(append(ids, asset.SourceProjectID))
}

func studioRootDirectory() (string, error) {
	assets, err := studioAssetsDirectory()
	return filepath.Dir(assets), err
}

// Read only regular, bounded records. Refuse links before reading user metadata.
func readStudioJSON(path string, limit int64) (map[string]json.RawMessage, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, nil, errors.New("Studio metadata is not a regular bounded file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, nil, errors.New("Could not read Studio metadata")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, nil, errors.New("Studio metadata is invalid")
	}
	return fields, data, nil
}

func studioJSONString(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

func studioManifestProjectID(fields map[string]json.RawMessage) string {
	for _, key := range []string{"projectUUID", "projectID", "importedUUID", "projectId"} {
		if id := normalizedStudioUUID(studioJSONString(fields, key)); id != "" {
			return id
		}
	}
	return ""
}

func readStudioProjectState(root string) (map[string]json.RawMessage, []byte, error) {
	dir, err := filepath.EvalSymlinks(filepath.Join(root, ".glowbom"))
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil, nil
	}
	if err != nil || !isPathWithin(root, dir) {
		return nil, nil, errors.New("Project image links must stay inside the project")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil, errors.New("The project metadata folder is unavailable")
	}
	fields, data, err := readStudioJSON(filepath.Join(dir, "studio.json"), 1<<20)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil, nil
	}
	return fields, data, err
}

func saveStudioProjectState(root string, fields map[string]json.RawMessage, previous []byte) error {
	dir, err := chatWriteDirectory(root, ".glowbom")
	if err != nil {
		return err
	}
	_, current, err := readStudioProjectState(root)
	if err != nil || !bytes.Equal(current, previous) {
		return errors.New("Project image links changed. Try again to keep the newer links.")
	}
	data, err := json.Marshal(fields)
	if err != nil || len(data) > 1<<20 {
		return errors.New("Project image links are too large")
	}
	return atomicChatFile(dir, "studio.json", data)
}

// The UUID travels with the project folder. Registering a moved folder updates
// its local lookup without renaming or rewriting the shared native project.
func registerStudioProjectLocked(path string) (studioProjectSummary, error) {
	root, err := chatProjectRoot(path)
	if err != nil {
		return studioProjectSummary{}, err
	}
	manifest, _, err := readStudioJSON(filepath.Join(root, "glowbom.json"), 1<<20)
	if err != nil {
		return studioProjectSummary{}, err
	}
	fields, previous, err := readStudioProjectState(root)
	if err != nil {
		return studioProjectSummary{}, err
	}
	id := normalizedStudioUUID(studioJSONString(fields, "projectID"))
	if id == "" {
		if len(fields["projectID"]) > 0 {
			return studioProjectSummary{}, errors.New("The saved Studio project identity is invalid")
		}
		id = studioManifestProjectID(manifest)
		if id == "" {
			id = strings.ToUpper(randomUUIDString())
		}
		fields["projectID"], _ = json.Marshal(id)
		if err := saveStudioProjectState(root, fields, previous); err != nil {
			return studioProjectSummary{}, err
		}
	}
	name := strings.TrimSpace(studioJSONString(manifest, "name"))
	if name == "" {
		name = filepath.Base(root)
	}
	stamp := studioJSONString(manifest, "createdAt")
	if stamp == "" {
		stamp = studioJSONString(manifest, "exportedAt")
	}
	if stamp == "" {
		stamp = time.Now().UTC().Format(time.RFC3339)
	}
	project := studioProjectSummary{ID: id, Name: name, Path: root, Timestamp: stamp, Available: true}
	if err := repairCompanionPhoneImagesLocked(root, id); err != nil {
		return project, err
	}
	project.AssetCount, err = studioProjectAssetCount(id)
	if err != nil {
		return project, err
	}
	studio, err := studioRootDirectory()
	if err != nil {
		return project, err
	}
	dir := filepath.Join(studio, "DesktopProjects")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return project, err
	}
	data, _ := json.Marshal(project)
	return project, atomicChatFile(dir, id+".json", data)
}

func registerStudioProject(path string) (studioProjectSummary, error) {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	return registerStudioProjectLocked(path)
}

func allStudioProjectAssets() ([]studioImageSummary, []studioImageSummary, error) {
	// The existing catalog cache reads metadata once, including native records.
	images, videos, _, _, err := listStudioCatalogPage(0, int(^uint(0)>>2), 0, int(^uint(0)>>2))
	return images, videos, err
}

func studioProjectAssetCount(id string) (int, error) {
	images, videos, err := allStudioProjectAssets()
	if err != nil {
		return 0, err
	}
	count := 0
	audio, _, err := listStudioAudioPage("", 0, int(^uint(0)>>2))
	if err != nil {
		return 0, err
	}
	for _, item := range audio {
		images = append(images, item.studioImageSummary)
	}
	for _, asset := range append(images, videos...) {
		if studioHasProject(studioAssetProjectIDs(asset), id) {
			count++
		}
	}
	return count, nil
}

func listStudioProjectCatalogPage(id string, imageOffset, imageLimit, videoOffset, videoLimit int) ([]studioImageSummary, []studioImageSummary, bool, bool, error) {
	if id == "" {
		return listStudioCatalogPage(imageOffset, imageLimit, videoOffset, videoLimit)
	}
	images, videos, err := allStudioProjectAssets()
	if err != nil {
		return nil, nil, false, false, err
	}
	filter := func(items []studioImageSummary, offset, limit int) ([]studioImageSummary, bool) {
		filtered := []studioImageSummary{}
		for _, item := range items {
			if studioHasProject(studioAssetProjectIDs(item), id) {
				filtered = append(filtered, item)
			}
		}
		start := min(offset, len(filtered))
		end := start + min(limit, len(filtered)-start)
		return filtered[start:end], end < len(filtered)
	}
	images, moreImages := filter(images, imageOffset, imageLimit)
	videos, moreVideos := filter(videos, videoOffset, videoLimit)
	return images, videos, moreImages, moreVideos, nil
}

// Run recovery needs registered folders without scanning the Studio media library.
func listRegisteredStudioProjects() ([]studioProjectSummary, error) {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	projects, err := readStudioProjectRecordsLocked()
	if err != nil {
		return nil, err
	}
	return sortedStudioProjects(projects), nil
}

func readStudioProjectRecordsLocked() (map[string]studioProjectSummary, error) {
	studio, err := studioRootDirectory()
	if err != nil {
		return nil, err
	}
	if studioProjectMetadataCache.root != studio || studioProjectMetadataCache.entries == nil {
		studioProjectMetadataCache.root = studio
		studioProjectMetadataCache.entries = map[string]studioProjectMetadataEntry{}
	}
	seen := map[string]bool{}
	defer func() {
		for path := range studioProjectMetadataCache.entries {
			if !seen[path] {
				delete(studioProjectMetadataCache.entries, path)
			}
		}
	}()
	projects := map[string]studioProjectSummary{}
	for _, folder := range []string{"Projects", "DesktopProjects"} {
		entries, err := os.ReadDir(filepath.Join(studio, folder))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			recordPath := filepath.Join(studio, folder, entry.Name())
			seen[recordPath] = true
			metadata, err := cachedStudioProjectMetadata(recordPath)
			if err != nil {
				continue
			}
			id := normalizedStudioUUID(metadata.ID)
			name, path := metadata.Name, metadata.Path
			if folder == "Projects" {
				name, path = metadata.ProjectName, metadata.ExportedProjectPath
			}
			if name == "" {
				name = "Untitled project"
			}
			available := false
			if path != "" {
				if root, err := chatProjectRoot(path); err == nil {
					state, _, stateErr := readStudioProjectState(root)
					// A stale path must not silently point at a different project.
					knownID := studioJSONString(state, "projectID")
					if stateErr == nil && knownID == "" {
						if manifest, _, err := readStudioJSON(filepath.Join(root, "glowbom.json"), 1<<20); err == nil {
							knownID = studioManifestProjectID(manifest)
						}
					}
					available = stateErr == nil && strings.EqualFold(knownID, id)
					if available {
						if project, err := LoadProject(GetProjectPaths(root).Manifest); err == nil && project.Name != "" {
							name = project.Name
						}
					}
				}
			}
			projects[id] = studioProjectSummary{ID: id, Name: name, Path: path, Timestamp: metadata.Timestamp, Available: available}
		}
	}
	return projects, nil
}

func listStudioProjects() ([]studioProjectSummary, error) {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	projects, err := readStudioProjectRecordsLocked()
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		if project.Available {
			_ = repairCompanionPhoneImagesLocked(project.Path, project.ID)
		}
	}
	images, videos, err := allStudioProjectAssets()
	if err != nil {
		return nil, err
	}
	audio, _, err := listStudioAudioPage("", 0, int(^uint(0)>>2))
	if err != nil {
		return nil, err
	}
	for _, item := range audio {
		images = append(images, item.studioImageSummary)
	}
	for _, asset := range append(images, videos...) {
		for _, id := range studioAssetProjectIDs(asset) {
			project, exists := projects[id]
			if !exists {
				project = studioProjectSummary{ID: id, Name: "Linked project"}
			}
			project.AssetCount++
			projects[id] = project
		}
	}
	return sortedStudioProjects(projects), nil
}

func sortedStudioProjects(projects map[string]studioProjectSummary) []studioProjectSummary {
	result := make([]studioProjectSummary, 0, len(projects))
	for _, project := range projects {
		result = append(result, project)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Timestamp != result[j].Timestamp {
			return result[i].Timestamp > result[j].Timestamp
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func studioProjectsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			http.Error(w, "Invalid project request", http.StatusBadRequest)
			return
		}
		project, err := registerStudioProject(req.Path)
		if err != nil {
			http.Error(w, "Could not register this Glowbom project.", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"project": project})
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	listProjects := listStudioProjects
	if r.URL.Query().Get("metadataOnly") == "true" {
		listProjects = listRegisteredStudioProjects
	}
	projects, err := listProjects()
	if err != nil {
		http.Error(w, "Could not load Studio projects.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"projects": projects})
}

func studioAssetRaw(id string) (string, map[string]json.RawMessage, []byte, error) {
	id = normalizedStudioUUID(id)
	if id == "" {
		return "", nil, nil, os.ErrNotExist
	}
	dir, err := studioAssetsDirectory()
	if err != nil {
		return "", nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") || !strings.Contains(strings.ToUpper(entry.Name()), id) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		fields, data, err := readStudioJSON(path, 128<<20)
		if err == nil && strings.EqualFold(studioJSONString(fields, "id"), id) {
			return path, fields, data, nil
		}
	}
	return "", nil, nil, os.ErrNotExist
}

func readStudioAssetForLink(id string) (studioImageRecord, error) {
	_, _, data, err := studioAssetRaw(id)
	if err != nil {
		return studioImageRecord{}, err
	}
	var record studioImageRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	if record.MediaType == "" {
		record.MediaType = "image"
	}
	return record, nil
}

func addStudioProjectUsageLocked(assetID, projectID string, expected []byte) (studioImageRecord, error) {
	path, fields, previous, err := studioAssetRaw(assetID)
	if err != nil {
		return studioImageRecord{}, err
	}
	var snapshot studioImageRecord
	if err := json.Unmarshal(previous, &snapshot); err != nil {
		return studioImageRecord{}, err
	}
	if snapshot.MediaType == "" {
		snapshot.MediaType = "image"
	}
	if !studioImageContentMatches(snapshot, expected) {
		return studioImageRecord{}, errors.New("The Studio image changed while copying. No project usage was added.")
	}
	var ids []string
	if value := fields["usedInProjects"]; len(value) > 0 {
		if err := json.Unmarshal(value, &ids); err != nil {
			return studioImageRecord{}, errors.New("The image has unreadable project links")
		}
	}
	if !studioHasProject(ids, projectID) {
		ids = append(ids, projectID)
		fields["usedInProjects"], _ = json.Marshal(ids)
		encoded, err := json.Marshal(fields)
		if err != nil {
			return studioImageRecord{}, err
		}
		_, current, err := readStudioJSON(path, 128<<20)
		if err != nil || !bytes.Equal(current, previous) {
			return studioImageRecord{}, errors.New("The image metadata changed. Try again.")
		}
		if err := atomicChatFile(filepath.Dir(path), filepath.Base(path), encoded); err != nil {
			return studioImageRecord{}, err
		}
		studioCatalogCache.Lock()
		studioCatalogCache.ready = false
		studioCatalogCache.Unlock()
	}
	return readStudioImageRecord(path)
}

func projectStudioImageMatches(root, relative string, data []byte) error {
	if !filepath.IsLocal(relative) || filepath.Clean(relative) != relative || (relative != "icon.png" && !strings.HasPrefix(filepath.ToSlash(relative), "prototype/assets/")) {
		return errors.New("Project images must stay in the project asset folder")
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer handle.Close()
	info, err := handle.Lstat(relative)
	if err != nil || !info.Mode().IsRegular() || info.Size() > studioGeneratedImageMaxBytes {
		return errors.New("The project image is missing or changed")
	}
	file, err := handle.Open(relative)
	if err != nil {
		return err
	}
	defer file.Close()
	current, err := io.ReadAll(io.LimitReader(file, studioGeneratedImageMaxBytes+1))
	if err != nil {
		return err
	}
	if relative == "icon.png" {
		current, err = normalizeProjectIcon(current)
	} else {
		current, err = normalizeStudioGeneratedImage(current)
	}
	if err != nil || !bytes.Equal(current, data) {
		return errors.New("The project image changed. Its newer content was kept.")
	}
	return nil
}

// Call after writing the project image. Cache reuse keeps its original Studio
// identity, and usage is recorded only while the actual copied bytes match.
func linkStudioProjectImage(root, relative string, data []byte, options studioSaveOptions) (studioImageRecord, error) {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	if err := projectStudioImageMatches(root, relative, data); err != nil {
		return studioImageRecord{}, err
	}
	project, err := registerStudioProjectLocked(root)
	if err != nil {
		return studioImageRecord{}, err
	}
	fields, previous, err := readStudioProjectState(project.Path)
	if err != nil {
		return studioImageRecord{}, err
	}
	links := map[string]string{}
	if value := fields["assets"]; len(value) > 0 {
		if err := json.Unmarshal(value, &links); err != nil {
			return studioImageRecord{}, err
		}
	}
	if links == nil {
		links = map[string]string{}
	}
	var asset studioImageRecord
	if id := links[relative]; id != "" && !options.NewGeneration {
		candidate, err := readStudioAssetForLink(id)
		if err == nil && studioImageContentMatches(candidate, data) {
			asset = candidate
		}
	}
	if asset.ID == "" {
		options.SourceProjectID = project.ID
		options.UsedInProjects = nil
		options.DataURI = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
		options.MediaType = "image"
		asset, err = saveStudioAsset(options)
		if err != nil {
			return asset, err
		}
	}
	if err := projectStudioImageMatches(project.Path, relative, data); err != nil {
		return asset, err
	}
	links[relative] = asset.ID
	fields["assets"], _ = json.Marshal(links)
	if err := saveStudioProjectState(project.Path, fields, previous); err != nil {
		return asset, err
	}
	return addStudioProjectUsageLocked(asset.ID, project.ID, data)
}

func studioImageContentMatches(record studioImageRecord, data []byte) bool {
	if record.MediaType != "image" || len(record.DataBase64) > base64.StdEncoding.EncodedLen(studioGeneratedImageMaxBytes) {
		return false
	}
	raw, _, err := decodeBase64Payload(record.DataBase64, "image/png")
	if err != nil {
		return false
	}
	normalized, err := normalizeStudioGeneratedImage(raw)
	return err == nil && bytes.Equal(normalized, data)
}

func studioImageUseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "Invalid image request", http.StatusBadRequest)
		return
	}
	asset, project, relative, err := useStudioImage(req.ID, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"success": true, "image": summarizeStudioRecord(asset), "project": project, "relativePath": relative})
}

func useStudioImage(id, path string) (studioImageRecord, studioProjectSummary, string, error) {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	var project studioProjectSummary
	asset, err := readStudioAssetForLink(id)
	if err != nil || asset.MediaType != "image" || len(asset.DataBase64) > base64.StdEncoding.EncodedLen(studioGeneratedImageMaxBytes) {
		return asset, project, "", errors.New("Choose an available Studio image smaller than 24 MB.")
	}
	raw, _, err := decodeBase64Payload(asset.DataBase64, "image/png")
	if err == nil {
		raw, err = normalizeStudioGeneratedImage(raw)
	}
	if err != nil {
		return asset, project, "", errors.New("This image cannot be copied into a project.")
	}
	project, err = registerStudioProjectLocked(path)
	if err != nil {
		return asset, project, "", errors.New("Open a writable Glowbom project first.")
	}
	filename := "studio-" + strings.ToLower(normalizedStudioUUID(asset.ID)) + ".png"
	relative := "prototype/assets/" + filename
	assets, err := chatWriteDirectory(project.Path, "prototype/assets")
	if err != nil {
		return asset, project, "", errors.New("The project asset folder is unavailable. No image link was added.")
	}
	if _, err := os.Lstat(filepath.Join(assets, filename)); os.IsNotExist(err) {
		if err := createChatImageFile(assets, filename, raw); err != nil {
			return asset, project, "", err
		}
	} else if err != nil {
		return asset, project, "", errors.New("The project image could not be read safely.")
	}
	if err := projectStudioImageMatches(project.Path, relative, raw); err != nil {
		return asset, project, "", err
	}
	fields, previous, err := readStudioProjectState(project.Path)
	if err != nil {
		return asset, project, "", err
	}
	links := map[string]string{}
	if value := fields["assets"]; len(value) > 0 {
		if err := json.Unmarshal(value, &links); err != nil {
			return asset, project, "", err
		}
	}
	if links == nil {
		links = map[string]string{}
	}
	links[relative] = asset.ID
	fields["assets"], _ = json.Marshal(links)
	if err := saveStudioProjectState(project.Path, fields, previous); err != nil {
		return asset, project, "", err
	}
	asset, err = addStudioProjectUsageLocked(asset.ID, project.ID, raw)
	if err == nil {
		project.AssetCount, err = studioProjectAssetCount(project.ID)
	}
	return asset, project, relative, err
}
