package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const maxChatResultBytes = 2 << 20
const maxChatTranslations = 20
const maxChatTranslationRecordBytes = 4 << 20

// Coordinate result snapshots and saves across chat requests in this process.
var chatResultMu sync.Mutex
var chatStackIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var chatTranslationFilePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.[a-f0-9]{32}\.json$`)

type chatStack struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type chatTranslation struct {
	chatStack
	Code       string `json:"code"`
	SourceHash string `json:"sourceHash"`
	Model      string `json:"model"`
	CreatedAt  string `json:"createdAt"`
	Outdated   bool   `json:"outdated"`
	Source     string `json:"source,omitempty"`
}

type chatResult struct {
	HTML         string            `json:"html"`
	SourceHash   string            `json:"sourceHash"`
	Translations []chatTranslation `json:"translations"`
	BookSource   string            `json:"bookSource,omitempty"`
}

func prototypeBookSource(record string) string {
	return ".glowbom/prototypes/" + filepath.Base(record) + "/request.json"
}

// Match the displayed HTML, rather than guessing from a repeated request.
// Unreadable or unfinished records never replace a known result's identity.
func currentPrototypeBookSource(root *os.Root, html string) string {
	if html == "" {
		return ""
	}
	dirs, err := bookDirs(root, ".glowbom/prototypes", 1000)
	if err != nil {
		return ""
	}
	type candidate struct {
		path     string
		created  time.Time
		modified time.Time
	}
	candidates := []candidate{}
	readBytes := 0
	for _, dir := range dirs {
		if !dir.IsDir() || dir.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(dir.Name(), "version-") {
			continue
		}
		path := ".glowbom/prototypes/" + dir.Name()
		data, err := bookRead(root, path+"/request.json", 2<<20)
		readBytes += len(data)
		if readBytes > 32<<20 {
			return ""
		}
		var request struct {
			Saved     *bool  `json:"saved"`
			CreatedAt string `json:"createdAt"`
		}
		if err != nil || json.Unmarshal(data, &request) != nil || (request.Saved != nil && !*request.Saved) {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, request.CreatedAt)
		if err != nil {
			continue
		}
		info, err := root.Lstat(path + "/request.json")
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		candidates = append(candidates, candidate{path: path, created: created, modified: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].created.Equal(candidates[j].created) {
			return candidates[i].modified.After(candidates[j].modified)
		}
		return candidates[i].created.After(candidates[j].created)
	})
	for _, candidate := range candidates {
		data, err := bookRead(root, candidate.path+"/result.html", maxChatResultBytes)
		readBytes += len(data)
		if readBytes > 32<<20 {
			return ""
		}
		if err == nil && string(data) == html {
			return candidate.path + "/request.json"
		}
	}
	return ""
}

type savedChatTranslation struct {
	chatTranslation
	filename string
	size     int
}

func validateChatStack(stack chatStack) error {
	if !chatStackIDPattern.MatchString(stack.ID) {
		return errors.New("Choose a stack with a short identifier using letters, numbers, underscores, or hyphens.")
	}
	if strings.TrimSpace(stack.Name) == "" || len(stack.Name) > 100 || strings.ContainsAny(stack.Name, "\r\n\x00") {
		return errors.New("Enter a stack name with at most 100 characters.")
	}
	if strings.TrimSpace(stack.Description) == "" || len(stack.Description) > 8000 || strings.ContainsRune(stack.Description, '\x00') {
		return errors.New("Describe the stack in at most 8,000 characters.")
	}
	return nil
}

func chatSourceHash(text string) string {
	hash := sha256.Sum256([]byte(text))
	return hex.EncodeToString(hash[:])
}

// Files are immutable versions. A failed write never destroys a previous result.
// Rooted file operations also keep symlinks inside the selected project.
func readChatTranslations(root *os.Root) ([]savedChatTranslation, error) {
	dir, err := root.Open(".glowbom/translations")
	if errors.Is(err, os.ErrNotExist) {
		return []savedChatTranslation{}, nil
	}
	if err != nil {
		return nil, errors.New("Could not read translations inside the project folder.")
	}
	defer dir.Close()
	entries, err := dir.ReadDir(513)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New("Could not read the saved translations.")
	}
	if len(entries) > 512 {
		return nil, errors.New("There are too many files in .glowbom/translations.")
	}
	translations := []savedChatTranslation{}
	total := 0
	for _, entry := range entries {
		if !chatTranslationFilePattern.MatchString(entry.Name()) {
			continue
		}
		// Publish a version only after its data was synced and closed. A crash
		// during generation leaves the last complete version readable.
		marker, err := root.Lstat(filepath.Join(".glowbom/translations", entry.Name()+".ready"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !marker.Mode().IsRegular() {
			return nil, errors.New("A saved translation has invalid completion information.")
		}
		file, err := root.Open(filepath.Join(".glowbom/translations", entry.Name()))
		if err != nil {
			return nil, errors.New("Could not read a translation inside the project folder.")
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxChatTranslationRecordBytes {
			file.Close()
			return nil, errors.New("A saved translation is invalid or too large.")
		}
		data, err := io.ReadAll(io.LimitReader(file, maxChatTranslationRecordBytes+1))
		file.Close()
		total += len(data)
		if err != nil || len(data) > maxChatTranslationRecordBytes || total > 16<<20 {
			return nil, errors.New("Saved translations are too large to open.")
		}
		var record chatTranslation
		if json.Unmarshal(data, &record) != nil || validateChatStack(record.chatStack) != nil || len(record.Code) > maxChatResultBytes || strings.TrimSpace(record.Code) == "" || !strings.HasPrefix(entry.Name(), record.ID+".") {
			return nil, errors.New("Could not read a saved translation.")
		}
		if hash, err := hex.DecodeString(record.SourceHash); err != nil || len(hash) != sha256.Size {
			return nil, errors.New("A saved translation has invalid source information.")
		}
		if _, err := time.Parse(time.RFC3339Nano, record.CreatedAt); err != nil {
			return nil, errors.New("A saved translation has invalid date information.")
		}
		translations = append(translations, savedChatTranslation{record, entry.Name(), len(data)})
	}
	sort.Slice(translations, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, translations[i].CreatedAt)
		b, _ := time.Parse(time.RFC3339Nano, translations[j].CreatedAt)
		if a.Equal(b) {
			return translations[i].filename > translations[j].filename
		}
		return a.After(b)
	})
	return translations, nil
}

func latestChatTranslations(saved []savedChatTranslation) []chatTranslation {
	result := []chatTranslation{}
	seen := map[string]bool{}
	for _, item := range saved {
		if !seen[item.ID] {
			result = append(result, item.chatTranslation)
			seen[item.ID] = true
		}
	}
	return result
}

func translationSnapshot(saved []savedChatTranslation, id string) string {
	for _, item := range saved {
		if item.ID == id {
			return item.filename
		}
	}
	return ""
}

func chatTranslationSnapshot(project, id string) (string, error) {
	chatResultMu.Lock()
	defer chatResultMu.Unlock()
	root, err := os.OpenRoot(project)
	if err != nil {
		return "", err
	}
	defer root.Close()
	saved, err := readChatTranslations(root)
	if err != nil {
		return "", err
	}
	available, err := appendImportedChatTranslations(root, latestChatTranslations(saved))
	if err != nil {
		return "", err
	}
	exists := false
	for _, translation := range available {
		if translation.ID == id {
			exists = true
			break
		}
	}
	if !exists && len(available) >= maxChatTranslations {
		return "", errors.New("This project already has 20 translated stacks.")
	}
	return translationSnapshot(saved, id), nil
}

func saveChatTranslation(project string, stack chatStack, code, source, expected, model string) (*chatTranslation, error) {
	if err := validateChatStack(stack); err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if strings.HasPrefix(code, "```") && strings.HasSuffix(code, "```") {
		// Remove one enclosing fence while preserving multi-file code blocks.
		if newline := strings.IndexByte(code, '\n'); newline >= 0 && strings.Count(code, "```") == 2 {
			code = strings.TrimSpace(code[newline+1 : len(code)-3])
		}
	}
	if code == "" || len(code) > maxChatResultBytes {
		return nil, errors.New("The translation was empty or too large. The previous result is unchanged.")
	}
	chatResultMu.Lock()
	defer chatResultMu.Unlock()
	current, err := readChatProjectFile(project, "prototype/index.html", maxChatResultBytes)
	if err != nil {
		return nil, err
	}
	if source == "" || current != source {
		return nil, errors.New("The prototype changed during translation. Retry using the latest version.")
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	saved, err := readChatTranslations(root)
	if err != nil {
		return nil, err
	}
	if translationSnapshot(saved, stack.ID) != expected {
		return nil, errors.New("This stack was translated in another request. Reopen the latest result before retrying.")
	}
	if expected == "" && len(latestChatTranslations(saved)) >= maxChatTranslations {
		return nil, errors.New("This project already has 20 translated stacks.")
	}
	record := &chatTranslation{chatStack: stack, Code: code, SourceHash: chatSourceHash(source), Model: model, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	candidate := []chatTranslation{*record}
	for _, existing := range latestChatTranslations(saved) {
		if existing.ID != stack.ID {
			candidate = append(candidate, existing)
		}
	}
	if _, err := appendImportedChatTranslations(root, candidate); err != nil {
		return nil, errors.New("This translation exceeds the project's saved translation limits. The previous result is unchanged.")
	}
	for _, path := range []string{".glowbom", ".glowbom/translations"} {
		if err := root.Mkdir(path, 0755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, errors.New("Could not create the project's translation folder.")
		}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	total := len(data)
	for _, old := range saved {
		if old.ID != stack.ID {
			total += old.size
		}
	}
	if len(data) > maxChatTranslationRecordBytes || total > 16<<20 {
		return nil, errors.New("The saved translations would be too large. The previous result is unchanged.")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	filename := filepath.Join(".glowbom/translations", stack.ID+"."+hex.EncodeToString(nonce[:])+".json")
	file, err := root.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("Could not save the translation inside the project folder.")
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = root.Remove(filename)
		return nil, errors.New("Could not save the translation. The previous result is unchanged.")
	}
	marker, err := root.OpenFile(filename+".ready", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = root.Remove(filename)
		return nil, errors.New("Could not finish saving the translation. The previous result is unchanged.")
	}
	if err := marker.Close(); err != nil {
		_ = root.Remove(filename + ".ready")
		_ = root.Remove(filename)
		return nil, errors.New("Could not finish saving the translation. The previous result is unchanged.")
	}
	for _, old := range saved {
		if old.ID == stack.ID {
			oldPath := filepath.Join(".glowbom/translations", old.filename)
			if err := root.Remove(oldPath + ".ready"); err == nil {
				_ = root.Remove(oldPath)
			}
		}
	}
	return record, nil
}

func (s *chatService) resultHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	project, err := chatProjectRoot(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chatResultMu.Lock()
	defer chatResultMu.Unlock()
	html, err := readChatProjectFile(project, "prototype/index.html", maxChatResultBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		http.Error(w, "Could not open the project.", http.StatusBadRequest)
		return
	}
	defer root.Close()
	saved, err := readChatTranslations(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result := chatResult{HTML: html, SourceHash: chatSourceHash(html), Translations: latestChatTranslations(saved), BookSource: currentPrototypeBookSource(root, html)}
	for i := range result.Translations {
		result.Translations[i].Outdated = result.Translations[i].SourceHash != result.SourceHash
	}
	result.Translations, err = appendImportedChatTranslations(root, result.Translations)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, result)
}
