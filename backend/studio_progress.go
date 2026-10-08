package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"
)

var studioGenerationID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var studioProviderURL = regexp.MustCompile(`https?://[^\s<>"']+`)
var studioGenerations = newStudioProgressStore()

type studioProgressState struct {
	ActualDurationSeconds float64             `json:"actualDurationSeconds,omitempty"`
	Found                 bool                `json:"found"`
	ID                    string              `json:"id,omitempty"`
	Stage                 string              `json:"stage,omitempty"`
	Kind                  string              `json:"kind,omitempty"`
	Prompt                string              `json:"prompt,omitempty"`
	AspectRatio           string              `json:"aspectRatio,omitempty"`
	DurationSeconds       int                 `json:"durationSeconds,omitempty"`
	ReferenceID           string              `json:"referenceId,omitempty"`
	ModelID               string              `json:"modelId,omitempty"`
	Resolution            string              `json:"resolution,omitempty"`
	Quality               string              `json:"quality,omitempty"`
	EstimatedCostUSD      float64             `json:"estimatedCostUSD,omitempty"`
	SourceID              string              `json:"sourceId,omitempty"`
	StartedAt             string              `json:"startedAt,omitempty"`
	Error                 string              `json:"error,omitempty"`
	CanResume             bool                `json:"canResume,omitempty"`
	ResumeStage           string              `json:"resumeStage,omitempty"`
	Asset                 *studioImageSummary `json:"asset,omitempty"`
	FirstFrame            *studioImageSummary `json:"firstFrame,omitempty"`
}

type studioProgressEntry struct {
	state       studioProgressState
	updated     time.Time
	active      bool
	durable     bool
	ctx         context.Context
	cancel      context.CancelFunc
	operationID string
	videoURL    string
	videoKey    string
}

type studioProgressStore struct {
	mu        sync.Mutex
	entries   map[string]studioProgressEntry
	cancelled map[string]time.Time
}

func newStudioProgressStore() *studioProgressStore {
	return &studioProgressStore{entries: make(map[string]studioProgressEntry), cancelled: make(map[string]time.Time)}
}

func (s *studioProgressStore) prune(now time.Time) {
	for id, entry := range s.entries {
		if !entry.active && now.Sub(entry.updated) > time.Hour {
			delete(s.entries, id)
		}
	}
	for id, when := range s.cancelled {
		if now.Sub(when) > time.Hour {
			delete(s.cancelled, id)
		}
	}
}

func studioStageTerminal(stage string) bool {
	return stage == "complete" || stage == "failed" || stage == "stopped"
}

// Private provider identifiers stay in the checkpoint and never enter status JSON.
func (s *studioProgressStore) save(entry studioProgressEntry) error {
	if !entry.durable {
		return nil
	}
	return saveStudioGenerationCheckpoint(studioGenerationCheckpoint{State: entry.state, OperationID: entry.operationID, VideoURL: entry.videoURL})
}

func (s *studioProgressStore) load(id string) (studioProgressEntry, bool, error) {
	if entry, found := s.entries[id]; found {
		return entry, true, nil
	}
	if !studioGenerationID.MatchString(id) {
		return studioProgressEntry{}, false, nil
	}
	checkpoint, found, err := loadStudioGenerationCheckpoint(id)
	if err != nil || !found {
		return studioProgressEntry{}, found, err
	}
	entry := studioProgressEntry{state: checkpoint.State, operationID: checkpoint.OperationID, videoURL: checkpoint.VideoURL, durable: true, updated: time.Now()}
	if entry.state.Asset == nil {
		if asset, err := findStudioAsset(studioGenerationAssetID(id)); err == nil && asset.MediaType == entry.state.Kind {
			summary := summarizeStudioRecord(asset)
			entry.state.Asset, entry.state.Stage = &summary, "complete"
			entry.state.Error, entry.state.CanResume, entry.state.ResumeStage = "", false, ""
			entry.videoURL = ""
			_ = s.save(entry)
		}
	}
	if !studioStageTerminal(entry.state.Stage) {
		entry.state.Stage = "stopped"
		entry.state.Error = "Studio restarted. Check this request to continue."
		entry.state.CanResume = entry.operationID != "" || entry.videoURL != "" || entry.state.Asset != nil
	}
	if len(s.entries) < 512 {
		s.entries[id] = entry
	}
	return entry, true, nil
}

func (s *studioProgressStore) read(id string) studioProgressState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	entry, found, err := s.load(id)
	if err != nil {
		return studioProgressState{Found: true, ID: id, Stage: "failed", Error: "Could not read the saved generation. Check Studio before generating again."}
	}
	if !found {
		return studioProgressState{}
	}
	return entry.state
}

func (s *studioProgressStore) begin(id, kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	if _, exists := s.cancelled[id]; exists {
		return http.StatusConflict
	}
	if _, found, err := s.load(id); err != nil {
		return http.StatusInternalServerError
	} else if found {
		return http.StatusConflict
	}
	if len(s.entries) >= 512 {
		return http.StatusTooManyRequests
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.entries[id] = studioProgressEntry{state: studioProgressState{Found: true, ID: id, Stage: "preparing", Kind: kind, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}, updated: time.Now(), active: true, ctx: ctx, cancel: cancel}
	return http.StatusOK
}

func (s *studioProgressStore) update(id, stage string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[id]
	if !exists || studioStageTerminal(entry.state.Stage) {
		return
	}
	entry.state.Stage, entry.updated = stage, time.Now()
	s.entries[id] = entry
	_ = s.save(entry)
}

func (s *studioProgressStore) edit(id string, change func(*studioProgressEntry)) error {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[id]
	if !found {
		return errors.New("Studio could not find this generation.")
	}
	change(&entry)
	entry.updated = time.Now()
	s.entries[id] = entry
	return s.save(entry)
}

func (s *studioProgressStore) stop(id string) (studioProgressState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	entry, found, err := s.load(id)
	if err != nil {
		return studioProgressState{}, err
	}
	if !found {
		if len(s.cancelled) >= 512 {
			return studioProgressState{}, errors.New("Studio is busy. Try again shortly.")
		}
		s.cancelled[id] = time.Now()
		return studioProgressState{}, nil
	}
	if entry.state.Stage == "complete" {
		return entry.state, nil
	}
	if entry.cancel != nil {
		entry.cancel()
	}
	entry.state.Stage, entry.state.Error = "stopped", "Stopped locally. The provider may still be working."
	entry.updated = time.Now()
	if _, cached := s.entries[id]; cached || len(s.entries) < 512 {
		s.entries[id] = entry
	}
	return entry.state, s.save(entry)
}

func (s *studioProgressStore) resume(id string) (studioProgress, studioProgressEntry, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	entry, found, err := s.load(id)
	if err != nil {
		return studioProgress{}, entry, http.StatusInternalServerError
	}
	if !found {
		return studioProgress{}, entry, http.StatusNotFound
	}
	if entry.state.Asset != nil {
		return studioProgress{}, entry, http.StatusOK
	}
	if entry.active {
		return studioProgress{}, entry, http.StatusConflict
	}
	if entry.state.Kind != "video" || !entry.state.CanResume {
		return studioProgress{}, entry, http.StatusUnprocessableEntity
	}
	if _, cached := s.entries[id]; !cached && len(s.entries) >= 512 {
		return studioProgress{}, entry, http.StatusTooManyRequests
	}
	entry.ctx, entry.cancel = context.WithCancel(context.Background())
	entry.active, entry.state.Error = true, ""
	entry.state.Stage = "generating"
	if entry.videoURL != "" {
		entry.state.Stage = "downloading"
	}
	entry.updated = time.Now()
	s.entries[id] = entry
	if err := s.save(entry); err != nil {
		entry.cancel()
		entry.active = false
		entry.state.Stage = "failed"
		s.entries[id] = entry
		return studioProgress{}, entry, http.StatusInternalServerError
	}
	return studioProgress{id: id, store: s}, entry, http.StatusOK
}

type studioProgress struct {
	id    string
	store *studioProgressStore
}

func (p studioProgress) stage(stage string) { p.store.update(p.id, stage) }
func (p studioProgress) finish() {
	_ = p.store.edit(p.id, func(entry *studioProgressEntry) {
		if !studioStageTerminal(entry.state.Stage) {
			entry.state.Stage = "failed"
		}
		if entry.state.Stage == "failed" && entry.state.Error == "" {
			entry.state.Error = "Generation could not finish. Your prompt is saved."
		}
		entry.active = false
		if entry.cancel != nil {
			entry.cancel()
		}
	})
}

func (p studioProgress) context(fallback context.Context) context.Context {
	if p.id == "" {
		return fallback
	}
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	return p.store.entries[p.id].ctx
}

func (p studioProgress) configure(prompt, aspect, referenceID, source string, duration int) error {
	return p.store.edit(p.id, func(entry *studioProgressEntry) {
		entry.durable = true
		entry.state.Prompt, entry.state.AspectRatio = prompt, aspect
		entry.state.ReferenceID, entry.state.SourceID = referenceID, source
		entry.state.DurationSeconds = duration
	})
}

func (p studioProgress) reference(id string, frame *studioImageSummary) error {
	return p.store.edit(p.id, func(entry *studioProgressEntry) { entry.state.ReferenceID = id; entry.state.FirstFrame = frame })
}

func (p studioProgress) operation(id string) error {
	return p.store.edit(p.id, func(entry *studioProgressEntry) {
		entry.operationID = id
		entry.state.CanResume = true
		entry.state.ResumeStage = "generating"
	})
}

func (p studioProgress) output(url string) error {
	return p.store.edit(p.id, func(entry *studioProgressEntry) {
		entry.videoURL = url
		entry.state.CanResume = true
		entry.state.ResumeStage = "downloading"
	})
}

func (p studioProgress) completed(asset studioImageSummary) error {
	return p.store.edit(p.id, func(entry *studioProgressEntry) {
		entry.state.Asset = &asset
		entry.state.Stage, entry.state.Error, entry.state.CanResume = "complete", "", false
		entry.state.ResumeStage = ""
		entry.videoURL = ""
	})
}

func (p studioProgress) failed(ctx context.Context, message string) {
	_ = p.store.edit(p.id, func(entry *studioProgressEntry) {
		if entry.state.Stage == "complete" {
			return
		}
		if errors.Is(ctx.Err(), context.Canceled) || entry.state.Stage == "stopped" {
			entry.state.Stage, entry.state.Error = "stopped", "Stopped locally. The provider may still be working."
		} else {
			entry.state.Stage, entry.state.Error = "failed", message
		}
	})
}

func beginStudioProgress(w http.ResponseWriter, r *http.Request, id, kind string) (studioProgress, bool) {
	progress := studioProgress{store: studioGenerations}
	if id == "" {
		return progress, true
	}
	if !studioGenerationID.MatchString(id) {
		http.Error(w, "Invalid generation identifier.", http.StatusBadRequest)
		return progress, false
	}
	if r.Context().Err() != nil {
		http.Error(w, "The request was stopped.", http.StatusRequestTimeout)
		return progress, false
	}
	// Optional tracking does not change manual development setups without auth.
	token := glowbomServerToken()
	if token == "" {
		return progress, true
	}
	if !hasValidGlowbomServerToken(r, token) || !isAllowedOrigin(r, glowbomAllowedOrigins()) {
		http.Error(w, "Local authentication is required.", http.StatusUnauthorized)
		return progress, false
	}
	if status := studioGenerations.begin(id, kind); status != http.StatusOK {
		if status == http.StatusConflict {
			http.Error(w, "This generation has already started. Check Studio before generating again.", status)
		} else if status == http.StatusTooManyRequests {
			http.Error(w, "Studio is busy. Try again shortly.", status)
		} else {
			http.Error(w, "Could not read the saved generation. No new generation was started.", status)
		}
		return progress, false
	}
	progress.id = id
	return progress, true
}

func studioGenerationStatusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !authorizeStudioGeneration(w, r, http.MethodGet) {
		return
	}
	id := r.URL.Query().Get("id")
	if !studioGenerationID.MatchString(id) {
		http.Error(w, "Invalid generation identifier.", http.StatusBadRequest)
		return
	}
	state := studioGenerations.read(id)
	if state.Found && state.Kind == "" && state.Error != "" {
		http.Error(w, state.Error, http.StatusInternalServerError)
		return
	}
	writeJSON(w, state)
}

func authorizeStudioGeneration(w http.ResponseWriter, r *http.Request, method string) bool {
	if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
		http.Error(w, "Local authentication is required.", http.StatusUnauthorized)
		return false
	}
	if !isAllowedOrigin(r, glowbomAllowedOrigins()) {
		http.Error(w, "This request is not allowed.", http.StatusForbidden)
		return false
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

type studioGenerationControl struct {
	GenerationID string `json:"generationId"`
	APIKey       string `json:"apiKey,omitempty"`
	UseSavedKey  bool   `json:"useSavedKey,omitempty"`
}

func studioGenerationControlRequest(w http.ResponseWriter, r *http.Request) (studioGenerationControl, bool) {
	if !authorizeStudioGeneration(w, r, http.MethodPost) {
		return studioGenerationControl{}, false
	}
	var request studioGenerationControl
	if json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&request) != nil || !studioGenerationID.MatchString(request.GenerationID) {
		http.Error(w, "Invalid generation identifier.", http.StatusBadRequest)
		return request, false
	}
	return request, true
}
func studioGenerationControlID(w http.ResponseWriter, r *http.Request) (string, bool) {
	request, ok := studioGenerationControlRequest(w, r)
	return request.GenerationID, ok
}

func studioGenerationCancelHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := studioGenerationControlID(w, r)
	if !ok {
		return
	}
	state, err := studioGenerations.stop(id)
	if err != nil {
		http.Error(w, "Could not save the stopped request. Check Studio before trying again.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, state)
}

func studioGenerationResumeHandler(w http.ResponseWriter, r *http.Request) {
	request, ok := studioGenerationControlRequest(w, r)
	id := request.GenerationID
	if !ok {
		return
	}
	progress, entry, status := studioGenerations.resume(id)
	if status != http.StatusOK {
		message := "Could not resume the saved generation."
		if status == http.StatusConflict {
			message = "This request is still running. Check its progress before trying again."
		}
		if status == http.StatusUnprocessableEntity {
			message = "This request cannot be resumed. No new generation was started."
		}
		http.Error(w, message, status)
		return
	}
	if entry.state.Asset != nil {
		writeStudioGenerationResult(w, entry.state)
		return
	}
	defer progress.finish()
	ctx := progress.context(r.Context())
	if request.APIKey != "" || request.UseSavedKey {
		options, keyErr := studioVideoOptionsFromState(entry.state)
		if keyErr == nil {
			entry.videoKey, keyErr = resolveStudioVideoKey(ctx, options.SourceID, request.APIKey, request.UseSavedKey)
		}
		if keyErr != nil {
			progress.failed(ctx, "Add the original provider API key to recover this video.")
			http.Error(w, keyErr.Error(), http.StatusBadRequest)
			return
		}
		_ = progress.store.edit(progress.id, func(cached *studioProgressEntry) { cached.videoKey = entry.videoKey })
	}
	result, err := resumeStudioVideo(ctx, progress, entry)
	if err != nil {
		progress.failed(ctx, "Could not recover this video. Your request is saved; you can check it again.")
		writeStudioProviderError(w, err, "Could not recover this video. Your request is saved; you can check it again.")
		return
	}
	writeStudioGenerationResult(w, result)
}

func writeStudioGenerationResult(w http.ResponseWriter, state studioProgressState) {
	key := "image"
	if state.Kind == "video" {
		key = "video"
	}
	response := map[string]any{key: state.Asset}
	if state.FirstFrame != nil {
		response["firstFrame"] = state.FirstFrame
	}
	writeJSON(w, response)
}

func (p studioProgress) imageOptions(options studioImageOptions) error {
	return p.store.edit(p.id, func(entry *studioProgressEntry) {
		entry.state.ModelID = options.ModelID
		entry.state.Resolution = options.Resolution
		entry.state.Quality = options.Quality
	})
}
