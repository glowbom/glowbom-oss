package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const companionDestinationPreferenceFile = "companion-project-destination.json"

var companionDestinationPreferenceMu sync.Mutex

func companionDestinationStudioRoot() (string, error) {
	root, err := studioRootDirectory()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

// Only an explicit native folder selection updates this preference. Pairing
// grants and requests stay in memory and are never restored from disk.
func rememberCompanionDestination(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 {
		return errors.New("invalid project destination")
	}
	companionDestinationPreferenceMu.Lock()
	defer companionDestinationPreferenceMu.Unlock()
	root, err := companionDestinationStudioRoot()
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]string{"parentPath": path})
	if err != nil {
		return err
	}
	return atomicChatFile(root, companionDestinationPreferenceFile, data)
}

func rememberedCompanionDestination() string {
	companionDestinationPreferenceMu.Lock()
	defer companionDestinationPreferenceMu.Unlock()
	root, err := studioRootDirectory()
	if err != nil {
		return ""
	}
	fields, _, err := readStudioJSON(filepath.Join(root, companionDestinationPreferenceFile), 32<<10)
	if err != nil {
		return ""
	}
	return studioJSONString(fields, "parentPath")
}

func writableCompanionDestination(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 {
		return nil, errors.New("invalid project destination")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, errors.New("project destination moved or changed")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() {
		return nil, errors.New("project destination is unavailable")
	}
	// A bounded, empty probe checks the actual directory permissions without
	// leaving a project behind or following a preexisting file or symlink.
	probe := ".glowbom-save-check-" + randomUUIDString()
	file, err := root.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	closeErr := file.Close()
	removeErr := root.Remove(probe)
	if closeErr != nil {
		return nil, closeErr
	}
	if removeErr != nil {
		return nil, removeErr
	}
	current, err := os.Stat(path)
	canonical, canonicalErr := filepath.EvalSymlinks(path)
	if err != nil || canonicalErr != nil || canonical != path || !os.SameFile(info, current) {
		return nil, errors.New("project destination moved or changed")
	}
	return info, nil
}

func managedCompanionDestination() (string, os.FileInfo, error) {
	studio, err := companionDestinationStudioRoot()
	if err != nil {
		return "", nil, err
	}
	root, err := os.OpenRoot(studio)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()
	if err := root.Mkdir("PhoneProjects", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", nil, err
	}
	info, err := root.Lstat("PhoneProjects")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("Desktop's project folder is unavailable")
	}
	path := filepath.Join(studio, "PhoneProjects")
	current, err := writableCompanionDestination(path)
	if err != nil || !os.SameFile(info, current) {
		return "", nil, errors.New("Desktop's project folder moved or changed")
	}
	return path, current, nil
}

func (s *companionSession) pruneImportDestinationsLocked() {
	for id, grant := range s.importDestinations {
		expires, err := time.Parse(time.RFC3339, grant.ExpiresAt)
		if err != nil || !s.now().Before(expires) {
			delete(s.importDestinations, id)
		}
	}
	for id, grantID := range s.destinationRequests {
		if _, exists := s.importDestinations[grantID]; !exists {
			delete(s.destinationRequests, id)
		}
	}
}

func (s *companionSession) defaultImportDestination(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RequestID string `json:"requestId"`
	}
	if !companionDecode(w, r, &request, 4096) {
		return
	}
	id := normalizedStudioUUID(request.RequestID)
	if id == "" {
		http.Error(w, "Send a valid save folder request ID.", http.StatusBadRequest)
		return
	}
	requestKey := "default/" + id
	ctx, cancel := s.chatContext(r.Context())
	defer cancel()
	s.mu.Lock()
	s.pruneImportDestinationsLocked()
	if grant, ok := s.importDestinations[s.destinationRequests[requestKey]]; ok && s.validImportDestination(grant) == nil {
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
	if !s.attachmentRequestActive(ctx) {
		http.Error(w, "This connection stopped before the save folder was ready.", http.StatusConflict)
		return
	}
	path := rememberedCompanionDestination()
	info, err := writableCompanionDestination(path)
	source := "recent"
	if err != nil {
		path, info, err = managedCompanionDestination()
		source = "managed"
	}
	if err != nil {
		http.Error(w, "Desktop could not prepare a save folder. Check its storage or choose another folder.", http.StatusServiceUnavailable)
		return
	}
	grant := companionImportDestination{ID: strings.ToLower(randomUUIDString()), DisplayPath: path,
		ExpiresAt: s.now().Add(companionDestinationLifetime).UTC().Format(time.RFC3339), Source: source, identity: info}
	s.mu.Lock()
	if !s.attachmentRequestActive(ctx) {
		s.mu.Unlock()
		http.Error(w, "This connection stopped before the save folder was ready.", http.StatusConflict)
		return
	}
	if existing, ok := s.importDestinations[s.destinationRequests[requestKey]]; ok && s.validImportDestination(existing) == nil {
		s.mu.Unlock()
		writeJSON(w, existing)
		return
	}
	if len(s.importDestinations) >= 32 {
		s.mu.Unlock()
		http.Error(w, "Pair again to choose more save folders.", http.StatusTooManyRequests)
		return
	}
	if s.importDestinations == nil {
		s.importDestinations = map[string]companionImportDestination{}
	}
	if s.destinationRequests == nil {
		s.destinationRequests = map[string]string{}
	}
	s.importDestinations[grant.ID], s.destinationRequests[requestKey] = grant, grant.ID
	s.mu.Unlock()
	writeJSON(w, grant)
}
