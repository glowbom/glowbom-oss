package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

const (
	companionImportHTMLBytes   = 12 << 20
	companionImportImageBytes  = 16 << 20
	companionImportVideoBytes  = 32 << 20
	companionImportBundleBytes = 64 << 20
)

var companionTransferMu sync.Mutex
var companionEmbeddedImage = regexp.MustCompile(`(?i)data:image/(png|jpeg|webp|gif);base64,[A-Za-z0-9+/]+={0,2}`)

type companionPrototypeImport struct {
	RequestID       string                        `json:"requestId"`
	Name            string                        `json:"name"`
	Prompt          string                        `json:"prompt"`
	HTML            string                        `json:"html"`
	AttachmentIDs   []string                      `json:"attachmentIds,omitempty"`
	SourceProjectID string                        `json:"sourceProjectId,omitempty"`
	SourceSnapshot  string                        `json:"sourceSnapshot,omitempty"`
	OriginalHTML    string                        `json:"originalHTML,omitempty"`
	Assets          []companionProjectImportAsset `json:"assets,omitempty"`
	DestinationID   string                        `json:"destinationId,omitempty"`
}

type companionMediaImport struct {
	RequestID  string `json:"requestId"`
	Filename   string `json:"filename,omitempty"`
	DataBase64 string `json:"dataBase64"`
	MimeType   string `json:"mimeType,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
}

type companionMediaImported struct {
	ID        string `json:"id"`
	MediaType string `json:"mediaType"`
	ProjectID string `json:"projectId,omitempty"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mimeType"`
	ByteCount int64  `json:"byteCount"`
}

type companionTransferRecord struct {
	Kind                string                          `json:"kind"`
	Hash                string                          `json:"hash"`
	Project             *companionProject               `json:"project,omitempty"`
	ProjectPath         string                          `json:"projectPath,omitempty"`
	Media               *companionMediaImported         `json:"media,omitempty"`
	ProjectAssets       []companionImportedAssetReceipt `json:"projectAssets,omitempty"`
	SuppliedAssetCount  int                             `json:"suppliedAssetCount,omitempty"`
	InputCount          int                             `json:"inputCount,omitempty"`
	SourceProjectID     string                          `json:"sourceProjectId,omitempty"`
	DestinationID       string                          `json:"destinationId,omitempty"`
	DestinationPath     string                          `json:"destinationPath,omitempty"`
	FullProjectTemplate bool                            `json:"fullProjectTemplate,omitempty"`
}

func (s *companionSession) routeTransfer(w http.ResponseWriter, r *http.Request, path string, parts []string) bool {
	if path == "/projects/import" && r.Method == http.MethodGet {
		capabilities := map[string]any{"version": 2, "projectAssets": true, "fullProjectTemplate": true, "maxAssets": 64, "maxBundleBytes": companionImportBundleBytes, "maxHTMLBytes": companionImportHTMLBytes, "maxImageBytes": companionImportImageBytes, "maxVideoBytes": companionImportVideoBytes, "destinationSelection": runtime.GOOS == "darwin" || s.pickImportFolder != nil, "defaultDestination": true}
		if studio, err := studioRootDirectory(); err == nil {
			capabilities["defaultParentPath"] = filepath.Join(canonicalDirectory(studio), "PhoneProjects")
		}
		writeJSON(w, capabilities)
		return true
	}
	if r.Method != http.MethodPost {
		return false
	}
	if path == "/projects/import" {
		s.importPrototype(w, r)
		return true
	}
	if path == "/projects/import/destination" {
		s.chooseImportDestination(w, r)
		return true
	}
	if path == "/projects/import/default-destination" {
		s.defaultImportDestination(w, r)
		return true
	}
	if path == "/studio/import" {
		s.importMedia(w, r, companionProject{})
		return true
	}
	if len(parts) == 4 && parts[0] == "projects" && parts[2] == "studio" && parts[3] == "import" {
		project, ok := s.project(parts[1])
		if !ok {
			http.NotFound(w, r)
			return true
		}
		s.importMedia(w, r, project)
		return true
	}
	return false
}

func (s *companionSession) beginTransfer(ctx context.Context, reserves ...int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	reserve := int64(companionImportVideoBytes)
	if len(reserves) > 0 {
		reserve = reserves[0]
	}
	if !s.attachmentRequestActive(ctx) || s.transfersActive >= 2 || s.transferCount >= 64 || s.transferredBytes+s.transferReservedBytes+reserve > 256<<20 {
		return false
	}
	s.transfersActive++
	s.transferReservedBytes += reserve
	return true
}

func (s *companionSession) finishTransfer(size int64, reserves ...int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transfersActive--
	reserve := int64(companionImportVideoBytes)
	if len(reserves) > 0 {
		reserve = reserves[0]
	}
	s.transferReservedBytes -= reserve
	if size > 0 {
		s.transferCount++
		s.transferredBytes += size
	}
}

func companionTransferDirectory() (string, error) {
	root, err := studioRootDirectory()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	if err = handle.Mkdir("PhoneImports", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := handle.Lstat("PhoneImports")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("The Desktop transfer folder is unavailable.")
	}
	return filepath.Join(root, "PhoneImports"), nil
}

func companionTransferState(id, kind, hash string) (string, companionTransferRecord, error) {
	directory, err := companionTransferDirectory()
	if err != nil {
		return "", companionTransferRecord{}, err
	}
	file := filepath.Join(directory, strings.ToLower(id)+".json")
	var record companionTransferRecord
	if _, data, err := readStudioJSON(file, 128<<10); err == nil {
		if json.Unmarshal(data, &record) != nil || record.Kind != kind || record.Hash != hash {
			return "", record, errors.New("This transfer ID was already used with different content. Review the transfer again.")
		}
		return directory, record, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", record, errors.New("Desktop could not recover this transfer. Review it again.")
	}
	record = companionTransferRecord{Kind: kind, Hash: hash}
	return directory, record, writeCompanionTransfer(directory, id, record)
}

func writeCompanionTransfer(directory, id string, record companionTransferRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return atomicChatFile(directory, strings.ToLower(id)+".json", data)
}

func (s *companionSession) shareImportedProject(project companionProject) {
	s.mu.Lock()
	s.projects[project.ID] = project
	s.mu.Unlock()
}

func (s *companionSession) importPrototype(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.chatContext(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	if !s.beginTransfer(ctx, companionImportBundleBytes) {
		http.Error(w, "Wait for the current transfer, or pair again to transfer more files.", http.StatusTooManyRequests)
		return
	}
	size := int64(0)
	defer func() { s.finishTransfer(size, companionImportBundleBytes) }()
	var request companionPrototypeImport
	if !companionDecode(w, r, &request, 128<<20) {
		return
	}
	id := normalizedStudioUUID(request.RequestID)
	if id == "" || strings.TrimSpace(request.Name) == "" || len(request.Name) > 160 || len(request.Prompt) > 8000 || len(request.HTML) > companionImportHTMLBytes || len(request.AttachmentIDs) > 4 {
		http.Error(w, "Send a transfer ID, a short project name and a complete prototype no larger than 12 MB.", 400)
		return
	}
	html := strings.TrimSpace(request.HTML)
	lower := strings.ToLower(html)
	if !strings.HasPrefix(lower, "<!doctype html>") || !strings.HasSuffix(lower, "</html>") {
		http.Error(w, "Save a complete HTML prototype before transferring.", 400)
		return
	}
	request.RequestID = id
	if request.DestinationID != "" {
		if normalizedStudioUUID(request.DestinationID) == "" {
			http.Error(w, "Choose a valid save folder on Desktop.", 400)
			return
		}
		request.DestinationID = strings.ToLower(request.DestinationID)
	}
	prepared, totalBytes, err := prepareCompanionImportAssets(request)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	encoded, _ := json.Marshal(request)
	hash := chatSourceHash(string(encoded))
	companionTransferMu.Lock()
	defer companionTransferMu.Unlock()
	directory, record, err := companionTransferState(id, "project", hash)
	if err != nil {
		http.Error(w, "This transfer could not be recovered, or its ID was used with different content. Review the transfer again.", 409)
		return
	}
	if record.Project != nil {
		project := *record.Project
		project.path = record.ProjectPath
		expected := filepath.Join(filepath.Dir(directory), "PhoneProjects", strings.ToLower(id))
		if record.DestinationID != request.DestinationID {
			http.Error(w, "This transfer is already saved in another folder. Review it again.", 409)
			return
		}
		if record.DestinationID != "" {
			if !filepath.IsAbs(record.DestinationPath) || filepath.Clean(record.DestinationPath) != record.DestinationPath {
				http.Error(w, "The saved transfer folder could not be verified.", 409)
				return
			}
			expected = filepath.Join(record.DestinationPath, companionChosenProjectFolder(request.Name, id))
		}
		canonical, rootErr := chatProjectRoot(project.path)
		if rootErr != nil || canonical != project.path || project.path != expected || project.ID != companionProjectID(expected) {
			code := http.StatusGone
			if record.DestinationID != "" {
				code = http.StatusConflict
			}
			http.Error(w, "The transferred Desktop project is no longer available.", code)
			return
		}
		if err = verifyCompanionImportedAssets(project.path, record.ProjectAssets); err != nil {
			http.Error(w, "The saved project media was removed or changed. Its current content was kept.", 409)
			return
		}
		registered, err := registerStudioProject(project.path)
		if err != nil {
			http.Error(w, "The transferred project media could not be opened.", 409)
			return
		}
		if len(record.ProjectAssets) == 0 {
			fields, _, readErr := readStudioProjectState(project.path)
			if readErr != nil {
				http.Error(w, "The transferred project media could not be checked.", 409)
				return
			}
			var repaired []companionImportedAssetReceipt
			_ = json.Unmarshal(fields["phoneImportedAssets"], &repaired)
			if err = verifyCompanionImportedAssets(project.path, repaired); err != nil {
				http.Error(w, "The saved project media was removed or changed. Its current content was kept.", 409)
				return
			}
		}
		project.AssetCount = registered.AssetCount
		project = companionProjectMetadata(project)
		s.shareImportedProject(project)
		writeCompanionProjectImportReceipt(w, project, record)
		return
	}
	localProject := companionProject{}
	if len(request.AttachmentIDs) > 0 {
		localProject, err = s.localChatProject(ctx)
		if err != nil {
			http.Error(w, "Pair again and attach the references before transferring.", 409)
			return
		}
	}
	parts, err := s.chatImages(ctx, localProject, request.AttachmentIDs)
	if err != nil {
		http.Error(w, errCompanionAttachmentUnavailable.Error(), 410)
		return
	}
	html, embedded, err := extractCompanionEmbeddedImages(html)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	prepared, html, totalBytes, err = completeCompanionImportAssets(request, prepared, html, embedded, parts, totalBytes)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if len(html) > maxChatResultBytes {
		http.Error(w, "The prototype code is larger than 2 MB after saving its embedded images. Shorten the prototype before transferring.", 400)
		return
	}
	parent := filepath.Join(filepath.Dir(directory), "PhoneProjects")
	root := filepath.Join(parent, strings.ToLower(id))
	parentPath := filepath.Dir(directory)
	child := filepath.Join("PhoneProjects", strings.ToLower(id))
	var parentIdentity os.FileInfo
	if request.DestinationID != "" {
		parent, parentIdentity, err = s.importDestinationParent(request.DestinationID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusGone)
			return
		}
		parentPath = parent
		child = companionChosenProjectFolder(request.Name, id)
		root = filepath.Join(parent, child)
	}
	parentRoot, err := os.OpenRoot(parentPath)
	if err != nil {
		code := http.StatusInternalServerError
		if request.DestinationID != "" {
			code = http.StatusGone
		}
		http.Error(w, "Desktop could not open its project folder.", code)
		return
	}
	defer parentRoot.Close()
	if parentIdentity != nil {
		current, identityErr := parentRoot.Stat(".")
		if identityErr != nil || !os.SameFile(current, parentIdentity) {
			http.Error(w, "The selected save folder changed. Choose it again on Desktop.", http.StatusGone)
			return
		}
	}
	if request.DestinationID == "" {
		if err = parentRoot.Mkdir("PhoneProjects", 0700); err != nil && !errors.Is(err, os.ErrExist) {
			http.Error(w, "Desktop could not prepare its project folder.", 500)
			return
		}
		info, err := parentRoot.Lstat("PhoneProjects")
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			http.Error(w, "Desktop's project folder is unavailable.", 500)
			return
		}
	}
	if !s.attachmentRequestActive(ctx) {
		http.Error(w, "This connection stopped before the project was saved.", 409)
		return
	}
	projectParent, err := parentRoot.OpenRoot(filepath.Dir(child))
	if err != nil {
		http.Error(w, "Desktop could not open its project folder.", 500)
		return
	}
	defer projectParent.Close()
	if _, err = projectParent.Lstat(filepath.Base(child)); !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "This transfer folder already exists. Review the transfer again.", 409)
		return
	}
	runTemplate := s.importTemplateCLI
	if runTemplate == nil {
		runTemplate = runAccountCLI
	}
	starter, err := loadStarterProject(ctx, request.Name, runTemplate)
	if err != nil {
		http.Error(w, err.Error()+" Your phone copy is still available.", http.StatusServiceUnavailable)
		return
	}
	starter, err = prepareCompanionStarterProject(starter, request, prepared)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.attachmentRequestActive(ctx) {
		http.Error(w, "This connection stopped before the project was saved.", 409)
		return
	}
	if request.DestinationID != "" {
		if _, _, err = s.importDestinationParent(request.DestinationID); err != nil {
			http.Error(w, err.Error(), http.StatusGone)
			return
		}
	}
	var installedRoot os.FileInfo
	var installedFiles []starterCreatedPath
	if err = installStarterProject(ctx, projectParent, filepath.Base(child), starter, func(info os.FileInfo, files []starterCreatedPath) {
		installedRoot, installedFiles = info, files
	}); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			cleanupCompanionStarter(projectParent, filepath.Base(child), installedRoot, installedFiles)
		}
	}()
	prototypeRecord := ""
	messages := []chatMessage{}
	if strings.TrimSpace(request.Prompt) != "" {
		messages = append(messages, chatMessage{Role: "user", Text: request.Prompt})
	}
	err = saveChatPrototype(root, html, "", messages, "", parts, &prototypeRecord)
	if err == nil {
		err = saveSharedChatHistory(root, messages)
	}
	if err == nil {
		err = saveCompanionImportSources(root, prototypeRecord, request, prepared)
	}
	if err == nil {
		err = initializeCompanionImportedBook(root)
	}
	if err == nil && !s.attachmentRequestActive(ctx) {
		err = context.Canceled
	}
	var registered studioProjectSummary
	if err == nil {
		err = saveCompanionImportStudioAssets(ctx, root, id, prepared, &record)
	}
	if err == nil {
		registered, err = registerStudioProject(root)
	}
	if err == nil && !s.attachmentRequestActive(ctx) {
		err = context.Canceled
	}
	if err != nil {
		http.Error(w, "Desktop could not save this prototype. Your phone copy is still available.", 500)
		return
	}
	project := companionProject{ID: companionProjectID(root), Name: request.Name, AssetCount: registered.AssetCount, Available: true, path: root}
	project = companionProjectMetadata(project)
	record.Project, record.ProjectPath = &project, root
	record.FullProjectTemplate = true
	record.DestinationID = request.DestinationID
	if request.DestinationID != "" {
		record.DestinationPath = parent
	}
	record.SuppliedAssetCount = len(request.Assets)
	record.SourceProjectID = request.SourceProjectID
	for _, asset := range request.Assets {
		if companionImportInputRole(asset.Role) {
			record.InputCount++
		}
	}
	if err = writeCompanionTransfer(directory, id, record); err != nil {
		http.Error(w, "Desktop could not finish saving the transfer. Try again with the same transfer ID.", 500)
		return
	}
	accepted = true
	size = totalBytes
	s.shareImportedProject(project)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeCompanionProjectImportReceipt(w, project, record)
}

func initializeCompanionImportedBook(path string) error {
	projectBookMu.Lock()
	defer projectBookMu.Unlock()
	root, name, err := openProjectBookProject(path)
	if err != nil {
		return err
	}
	defer root.Close()
	book, exists, err := readProjectBook(root, name)
	if err != nil {
		return err
	}
	if !exists {
		if err = initializeProjectBook(root); err != nil {
			return err
		}
	}
	return syncProjectBook(root, &book)
}

func extractCompanionEmbeddedImages(html string) (string, map[string][]byte, error) {
	images := map[string][]byte{}
	var failure error
	result := companionEmbeddedImage.ReplaceAllStringFunc(html, func(uri string) string {
		if failure != nil {
			return uri
		}
		prefix, payload, _ := strings.Cut(uri, ",")
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			failure = errors.New("An embedded image could not be read. Save the prototype again before transferring.")
			return uri
		}
		mime, _, _, err := companionImportMediaType(data)
		if err != nil || strings.ToLower(prefix) != "data:"+mime+";base64" {
			failure = errors.New("An embedded prototype image is unsupported or too large.")
			return uri
		}
		name := "phone-inline-" + chatSourceHash(string(data))[:24] + companionMediaExtension(mime)
		images[name] = data
		if len(images) > 64 {
			failure = errors.New("Transfer a prototype with no more than 64 embedded images.")
			return uri
		}
		return "assets/" + name
	})
	return result, images, failure
}

func companionMediaExtension(mime string) string {
	return map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif", "video/mp4": ".mp4", "video/quicktime": ".mov"}[mime]
}

func (s *companionSession) importMedia(w http.ResponseWriter, r *http.Request, project companionProject) {
	ctx, cancel := s.chatContext(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	if !s.beginTransfer(ctx) {
		http.Error(w, "Wait for the current transfer, or pair again to transfer more files.", 429)
		return
	}
	size := int64(0)
	defer func() { s.finishTransfer(size) }()
	var request companionMediaImport
	if !companionDecode(w, r, &request, 45<<20) {
		return
	}
	id := normalizedStudioUUID(request.RequestID)
	if id == "" || len(request.Filename) > 160 || len(request.Prompt) > 8000 || len(request.DataBase64) > base64.StdEncoding.EncodedLen(companionImportVideoBytes) {
		http.Error(w, "Choose a supported image or video with a short filename and a transfer ID.", 400)
		return
	}
	data, err := base64.StdEncoding.DecodeString(request.DataBase64)
	if err != nil {
		http.Error(w, "This media could not be read.", 400)
		return
	}
	mime, media, dimensions, err := companionImportMediaType(data)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if request.MimeType != "" && request.MimeType != mime {
		http.Error(w, "The media type does not match this file.", 400)
		return
	}
	filename := request.Filename
	if filename == "" {
		filename = "phone-media" + companionMediaExtension(mime)
	}
	if filename != filepath.Base(filename) || sanitizeAttachmentFilename(filename) != filename || strings.ContainsAny(filename, "\\\x00") || !strings.EqualFold(filepath.Ext(filename), companionMediaExtension(mime)) {
		http.Error(w, "Choose a short filename with the correct media extension.", 400)
		return
	}
	request.RequestID, request.Filename, request.MimeType = id, filename, mime
	encoded, _ := json.Marshal(struct {
		Request   companionMediaImport
		ProjectID string
	}{request, project.ID})
	hash := chatSourceHash(string(encoded))
	companionTransferMu.Lock()
	defer companionTransferMu.Unlock()
	directory, record, err := companionTransferState(id, "media", hash)
	if err != nil {
		http.Error(w, "This transfer ID was already used with different content. Review the transfer again.", 409)
		return
	}
	if record.Media != nil {
		asset, err := readStudioAssetForLink(record.Media.ID)
		if err != nil {
			http.Error(w, "The transferred media is no longer available on Desktop.", http.StatusGone)
			return
		}
		if asset.DataBase64 != base64.StdEncoding.EncodeToString(data) || asset.MediaType != media {
			http.Error(w, "The transferred media changed on Desktop. Its current content was kept.", http.StatusConflict)
			return
		}
		writeJSON(w, record.Media)
		return
	}
	var registered studioProjectSummary
	if project.ID != "" {
		registered, err = registerStudioProject(project.path)
		if err != nil {
			http.Error(w, "This Desktop project is unavailable.", 400)
			return
		}
	}
	if !s.attachmentRequestActive(ctx) {
		http.Error(w, "This connection stopped before the media was saved.", 409)
		return
	}
	assetID := studioGenerationAssetID(id)
	asset, existingErr := readStudioAssetForLink(assetID)
	if existingErr == nil {
		if asset.DataBase64 != base64.StdEncoding.EncodeToString(data) || asset.MediaType != media {
			http.Error(w, "The saved media has changed. Review this transfer again.", 409)
			return
		}
	} else {
		asset, err = saveStudioAsset(studioSaveOptions{GenerationID: id, Prompt: request.Prompt, DataURI: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), MediaType: media, MimeType: mime, Source: "Glowbom iPhone", AssetType: "reference", SourceType: "uploaded", SourceProjectID: registered.ID, UsedInProjects: []string{registered.ID}, Dimensions: dimensions})
		if err != nil {
			http.Error(w, "Desktop could not save this media. Your phone copy is still available.", 500)
			return
		}
	}
	if project.ID != "" {
		if err = linkCompanionImportedMedia(project, asset, data, mime); err != nil {
			http.Error(w, "The media is saved in Studio, but could not be linked to the project. Retry this transfer.", 500)
			return
		}
	}
	result := companionMediaImported{ID: asset.ID, MediaType: media, ProjectID: project.ID, Filename: filename, MimeType: mime, ByteCount: int64(len(data))}
	record.Media = &result
	if err = writeCompanionTransfer(directory, id, record); err != nil {
		http.Error(w, "The media is saved. Retry with the same transfer ID to recover it.", 500)
		return
	}
	size = int64(len(data))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	writeJSON(w, result)
}

func linkCompanionImportedMedia(project companionProject, asset studioImageRecord, data []byte, mime string) error {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	assets, err := chatWriteDirectory(project.path, "prototype/assets")
	if err != nil {
		return err
	}
	filename := "phone-studio-" + strings.ToLower(asset.ID) + companionMediaExtension(mime)
	path := filepath.Join(assets, filename)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err = createChatImageFile(assets, filename, data); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	handle, err := os.OpenRoot(project.path)
	if err != nil {
		return err
	}
	defer handle.Close()
	relative := filepath.Join("prototype/assets", filename)
	current, err := bookRead(handle, relative, companionImportVideoBytes)
	if err != nil || sha256.Sum256(current) != sha256.Sum256(data) {
		return errors.New("The copied project media changed.")
	}
	fields, previous, err := readStudioProjectState(project.path)
	if err != nil {
		return err
	}
	links := map[string]string{}
	if raw := fields["assets"]; len(raw) > 0 {
		if json.Unmarshal(raw, &links) != nil {
			return errors.New("The project media links could not be read.")
		}
	}
	if links == nil {
		links = map[string]string{}
	}
	links[filepath.ToSlash(relative)] = asset.ID
	fields["assets"], _ = json.Marshal(links)
	return saveStudioProjectState(project.path, fields, previous)
}

func companionImportMediaType(data []byte) (string, string, *studioDimensions, error) {
	if len(data) == 0 {
		return "", "", nil, errors.New("Choose an image or video to transfer.")
	}
	mime := http.DetectContentType(data)
	if len(data) >= 16 && string(data[4:8]) == "ftyp" {
		if len(data) > companionImportVideoBytes || !validCompanionVideo(data) {
			return "", "", nil, errors.New("Choose a complete MP4 or MOV video no larger than 32 MB.")
		}
		mime = "video/mp4"
		if string(data[8:12]) == "qt  " {
			mime = "video/quicktime"
		}
		return mime, "video", nil, nil
	}
	if len(data) > companionImportImageBytes {
		return "", "", nil, errors.New("Choose an original image no larger than 16 MB.")
	}
	width, height := 0, 0
	if mime == "image/webp" {
		width, height = companionWebPDimensions(data)
	} else if mime == "image/png" || mime == "image/jpeg" || mime == "image/gif" {
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err == nil {
			width, height = config.Width, config.Height
		}
		if width > 0 && height > 0 && width <= 8192 && height <= 8192 && int64(width)*int64(height) <= 20_000_000 {
			if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
				width = 0
			}
		}
	}
	if width < 1 || height < 1 || width > 8192 || height > 8192 || int64(width)*int64(height) > 20_000_000 {
		return "", "", nil, errors.New("Choose a complete PNG, JPEG, WebP or GIF image no larger than 20 megapixels.")
	}
	return mime, "image", &studioDimensions{Width: width, Height: height}, nil
}

func validCompanionVideo(data []byte) bool {
	moov, mdat := false, false
	for offset := 0; offset < len(data); {
		if len(data)-offset < 8 {
			return false
		}
		size := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		header := 8
		if size == 1 {
			if len(data)-offset < 16 {
				return false
			}
			size = binary.BigEndian.Uint64(data[offset+8 : offset+16])
			header = 16
		}
		if size == 0 {
			size = uint64(len(data) - offset)
		}
		if size < uint64(header) || size > uint64(len(data)-offset) {
			return false
		}
		switch string(data[offset+4 : offset+8]) {
		case "moov":
			moov = size > uint64(header)
		case "mdat":
			mdat = size > uint64(header)
		}
		offset += int(size)
	}
	return moov && mdat
}

func companionWebPDimensions(data []byte) (int, int) {
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return 0, 0
	}
	width, height := 0, 0
	pixels := false
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return 0, 0
		}
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		if size > len(data)-start {
			return 0, 0
		}
		chunk := data[start : start+size]
		switch string(data[offset : offset+4]) {
		case "VP8X":
			if len(chunk) >= 10 {
				width = 1 + int(chunk[4]) + int(chunk[5])<<8 + int(chunk[6])<<16
				height = 1 + int(chunk[7]) + int(chunk[8])<<8 + int(chunk[9])<<16
			}
		case "VP8 ":
			if len(chunk) > 10 && bytes.Equal(chunk[3:6], []byte{0x9d, 0x01, 0x2a}) {
				pixels = true
				width = int(binary.LittleEndian.Uint16(chunk[6:8]) & 0x3fff)
				height = int(binary.LittleEndian.Uint16(chunk[8:10]) & 0x3fff)
			}
		case "VP8L":
			if len(chunk) > 5 && chunk[0] == 0x2f {
				pixels = true
				bits := binary.LittleEndian.Uint32(chunk[1:5])
				width = int(bits&0x3fff) + 1
				height = int((bits>>14)&0x3fff) + 1
			}
		}
		offset = start + size + (size & 1)
		if offset > len(data) {
			return 0, 0
		}
	}
	if !pixels {
		return 0, 0
	}
	return width, height
}
