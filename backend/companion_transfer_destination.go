package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const companionDestinationLifetime = 30 * time.Minute

var companionDestinationPickerMu sync.Mutex

type companionImportDestination struct {
	Canceled    bool   `json:"canceled"`
	ID          string `json:"id"`
	DisplayPath string `json:"displayPath"`
	ExpiresAt   string `json:"expiresAt"`
	Source      string `json:"source,omitempty"`
	identity    os.FileInfo
}

func (s *companionSession) chooseImportDestination(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RequestID string `json:"requestId"`
	}
	if !companionDecode(w, r, &request, 4096) {
		return
	}
	id := normalizedStudioUUID(request.RequestID)
	if id == "" {
		http.Error(w, "Send a valid folder selection ID.", 400)
		return
	}
	if runtime.GOOS != "darwin" && s.pickImportFolder == nil {
		http.Error(w, "Choosing a save folder from the phone is available on Mac Desktop.", http.StatusNotImplemented)
		return
	}
	ctx, cancel := s.chatContext(r.Context())
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	s.mu.Lock()
	s.pruneImportDestinationsLocked()
	if grant, ok := s.importDestinations[s.destinationRequests[id]]; ok && s.validImportDestination(grant) == nil {
		s.mu.Unlock()
		writeJSON(w, grant)
		return
	}
	if len(s.importDestinations) >= 32 {
		s.mu.Unlock()
		http.Error(w, "Pair again to choose more save folders.", http.StatusTooManyRequests)
		return
	}
	s.mu.Unlock()
	if !companionDestinationPickerMu.TryLock() {
		http.Error(w, "A folder chooser is already open on Desktop.", 409)
		return
	}
	defer companionDestinationPickerMu.Unlock()
	picker := s.pickImportFolder
	if picker == nil {
		picker = func(ctx context.Context) (string, bool, error) {
			home, _ := os.UserHomeDir()
			return pickProjectFolderMacOSWithContext(ctx, home, folderPickerSavePrompt, true)
		}
	}
	path, canceled, err := picker(ctx)
	if errors.Is(err, errNativeProjectPickerBusy) {
		http.Error(w, "A folder chooser is already open on Desktop.", 409)
		return
	}
	if ctx.Err() != nil || !s.attachmentRequestActive(ctx) {
		http.Error(w, "The folder selection stopped. Pair again if needed.", 409)
		return
	}
	if canceled {
		writeJSON(w, map[string]any{"canceled": true})
		return
	}
	if err != nil {
		http.Error(w, "Desktop could not open its folder chooser. Choose a folder on Desktop and try again.", 503)
		return
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(canonical) || len(canonical) > 4096 {
		http.Error(w, "The selected save folder is unavailable.", 409)
		return
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		http.Error(w, "Choose an existing save folder on Desktop.", 409)
		return
	}
	grant := companionImportDestination{ID: strings.ToLower(randomUUIDString()), DisplayPath: canonical, ExpiresAt: s.now().Add(companionDestinationLifetime).UTC().Format(time.RFC3339), identity: info}
	s.mu.Lock()
	if !s.attachmentRequestActive(ctx) {
		s.mu.Unlock()
		http.Error(w, "This connection stopped before the folder was selected.", 409)
		return
	}
	if s.importDestinations == nil {
		s.importDestinations = map[string]companionImportDestination{}
		s.destinationRequests = map[string]string{}
	}
	s.importDestinations[grant.ID], s.destinationRequests[id] = grant, grant.ID
	s.mu.Unlock()
	// The selected grant remains usable if Desktop cannot remember the preference.
	_ = rememberCompanionDestination(canonical)
	writeJSON(w, grant)
}

func (s *companionSession) validImportDestination(grant companionImportDestination) error {
	expires, err := time.Parse(time.RFC3339, grant.ExpiresAt)
	if err != nil || !s.now().Before(expires) || s.ctx.Err() != nil || grant.identity == nil {
		return errors.New("This save folder choice expired. Review it again.")
	}
	canonical, err := filepath.EvalSymlinks(grant.DisplayPath)
	if err != nil || canonical != grant.DisplayPath {
		return errors.New("The selected save folder moved or changed.")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() || !os.SameFile(info, grant.identity) {
		return errors.New("The selected save folder moved or changed.")
	}
	return nil
}

func (s *companionSession) importDestinationParent(id string) (string, os.FileInfo, error) {
	s.mu.Lock()
	grant, ok := s.importDestinations[id]
	s.mu.Unlock()
	if !ok {
		return "", nil, errors.New("Review the save folder again before sending.")
	}
	if err := s.validImportDestination(grant); err != nil {
		return "", nil, err
	}
	return grant.DisplayPath, grant.identity, nil
}

func companionChosenProjectFolder(name, id string) string {
	name = sanitizeAttachmentFilename(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." {
		name = "Glowbom project"
	}
	if len(name) > 80 {
		name = companionPublicText(name, 80)
	}
	return name + "-" + strings.ToLower(id)[:8]
}
