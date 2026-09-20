package main

import (
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
)

// Uploads live only for this backend session. Builds copy them into project history.
func instructionUploadHandler(directory string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 40*1024*1024+1024*1024)
		if err := r.ParseMultipartForm(1024 * 1024); err != nil {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
			http.Error(w, "Choose files totaling no more than 40MB.", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		files := r.MultipartForm.File["files"]
		if len(files) == 0 || len(files) > 20 {
			http.Error(w, "Choose between 1 and 20 files.", http.StatusBadRequest)
			return
		}
		var total int64
		for _, file := range files {
			total += file.Size
		}
		if total > 40*1024*1024 {
			http.Error(w, "Choose files totaling no more than 40MB.", http.StatusBadRequest)
			return
		}
		batch, err := os.MkdirTemp(directory, "attachment-")
		if err != nil {
			http.Error(w, "Could not save attachments.", http.StatusInternalServerError)
			return
		}
		success := false
		defer func() {
			if !success {
				_ = os.RemoveAll(batch)
			}
		}()
		result := []openCodeInstructionPickedFile{}
		for _, file := range files {
			source, err := file.Open()
			if err != nil {
				http.Error(w, "Could not read attachment.", http.StatusBadRequest)
				return
			}
			destination, err := os.CreateTemp(batch, "file-*"+filepath.Ext(file.Filename))
			if err != nil {
				source.Close()
				http.Error(w, "Could not save attachment.", http.StatusInternalServerError)
				return
			}
			size, copyErr := io.Copy(destination, source)
			source.Close()
			closeErr := destination.Close()
			if copyErr != nil || closeErr != nil {
				http.Error(w, "Could not save attachment.", http.StatusInternalServerError)
				return
			}
			result = append(result, openCodeInstructionPickedFile{Path: destination.Name(), Name: filepath.Base(file.Filename), SizeBytes: size, MimeType: mime.TypeByExtension(filepath.Ext(file.Filename))})
		}
		success = true
		writeJSON(w, openCodeInstructionFilesPickResponse{Success: true, Files: result, Source: "upload"})
	}
}
