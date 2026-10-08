package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const projectBookFormat = "glowbom-project-book"
const projectBookImageLimit = 8 << 20
const projectBookTextLimit = 256 << 10
const projectBookSketchLimit = 2 << 20
const projectBookEntryLimit = 1000

var projectBookMu sync.Mutex
var projectBookID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,95}$`)
var projectBookAsset = regexp.MustCompile(`^assets/[a-f0-9]{64}\.(png|jpg|webp)$`)
var errProjectBookFormat = errors.New("This project has an existing Book in another format. Its files were left unchanged.")
var errProjectBookConflict = errors.New("This entry changed in another window. Refresh the Book before saving your edit.")

type bookImage struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Caption string `json:"caption"`
	Name    string `json:"name"`
	Role    string `json:"role"`
}
type bookSketchNode struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}
type bookSketchEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type bookSketch struct {
	Version int              `json:"version"`
	Title   string           `json:"title"`
	Caption string           `json:"caption"`
	Nodes   []bookSketchNode `json:"nodes"`
	Edges   []bookSketchEdge `json:"edges"`
	Visual  *bookVisual      `json:"visual,omitempty"`
}
type bookVisual struct {
	Description   string         `json:"description"`
	Model         string         `json:"model"`
	CreatedAt     string         `json:"createdAt"`
	SourceVersion string         `json:"sourceVersion,omitempty"`
	Document      sketchDocument `json:"document"`
}
type bookStoryGeneration struct {
	Model         string `json:"model"`
	CreatedAt     string `json:"createdAt"`
	SourceVersion string `json:"sourceVersion"`
}

func setBookStoryGeneration(entry *bookEntry, model, sourceVersion string) {
	entry.StoryModel = model
	entry.StoryGeneration = &bookStoryGeneration{Model: model, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), SourceVersion: sourceVersion}
}

type bookEntry struct {
	ID              string               `json:"id"`
	Title           string               `json:"title"`
	Body            string               `json:"body"`
	CreatedAt       string               `json:"createdAt"`
	UpdatedAt       string               `json:"updatedAt"`
	Kind            string               `json:"kind"`
	Status          string               `json:"status"`
	Request         string               `json:"request"`
	Model           string               `json:"model,omitempty"`
	StoryModel      string               `json:"storyModel,omitempty"`
	StoryGeneration *bookStoryGeneration `json:"storyGeneration,omitempty"`
	Contributor     string               `json:"contributor,omitempty"`
	Source          string               `json:"source"`
	RunID           string               `json:"runId,omitempty"`
	ContentVersion  string               `json:"contentVersion,omitempty"`
	ChangedFiles    []string             `json:"changedFiles"`
	Images          []bookImage          `json:"images"`
	Sketch          bookSketch           `json:"sketch"`
	Reviewed        bool                 `json:"reviewed,omitempty"`
	AutoStoryHash   string               `json:"autoStoryHash,omitempty"`
	CanWriteStory   bool                 `json:"canWriteStory,omitempty"`
}
type projectBook struct {
	Version     int         `json:"version"`
	ProjectName string      `json:"projectName"`
	UpdatedAt   string      `json:"updatedAt"`
	Entries     []bookEntry `json:"entries"`
	Warnings    []string    `json:"warnings,omitempty"`
	skippedIDs  []string
}
type projectBookRequest struct {
	ProjectPath            string               `json:"projectPath"`
	Action                 string               `json:"action"`
	EntryID                string               `json:"entryId"`
	Title                  string               `json:"title"`
	Body                   string               `json:"body"`
	StoryModel             *string              `json:"storyModel,omitempty"`
	StoryGeneration        *bookStoryGeneration `json:"storyGeneration,omitempty"`
	Images                 *[]bookImage         `json:"images,omitempty"`
	Filename               string               `json:"filename"`
	DataURL                string               `json:"dataURL"`
	Caption                string               `json:"caption"`
	ImageID                string               `json:"imageId"`
	ExpectedUpdatedAt      string               `json:"expectedUpdatedAt,omitempty"`
	ExpectedContentVersion string               `json:"expectedContentVersion,omitempty"`
}

func projectBookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req projectBookRequest
	if r.Method == http.MethodGet {
		req.ProjectPath = r.URL.Query().Get("path")
	} else {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20))
		if decoder.Decode(&req) != nil {
			http.Error(w, "Invalid Project Book request.", 400)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			http.Error(w, "Send one Project Book request.", 400)
			return
		}
		if req.Action != "sync" && req.Action != "update-entry" && req.Action != "add-image" && req.Action != "remove-image" {
			http.Error(w, "Unknown Project Book action.", 400)
			return
		}
	}
	projectBookMu.Lock()
	defer projectBookMu.Unlock()
	root, name, err := openProjectBookProject(req.ProjectPath)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer root.Close()
	book, exists, err := readProjectBook(root, name)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if r.Method == http.MethodPost {
		if req.Action == "sync" {
			if !exists {
				err = initializeProjectBook(root)
			}
			if err == nil {
				err = syncProjectBook(root, &book)
			}
		} else if !exists {
			err = errors.New("Create the Project Book before editing it.")
		} else {
			err = editProjectBook(root, &book, req)
		}
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errProjectBookConflict) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
	}
	setBookStoryEligibility(&book)
	writeJSON(w, book)
}

func openProjectBookProject(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) {
		return nil, "", errors.New("Choose an existing Glowbom project folder.")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", errors.New("Could not open the project folder.")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, "", errors.New("Could not open the project folder.")
	}
	data, err := bookRead(root, "glowbom.json", 2<<20)
	var manifest struct {
		Name string `json:"name"`
	}
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		root.Close()
		return nil, "", errors.New("Open a Glowbom project first.")
	}
	name := strings.TrimSpace(manifest.Name)
	if name == "" {
		name = filepath.Base(resolved)
	}
	return root, name, nil
}

// All reads and writes stay beneath an open project descriptor. Reject links
// even when they point inside the project, so Book paths keep one meaning.
func bookNoLinks(root *os.Root, path string) error {
	if path == "" || !filepath.IsLocal(path) || strings.Contains(path, `\`) {
		return errors.New("Invalid Project Book path.")
	}
	current := ""
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("Invalid Project Book path.")
		}
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Project Book files must not be symbolic links.")
		}
	}
	return nil
}
func bookRead(root *os.Root, path string, limit int64) ([]byte, error) {
	if err := bookNoLinks(root, path); err != nil {
		return nil, err
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("Project Book data must be a regular file.")
	}
	if info.Size() > limit {
		return nil, errors.New("A Project Book file is too large.")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("A Project Book file is too large.")
	}
	return data, err
}
func bookDirs(root *os.Root, path string, limit int) ([]os.DirEntry, error) {
	if err := bookNoLinks(root, path); err != nil {
		return nil, err
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(limit + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > limit {
		return nil, errors.New("The Project Book has too many files to open safely.")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
func bookMkdir(root *os.Root, path string) error {
	if err := bookNoLinks(root, filepath.Dir(path)); filepath.Dir(path) != "." && err != nil {
		return err
	}
	if err := root.Mkdir(path, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return bookNoLinks(root, path)
}
func bookWriteNew(root *os.Root, path string, data []byte) error {
	if err := bookNoLinks(root, filepath.Dir(path)); err != nil {
		return err
	}
	file, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func initializeProjectBook(root *os.Root) error {
	// Never adopt or rewrite an existing Book whose format is unknown.
	if _, err := root.Lstat("project-book"); err == nil {
		return errProjectBookFormat
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := bookMkdir(root, "project-book"); err != nil {
		return err
	}
	for _, path := range []string{"project-book/history", "project-book/assets"} {
		if err := bookMkdir(root, path); err != nil {
			return err
		}
	}
	files := map[string]string{
		"README.md":         "# Project Book\n\nThis Book tells the story of each saved change in words a person can read. A story describes what the recorded work shows, how it was made, and why it matters when the evidence supports that. The original request and result stay linked beside it.\n\nEach entry lives in history. Its readable story is in entry.md, recorded source details are in source.json, and drawings are in sketch.json. Each edit creates a revision. The latest revision with a ready file is current. You can edit its Markdown directly. Sync adds newly recorded work and preserves existing entries.\n\nAn automatically written story or visual sketch is an interpretation of saved work, not a verified claim that every feature was tested. The run outline describes recorded work. Model or contributor details may be absent in older records.\n",
		"intent.md":         "# Intent\n\nDescribe who this project is for and what it should help them do.\n",
		"current-result.md": "# Current result\n\nThe dated entries in history describe recorded results. A completed run does not by itself confirm that every feature was tested.\n",
		"decisions.md":      "# Decisions\n\nRecord important choices and their reasons here.\n",
		"next.md":           "# Next\n\nRecord the next change you want here.\n",
		"PROJECT_BRIEF.md":  "# Project Brief\n\nRead README.md, intent.md, current-result.md, decisions.md, next.md, and the latest committed Markdown entries in history. This initial brief points to the Book; it is not a separate account of the project.\n",
		"book.json":         "{\"format\":\"glowbom-project-book\",\"version\":1}\n",
	}
	for _, name := range []string{"README.md", "intent.md", "current-result.md", "decisions.md", "next.md", "PROJECT_BRIEF.md", "book.json"} {
		if err := bookWriteNew(root, "project-book/"+name, []byte(files[name])); err != nil {
			return err
		}
	}
	return nil
}

func readProjectBook(root *os.Root, name string) (projectBook, bool, error) {
	book := projectBook{Version: 1, ProjectName: name, Entries: []bookEntry{}}
	if _, err := root.Lstat("project-book"); errors.Is(err, os.ErrNotExist) {
		return book, false, nil
	}
	data, err := bookRead(root, "project-book/book.json", 4096)
	var marker struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
	}
	if err != nil || json.Unmarshal(data, &marker) != nil || marker.Format != projectBookFormat || marker.Version != 1 {
		return book, true, errProjectBookFormat
	}
	dirs, err := bookDirs(root, "project-book/history", projectBookEntryLimit)
	if err != nil {
		return book, true, errors.New("Could not read Project Book entries.")
	}
	for _, dir := range dirs {
		if !dir.IsDir() {
			if dir.Type()&os.ModeSymlink != 0 {
				book.Warnings = append(book.Warnings, "A linked Book entry was skipped.")
			}
			continue
		}
		if !projectBookID.MatchString(dir.Name()) {
			book.Warnings = append(book.Warnings, "An unrecognized folder in Book history was skipped.")
			continue
		}
		entry, found, err := readBookEntry(root, dir.Name())
		if err != nil {
			book.Warnings = append(book.Warnings, fmt.Sprintf("Could not read Book entry %s. Its files were left unchanged.", dir.Name()))
			book.skippedIDs = append(book.skippedIDs, dir.Name())
			continue
		}
		if !found {
			book.Warnings = append(book.Warnings, "An unfinished Book entry was skipped.")
			continue
		}
		book.Entries = append(book.Entries, entry)
	}
	orderProjectBook(&book)
	return book, true, nil
}

func readBookEntry(root *os.Root, id string) (bookEntry, bool, error) {
	entry := bookEntry{}
	base := "project-book/history/" + id
	revisions, err := bookDirs(root, base, 1000)
	if err != nil {
		return entry, false, err
	}
	for i := len(revisions) - 1; i >= 0; i-- {
		revision := revisions[i]
		if !revision.IsDir() && revision.Type()&os.ModeSymlink == 0 {
			continue
		}
		if !revision.IsDir() || !projectBookID.MatchString(revision.Name()) {
			return entry, false, errors.New("Invalid Book revision.")
		}
		path := base + "/" + revision.Name()
		if _, err := bookRead(root, path+"/ready", 8); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return entry, false, err
		}
		data, err := bookRead(root, path+"/source.json", 1<<20)
		if err != nil || json.Unmarshal(data, &entry) != nil || entry.ID != id {
			return entry, false, errors.New("Invalid Book metadata.")
		}
		text, err := bookRead(root, path+"/entry.md", projectBookTextLimit)
		if err != nil {
			return entry, false, err
		}
		entry.Title, entry.Body, err = parseBookMarkdown(string(text))
		if err != nil {
			return entry, false, err
		}
		data, err = bookRead(root, path+"/sketch.json", projectBookSketchLimit)
		if err != nil || json.Unmarshal(data, &entry.Sketch) != nil {
			return entry, false, errors.New("Invalid Book sketch.")
		}
		if entry.Images == nil {
			entry.Images = []bookImage{}
		}
		if entry.ChangedFiles == nil {
			entry.ChangedFiles = []string{}
		}
		if entry.Sketch.Nodes == nil {
			entry.Sketch.Nodes = []bookSketchNode{}
		}
		if entry.Sketch.Edges == nil {
			entry.Sketch.Edges = []bookSketchEdge{}
		}
		if err := validateBookEntry(entry); err != nil {
			return entry, false, err
		}
		entry.ContentVersion = bookEntryContentVersion(entry)
		return entry, true, nil
	}
	return entry, false, nil
}
func parseBookMarkdown(text string) (string, string, error) {
	first, body, _ := strings.Cut(strings.TrimPrefix(text, "\ufeff"), "\n")
	if !strings.HasPrefix(first, "# ") || strings.TrimSpace(first[2:]) == "" {
		return "", "", errors.New("A Book entry must begin with a # title.")
	}
	return strings.TrimSpace(first[2:]), strings.TrimSpace(body), nil
}
func validateBookEntry(entry bookEntry) error {
	if !projectBookID.MatchString(entry.ID) || len(entry.Title) > 240 || strings.ContainsAny(entry.Title, "\r\n\x00") || strings.TrimSpace(entry.Title) == "" || len(entry.Body) > 240<<10 || len(entry.Request) > 128<<10 || len(entry.Source) > 2048 || len(entry.Images) > 32 || len(entry.ChangedFiles) > 2000 || (entry.Kind != "build" && entry.Kind != "prototype") {
		return errors.New("This Project Book entry is invalid or too large.")
	}
	if !validBookModelID(entry.StoryModel) {
		return errors.New("Invalid Project Book story model.")
	}
	if generation := entry.StoryGeneration; generation != nil {
		if generation.Model != entry.StoryModel || !validBookModelID(generation.Model) || len(generation.CreatedAt) > 64 || len(generation.SourceVersion) > 128 {
			return errors.New("Invalid Project Book story generation record.")
		}
	}
	seen := map[string]bool{}
	for _, image := range entry.Images {
		if !projectBookAsset.MatchString(image.Path) || !projectBookID.MatchString(image.ID) || seen[image.ID] || len(image.Caption) > 2000 || len(image.Name) > 240 || (image.Role != "input" && image.Role != "image") {
			return errors.New("Invalid Project Book image.")
		}
		seen[image.ID] = true
	}
	if entry.Sketch.Version != 1 || len(entry.Sketch.Nodes) > 40 || len(entry.Sketch.Edges) > 80 {
		return errors.New("Invalid Project Book sketch.")
	}
	if visual := entry.Sketch.Visual; visual != nil {
		if len(visual.Description) > 600 || len(visual.Model) > 200 || len(visual.CreatedAt) > 64 || len(visual.SourceVersion) > 128 || len(visual.Document.Annotations) > 200 || validateSketchDocument(&visual.Document) != nil {
			return errors.New("Invalid Project Book visual sketch.")
		}
	}
	return nil
}

// A ready marker commits a complete revision. Failed writes leave the previous
// Markdown readable, without a path-based rename outside the rooted API.
func saveBookEntry(root *os.Root, entry bookEntry) error {
	if err := validateBookEntry(entry); err != nil {
		return err
	}
	source := entry
	source.Title, source.Body = "", ""
	source.ContentVersion = ""
	source.CanWriteStory = false
	source.Sketch = bookSketch{}
	data, _ := json.MarshalIndent(source, "", "  ")
	sketch, _ := json.MarshalIndent(entry.Sketch, "", "  ")
	markdown := []byte("# " + entry.Title + "\n\n" + strings.TrimSpace(entry.Body) + "\n")
	if len(data) > 1<<20 || len(sketch) > projectBookSketchLimit || len(markdown) > projectBookTextLimit {
		return errors.New("The Book entry is too large to save safely.")
	}
	base := "project-book/history/" + entry.ID
	if err := bookMkdir(root, base); err != nil {
		return err
	}
	revision := fmt.Sprintf("r%020d-%s", time.Now().UTC().UnixNano(), strings.ReplaceAll(randomUUIDString(), "-", ""))
	path := base + "/" + revision
	if err := bookMkdir(root, path); err != nil {
		return err
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"source.json", data}, {"entry.md", markdown}, {"sketch.json", sketch}, {"ready", []byte("1\n")}} {
		if err := bookWriteNew(root, path+"/"+file.name, file.data); err != nil {
			return errors.New("Could not save the complete Book entry. The previous revision is unchanged.")
		}
	}
	return nil
}
func orderProjectBook(book *projectBook) {
	sort.Slice(book.Entries, func(i, j int) bool {
		if book.Entries[i].CreatedAt == book.Entries[j].CreatedAt {
			return book.Entries[i].ID > book.Entries[j].ID
		}
		return bookDateAfter(book.Entries[i].CreatedAt, book.Entries[j].CreatedAt)
	})
	for _, entry := range book.Entries {
		if bookDateAfter(entry.UpdatedAt, book.UpdatedAt) {
			book.UpdatedAt = entry.UpdatedAt
		}
	}
}

func bookDateAfter(left, right string) bool {
	a, _ := time.Parse(time.RFC3339Nano, left)
	b, _ := time.Parse(time.RFC3339Nano, right)
	return a.After(b)
}

func editProjectBook(root *os.Root, book *projectBook, req projectBookRequest) error {
	index := -1
	for i := range book.Entries {
		if book.Entries[i].ID == req.EntryID {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("That Project Book entry was not found.")
	}
	entry := book.Entries[index]
	if req.ExpectedUpdatedAt != "" && req.ExpectedUpdatedAt != entry.UpdatedAt {
		return errProjectBookConflict
	}
	if req.ExpectedContentVersion != "" && req.ExpectedContentVersion != bookEntryContentVersion(entry) {
		return errProjectBookConflict
	}
	entry.Images = append([]bookImage{}, entry.Images...)
	switch req.Action {
	case "update-entry":
		entry.Title, entry.Body = strings.TrimSpace(req.Title), strings.TrimSpace(req.Body)
		if req.StoryGeneration != nil {
			if req.StoryGeneration.SourceVersion != bookEntryContentVersion(book.Entries[index]) {
				return errProjectBookConflict
			}
			generation := *req.StoryGeneration
			entry.StoryGeneration = &generation
			entry.StoryModel = generation.Model
		}
		if req.StoryModel != nil && *req.StoryModel != entry.StoryModel {
			setBookStoryGeneration(&entry, *req.StoryModel, bookEntryContentVersion(book.Entries[index]))
		}
		if req.Images != nil {
			old := map[string]bookImage{}
			for _, image := range entry.Images {
				old[image.ID] = image
			}
			images := []bookImage{}
			for _, image := range *req.Images {
				existing, ok := old[image.ID]
				if !ok || image.Path != existing.Path || image.Role != existing.Role || image.Name != existing.Name {
					return errors.New("Only existing Book images can be reordered or captioned here.")
				}
				existing.Caption = image.Caption
				images = append(images, existing)
			}
			entry.Images = images
		}
	case "add-image":
		if len(entry.Images) >= 32 {
			return errors.New("An entry can contain up to 32 images.")
		}
		if len(req.Caption) > 2000 || len(req.Filename) > 240 {
			return errors.New("The image name or caption is too long.")
		}
		prefix, encoded, ok := strings.Cut(req.DataURL, ",")
		if !ok || (prefix != "data:image/png;base64" && prefix != "data:image/jpeg;base64" && prefix != "data:image/webp;base64") {
			return errors.New("Choose a PNG, JPEG, or WebP image.")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return errors.New("Could not decode the image.")
		}
		image, err := storeBookImage(root, data, req.Filename, req.Caption, "image")
		if err != nil {
			return err
		}
		entry.Images = append(entry.Images, image)
	case "remove-image":
		images := []bookImage{}
		found := false
		for _, image := range entry.Images {
			if image.ID == req.ImageID {
				found = true
			} else {
				images = append(images, image)
			}
		}
		if !found {
			return errors.New("That image was not found in the entry.")
		}
		entry.Images = images
	}
	entry.Reviewed = true
	entry.AutoStoryHash = ""
	entry.CanWriteStory = false
	entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveBookEntry(root, entry); err != nil {
		return err
	}
	entry.ContentVersion = bookEntryContentVersion(entry)
	book.Entries[index] = entry
	orderProjectBook(book)
	return nil
}

func bookEntryContentVersion(entry bookEntry) string {
	entry.ContentVersion = ""
	entry.CanWriteStory = false
	data, _ := json.Marshal(entry)
	return chatSourceHash(string(data))
}

func validateBookImage(data []byte) (string, string, error) {
	if len(data) == 0 || len(data) > projectBookImageLimit {
		return "", "", errors.New("Choose an image smaller than 8 MiB.")
	}
	mime := http.DetectContentType(data)
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}[mime]
	if ext == "" {
		return "", "", errors.New("Choose a PNG, JPEG, or WebP image.")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	w, h := config.Width, config.Height
	if mime == "image/webp" {
		w, h, err = bookWebPSize(data)
	}
	if err != nil || w < 1 || h < 1 || w > 8192 || h > 8192 || int64(w)*int64(h) > 32_000_000 {
		return "", "", errors.New("The image is invalid or exceeds 32 megapixels or 8192 pixels per side.")
	}
	return mime, ext, nil
}
func bookWebPSize(data []byte) (int, int, error) {
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, errors.New("Invalid WebP.")
	}
	switch string(data[12:16]) {
	case "VP8X":
		return 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16, 1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16, nil
	case "VP8 ":
		if string(data[23:26]) == "\x9d\x01\x2a" {
			return int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff), nil
		}
	case "VP8L":
		if data[20] == 0x2f {
			bits := binary.LittleEndian.Uint32(data[21:25])
			return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
		}
	}
	return 0, 0, errors.New("Invalid WebP.")
}
func storeBookImage(root *os.Root, data []byte, name, caption, role string) (bookImage, error) {
	_, ext, err := validateBookImage(data)
	if err != nil {
		return bookImage{}, err
	}
	hash := chatSourceHash(string(data))
	path := "assets/" + hash + "." + ext
	if err := bookWriteNew(root, "project-book/"+path, data); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return bookImage{}, errors.New("Could not save the Book image.")
		}
		existing, readErr := bookRead(root, "project-book/"+path, projectBookImageLimit)
		if readErr != nil || !bytes.Equal(existing, data) {
			return bookImage{}, errors.New("The existing Book image does not match its source.")
		}
	}
	if name == "" {
		name = "Image." + ext
	}
	return bookImage{ID: "image-" + strings.ReplaceAll(randomUUIDString(), "-", ""), Path: path, Name: bookClip(filepath.Base(name), 237), Caption: caption, Role: role}, nil
}
func projectBookMediaHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", 405)
		return
	}
	asset := r.URL.Query().Get("asset")
	if !projectBookAsset.MatchString(asset) {
		http.Error(w, "Invalid Book image path.", 400)
		return
	}
	projectBookMu.Lock()
	defer projectBookMu.Unlock()
	root, name, err := openProjectBookProject(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer root.Close()
	book, _, err := readProjectBook(root, name)
	if err != nil {
		http.Error(w, "Could not open the Project Book.", 400)
		return
	}
	found := false
	for _, entry := range book.Entries {
		for _, image := range entry.Images {
			if image.Path == asset {
				found = true
			}
		}
	}
	if !found {
		http.Error(w, "Book image not found.", 404)
		return
	}
	data, err := bookRead(root, "project-book/"+asset, projectBookImageLimit)
	if err != nil {
		http.Error(w, "Could not read the Book image.", 404)
		return
	}
	mime, _, err := validateBookImage(data)
	if err != nil {
		http.Error(w, "Invalid Book image.", 400)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Write(data)
}
