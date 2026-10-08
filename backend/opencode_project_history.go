package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type openCodeProjectHistoryAttachmentResponse struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	Filename     string `json:"filename"`
	SizeBytes    int64  `json:"sizeBytes"`
	MimeType     string `json:"mimeType,omitempty"`
	MediaType    string `json:"mediaType,omitempty"`
	RelativePath string `json:"relativePath,omitempty"`
}

type openCodeProjectHistoryEntryResponse struct {
	ID                     string                                     `json:"id"`
	Timestamp              string                                     `json:"timestamp"`
	Instructions           string                                     `json:"instructions"`
	TaskType               string                                     `json:"taskType"`
	Status                 string                                     `json:"status,omitempty"`
	OutputSummary          string                                     `json:"outputSummary,omitempty"`
	Model                  string                                     `json:"model,omitempty"`
	Provider               string                                     `json:"provider,omitempty"`
	Contributor            string                                     `json:"contributor,omitempty"`
	RunID                  string                                     `json:"runId,omitempty"`
	ChangedFiles           []string                                   `json:"changedFiles,omitempty"`
	FolderName             string                                     `json:"folderName"`
	MissingAttachmentCount int                                        `json:"missingAttachmentCount,omitempty"`
	Attachments            []openCodeProjectHistoryAttachmentResponse `json:"attachments,omitempty"`
}

type openCodeProjectHistoryResponse struct {
	Success bool                                  `json:"success"`
	Path    string                                `json:"path,omitempty"`
	Entries []openCodeProjectHistoryEntryResponse `json:"entries,omitempty"`
	Error   string                                `json:"error,omitempty"`
}

func openCodeProjectHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	projectPath, err := readProjectPathFromRequest(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, openCodeProjectHistoryResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	normalizedPath, err := normalizeExistingDirectoryPath(projectPath)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, openCodeProjectHistoryResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	paths := GetProjectPaths(normalizedPath)
	if _, err := os.Stat(paths.Manifest); os.IsNotExist(err) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, openCodeProjectHistoryResponse{
			Success: false,
			Error:   "Not a Glowbom project (missing glowbom.json)",
		})
		return
	}

	entries, err := loadProjectHistoryEntries(normalizedPath)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, openCodeProjectHistoryResponse{
			Success: false,
			Path:    normalizedPath,
			Error:   err.Error(),
		})
		return
	}

	writeJSON(w, openCodeProjectHistoryResponse{
		Success: true,
		Path:    normalizedPath,
		Entries: entries,
	})
}

func loadProjectHistoryEntries(projectPath string) ([]openCodeProjectHistoryEntryResponse, error) {
	historyRoot := filepath.Join(projectPath, "history")
	entries, err := os.ReadDir(historyRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return []openCodeProjectHistoryEntryResponse{}, nil
		}
		return nil, err
	}

	type sortableHistoryEntry struct {
		entry      openCodeProjectHistoryEntryResponse
		sortTime   time.Time
		folderName string
	}

	loaded := make([]sortableHistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		folderName := strings.TrimSpace(entry.Name())
		if folderName == "" {
			continue
		}

		entryDir := filepath.Join(historyRoot, folderName)
		entryPath := filepath.Join(entryDir, "entry.json")
		data, err := os.ReadFile(entryPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			log.Printf("[OPENCODE] Warning: failed reading history entry %s: %v", entryPath, err)
			continue
		}

		var record agentHistoryEntryRecord
		if err := json.Unmarshal(data, &record); err != nil {
			log.Printf("[OPENCODE] Warning: failed decoding history entry %s: %v", entryPath, err)
			continue
		}

		sortTime := historyEntrySortTime(record.Timestamp, folderName)
		timestamp := strings.TrimSpace(record.Timestamp)
		if timestamp == "" && !sortTime.IsZero() {
			timestamp = sortTime.UTC().Format(time.RFC3339)
		}

		attachments := make([]openCodeProjectHistoryAttachmentResponse, 0, len(record.Attachments))
		missingAttachmentCount := 0
		for _, attachment := range record.Attachments {
			filename := strings.TrimSpace(attachment.Filename)
			if filename == "" {
				continue
			}

			archivedPath := filepath.Join(entryDir, filename)
			info, statErr := os.Stat(archivedPath)
			if statErr != nil || info.IsDir() {
				missingAttachmentCount += 1
				continue
			}

			sizeBytes := attachment.FileSizeBytes
			if sizeBytes <= 0 {
				sizeBytes = info.Size()
			}

			mimeType := strings.TrimSpace(attachment.MimeType)
			if mimeType == "" {
				mimeType = inferMimeTypeForFilename(filename)
			}

			mediaType := strings.TrimSpace(attachment.MediaType)
			if mediaType == "" {
				mediaType = inferHistoryMediaType(mimeType, filename)
			}

			attachments = append(attachments, openCodeProjectHistoryAttachmentResponse{
				Path:         archivedPath,
				Name:         filename,
				Filename:     filename,
				SizeBytes:    sizeBytes,
				MimeType:     mimeType,
				MediaType:    mediaType,
				RelativePath: filepath.ToSlash(filepath.Join("history", folderName, filename)),
			})
		}

		taskType := strings.TrimSpace(record.TaskType)
		if taskType == "" {
			taskType = "refine"
		}

		loaded = append(loaded, sortableHistoryEntry{
			entry: openCodeProjectHistoryEntryResponse{
				ID:                     strings.TrimSpace(record.ID),
				Timestamp:              timestamp,
				Instructions:           strings.TrimSpace(record.Instructions),
				TaskType:               taskType,
				Status:                 normalizeHistoryStatus(record.Status),
				OutputSummary:          strings.TrimSpace(record.OutputSummary),
				Model:                  strings.TrimSpace(record.Model),
				Provider:               strings.TrimSpace(record.Provider),
				Contributor:            strings.TrimSpace(record.Contributor),
				RunID:                  strings.TrimSpace(record.RunID),
				ChangedFiles:           record.ChangedFiles,
				FolderName:             folderName,
				MissingAttachmentCount: missingAttachmentCount,
				Attachments:            attachments,
			},
			sortTime:   sortTime,
			folderName: folderName,
		})
	}

	sort.Slice(loaded, func(i, j int) bool {
		left := loaded[i]
		right := loaded[j]
		if left.sortTime.Equal(right.sortTime) {
			return left.folderName > right.folderName
		}
		return left.sortTime.After(right.sortTime)
	})

	result := make([]openCodeProjectHistoryEntryResponse, 0, len(loaded))
	for _, item := range loaded {
		result = append(result, item.entry)
	}

	return result, nil
}

func historyEntrySortTime(timestamp string, folderName string) time.Time {
	trimmedTimestamp := strings.TrimSpace(timestamp)
	if trimmedTimestamp != "" {
		if parsed, err := time.Parse(time.RFC3339, trimmedTimestamp); err == nil {
			return parsed
		}
	}

	prefix := folderName
	if underscore := strings.Index(prefix, "_"); underscore >= 0 {
		if second := strings.Index(prefix[underscore+1:], "_"); second >= 0 {
			prefix = prefix[:underscore+1+second]
		}
	}

	if parsed, err := time.Parse("2006-01-02_150405", prefix); err == nil {
		return parsed.UTC()
	}

	return time.Time{}
}
