package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const studioAudioLimit = 24

type studioAudioSummary struct {
	PromptInfluence   *float64 `json:"promptInfluence,omitempty"`
	Loop              bool     `json:"loop,omitempty"`
	ForceInstrumental bool     `json:"forceInstrumental,omitempty"`
	studioImageSummary
	MediaType                string   `json:"mediaType"`
	AudioType                string   `json:"audioType,omitempty"`
	MimeType                 string   `json:"mimeType,omitempty"`
	Model                    string   `json:"model,omitempty"`
	VoiceID                  string   `json:"voiceId,omitempty"`
	OutputFormat             string   `json:"outputFormat,omitempty"`
	RequestedDurationSeconds *float64 `json:"requestedDurationSeconds,omitempty"`
}

func studioAudioType(audioType, source string) string {
	if value := strings.TrimSpace(audioType); value != "" {
		return normalizeAudioType(value)
	}
	// Native Studio audio predates separate audioType metadata.
	lower := strings.ToLower(source)
	switch {
	case strings.Contains(lower, "music"):
		return "music"
	case strings.Contains(lower, "sound"):
		return "sound"
	case strings.Contains(lower, "voice"):
		return "voice"
	default:
		return ""
	}
}

func summarizeStudioAudio(record studioImageRecord) studioAudioSummary {
	return studioAudioSummary{
		studioImageSummary: summarizeStudioRecord(record), MediaType: "audio",
		AudioType: studioAudioType(record.AudioType, record.SourceService), MimeType: record.MimeType,
		Model: record.Model, VoiceID: record.VoiceID, OutputFormat: record.OutputFormat,
		RequestedDurationSeconds: record.RequestedDurationSeconds,
		PromptInfluence:          record.PromptInfluence, Loop: record.Loop, ForceInstrumental: record.ForceInstrumental,
	}
}

func listStudioAudioPage(projectID string, offset, limit int) ([]studioAudioSummary, bool, error) {
	dir, err := studioAssetsDirectory()
	if err != nil {
		return nil, false, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []studioAudioSummary{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	items := []studioAudioSummary{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		// The bounded streaming reader skips large media strings while listing.
		metadata, err := readStudioAssetMetadata(filepath.Join(dir, entry.Name()))
		if err != nil || metadata.MediaType != "audio" || normalizedStudioUUID(metadata.ID) == "" {
			continue
		}
		summary := studioAudioSummary{
			studioImageSummary: summarizeStudioMetadata(metadata), MediaType: "audio",
			AudioType: studioAudioType(metadata.AudioType, metadata.SourceService), MimeType: metadata.MimeType,
			Model: metadata.Model, VoiceID: metadata.VoiceID, OutputFormat: metadata.OutputFormat,
			RequestedDurationSeconds: metadata.RequestedDurationSeconds,
			PromptInfluence:          metadata.PromptInfluence, Loop: metadata.Loop, ForceInstrumental: metadata.ForceInstrumental,
		}
		if projectID == "" || studioHasProject(studioAssetProjectIDs(summary.studioImageSummary), projectID) {
			items = append(items, summary)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Timestamp != items[j].Timestamp {
			return items[i].Timestamp > items[j].Timestamp
		}
		return items[i].ID > items[j].ID
	})
	start := min(offset, len(items))
	end := start + min(limit, len(items)-start)
	return items[start:end], end < len(items), nil
}

func studioAudioHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		deleteStudioAudioHandler(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	projectID := strings.TrimSpace(query.Get("projectId"))
	if projectID != "" && normalizedStudioUUID(projectID) == "" {
		http.Error(w, "Choose a valid Studio project.", http.StatusBadRequest)
		return
	}
	offset := studioPageParameter(query.Get("offset"), 0, int(^uint(0)>>1))
	limit := studioPageParameter(query.Get("limit"), studioAudioLimit, 48)
	items, more, err := listStudioAudioPage(projectID, offset, limit)
	if err != nil {
		http.Error(w, "Could not load Studio audio.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"audio": items, "hasMore": more})
}

func readStudioAudio(id string) (string, studioImageRecord, []byte, error) {
	path, _, encoded, err := studioAssetRaw(id)
	if err != nil {
		return "", studioImageRecord{}, nil, err
	}
	var record studioImageRecord
	if err := json.Unmarshal(encoded, &record); err != nil || record.MediaType != "audio" || len(record.DataBase64) > (elevenLabsAudioMaxBytes*4/3)+256 {
		return "", record, nil, os.ErrNotExist
	}
	return path, record, encoded, nil
}

func studioAudioBytes(record studioImageRecord) ([]byte, string, error) {
	fallback := record.MimeType
	if !strings.HasPrefix(fallback, "audio/") {
		fallback = "audio/mpeg"
	}
	data, mimeType, err := decodeBase64Payload(record.DataBase64, fallback)
	if err != nil || len(data) == 0 || len(data) > elevenLabsAudioMaxBytes {
		return nil, "", errors.New("The saved audio is unavailable or exceeds 32 MB.")
	}
	if record.MimeType == "" && !strings.HasPrefix(record.DataBase64, "data:") {
		mimeType = ""
	}
	return data, inferAudioMimeType(mimeType, data), nil
}

func studioAudioContentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, record, _, err := readStudioAudio(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "Audio not found", http.StatusNotFound)
		return
	}
	data, mimeType, err := studioAudioBytes(record)
	if err != nil {
		http.Error(w, "The saved audio could not be read.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	filename := "glowbom-" + strings.ToLower(record.ID) + "." + extensionForAudioMimeType(mimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))
	stamp, _ := time.Parse(time.RFC3339, record.Timestamp)
	http.ServeContent(w, r, filename, stamp, bytes.NewReader(data))
}

func studioPlayableAudioFormat(format string) bool {
	switch format {
	case "mp3_22050_32", "mp3_24000_48", "mp3_44100_32", "mp3_44100_64", "mp3_44100_96", "mp3_44100_128", "mp3_44100_192", "mp3_48000_128", "mp3_48000_192",
		"opus_48000_32", "opus_48000_64", "opus_48000_96", "opus_48000_128", "opus_48000_192":
		return true
	default:
		return false
	}
}

func studioAudioGenerateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		ElevenLabsAudioRequest
		ProjectPath string `json:"projectPath,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
		http.Error(w, "Invalid audio request", http.StatusBadRequest)
		return
	}
	req, err := normalizeElevenLabsAudioRequest(request.ElevenLabsAudioRequest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !studioPlayableAudioFormat(req.OutputFormat) {
		http.Error(w, "Choose MP3 or Opus for Studio playback.", http.StatusBadRequest)
		return
	}
	apiKey, ok := resolveVoiceKey(w, r, req.ElevenLabsKey, req.UseSavedKey, systemVoiceKeyStore{})
	if !ok {
		return
	}
	if apiKey == "" {
		http.Error(w, "Add your ElevenLabs key in Voice settings.", http.StatusBadRequest)
		return
	}
	projectID := ""
	if strings.TrimSpace(request.ProjectPath) != "" {
		project, err := registerStudioProject(request.ProjectPath)
		if err != nil {
			http.Error(w, "Open a writable Glowbom project first.", http.StatusBadRequest)
			return
		}
		projectID = project.ID
	}
	ctx, cancel := context.WithTimeout(r.Context(), elevenLabsGenerationTimeout)
	defer cancel()
	var data []byte
	var mimeType string
	switch req.AudioType {
	case "voice":
		data, mimeType, err = callElevenLabsVoice(req, apiKey, ctx)
	case "sound":
		data, mimeType, err = callElevenLabsSound(req, apiKey, ctx)
	case "music":
		data, mimeType, err = callElevenLabsMusic(req, apiKey, ctx)
	}
	if ctx.Err() != nil {
		http.Error(w, "The audio request was stopped.", http.StatusRequestTimeout)
		return
	}
	if err != nil {
		http.Error(w, "ElevenLabs could not generate this audio. Check your key, options, and connection.", http.StatusBadGateway)
		return
	}
	asset, err := saveStudioAudioAsset(req, data, mimeType, projectID)
	if err != nil {
		http.Error(w, "The audio was generated but could not be saved to Studio.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"asset": summarizeStudioAudio(asset)})
}

func saveStudioAudioAsset(req ElevenLabsAudioRequest, data []byte, mimeType, projectID string) (studioImageRecord, error) {
	var err error
	req, err = normalizeElevenLabsAudioRequest(req)
	if err != nil {
		return studioImageRecord{}, err
	}
	if len(data) == 0 || len(data) > elevenLabsAudioMaxBytes || !strings.HasPrefix(mimeType, "audio/") {
		return studioImageRecord{}, errors.New("The generated audio is invalid or exceeds 32 MB.")
	}
	model, voiceID, source := req.VoiceModel, req.VoiceID, "ElevenLabs (Voice)"
	switch req.AudioType {
	case "sound":
		model, voiceID, source = req.SoundModel, "", "ElevenLabs (Sound FX)"
	case "music":
		model, voiceID, source = req.MusicModel, "", "ElevenLabs (Music)"
	}
	options := studioSaveOptions{
		Prompt: req.Prompt, DataURI: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
		MediaType: "audio", Source: source, AssetType: "generated", SourceProjectID: projectID,
		PromptInfluence: req.PromptInfluence, Loop: req.Loop, ForceInstrumental: req.ForceInstrumental,
		AudioType: req.AudioType, MimeType: mimeType, Model: model, VoiceID: voiceID, OutputFormat: req.OutputFormat,
	}
	if req.AudioType != "voice" {
		options.RequestedDurationSeconds = req.DurationSeconds
	}
	return saveStudioAsset(options)
}

func studioAudioUseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil {
		http.Error(w, "Invalid audio request", http.StatusBadRequest)
		return
	}
	asset, project, relative, err := useStudioAudio(request.ID, request.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"asset": summarizeStudioAudio(asset), "project": project, "relativePath": relative})
}

func useStudioAudio(id, projectPath string) (studioImageRecord, studioProjectSummary, string, error) {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	var project studioProjectSummary
	path, asset, original, err := readStudioAudio(id)
	if err != nil {
		return asset, project, "", errors.New("Choose an available Studio audio asset.")
	}
	data, mimeType, err := studioAudioBytes(asset)
	if err != nil {
		return asset, project, "", err
	}
	project, err = registerStudioProjectLocked(projectPath)
	if err != nil {
		return asset, project, "", errors.New("Open a writable Glowbom project first.")
	}
	filename := "studio-" + strings.ToLower(normalizedStudioUUID(asset.ID)) + "." + extensionForAudioMimeType(mimeType)
	relative := "prototype/assets/" + filename
	dir, err := chatWriteDirectory(project.Path, "prototype/assets")
	if err != nil {
		return asset, project, "", errors.New("The project asset folder is unavailable.")
	}
	if _, err := os.Lstat(filepath.Join(dir, filename)); os.IsNotExist(err) {
		if err := createChatImageFile(dir, filename, data); err != nil {
			return asset, project, "", errors.New("The audio could not be added safely. Existing project files were kept.")
		}
	} else if err != nil {
		return asset, project, "", errors.New("The project audio could not be read safely.")
	}
	if err := projectStudioAudioMatches(project.Path, relative, data); err != nil {
		return asset, project, "", err
	}
	fields, previous, err := readStudioProjectState(project.Path)
	if err != nil {
		return asset, project, "", err
	}
	links := map[string]string{}
	if raw := fields["assets"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &links); err != nil {
			return asset, project, "", errors.New("The project has unreadable asset links.")
		}
	}
	if links == nil {
		links = map[string]string{}
	}
	_, metadata, current, err := studioAssetRaw(asset.ID)
	if err != nil || !bytes.Equal(original, current) {
		return asset, project, "", errors.New("The Studio audio changed while copying. Try again.")
	}
	var ids []string
	if raw := metadata["usedInProjects"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &ids); err != nil {
			return asset, project, "", errors.New("The audio has unreadable project links.")
		}
	}
	if !studioHasProject(ids, project.ID) {
		ids = append(ids, project.ID)
	}
	metadata["usedInProjects"], _ = json.Marshal(ids)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return asset, project, "", errors.New("Could not save the audio project link.")
	}
	links[relative] = asset.ID
	fields["assets"], _ = json.Marshal(links)
	if err := saveStudioProjectState(project.Path, fields, previous); err != nil {
		return asset, project, "", err
	}
	if err := atomicChatFile(filepath.Dir(path), filepath.Base(path), encoded); err != nil {
		return asset, project, "", errors.New("The audio was copied but its Studio usage could not be saved.")
	}
	asset.UsedInProjects = normalizedStudioUUIDs(ids)
	project.AssetCount, err = studioProjectAssetCount(project.ID)
	return asset, project, relative, err
}

func projectStudioAudioMatches(root, relative string, expected []byte) error {
	if !filepath.IsLocal(relative) || filepath.Clean(relative) != relative || !strings.HasPrefix(filepath.ToSlash(relative), "prototype/assets/") {
		return errors.New("Project audio must stay in the project asset folder.")
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer handle.Close()
	info, err := handle.Lstat(relative)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(expected)) {
		return errors.New("The project audio is missing or changed. Its newer content was kept.")
	}
	file, err := handle.Open(relative)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, elevenLabsAudioMaxBytes+1))
	if err != nil || !bytes.Equal(data, expected) {
		return errors.New("The project audio changed. Its newer content was kept.")
	}
	return nil
}

func deleteStudioAudioHandler(w http.ResponseWriter, r *http.Request) {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	path, _, original, err := readStudioAudio(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "Audio not found", http.StatusNotFound)
		return
	}
	_, current, err := readStudioJSON(path, 128<<20)
	if err != nil || !bytes.Equal(current, original) {
		http.Error(w, "The audio changed. Try deleting it again.", http.StatusConflict)
		return
	}
	if err := os.Remove(path); err != nil {
		http.Error(w, "Could not remove the Studio audio.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"success": true})
}
