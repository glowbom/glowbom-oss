package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type studioClipState struct {
	Found    bool                `json:"found"`
	ID       string              `json:"id,omitempty"`
	AssetID  string              `json:"assetId,omitempty"`
	Stage    string              `json:"stage,omitempty"`
	Progress float64             `json:"progress,omitempty"`
	Error    string              `json:"error,omitempty"`
	Asset    *studioImageSummary `json:"asset,omitempty"`
}

type studioClipJob struct {
	State     studioClipState     `json:"state"`
	Selection studioClipSelection `json:"selection"`
	cancel    context.CancelFunc
	updated   time.Time
}

type studioClipPlayback struct {
	assetID string
	expires time.Time
}

type studioClipCached struct {
	path    string
	record  studioAssetMetadata
	media   studioClipMediaInfo
	stamp   time.Time
	size    int64
	refs    int
	updated time.Time
}

type studioClipService struct {
	mu        sync.Mutex
	jobs      map[string]*studioClipJob
	active    string
	tickets   map[string]studioClipPlayback
	cacheMu   sync.Mutex
	cache     map[string]*studioClipCached
	prepare   chan struct{}
	lifetime  context.Context
	cancel    context.CancelFunc
	closed    bool
	closeDone chan struct{}
	workers   sync.WaitGroup
	readers   sync.WaitGroup
	cacheDir  string
	tools     func() (string, string, error)
	probe     func(context.Context, string, string) (studioClipMediaInfo, error)
	render    func(context.Context, string, string, string, string, studioClipSelection, func(float64)) (studioClipMediaInfo, error)
}

var studioClips = newStudioClipService()

func newStudioClipService() *studioClipService {
	ctx, cancel := context.WithCancel(context.Background())
	return &studioClipService{jobs: make(map[string]*studioClipJob), tickets: make(map[string]studioClipPlayback), cache: make(map[string]*studioClipCached), prepare: make(chan struct{}, 1), lifetime: ctx, cancel: cancel, closeDone: make(chan struct{}), tools: studioClipTools, probe: probeStudioClip, render: renderStudioClip}
}

func (s *studioClipService) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		select {
		case <-s.closeDone:
		case <-time.After(3 * time.Second):
		}
		return
	}
	s.closed = true
	s.cancel()
	for _, job := range s.jobs {
		if job.cancel != nil {
			job.cancel()
		}
	}
	s.mu.Unlock()
	go func() {
		s.workers.Wait()
		s.readers.Wait()
		s.cacheMu.Lock()
		defer s.cacheMu.Unlock()
		if s.cacheDir != "" {
			_ = os.RemoveAll(s.cacheDir)
		}
		close(s.closeDone)
	}()
	select {
	case <-s.closeDone:
	case <-time.After(3 * time.Second):
	}
}

// Each backend owns one disposable cache folder. Recover old crash leftovers.
func (s *studioClipService) cacheDirectory() (string, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheDir != "" {
		return s.cacheDir, nil
	}
	root, err := studioClipPrivateDirectory("ClipCache")
	if err != nil {
		return "", err
	}
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".session-") {
				continue
			}
			if info, err := entry.Info(); err == nil && info.IsDir() && time.Since(info.ModTime()) > 24*time.Hour {
				_ = os.RemoveAll(filepath.Join(root, entry.Name()))
			}
		}
	}
	s.cacheDir, err = os.MkdirTemp(root, ".session-")
	return s.cacheDir, err
}

func studioClipPrivateDirectory(name string) (string, error) {
	root, err := studioRootDirectory()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("The Studio folder is unavailable.")
	}
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("The Studio editing folder is unavailable.")
	}
	return dir, os.Chmod(dir, 0700)
}

func (s *studioClipService) saveJob(job *studioClipJob) error {
	dir, err := studioClipPrivateDirectory("ClipEdits")
	if err != nil {
		return err
	}
	data, err := json.Marshal(job)
	if err != nil || len(data) > 16<<10 {
		return errors.New("The saved clip edit is too large.")
	}
	return atomicChatFile(dir, job.State.ID+".json", data)
}

func (s *studioClipService) loadJob(id string) (*studioClipJob, error) {
	if job := s.jobs[id]; job != nil {
		return job, nil
	}
	// Recovery reads media on disk without holding up Stop or render progress.
	s.mu.Unlock()
	job, err := s.readJob(id)
	s.mu.Lock()
	if existing := s.jobs[id]; existing != nil {
		return existing, nil
	}
	if err != nil || job == nil {
		return job, err
	}
	if !s.closed && len(s.jobs) < 64 {
		s.jobs[id] = job
	}
	return job, nil
}

func (s *studioClipService) readJob(id string) (*studioClipJob, error) {
	dir, err := studioClipPrivateDirectory("ClipEdits")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return nil, errors.New("The saved clip edit is unavailable.")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("The saved clip edit changed while reading.")
	}
	var job studioClipJob
	if err := json.NewDecoder(io.LimitReader(file, (16<<10)+1)).Decode(&job); err != nil || job.State.ID != id || !job.State.Found {
		return nil, errors.New("The saved clip edit is invalid.")
	}
	if job.State.Asset == nil {
		if path, err := studioClipRecordPath(studioGenerationAssetID("clip_" + id)); err == nil {
			if record, err := readStudioClipEnvelope(s.lifetime, path, nil); err == nil && record.MediaType == "video" {
				asset := summarizeStudioMetadata(record)
				job.State.Asset, job.State.Stage, job.State.Error, job.State.Progress = &asset, "complete", "", 1
			}
		}
	}
	if !studioStageTerminal(job.State.Stage) {
		job.State.Stage, job.State.Error = "stopped", "Studio restarted during this edit. Your original clip and edit settings are kept."
	}
	job.updated = time.Now()
	return &job, nil
}

func (s *studioClipService) prune() {
	for id, job := range s.jobs {
		if id != s.active && time.Since(job.updated) > time.Hour {
			delete(s.jobs, id)
		}
	}
	for len(s.jobs) >= 64 {
		oldestID := ""
		var oldest time.Time
		for id, job := range s.jobs {
			if id != s.active && studioStageTerminal(job.State.Stage) && (oldestID == "" || job.updated.Before(oldest)) {
				oldestID, oldest = id, job.updated
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.jobs, oldestID)
	}
	for ticket, playback := range s.tickets {
		if time.Now().After(playback.expires) {
			delete(s.tickets, ticket)
		}
	}
}

func (s *studioClipService) state(id string) (studioClipState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	job, err := s.loadJob(id)
	if job == nil || err != nil {
		return studioClipState{}, err
	}
	return job.State, nil
}

func (s *studioClipService) update(id, stage, message string, progress float64, asset *studioImageSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil || (studioStageTerminal(job.State.Stage) && stage != "complete") {
		return
	}
	persist := stage != job.State.Stage || time.Since(job.updated) > time.Second || asset != nil
	job.State.Stage, job.State.Error = stage, message
	job.State.Progress = math.Max(job.State.Progress, math.Min(1, math.Max(0, progress)))
	if asset != nil {
		job.State.Asset = asset
	}
	if persist {
		job.updated = time.Now()
		_ = s.saveJob(job)
	}
}

func (s *studioClipService) start(id, assetID string, selection studioClipSelection) (studioClipState, int, error) {
	ffmpeg, ffprobe, err := s.tools()
	if err != nil {
		return studioClipState{}, http.StatusPreconditionFailed, err
	}
	s.mu.Lock()
	s.prune()
	if s.closed {
		s.mu.Unlock()
		return studioClipState{}, http.StatusServiceUnavailable, errors.New("Studio is shutting down. Reopen it before saving this clip.")
	}
	if job, err := s.loadJob(id); err != nil || job != nil {
		s.mu.Unlock()
		return studioClipState{}, http.StatusConflict, errors.New("This edit has already started. Check its result before saving another clip.")
	}
	if s.closed {
		s.mu.Unlock()
		return studioClipState{}, http.StatusServiceUnavailable, errors.New("Studio is shutting down.")
	}
	if s.active != "" || len(s.jobs) >= 64 {
		s.mu.Unlock()
		return studioClipState{}, http.StatusConflict, errors.New("Another clip is being saved. Wait for it to finish or stop it before starting this edit.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	job := &studioClipJob{State: studioClipState{Found: true, ID: id, AssetID: assetID, Stage: "preparing"}, Selection: selection, updated: time.Now(), cancel: cancel}
	if err := s.saveJob(job); err != nil {
		cancel()
		s.mu.Unlock()
		return studioClipState{}, http.StatusInternalServerError, errors.New("Could not save your edit settings. No rendering was started.")
	}
	s.jobs[id], s.active = job, id
	s.workers.Add(1)
	state := job.State
	s.mu.Unlock()
	go s.run(ctx, id, assetID, ffmpeg, ffprobe, selection)
	return state, http.StatusAccepted, nil
}

func (s *studioClipService) run(ctx context.Context, id, assetID, ffmpeg, ffprobe string, selection studioClipSelection) {
	defer s.workers.Done()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if job := s.jobs[id]; job != nil {
			job.cancel()
			job.cancel = nil
			job.updated = time.Now()
		}
		if s.active == id {
			s.active = ""
		}
	}()
	fail := func(message string) {
		stage := "failed"
		if errors.Is(ctx.Err(), context.Canceled) {
			stage, message = "stopped", "Stopped. Your original clip and edit settings are kept."
		}
		s.update(id, stage, message, 0, nil)
	}
	cached, release, err := s.source(ctx, assetID, ffprobe)
	if err != nil {
		fail("Could not open this video for editing. Check that the original clip is still in Studio.")
		return
	}
	defer release()
	if err := validateStudioClipSelection(selection, cached.media); err != nil {
		fail(err.Error())
		return
	}
	dir, err := s.cacheDirectory()
	if err != nil {
		fail("Could not prepare the local render folder.")
		return
	}
	work, err := os.MkdirTemp(dir, ".render-")
	if err != nil {
		fail("Could not prepare this edit. Check available disk space.")
		return
	}
	defer os.RemoveAll(work)
	output := filepath.Join(work, "edited.mp4")
	s.update(id, "rendering", "", .05, nil)
	media, err := s.render(ctx, ffmpeg, ffprobe, cached.path, output, selection, func(value float64) { s.update(id, "rendering", "", .05+.85*value, nil) })
	if err != nil {
		fail("Could not finish this edit. Your original clip and settings are kept. Try saving again.")
		return
	}
	s.update(id, "saving", "", .95, nil)
	_, err = saveStudioClipFileWithCommit(ctx, id, cached.record, output, media, selection.Mode, func(asset studioImageSummary, publish func() error) error {
		// Stop and publishing use the same lock, so the response reflects the winner.
		s.mu.Lock()
		defer s.mu.Unlock()
		job := s.jobs[id]
		if job == nil || ctx.Err() != nil || job.State.Stage == "stopped" {
			return context.Canceled
		}
		if err := publish(); err != nil {
			return err
		}
		job.State.Stage, job.State.Error, job.State.Progress, job.State.Asset = "complete", "", 1, &asset
		job.updated = time.Now()
		_ = s.saveJob(job)
		return nil
	})
	if err != nil {
		fail("Could not save the edited clip. Check available disk space, then try again.")
		return
	}
}

func (s *studioClipService) stop(id string) (studioClipState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	job, err := s.loadJob(id)
	if err != nil {
		return studioClipState{}, err
	}
	if job == nil {
		if len(s.jobs) >= 64 {
			return studioClipState{}, errors.New("Studio is busy. Try again shortly.")
		}
		job = &studioClipJob{State: studioClipState{Found: true, ID: id, Stage: "stopped"}, updated: time.Now()}
		s.jobs[id] = job
	}
	if job.State.Stage == "complete" {
		return job.State, nil
	}
	if job.cancel != nil {
		job.cancel()
	}
	job.State.Stage, job.State.Error = "stopped", "Stopped. Your original clip and edit settings are kept."
	job.updated = time.Now()
	return job.State, s.saveJob(job)
}

func (s *studioClipService) source(ctx context.Context, id, ffprobe string) (*studioClipCached, func(), error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, context.Canceled
	}
	s.readers.Add(1)
	s.mu.Unlock()
	defer s.readers.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	select {
	case s.prepare <- struct{}{}:
		defer func() { <-s.prepare }()
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	path, err := studioClipRecordPath(id)
	if err != nil {
		return nil, nil, err
	}
	stamp, err := os.Lstat(path)
	if err != nil || !stamp.Mode().IsRegular() || stamp.Size() > studioClipRecordLimit {
		return nil, nil, errors.New("The clip is unavailable or too large.")
	}
	s.cacheMu.Lock()
	cached := s.cache[id]
	if cached != nil && (!cached.stamp.Equal(stamp.ModTime()) || cached.size != stamp.Size()) {
		if cached.refs > 0 {
			s.cacheMu.Unlock()
			return nil, nil, errors.New("This clip changed while it was being used.")
		}
		_ = os.Remove(cached.path)
		delete(s.cache, id)
		cached = nil
	}
	if cached == nil {
		for len(s.cache) >= 2 {
			var oldestID string
			var oldest *studioClipCached
			for key, candidate := range s.cache {
				if candidate.refs == 0 && (oldest == nil || candidate.updated.Before(oldest.updated)) {
					oldestID, oldest = key, candidate
				}
			}
			if oldest == nil {
				s.cacheMu.Unlock()
				return nil, nil, errors.New("Another clip is being read. Try again shortly.")
			}
			_ = os.Remove(oldest.path)
			delete(s.cache, oldestID)
		}
	}
	s.cacheMu.Unlock()
	if cached == nil {
		dir, err := s.cacheDirectory()
		if err != nil {
			return nil, nil, err
		}
		file, err := os.CreateTemp(dir, ".source-*.mp4")
		if err != nil {
			return nil, nil, err
		}
		record, readErr := readStudioClipEnvelope(ctx, path, file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || record.MediaType != "video" || !strings.EqualFold(record.ID, id) {
			_ = os.Remove(file.Name())
			return nil, nil, errors.New("The Studio video is invalid or too large.")
		}
		media, err := s.probe(ctx, ffprobe, file.Name())
		if err != nil {
			_ = os.Remove(file.Name())
			return nil, nil, err
		}
		cached = &studioClipCached{path: file.Name(), record: record, media: media, stamp: stamp.ModTime(), size: stamp.Size()}
		s.cacheMu.Lock()
		s.cache[id] = cached
		s.cacheMu.Unlock()
	}
	s.cacheMu.Lock()
	cached.refs++
	cached.updated = time.Now()
	s.cacheMu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { s.cacheMu.Lock(); cached.refs--; s.cacheMu.Unlock() }) }
	return cached, release, nil
}

func authorizeStudioClip(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !isAllowedOrigin(r, glowbomAllowedOrigins()) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "This request is not allowed.", http.StatusForbidden)
		return false
	}
	if token := glowbomServerToken(); token != "" && !hasValidGlowbomServerToken(r, token) {
		http.Error(w, "Local authentication is required.", http.StatusUnauthorized)
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	return true
}

func studioClipCapabilitiesHandler(w http.ResponseWriter, r *http.Request) {
	if authorizeStudioClip(w, r, http.MethodGet) {
		writeStudioClipCapabilities(w)
	}
}

func writeStudioClipCapabilities(w http.ResponseWriter) {
	_, _, err := studioClips.tools()
	message := ""
	if err != nil {
		message = err.Error()
		if len(studioClipToolDownloads()) == 0 {
			message = "Install FFmpeg and ffprobe on this computer, then choose Check again."
		}
	}
	writeJSON(w, map[string]any{"available": err == nil, "setupAvailable": len(studioClipToolDownloads()) > 0, "message": message, "maxSelectionSeconds": 10, "maxSourceBytes": studioClipSourceLimit})
}

func studioClipSetupHandler(w http.ResponseWriter, r *http.Request) {
	if !authorizeStudioClip(w, r, http.MethodPost) {
		return
	}
	if err := installStudioClipTools(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeStudioClipCapabilities(w)
}

func studioClipProbeHandler(w http.ResponseWriter, r *http.Request) {
	if !authorizeStudioClip(w, r, http.MethodGet) {
		return
	}
	id := normalizedStudioUUID(r.URL.Query().Get("id"))
	_, ffprobe, err := studioClips.tools()
	if id == "" || err != nil {
		http.Error(w, "The video tools or clip are unavailable.", http.StatusPreconditionFailed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	source, release, err := studioClips.source(ctx, id, ffprobe)
	if err != nil {
		http.Error(w, "Could not inspect this video. Check that it is still saved in Studio.", http.StatusBadRequest)
		return
	}
	defer release()
	writeJSON(w, source.media)
}

func studioClipRenderHandler(w http.ResponseWriter, r *http.Request) {
	if !authorizeStudioClip(w, r, http.MethodPost) {
		return
	}
	var request struct {
		ID      string `json:"id"`
		AssetID string `json:"assetId"`
		studioClipSelection
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&request); err != nil || normalizedStudioUUID(request.ID) == "" || normalizedStudioUUID(request.AssetID) == "" {
		http.Error(w, "Choose a valid clip and edit.", http.StatusBadRequest)
		return
	}
	if err := validateStudioClipSelection(request.studioClipSelection, studioClipMediaInfo{DurationSeconds: 600}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Context().Err() != nil {
		http.Error(w, "The edit was stopped before it started.", http.StatusRequestTimeout)
		return
	}
	state, status, err := studioClips.start(strings.ToLower(normalizedStudioUUID(request.ID)), normalizedStudioUUID(request.AssetID), request.studioClipSelection)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, state)
}

func studioClipStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !authorizeStudioClip(w, r, http.MethodGet) {
		return
	}
	id := strings.ToLower(normalizedStudioUUID(r.URL.Query().Get("id")))
	if normalizedStudioUUID(id) == "" {
		http.Error(w, "Choose a valid saved edit.", http.StatusBadRequest)
		return
	}
	state, err := studioClips.state(id)
	if err != nil {
		http.Error(w, "Could not read the saved clip edit.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, state)
}

func studioClipCancelHandler(w http.ResponseWriter, r *http.Request) {
	if !authorizeStudioClip(w, r, http.MethodPost) {
		return
	}
	var request struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil || normalizedStudioUUID(request.ID) == "" {
		http.Error(w, "Choose a valid edit to stop.", http.StatusBadRequest)
		return
	}
	state, err := studioClips.stop(strings.ToLower(normalizedStudioUUID(request.ID)))
	if err != nil {
		http.Error(w, "Could not confirm that this edit stopped. Check its status before saving again.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, state)
}

func studioClipPlaybackHandler(w http.ResponseWriter, r *http.Request) {
	if !authorizeStudioClip(w, r, http.MethodPost) {
		return
	}
	var request struct {
		AssetID string `json:"assetId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil || normalizedStudioUUID(request.AssetID) == "" {
		http.Error(w, "Choose a valid video.", http.StatusBadRequest)
		return
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		http.Error(w, "Could not prepare video playback.", http.StatusInternalServerError)
		return
	}
	ticket, expires := hex.EncodeToString(random[:]), time.Now().Add(15*time.Minute)
	studioClips.mu.Lock()
	studioClips.prune()
	if len(studioClips.tickets) >= 128 {
		studioClips.mu.Unlock()
		http.Error(w, "Too many videos are open. Try again shortly.", http.StatusTooManyRequests)
		return
	}
	studioClips.tickets[ticket] = studioClipPlayback{assetID: normalizedStudioUUID(request.AssetID), expires: expires}
	studioClips.mu.Unlock()
	writeJSON(w, map[string]any{"url": "/api/studio/clips/source?ticket=" + ticket, "expiresAt": expires.UTC().Format(time.RFC3339)})
}

func studioClipPlaybackAllowed(r *http.Request) bool {
	if r.URL.Path != "/studio/clips/source" || (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	studioClips.mu.Lock()
	defer studioClips.mu.Unlock()
	playback, found := studioClips.tickets[r.URL.Query().Get("ticket")]
	return found && time.Now().Before(playback.expires)
}

func studioClipSourceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !isAllowedOrigin(r, glowbomAllowedOrigins()) || !studioClipPlaybackAllowed(r) {
		http.Error(w, "Video playback has expired. Reopen the clip to continue.", http.StatusUnauthorized)
		return
	}
	studioClips.mu.Lock()
	playback := studioClips.tickets[r.URL.Query().Get("ticket")]
	studioClips.mu.Unlock()
	_, ffprobe, err := studioClips.tools()
	if err != nil {
		http.Error(w, "The local video tools are unavailable.", http.StatusPreconditionFailed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	source, release, err := studioClips.source(ctx, playback.assetID, ffprobe)
	if err != nil {
		http.Error(w, "The saved video is unavailable.", http.StatusNotFound)
		return
	}
	defer release()
	file, err := os.Open(source.path)
	if err != nil {
		http.Error(w, "The saved video is unavailable.", http.StatusNotFound)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, "clip.mp4", source.stamp, file)
}
