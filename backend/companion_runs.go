package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type companionRunContextKey struct{}

func managedInstructionsDirectory(ctx context.Context) string {
	if id, ok := ctx.Value(companionRunContextKey{}).(string); ok && cursorSessionID.MatchString(id) {
		return filepath.Join(".glowbom", "runs", id, "instructions")
	}
	return "current_instructions"
}

func (m *companionManager) newRun(project companionProject, source string, req OpenCodeAgentRequest) *companionJob {
	ctx, cancel := context.WithCancel(m.runContext)
	job := &companionJob{id: strings.ToLower(randomUUIDString()), projectID: project.ID, projectPath: project.path,
		kind: "build", status: "running", source: source, instructions: companionPublicRunText(req.Instructions, 8000),
		model: req.Model, agentDriver: normalizedBuildDriver(req.AgentDriver), buildTargets: appendBuildTargets(req.BuildTargets),
		output: []string{}, startedAt: m.now().UTC().Format(time.RFC3339Nano), ctx: ctx, cancel: cancel}
	job.ctx = context.WithValue(ctx, companionRunContextKey{}, job.id)
	job.permissionPolicy = newBuildPermissionPolicy(job.ctx, job.agentDriver, project.path, req.PermissionMode)
	job.ctx = withBuildPermissionPolicy(job.ctx, job.permissionPolicy)
	job.reasoningEffort = companionRunReasoningEffort(job.agentDriver, req.ReasoningEffort)
	if job.agentDriver != "opencode" && !strings.HasPrefix(job.model, job.agentDriver+"/") {
		job.model = job.agentDriver + "/" + job.model
	}
	job.save = func() { m.saveRun(job) }
	m.registerRun(job)
	m.saveRun(job)
	return job
}

func companionRunReasoningEffort(driver, effort string) string {
	if driver != "codex" || len(effort) > 64 || strings.ContainsAny(effort, "\r\n\x00") {
		return ""
	}
	return effort
}

func (m *companionManager) registerRun(job *companionJob) {
	m.runMu.Lock()
	m.runs[job.id] = job
	if len(m.runs) > 128 {
		var oldest *companionJob
		for _, candidate := range m.runs {
			candidate.mu.Lock()
			finished := candidate.status != "running" && candidate.status != "waiting"
			candidate.mu.Unlock()
			if finished && (oldest == nil || companionCompareStartedAt(candidate.startedAt, oldest.startedAt) < 0) {
				oldest = candidate
			}
		}
		if oldest != nil {
			delete(m.runs, oldest.id)
		}
	}
	m.runMu.Unlock()
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	if session != nil && session.manager == nil {
		if _, ok := session.project(job.projectID); ok {
			session.mu.Lock()
			session.jobs[job.id] = job
			session.mu.Unlock()
		}
	}
}

func (m *companionManager) runList() []*companionJob {
	m.runMu.Lock()
	jobs := make(map[string]*companionJob, len(m.runs))
	for id, job := range m.runs {
		jobs[id] = job
	}
	m.runMu.Unlock()
	// Sessions created by older callers can still hold image jobs and fixtures.
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	if session != nil {
		session.mu.Lock()
		for id, job := range session.jobs {
			jobs[id] = job
		}
		session.mu.Unlock()
	}
	result := make([]*companionJob, 0, len(jobs))
	for _, job := range jobs {
		result = append(result, job)
	}
	sort.Slice(result, func(i, j int) bool { return companionCompareStartedAt(result[i].startedAt, result[j].startedAt) > 0 })
	return result
}

func (m *companionManager) runSnapshots(local bool) []any {
	result := []any{}
	for _, job := range m.runList() {
		result = append(result, job.snapshot(local))
	}
	return result
}

func (m *companionManager) run(id string) *companionJob {
	for _, job := range m.runList() {
		if job.id == id {
			return job
		}
	}
	return nil
}

func (s *companionSession) sharedJob(id string) *companionJob {
	var job *companionJob
	if s.manager != nil {
		job = s.manager.run(id)
	}
	if job == nil {
		s.mu.Lock()
		job = s.jobs[id]
		s.mu.Unlock()
	}
	return job
}

func (m *companionManager) restoreRecentRuns() {
	if projects, err := listRegisteredStudioProjects(); err == nil {
		for _, project := range projects {
			if project.Available {
				m.loadProjectRuns(project.Path)
			}
		}
	}
}

// Disconnecting the phone revokes phone access; accepted builds keep running.
// Process shutdown and per-run Stop are the only owner cancellation controls.
func (m *companionManager) Shutdown() { m.Close(); m.stopRuns() }

func (m *companionManager) finishRun(job *companionJob, writer *companionWriter) {
	writer.mu.Lock()
	code := writer.code
	writer.mu.Unlock()
	job.mu.Lock()
	if job.status != "completed" && job.status != "failed" && job.status != "canceled" {
		job.status = "failed"
		job.errorText = "The build ended without a completion result. Check Desktop before trying again."
		if job.ctx.Err() != nil {
			job.status, job.errorText = "canceled", ""
		}
		if code >= 400 {
			job.errorText = "Desktop could not start this build. Check its selected model and try again."
		}
	}
	job.pendingPermission, job.pendingQuestion, job.pendingMediaApproval = nil, nil, nil
	job.permissionPolicy.close()
	job.finishedAt = m.now().UTC().Format(time.RFC3339Nano)
	job.mu.Unlock()
	m.saveRun(job)
}

func (m *companionManager) saveRun(job *companionJob) {
	job.saveMu.Lock()
	defer job.saveMu.Unlock()
	snapshot := job.snapshot(false)
	delete(snapshot, "permissionMode")
	// Native CLI sessions can resume after a backend restart. This identifier
	// belongs only in the local run file, not the phone's public snapshot.
	job.mu.Lock()
	if strings.HasPrefix(job.agentSessionID, codexSessionPrefix) || strings.HasPrefix(job.agentSessionID, claudeCodeSessionPrefix) || validSavedACPSession(job.agentSessionID) {
		snapshot["sessionID"] = job.agentSessionID
	}
	job.mu.Unlock()
	status, _ := snapshot["status"].(string)
	finishedAt, _ := snapshot["finishedAt"].(string)
	if time.Since(job.savedAt) < 250*time.Millisecond && status == job.savedStatus && finishedAt == job.savedFinishedAt {
		return
	}
	job.savedAt, job.savedStatus, job.savedFinishedAt = time.Now(), status, finishedAt
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > 1<<20 {
		return
	}
	root, err := os.OpenRoot(job.projectPath)
	if err != nil {
		return
	}
	defer root.Close()
	directory := filepath.Join(".glowbom", "runs", job.id)
	for _, dir := range []string{".glowbom", filepath.Join(".glowbom", "runs"), directory} {
		if err := root.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return
		}
	}
	file, err := root.OpenFile(filepath.Join(directory, "run.tmp"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr == nil && closeErr == nil {
		_ = root.Rename(filepath.Join(directory, "run.tmp"), filepath.Join(directory, "run.json"))
	}
}

// Local saved snapshots contain bounded public progress, never login credentials
// or private permission metadata. Live approvals are not replayed after restart.
func (m *companionManager) loadProjectRuns(project string) {
	root, err := os.OpenRoot(project)
	if err != nil {
		return
	}
	defer root.Close()
	directory, err := root.Open(filepath.Join(".glowbom", "runs"))
	if err != nil {
		return
	}
	entries, err := directory.ReadDir(-1)
	_ = directory.Close()
	if err != nil && len(entries) == 0 {
		return
	}
	type recentDirectory struct {
		entry    os.DirEntry
		modified time.Time
	}
	recent := []recentDirectory{}
	for _, entry := range entries {
		if entry.IsDir() && cursorSessionID.MatchString(entry.Name()) {
			if info, err := entry.Info(); err == nil {
				recent = append(recent, recentDirectory{entry, info.ModTime()})
			}
		}
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].modified.After(recent[j].modified) })
	if len(recent) > 128 {
		recent = recent[:128]
	}
	entries = nil
	for _, directory := range recent {
		entries = append(entries, directory.entry)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !cursorSessionID.MatchString(entry.Name()) {
			continue
		}
		file, err := root.Open(filepath.Join(".glowbom", "runs", entry.Name(), "run.json"))
		if err != nil {
			continue
		}
		var saved struct {
			ID              string                     `json:"id"`
			ProjectID       string                     `json:"projectId"`
			Kind            string                     `json:"kind"`
			Status          string                     `json:"status"`
			StartedAt       string                     `json:"startedAt"`
			FinishedAt      string                     `json:"finishedAt"`
			Instructions    string                     `json:"instructions"`
			Model           string                     `json:"model"`
			ReasoningEffort string                     `json:"reasoningEffort"`
			AgentDriver     string                     `json:"agentDriver"`
			AgentName       string                     `json:"agentName"`
			SessionID       string                     `json:"sessionID"`
			BuildTargets    []string                   `json:"buildTargets"`
			Output          []string                   `json:"output"`
			RunID           string                     `json:"runId"`
			ResultText      string                     `json:"resultText"`
			Source          string                     `json:"source"`
			Attachments     []companionImageAttachment `json:"attachments"`
			ChangedFiles    []string                   `json:"changedFiles"`
			BuildStatus     json.RawMessage            `json:"buildStatus"`
			LiveStatuses    []json.RawMessage          `json:"liveStatuses"`
			PartialLine     string                     `json:"partialLine"`
			ErrorText       string                     `json:"error"`
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			_ = file.Close()
			continue
		}
		decodeErr := json.NewDecoder(file).Decode(&saved)
		_ = file.Close()
		if decodeErr != nil || saved.ID != entry.Name() || saved.ProjectID != companionProjectID(project) || saved.Kind != "build" {
			continue
		}
		m.runMu.Lock()
		_, existing := m.runs[saved.ID]
		m.runMu.Unlock()
		if existing {
			continue
		}
		job := &companionJob{id: saved.ID, projectID: saved.ProjectID, projectPath: project, kind: saved.Kind, status: saved.Status,
			startedAt: saved.StartedAt, finishedAt: saved.FinishedAt, instructions: saved.Instructions, model: saved.Model,
			agentDriver: normalizedBuildDriver(saved.AgentDriver), buildTargets: saved.BuildTargets, output: saved.Output,
			runID: saved.RunID, resultText: saved.ResultText, source: saved.Source, attachments: saved.Attachments, changedFiles: saved.ChangedFiles,
			buildStatus: saved.BuildStatus, liveStatuses: saved.LiveStatuses, partialLine: saved.PartialLine, errorText: saved.ErrorText}
		job.reasoningEffort = companionRunReasoningEffort(job.agentDriver, saved.ReasoningEffort)
		if job.agentDriver == "acp" {
			job.agentName = companionPublicRunText(saved.AgentName, 80)
		}
		if saved.AgentDriver == "codex" && strings.HasPrefix(saved.SessionID, codexSessionPrefix) && len(saved.SessionID) <= 512 && !strings.ContainsAny(saved.SessionID, "\r\n\x00") {
			job.agentSessionID = saved.SessionID
		}
		if saved.AgentDriver == "claude-code" && strings.HasPrefix(saved.SessionID, claudeCodeSessionPrefix) && claudeCodeSessionID.MatchString(strings.TrimPrefix(saved.SessionID, claudeCodeSessionPrefix)) {
			job.agentSessionID = saved.SessionID
		}
		if saved.AgentDriver == "acp" && validSavedACPSession(saved.SessionID) {
			job.agentSessionID = saved.SessionID
		}
		if job.status == "running" || job.status == "waiting" {
			job.status, job.errorText = "failed", "Desktop restarted before this build finished. Start another build to continue."
			job.finishedAt = m.now().UTC().Format(time.RFC3339Nano)
		}
		m.registerRun(job)
	}
}

type companionStreamChunk struct {
	data   []byte
	header http.Header
	code   int
}
