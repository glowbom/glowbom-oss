package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
)

type companionProjectIconRequest struct {
	RequestID   string `json:"requestId"`
	Prompt      string `json:"prompt"`
	SourceID    string `json:"sourceId"`
	ReferenceID string `json:"referenceId,omitempty"`
}

type companionProjectIconJob struct {
	job     *companionJob
	request companionProjectIconRequest
	warning string
}

func (j *companionProjectIconJob) snapshot() map[string]any {
	j.job.mu.Lock()
	defer j.job.mu.Unlock()
	result := map[string]any{"id": j.job.id, "projectId": j.job.projectID, "status": j.job.status,
		"startedAt": j.job.startedAt, "prompt": j.request.Prompt, "sourceId": j.request.SourceID}
	if j.job.errorText != "" {
		result["error"] = j.job.errorText
	}
	if j.warning != "" {
		result["warning"] = j.warning
	}
	return result
}

func (s *companionSession) routeProjectSettings(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if len(parts) < 3 || parts[0] != "projects" || (parts[2] != "settings" && parts[2] != "icon") {
		return false
	}
	project, ok := s.project(parts[1])
	if !ok {
		http.NotFound(w, r)
		return true
	}
	if len(parts) == 3 && parts[2] == "settings" {
		s.projectSettings(w, r, project)
		return true
	}
	if len(parts) == 4 && parts[2] == "icon" && parts[3] == "content" && r.Method == http.MethodPut {
		s.setProjectIcon(w, r, project)
		return true
	}
	if len(parts) == 4 && parts[2] == "icon" && parts[3] == "content" && r.Method == http.MethodGet {
		root, err := os.OpenRoot(project.path)
		if err != nil {
			http.NotFound(w, r)
			return true
		}
		defer root.Close()
		data, err := readProjectIcon(root)
		if err == nil && len(data) > 0 {
			data, err = normalizeProjectIcon(data)
		}
		if err != nil || len(data) == 0 {
			http.NotFound(w, r)
			return true
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(data)
		return true
	}
	if len(parts) == 4 && parts[2] == "icon" && parts[3] == "generations" {
		if r.Method == http.MethodPost {
			s.startProjectIcon(w, r, project)
			return true
		}
		if r.Method == http.MethodGet {
			jobs := []map[string]any{}
			s.mu.Lock()
			for _, job := range s.iconJobs {
				if job.job.projectID == project.ID {
					jobs = append(jobs, job.snapshot())
				}
			}
			s.mu.Unlock()
			sort.Slice(jobs, func(i, j int) bool { return jobs[i]["startedAt"].(string) > jobs[j]["startedAt"].(string) })
			writeJSON(w, map[string]any{"generations": jobs})
			return true
		}
	}
	if len(parts) == 5 && parts[2] == "icon" && parts[3] == "generations" {
		id := strings.ToLower(normalizedStudioUUID(parts[4]))
		s.mu.Lock()
		job := s.iconJobs[id]
		s.mu.Unlock()
		if id == "" || job == nil || job.job.projectID != project.ID {
			http.NotFound(w, r)
			return true
		}
		if r.Method == http.MethodDelete {
			job.job.cancelJob(s.now())
		} else if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", 405)
			return true
		}
		writeJSON(w, job.snapshot())
		return true
	}
	http.NotFound(w, r)
	return true
}

func companionProjectSettings(root *os.Root, project companionProject) (map[string]any, error) {
	file, err := root.Open("glowbom.json")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("invalid project manifest")
	}
	var manifest struct {
		Name      string `json:"name"`
		CreatedAt string `json:"createdAt"`
	}
	if json.NewDecoder(io.LimitReader(file, (1<<20)+1)).Decode(&manifest) != nil || strings.TrimSpace(manifest.Name) == "" {
		return nil, errors.New("invalid project name")
	}
	project.Name = manifest.Name
	project.CreatedAt = companionProjectDate(manifest.CreatedAt)
	icon, err := readProjectIcon(root)
	if err != nil {
		return nil, err
	}
	return map[string]any{"project": project, "hasIcon": len(icon) > 0, "iconGeneration": true}, nil
}

func (s *companionSession) projectSettings(w http.ResponseWriter, r *http.Request, project companionProject) {
	ctx, cancel := s.chatContext(r.Context())
	defer cancel()
	root, err := os.OpenRoot(project.path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	if r.Method == http.MethodPatch {
		var request struct {
			Name         string `json:"name"`
			ExpectedName string `json:"expectedName"`
		}
		if !companionDecode(w, r, &request, 8192) {
			return
		}
		request.Name = strings.TrimSpace(request.Name)
		if request.Name == "" || len(request.Name) > 120 || strings.IndexFunc(request.Name, unicode.IsControl) >= 0 {
			http.Error(w, "Enter a project name of up to 120 bytes.", 400)
			return
		}
		if !s.attachmentRequestActive(ctx) {
			http.Error(w, "This connection stopped before the name was saved.", 409)
			return
		}
		if err := renameCompanionProject(root, request.Name, request.ExpectedName, ctx); err != nil {
			http.Error(w, "The project name could not be saved. Refresh its settings before trying again.", 409)
			return
		}
	} else if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", 405)
		return
	}
	projectRenameMu.Lock()
	result, err := companionProjectSettings(root, project)
	if err != nil {
		projectRenameMu.Unlock()
		http.Error(w, "Desktop could not read these project settings.", 409)
		return
	}
	updated := result["project"].(companionProject)
	s.mu.Lock()
	s.projects[project.ID] = updated
	s.mu.Unlock()
	projectRenameMu.Unlock()
	writeJSON(w, result)
}

// The existing Desktop rename lock also protects the companion's manifest update.
func renameCompanionProject(root *os.Root, name, expected string, contexts ...context.Context) error {
	projectRenameMu.Lock()
	defer projectRenameMu.Unlock()
	info, err := root.Lstat("glowbom.json")
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return errors.New("invalid manifest")
	}
	file, err := root.Open("glowbom.json")
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	_ = file.Close()
	if err != nil || len(data) > 1<<20 {
		return errors.New("invalid manifest")
	}
	var manifest map[string]json.RawMessage
	if json.Unmarshal(data, &manifest) != nil || manifest == nil {
		return errors.New("invalid manifest")
	}
	var current string
	if json.Unmarshal(manifest["name"], &current) != nil {
		return errors.New("invalid project name")
	}
	if current == name {
		return nil
	}
	if current != expected {
		return errors.New("name changed")
	}
	manifest["name"], _ = json.Marshal(name)
	manifest["updatedAt"], _ = json.Marshal(time.Now().UTC().Format(time.RFC3339))
	data, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	temporary := ".glowbom-name-" + strings.ToLower(randomUUIDString())
	output, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	_, writeErr := output.Write(data)
	closeErr := output.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	rootInfo, rootErr := root.Stat(".")
	pathInfo, pathErr := os.Stat(root.Name())
	if rootErr != nil || pathErr != nil || !os.SameFile(rootInfo, pathInfo) {
		return errors.New("folder changed")
	}
	if imageRequestContext(contexts).Err() != nil {
		return context.Canceled
	}
	return root.Rename(temporary, "glowbom.json")
}

func (s *companionSession) setProjectIcon(w http.ResponseWriter, r *http.Request, project companionProject) {
	var request struct {
		AttachmentID string `json:"attachmentId"`
	}
	if !companionDecode(w, r, &request, 1024) {
		return
	}
	request.AttachmentID = strings.ToLower(normalizedStudioUUID(request.AttachmentID))
	if request.AttachmentID == "" {
		http.Error(w, "Choose an icon image again.", 400)
		return
	}
	ctx, cancel := s.chatContext(r.Context())
	defer cancel()
	images, err := s.chatImages(ctx, project, []string{request.AttachmentID})
	if err != nil || len(images) != 1 {
		http.Error(w, "Attach this icon again for this project.", 410)
		return
	}
	uri, _ := images[0]["url"].(string)
	data, _, err := decodeBase64Payload(uri, "image/png")
	if err == nil {
		config, _, imageErr := image.DecodeConfig(bytes.NewReader(data))
		if imageErr != nil || config.Width != config.Height {
			err = errors.New("icon must be square")
		}
	}
	if err == nil {
		data, err = normalizeProjectIcon(data)
	}
	if err != nil {
		http.Error(w, "Choose a complete square icon image.", 400)
		return
	}
	root, err := os.OpenRoot(project.path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	previous, err := readProjectIcon(root)
	if err != nil {
		http.Error(w, "The current icon could not be read.", 409)
		return
	}
	s.mu.Lock()
	busy := false
	for _, job := range s.iconJobs {
		job.job.mu.Lock()
		busy = busy || (job.job.projectID == project.ID && job.job.status == "running")
		job.job.mu.Unlock()
	}
	if busy || !s.attachmentRequestActive(ctx) {
		s.mu.Unlock()
		http.Error(w, "Wait for this project's icon creation to finish, or check the connection.", 409)
		return
	}
	err = saveProjectIcon(root, previous, data, ctx)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "The icon changed or could not be saved. Refresh before trying again.", 409)
		return
	}
	result, err := companionProjectSettings(root, project)
	if err != nil {
		http.Error(w, "The icon was saved, but its project settings could not be read.", 409)
		return
	}
	writeJSON(w, result)
}

func (s *companionSession) projectIconSourceAvailable(ctx context.Context, id string) bool {
	writer := &companionProjectSettingsWriter{header: make(http.Header)}
	s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodGet, "/opencode/project/icon/sources", nil))
	var catalog struct {
		Sources []projectIconSource `json:"sources"`
	}
	if writer.code != http.StatusOK || len(writer.body) > 64<<10 || json.Unmarshal(writer.body, &catalog) != nil || len(catalog.Sources) > 64 {
		return false
	}
	for _, source := range catalog.Sources {
		if source.ID == id && source.Available {
			return true
		}
	}
	return false
}

func (s *companionSession) startProjectIcon(w http.ResponseWriter, r *http.Request, project companionProject) {
	var request companionProjectIconRequest
	if !companionDecode(w, r, &request, 8192) {
		return
	}
	request.RequestID = strings.ToLower(normalizedStudioUUID(request.RequestID))
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.ReferenceID != "" {
		request.ReferenceID = strings.ToLower(normalizedStudioUUID(request.ReferenceID))
		if request.ReferenceID == "" {
			http.Error(w, "Attach this reference again.", 400)
			return
		}
	}
	if request.RequestID == "" || request.Prompt == "" || len(request.Prompt) > 4000 || request.SourceID == "" || len(request.SourceID) > 80 || strings.IndexFunc(request.SourceID, unicode.IsControl) >= 0 {
		http.Error(w, "Choose an image provider and an icon description up to 4,000 bytes.", 400)
		return
	}
	// Replaying an accepted request never starts a second paid generation.
	metadataContext, finishMetadata := s.chatContext(r.Context())
	defer finishMetadata()
	s.mu.Lock()
	existing := s.iconJobs[request.RequestID]
	s.mu.Unlock()
	if existing != nil {
		if existing.job.projectID != project.ID || existing.request != request {
			http.Error(w, "This icon request changed. Start a new request.", 409)
			return
		}
		writeJSON(w, existing.snapshot())
		return
	}
	if !s.projectIconSourceAvailable(metadataContext, request.SourceID) {
		http.Error(w, "Connect this image provider on Desktop first.", 400)
		return
	}
	payload := projectIconRequest{Path: project.path, Prompt: request.Prompt, SourceID: request.SourceID}
	if request.ReferenceID != "" {
		images, err := s.chatImages(metadataContext, project, []string{request.ReferenceID})
		if err != nil || len(images) != 1 {
			http.Error(w, "Attach this reference again before generating the icon.", 400)
			return
		}
		payload.ReferenceImage, _ = images[0]["url"].(string)
	}
	s.mu.Lock()
	if !s.attachmentRequestActive(metadataContext) {
		s.mu.Unlock()
		http.Error(w, "This connection stopped before the icon request started.", 409)
		return
	}
	if existing := s.iconJobs[request.RequestID]; existing != nil {
		s.mu.Unlock()
		if existing.job.projectID != project.ID || existing.request != request {
			http.Error(w, "This icon request changed.", 409)
			return
		}
		writeJSON(w, existing.snapshot())
		return
	}
	if len(s.iconJobs) >= 32 {
		s.mu.Unlock()
		http.Error(w, "Pair again to start more icon requests.", 429)
		return
	}
	for _, active := range s.iconJobs {
		active.job.mu.Lock()
		busy := active.job.projectID == project.ID && active.job.status == "running"
		active.job.mu.Unlock()
		if busy {
			s.mu.Unlock()
			http.Error(w, "An icon is already being created for this project.", 409)
			return
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Minute)
	job := &companionProjectIconJob{request: request, job: &companionJob{id: request.RequestID, projectID: project.ID,
		kind: "icon", status: "running", startedAt: s.now().UTC().Format(time.RFC3339Nano), ctx: ctx, cancel: cancel}}
	if s.iconJobs == nil {
		s.iconJobs = map[string]*companionProjectIconJob{}
	}
	s.iconJobs[request.RequestID] = job
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, job.snapshot())
	go func() {
		defer cancel()
		writer := &companionProjectSettingsWriter{header: make(http.Header)}
		s.api.ServeHTTP(writer, s.internalRequest(ctx, http.MethodPost, "/opencode/project/icon/generate", payload))
		var result struct {
			Success bool   `json:"success"`
			Warning string `json:"warning"`
		}
		valid := writer.code < 400 && json.Unmarshal(writer.body, &result) == nil && result.Success
		job.job.mu.Lock()
		defer job.job.mu.Unlock()
		if job.job.status != "running" {
			return
		}
		if ctx.Err() != nil {
			job.job.status = "canceled"
		} else if !valid {
			job.job.status = "failed"
			job.job.errorText = "Desktop could not create this icon. Check the image provider and try again. Your previous icon was kept."
		} else {
			job.job.status = "completed"
			if result.Warning != "" {
				job.warning = "The icon was saved, but Desktop could not add it to Studio."
			}
		}
		job.job.finishedAt = s.now().UTC().Format(time.RFC3339Nano)
	}()
}

// Icon provider responses contain image data, which is never part of job progress.
type companionProjectSettingsWriter struct {
	header http.Header
	code   int
	body   []byte
}

func (w *companionProjectSettingsWriter) Header() http.Header { return w.header }
func (w *companionProjectSettingsWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *companionProjectSettingsWriter) Write(data []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	if len(w.body)+len(data) > 18<<20 {
		return 0, errors.New("icon response too large")
	}
	w.body = append(w.body, data...)
	return len(data), nil
}
