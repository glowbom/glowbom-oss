package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	companionAttachmentMaxBytes   = 2 << 20
	companionAttachmentMaxPixels  = 4_000_000
	companionAttachmentMaxEdge    = 4096
	companionAttachmentBuildCount = 4
	companionAttachmentLimit      = 32
	companionAttachmentTotalBytes = 64 << 20
)

var errCompanionAttachmentUnavailable = errors.New("These images are no longer available for this connection. Attach them again.")

type companionImageAttachment struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mimeType"`
	ByteCount int64  `json:"byteCount"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type companionAttachment struct {
	companionImageAttachment
	relativePath string
	digest       [32]byte
}

func companionAttachmentImage(data []byte) (image.Config, string, error) {
	if len(data) == 0 || len(data) > companionAttachmentMaxBytes {
		return image.Config{}, "", errors.New("Choose an image no larger than 2 MB.")
	}
	mime := http.DetectContentType(data)
	if mime != "image/jpeg" && mime != "image/png" {
		return image.Config{}, "", errors.New("Choose a JPEG or PNG image.")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return image.Config{}, "", errors.New("This image could not be read. Choose another image.")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > companionAttachmentMaxEdge || config.Height > companionAttachmentMaxEdge || int64(config.Width)*int64(config.Height) > companionAttachmentMaxPixels {
		return image.Config{}, "", errors.New("Choose an image no larger than 4 megapixels.")
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return image.Config{}, "", errors.New("This image could not be read. Choose another image.")
	}
	return config, mime, nil
}

func (s *companionSession) attachmentRequestActive(ctx context.Context) bool {
	now := s.now()
	return ctx.Err() == nil && s.ctx.Err() == nil && now.Before(s.expires)
}

func (s *companionSession) uploadAttachment(w http.ResponseWriter, r *http.Request, project companionProject) {
	s.mu.Lock()
	if !s.attachmentRequestActive(r.Context()) || len(s.attachments)+s.uploadsActive >= companionAttachmentLimit || s.uploadsActive >= companionAttachmentBuildCount || s.uploadBytes+int64(s.uploadsActive+1)*companionAttachmentMaxBytes > companionAttachmentTotalBytes {
		s.mu.Unlock()
		http.Error(w, "Pair again to add more images, or wait for the current upload to finish.", http.StatusTooManyRequests)
		return
	}
	s.uploadsActive++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.uploadsActive--
		s.mu.Unlock()
	}()
	var input struct {
		Filename   string `json:"filename,omitempty"`
		DataBase64 string `json:"dataBase64"`
	}
	if !companionDecode(w, r, &input, 3<<20) {
		return
	}
	if len(input.Filename) > 160 || len(input.DataBase64) > base64.StdEncoding.EncodedLen(companionAttachmentMaxBytes) {
		http.Error(w, "Choose an image no larger than 2 MB with a short filename.", http.StatusBadRequest)
		return
	}
	data, err := base64.StdEncoding.DecodeString(input.DataBase64)
	if err != nil {
		http.Error(w, "This image could not be read. Choose another image.", http.StatusBadRequest)
		return
	}
	config, mime, err := companionAttachmentImage(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.attachmentRequestActive(r.Context()) {
		http.Error(w, "This connection stopped before the image was saved. Attach it again.", http.StatusConflict)
		return
	}
	filename := sanitizeAttachmentFilename(filepath.Base(strings.ReplaceAll(input.Filename, "\\", "/")))
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	if stem == "" || stem == "." {
		stem = "reference"
	}
	extension := ".jpg"
	if mime == "image/png" {
		extension = ".png"
	}
	filename = stem + extension
	attachment := companionAttachment{companionImageAttachment: companionImageAttachment{
		ID: strings.ToLower(randomUUIDString()), ProjectID: project.ID, Filename: filename, MimeType: mime,
		ByteCount: int64(len(data)), Width: config.Width, Height: config.Height}, digest: sha256.Sum256(data)}
	root, err := os.OpenRoot(project.path)
	if err != nil {
		http.Error(w, "Desktop could not save this project image.", http.StatusBadRequest)
		return
	}
	defer root.Close()
	for _, directory := range []string{".glowbom", ".glowbom/attachments"} {
		if err := root.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			http.Error(w, "Desktop could not save this project image.", http.StatusBadRequest)
			return
		}
	}
	directory := filepath.Join(".glowbom", "attachments", attachment.ID)
	if err := root.Mkdir(directory, 0700); err != nil {
		http.Error(w, "Desktop could not save this project image.", http.StatusBadRequest)
		return
	}
	attachment.relativePath = filepath.Join(directory, filename)
	accepted := false
	defer func() {
		if !accepted {
			_ = root.Remove(attachment.relativePath)
			_ = root.Remove(directory)
		}
	}()
	file, err := root.OpenFile(attachment.relativePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		http.Error(w, "Desktop could not save this project image.", http.StatusBadRequest)
		return
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		http.Error(w, "Desktop could not save this project image.", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	if !s.attachmentRequestActive(r.Context()) {
		s.mu.Unlock()
		http.Error(w, "This connection stopped before the image was saved. Attach it again.", http.StatusConflict)
		return
	}
	if s.attachments == nil {
		s.attachments = map[string]companionAttachment{}
	}
	s.attachments[attachment.ID] = attachment
	s.uploadBytes += attachment.ByteCount
	accepted = true
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, attachment.companionImageAttachment)
}

func (s *companionSession) buildAttachments(ctx context.Context, project companionProject, ids []string) ([]string, []companionImageAttachment, error) {
	if len(ids) > companionAttachmentBuildCount {
		return nil, nil, errors.New("Choose no more than four images for this build.")
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	selected := []companionAttachment{}
	seen := map[string]bool{}
	s.mu.Lock()
	for _, id := range ids {
		attachment, ok := s.attachments[id]
		if !ok || attachment.ProjectID != project.ID {
			s.mu.Unlock()
			return nil, nil, errCompanionAttachmentUnavailable
		}
		if !seen[id] {
			selected = append(selected, attachment)
			seen[id] = true
		}
	}
	s.mu.Unlock()
	root, err := os.OpenRoot(project.path)
	if err != nil {
		return nil, nil, errCompanionAttachmentUnavailable
	}
	defer root.Close()
	paths := []string{}
	attachments := []companionImageAttachment{}
	for _, attachment := range selected {
		file, err := root.Open(attachment.relativePath)
		if err != nil {
			return nil, nil, errCompanionAttachmentUnavailable
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != attachment.ByteCount || info.Size() > companionAttachmentMaxBytes {
			_ = file.Close()
			return nil, nil, errCompanionAttachmentUnavailable
		}
		data, readErr := io.ReadAll(io.LimitReader(file, companionAttachmentMaxBytes+1))
		_ = file.Close()
		if readErr != nil || sha256.Sum256(data) != attachment.digest {
			return nil, nil, errCompanionAttachmentUnavailable
		}
		config, mime, imageErr := companionAttachmentImage(data)
		if imageErr != nil || mime != attachment.MimeType || config.Width != attachment.Width || config.Height != attachment.Height {
			return nil, nil, errCompanionAttachmentUnavailable
		}
		path, resolveErr := filepath.EvalSymlinks(filepath.Join(project.path, attachment.relativePath))
		if resolveErr != nil || !isPathWithin(project.path, path) || !s.attachmentRequestActive(ctx) {
			return nil, nil, errCompanionAttachmentUnavailable
		}
		paths = append(paths, path)
		attachments = append(attachments, attachment.companionImageAttachment)
	}
	return paths, attachments, nil
}
