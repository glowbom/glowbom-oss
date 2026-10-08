package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
)

// Native records also contain large histories and images. Keep only the fields
// needed to find projects, and validate the project folder separately each time.
type studioProjectRecordMetadata struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Path                string `json:"path"`
	ProjectName         string `json:"projectName"`
	ExportedProjectPath string `json:"exportedProjectPath"`
	Timestamp           string `json:"timestamp"`
}

type studioProjectMetadataEntry struct {
	info     os.FileInfo
	metadata studioProjectRecordMetadata
}

// Access is protected by studioProjectMu along with registry reads and writes.
var studioProjectMetadataCache = struct {
	root    string
	entries map[string]studioProjectMetadataEntry
}{}

func sameStudioProjectRecord(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() &&
		os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func cachedStudioProjectMetadata(path string) (studioProjectRecordMetadata, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		delete(studioProjectMetadataCache.entries, path)
		return studioProjectRecordMetadata{}, errors.New("Studio metadata is not a regular bounded file")
	}
	if cached, found := studioProjectMetadataCache.entries[path]; found && sameStudioProjectRecord(cached.info, info) {
		return cached.metadata, nil
	}
	delete(studioProjectMetadataCache.entries, path)
	file, err := os.Open(path)
	if err != nil {
		return studioProjectRecordMetadata{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameStudioProjectRecord(info, opened) {
		return studioProjectRecordMetadata{}, errors.New("Studio metadata changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<20)+1))
	if err != nil || len(data) > 64<<20 {
		return studioProjectRecordMetadata{}, errors.New("Could not read Studio metadata")
	}
	var fields *struct {
		ID                  json.RawMessage `json:"id"`
		Name                json.RawMessage `json:"name"`
		Path                json.RawMessage `json:"path"`
		ProjectName         json.RawMessage `json:"projectName"`
		ExportedProjectPath json.RawMessage `json:"exportedProjectPath"`
		Timestamp           json.RawMessage `json:"timestamp"`
	}
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return studioProjectRecordMetadata{}, errors.New("Studio metadata is invalid")
	}
	stringField := func(raw json.RawMessage) string {
		var value string
		_ = json.Unmarshal(raw, &value)
		return value
	}
	metadata := studioProjectRecordMetadata{ID: stringField(fields.ID), Name: stringField(fields.Name), Path: stringField(fields.Path),
		ProjectName: stringField(fields.ProjectName), ExportedProjectPath: stringField(fields.ExportedProjectPath), Timestamp: stringField(fields.Timestamp)}
	if normalizedStudioUUID(metadata.ID) == "" {
		return studioProjectRecordMetadata{}, errors.New("Studio metadata is invalid")
	}
	current, pathErr := os.Lstat(path)
	finished, fileErr := file.Stat()
	if pathErr != nil || fileErr != nil || !sameStudioProjectRecord(opened, current) || !sameStudioProjectRecord(opened, finished) {
		return studioProjectRecordMetadata{}, errors.New("Studio metadata changed while reading")
	}
	studioProjectMetadataCache.entries[path] = studioProjectMetadataEntry{info: current, metadata: metadata}
	return metadata, nil
}
