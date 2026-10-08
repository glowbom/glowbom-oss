package main

import (
	"encoding/json"
	"net/http"
)

// The phone can read Books only for projects selected during pairing.
func (s *companionSession) routeBook(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if r.Method != http.MethodGet || len(parts) < 3 || parts[0] != "projects" || parts[2] != "book" {
		return false
	}
	if len(parts) != 3 && (len(parts) != 5 || parts[3] != "media" || !projectBookID.MatchString(parts[4])) {
		return false
	}
	project, ok := s.project(parts[1])
	if !ok {
		http.NotFound(w, r)
		return true
	}
	projectBookMu.Lock()
	defer projectBookMu.Unlock()
	root, _, err := openProjectBookProject(project.path)
	if err != nil {
		http.Error(w, "Desktop could not open this project's Book.", http.StatusConflict)
		return true
	}
	defer root.Close()
	book, _, err := readProjectBook(root, project.Name)
	if err != nil {
		http.Error(w, "Desktop could not read this Book. Check its files on your computer.", http.StatusConflict)
		return true
	}
	if len(parts) == 5 {
		for _, entry := range book.Entries {
			for _, image := range entry.Images {
				if image.ID != parts[4] || !projectBookAsset.MatchString(image.Path) {
					continue
				}
				data, err := bookRead(root, "project-book/"+image.Path, projectBookImageLimit)
				if err != nil {
					http.NotFound(w, r)
					return true
				}
				mime, _, err := validateBookImage(data)
				if err != nil {
					http.Error(w, "This Book image could not be read.", http.StatusBadRequest)
					return true
				}
				w.Header().Set("Content-Type", mime)
				w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
				_, _ = w.Write(data)
				return true
			}
		}
		http.NotFound(w, r)
		return true
	}
	book.ProjectName = companionPublicText(project.Name, 160)
	if len(book.Entries) > 80 {
		book.Entries = book.Entries[:80]
		book.Warnings = append(book.Warnings, "The phone shows the 80 most recent entries. Open Desktop to read older entries.")
	}
	for i := range book.Entries {
		entry := &book.Entries[i]
		if len(entry.Body) > 32<<10 {
			book.Warnings = append(book.Warnings, "A long story is shown as an excerpt. Open Desktop to read it in full.")
		}
		entry.Title = companionPublicRunText(entry.Title, 300)
		entry.Body = companionPublicRunText(entry.Body, 32<<10)
		entry.Request = companionPublicRunText(entry.Request, 8000)
		entry.Model = companionPublicText(entry.Model, 160)
		entry.StoryModel = companionPublicText(entry.StoryModel, 160)
		entry.Contributor = companionPublicText(entry.Contributor, 160)
		entry.AutoStoryHash = ""
	}
	data, err := json.Marshal(book)
	for err == nil && len(data) > 4<<20 && len(book.Entries) > 1 {
		book.Entries = book.Entries[:len(book.Entries)-1]
		if len(book.Warnings) == 0 || book.Warnings[len(book.Warnings)-1] != "Some older entries are available on Desktop only." {
			book.Warnings = append(book.Warnings, "Some older entries are available on Desktop only.")
		}
		data, err = json.Marshal(book)
	}
	if err != nil || len(data) > 4<<20 {
		http.Error(w, "This Book is too large to read on the phone. Open it on Desktop.", http.StatusRequestEntityTooLarge)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
	return true
}
