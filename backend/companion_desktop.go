package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// A tap keeps the Desktop's original stream intact while recording the same
// run for the paired phone. Phone-started workers already have their collector.
type companionTapWriter struct {
	http.ResponseWriter
	collector *companionWriter
}

func (w *companionTapWriter) Header() http.Header { return w.ResponseWriter.Header() }
func (w *companionTapWriter) WriteHeader(code int) {
	w.collector.WriteHeader(code)
	w.ResponseWriter.WriteHeader(code)
}
func (w *companionTapWriter) Write(data []byte) (int, error) {
	w.collector.header.Set("Content-Type", w.Header().Get("Content-Type"))
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		_, _ = w.collector.Write(data[:n])
	}
	return n, err
}
func (w *companionTapWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (m *companionManager) trackDesktopBuild(next http.HandlerFunc, w http.ResponseWriter, r *http.Request, root string, data []byte, release func()) {
	if _, phone := w.(*companionWriter); phone {
		defer release()
		next(w, r)
		return
	}
	var request OpenCodeAgentRequest
	if json.Unmarshal(data, &request) != nil {
		defer release()
		next(w, r)
		return
	}
	if r.Context().Err() != nil {
		release()
		return
	}
	project := companionProject{ID: companionProjectID(root), path: root}
	job := m.newRun(project, "desktop", request)
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	if session != nil {
		if shared, ok := session.project(project.ID); ok {
			session.rememberBuildTargets(shared, job.buildTargets)
		}
	}
	chunks := make(chan companionStreamChunk, 64)
	done := make(chan struct{})
	writer := &companionWriter{header: make(http.Header), job: job}
	writer.observe = func(data []byte, header http.Header, code int) {
		select {
		case chunks <- companionStreamChunk{append([]byte{}, data...), header.Clone(), code}:
		case <-r.Context().Done():
		case <-job.ctx.Done():
		}
	}
	writer.header.Set("X-Glowbom-Job-ID", job.id)
	writer.header.Set("Access-Control-Expose-Headers", "X-Glowbom-Job-ID, X-Glowbom-Run-ID")
	go func() {
		defer release()
		defer job.cancel()
		defer close(done)
		defer func() { m.finishRun(job, writer) }()
		next(writer, r.WithContext(job.ctx))
	}()
	w.Header().Set("X-Glowbom-Job-ID", job.id)
	w.Header().Set("Access-Control-Expose-Headers", "X-Glowbom-Job-ID, X-Glowbom-Run-ID")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	initial, _ := json.Marshal(map[string]string{"jobId": job.id})
	_, _ = w.Write(append(append([]byte("data: "), initial...), []byte("\n\n")...))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	write := func(chunk companionStreamChunk) bool {
		data := chunk.data
		if chunk.code >= 400 {
			failed, _ := json.Marshal(map[string]any{"done": true, "success": false, "jobId": job.id, "error": "Desktop could not start this build. Check its selected model and try again."})
			data = append(append([]byte("data: "), failed...), []byte("\n\n")...)
		}
		if _, err := w.Write(data); err != nil {
			return false
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return true
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case chunk := <-chunks:
			if !write(chunk) {
				return
			}
		case <-done:
			for {
				select {
				case chunk := <-chunks:
					if !write(chunk) {
						return
					}
				default:
					final := job.snapshot(true)
					terminal, _ := json.Marshal(map[string]any{"jobId": job.id, "jobStatus": final["status"], "cancelled": final["status"] == "canceled", "done": true, "success": final["status"] == "completed"})
					_, _ = w.Write(append(append([]byte("data: "), terminal...), []byte("\n\n")...))
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					return
				}
			}
		}
	}
}

// Desktop's existing approval controls use the original OpenCode routes. Match
// only the captured project's exact agent session and pending request, then use
// the same response mutex as the companion controls to avoid duplicate replies.
func (m *companionManager) guardResponse(kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, phone := w.(*companionWriter); phone || r.Method != http.MethodPost {
			next(w, r)
			return
		}
		limit := int64(16 << 10)
		if kind == "media" {
			limit = 36 << 20
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			http.Error(w, "This response is too large.", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var request struct {
			SessionID    string `json:"sessionID"`
			PermissionID string `json:"permissionID"`
			QuestionID   string `json:"questionID"`
			ApprovalID   string `json:"approvalID"`
			ProjectPath  string `json:"projectPath"`
			Response     string `json:"response"`
		}
		if json.Unmarshal(data, &request) != nil {
			next(w, r)
			return
		}
		id := request.PermissionID
		if kind == "question" {
			id = request.QuestionID
		}
		if kind == "media" {
			id = request.ApprovalID
		}
		root, err := chatProjectRoot(request.ProjectPath)
		if err != nil || id == "" || (kind != "media" && request.SessionID == "") {
			next(w, r)
			return
		}
		var job *companionJob
		for _, candidate := range m.runList() {
			candidate.mu.Lock()
			match := candidate.projectPath == root && candidate.agentSessionID == request.SessionID
			if kind == "media" {
				match = candidate.projectPath == root && (companionPendingID(candidate.pendingMediaApproval) == id || candidate.responded["media:"+id])
			}
			candidate.mu.Unlock()
			if match {
				job = candidate
				break
			}
		}
		if job == nil {
			next(w, r)
			return
		}
		job.responseMu.Lock()
		defer job.responseMu.Unlock()
		key := kind + ":" + id
		job.mu.Lock()
		if job.responded[key] {
			job.mu.Unlock()
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		pending := job.pendingPermission
		if kind == "question" {
			pending = job.pendingQuestion
		}
		if kind == "media" {
			pending = job.pendingMediaApproval
		}
		waiting := job.status == "waiting" && companionPendingID(pending) == id
		jobContext, driver := job.ctx, job.agentDriver
		job.mu.Unlock()
		if !waiting {
			http.Error(w, "This request is no longer waiting.", http.StatusConflict)
			return
		}
		var permissionChange *buildPermissionChange
		defer func() { permissionChange.finish(false) }()
		if kind == "permission" && request.Response == "all" {
			if jobContext == nil || !companionPermissionOffered(pending, "all") || !companionPermissionOffered(pending, "once") ||
				!buildPermissionAllAvailable(jobContext, driver, root, request.SessionID, true) {
				http.Error(w, "This build does not offer all permissions for this build.", http.StatusBadRequest)
				return
			}
			var err error
			permissionChange, err = beginBuildPermissionAll(jobContext, driver, root, request.SessionID, id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			body, _ := json.Marshal(OpenCodePermissionRespondRequest{ProjectPath: root, SessionID: request.SessionID, PermissionID: id, Response: "once"})
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		collector := &companionWriter{header: make(http.Header), job: job}
		if kind == "permission" {
			buildPermissionReserve(jobContext, driver, root, request.SessionID, id)
		}
		ctx, cancel := context.WithCancel(r.Context())
		ctx = withBuildPermissionPolicy(ctx, job.permissionPolicy)
		defer cancel()
		if job.ctx != nil {
			stop := context.AfterFunc(job.ctx, cancel)
			defer stop()
		}
		next(&companionTapWriter{ResponseWriter: w, collector: collector}, r.WithContext(ctx))
		if collector.code >= 400 || ctx.Err() != nil {
			return
		}
		if kind == "permission" {
			buildPermissionConfirm(ctx, job.agentDriver, root, request.SessionID, id)
		}
		permissionChange.finish(true)
		job.mu.Lock()
		if job.responded == nil {
			job.responded = map[string]bool{}
		}
		job.responded[key] = true
		job.recordDecision(kind, id)
		job.mu.Unlock()
		job.acknowledge(kind, id)
	}
}
