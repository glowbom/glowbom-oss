package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type bookImportImage struct{ path, name string }
type bookImport struct {
	entry       bookEntry
	images      []bookImportImage
	legacyTitle string
	legacyBody  string
}

func syncProjectBook(root *os.Root, book *projectBook) error {
	known := map[string]int{}
	for index, entry := range book.Entries {
		known[entry.ID] = index
	}
	for _, id := range book.skippedIDs {
		known[id] = -1
	}
	imports, warnings := collectBookImports(root)
	book.Warnings = append(book.Warnings, warnings...)
	for _, item := range imports {
		entry := item.entry
		if index, exists := known[entry.ID]; exists {
			if index >= 0 {
				old := book.Entries[index]
				changed := false
				if !old.Reviewed && old.Title == item.legacyTitle && old.Body == item.legacyBody &&
					(old.Title != entry.Title || old.Body != entry.Body) {
					old.Title, old.Body = entry.Title, entry.Body
					changed = true
				}
				if !old.Reviewed && old.AutoStoryHash == "" && old.Title == entry.Title && old.Body == entry.Body {
					old.AutoStoryHash = bookStoryHash(old.Title, old.Body)
					changed = true
				}
				if changed {
					old.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
					if err := saveBookEntry(root, old); err != nil {
						return err
					}
					old.ContentVersion = bookEntryContentVersion(old)
					book.Entries[index] = old
				}
			}
			continue
		}
		if len(book.Entries) >= projectBookEntryLimit {
			return errors.New("This Project Book has reached its limit of 1000 entries.")
		}
		for _, input := range item.images {
			if len(entry.Images) >= 32 {
				book.Warnings = append(book.Warnings, "Some input images were omitted because an entry already contains 32 images.")
				break
			}
			data, err := bookRead(root, input.path, projectBookImageLimit)
			if err != nil {
				book.Warnings = append(book.Warnings, fmt.Sprintf("An input image for %s could not be copied. Its source is unchanged.", entry.ID))
				continue
			}
			image, err := storeBookImage(root, data, input.name, "Original input", "input")
			if err != nil {
				book.Warnings = append(book.Warnings, fmt.Sprintf("An input image for %s is unsupported or too large. Its source is unchanged.", entry.ID))
				continue
			}
			entry.Images = append(entry.Images, image)
		}
		if err := saveBookEntry(root, entry); err != nil {
			return err
		}
		entry.ContentVersion = bookEntryContentVersion(entry)
		book.Entries = append(book.Entries, entry)
		known[entry.ID] = len(book.Entries) - 1
	}
	if len(book.Warnings) > 100 {
		book.Warnings = append(book.Warnings[:100], "More source records need attention.")
	}
	orderProjectBook(book)
	return nil
}

func collectBookImports(root *os.Root) ([]bookImport, []string) {
	imports := []bookImport{}
	warnings := []string{}
	for _, source := range []struct{ path, kind string }{{"history", "build"}, {".glowbom/prototypes", "prototype"}} {
		dirs, err := bookDirs(root, source.path, projectBookEntryLimit)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, "Could not read recorded "+source.kind+" history. Check the project files and try Sync again.")
			continue
		}
		for _, dir := range dirs {
			if dir.Type()&os.ModeSymlink != 0 {
				warnings = append(warnings, "A linked history folder was not imported.")
				continue
			}
			if !dir.IsDir() {
				continue
			}
			base := source.path + "/" + dir.Name()
			var item bookImport
			var warning string
			if source.kind == "build" {
				item, warning = importBookBuild(root, base)
			} else {
				item, warning = importBookPrototype(root, base)
			}
			if warning != "" {
				warnings = append(warnings, warning)
			}
			if item.entry.ID != "" {
				imports = append(imports, item)
			}
		}
	}
	return imports, warnings
}

func newImportedBookEntry(source, kind, request, timestamp, status string) bookEntry {
	created := ""
	if parsed, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
		created = parsed.UTC().Format(time.RFC3339Nano)
	}
	request = bookClip(strings.TrimSpace(request), 96<<10)
	title := "Build recorded"
	if kind == "prototype" {
		title = "Prototype recorded"
	}
	if request != "" {
		line, _, _ := strings.Cut(request, "\n")
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			title = bookClip(line, 87)
		}
	}
	return bookEntry{ID: kind + "-" + chatSourceHash(source)[:24], Title: title, CreatedAt: created, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Status: status, Request: request, Source: source, ChangedFiles: []string{}, Images: []bookImage{}}
}

func bookClip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && text[limit]&0xc0 == 0x80 {
		limit--
	}
	return text[:limit] + "…"
}

func bookRecordedBody(entry bookEntry, result string) string {
	body := result
	if entry.Request != "" {
		body += "\n\n## What we asked for\n\n> " + strings.ReplaceAll(entry.Request, "\n", "\n> ")
	}
	return body
}

func importBookBuild(root *os.Root, base string) (bookImport, string) {
	source := base + "/entry.json"
	data, err := bookRead(root, source, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return bookImport{}, "A history folder has no entry.json and was not imported."
	}
	var record agentHistoryEntryRecord
	if err != nil || json.Unmarshal(data, &record) != nil {
		return bookImport{}, "A build history record is unreadable or invalid and was not imported."
	}
	if strings.TrimSpace(record.ID) == "" && strings.TrimSpace(record.Instructions) == "" {
		return bookImport{}, "An empty build history record was not imported."
	}
	status := normalizeHistoryStatus(record.Status)
	if status == "" {
		status = "unknown"
	}
	request := record.Request
	if request == "" && record.RunID == "" {
		request = record.Instructions
	}
	entry := newImportedBookEntry(source, "build", request, record.Timestamp, status)
	entry.RunID = record.RunID
	entry.Model = bookClip(record.Model, 200)
	entry.Contributor = bookClip(record.Contributor, 200)
	pathBytes := 0
	for _, path := range record.ChangedFiles {
		if len(entry.ChangedFiles) == 2000 {
			break
		}
		if filepath.IsLocal(path) && len(path) <= 2048 && !strings.ContainsAny(path, "\x00\r\n") {
			if pathBytes+len(path) > 256<<10 {
				break
			}
			entry.ChangedFiles = append(entry.ChangedFiles, filepath.ToSlash(path))
			pathBytes += len(path)
		}
	}
	result := strings.TrimSpace(record.OutputSummary)
	if result == "" {
		result = "Recorded status: " + status + ". No result summary was saved."
	}
	result = bookClip(result, 16<<10)
	first, _, _ := strings.Cut(strings.TrimSpace(record.OutputSummary), "\n")
	first = strings.TrimSpace(strings.TrimPrefix(first, "✅"))
	lower := strings.ToLower(first)
	if first != "" && !strings.HasPrefix(lower, "refinement completed") && !strings.HasPrefix(lower, "build completed") {
		if sentence, _, ok := strings.Cut(first, ". "); ok {
			first = sentence + "."
		}
		entry.Title = bookClip(first, 87)
	}
	entry.Body = bookRecordedBody(entry, result)
	entry.Sketch = makeBookRunSketch(entry, result)
	item := bookImport{legacyTitle: entry.Title, legacyBody: entry.Body}
	story := fallbackBookStory(entry, result)
	entry.Title, entry.Body = story.Title, story.Body
	entry.AutoStoryHash = bookStoryHash(entry.Title, entry.Body)
	item.entry = entry
	warning := ""
	for _, attachment := range record.Attachments {
		if !bookImageFilename(attachment.Filename) {
			continue
		}
		if filepath.Base(attachment.Filename) != attachment.Filename || strings.Contains(attachment.Filename, `\`) {
			warning = "A build input image had an invalid path and was not copied."
			continue
		}
		item.images = append(item.images, bookImportImage{path: base + "/" + attachment.Filename, name: attachment.Filename})
	}
	return item, warning
}

func importBookPrototype(root *os.Root, base string) (bookImport, string) {
	source := base + "/request.json"
	data, err := bookRead(root, source, 2<<20)
	var record struct {
		Model       string `json:"model"`
		CreatedAt   string `json:"createdAt"`
		Contributor string `json:"contributor"`
		Messages    []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"messages"`
		Saved       *bool `json:"saved"`
		Attachments []struct {
			Filename string `json:"filename"`
			Path     string `json:"path"`
		} `json:"attachments"`
	}
	if err != nil || json.Unmarshal(data, &record) != nil {
		return bookImport{}, "A prototype history record is unreadable or invalid and was not imported."
	}
	if record.Saved != nil && !*record.Saved {
		return bookImport{}, "An unfinished prototype record was not imported."
	}
	result, err := bookRead(root, base+"/result.html", 2<<20)
	if err != nil || len(result) == 0 {
		return bookImport{}, "A prototype record has no readable result and was not imported."
	}
	request := ""
	for _, message := range record.Messages {
		if message.Role == "user" {
			request = message.Text
		}
	}
	entry := newImportedBookEntry(source, "prototype", request, record.CreatedAt, "completed")
	entry.Model = bookClip(record.Model, 200)
	entry.Contributor = bookClip(record.Contributor, 200)
	entry.ChangedFiles = []string{"prototype/index.html"}
	note := "A browser prototype was saved. The original HTML result is kept with this run. This record does not confirm that the result was tested."
	entry.Body = bookRecordedBody(entry, note)
	entry.Sketch = makeBookRunSketch(entry, "Browser prototype saved")
	item := bookImport{legacyTitle: entry.Title, legacyBody: entry.Body}
	story := fallbackBookStory(entry, note)
	entry.Title, entry.Body = story.Title, story.Body
	entry.AutoStoryHash = bookStoryHash(entry.Title, entry.Body)
	item.entry = entry
	warning := ""
	for _, attachment := range record.Attachments {
		if !bookImageFilename(attachment.Filename) {
			continue
		}
		if !filepath.IsLocal(attachment.Path) || !strings.HasPrefix(attachment.Path, "inputs/") || filepath.ToSlash(filepath.Clean(attachment.Path)) != attachment.Path || strings.Contains(attachment.Path, `\`) {
			warning = "A prototype input image had an invalid path and was not copied."
			continue
		}
		item.images = append(item.images, bookImportImage{path: base + "/" + attachment.Path, name: attachment.Filename})
	}
	return item, warning
}

func bookImageFilename(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return true
	}
	return false
}

func makeBookRunSketch(entry bookEntry, result string) bookSketch {
	sketch := bookSketch{Version: 1, Title: "Sketch of this run", Caption: "An outline of the recorded request and result. It does not describe the application's screen layout.", Nodes: []bookSketchNode{}, Edges: []bookSketchEdge{}}
	request := entry.Request
	if request == "" {
		request = "No request was recorded."
	}
	sketch.Nodes = append(sketch.Nodes, bookSketchNode{ID: "request", Label: "Request", Detail: bookClip(strings.Join(strings.Fields(request), " "), 280)})
	label := "Build"
	if entry.Kind == "prototype" {
		label = "Prototype"
	}
	detail := "Model not recorded"
	if entry.Model != "" {
		detail = entry.Model
	}
	sketch.Nodes = append(sketch.Nodes, bookSketchNode{ID: "work", Label: label, Detail: detail})
	sketch.Edges = append(sketch.Edges, bookSketchEdge{From: "request", To: "work"})
	sketch.Nodes = append(sketch.Nodes, bookSketchNode{ID: "result", Label: "Result: " + entry.Status, Detail: bookClip(strings.Join(strings.Fields(result), " "), 280)})
	sketch.Edges = append(sketch.Edges, bookSketchEdge{From: "work", To: "result"})
	areas := map[string]bool{}
	for _, path := range entry.ChangedFiles {
		area, _, _ := strings.Cut(path, "/")
		if areas[area] || len(areas) >= 6 {
			continue
		}
		areas[area] = true
		id := fmt.Sprintf("area-%d", len(areas))
		sketch.Nodes = append(sketch.Nodes, bookSketchNode{ID: id, Label: bookClip(area, 80), Detail: "Recorded changed files"})
		sketch.Edges = append(sketch.Edges, bookSketchEdge{From: "result", To: id})
	}
	return sketch
}
