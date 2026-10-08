package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	xAIOAuthClientID    = "b1a00492-073a-47ea-816f-4c329264a828"
	xAIOAuthTokenURL    = "https://auth.x.ai/oauth2/token"
	studioImageLimit    = 24
	studioVideoLimit    = 12
	studioImageMaxPage  = 48
	studioVideoMaxPage  = 24
	xAIVideoSourceLabel = "Glowbom Videos (Grok Imagine Video)"
)

var xAIAuthMu sync.Mutex
var xAIMediaAuthFileCandidates = xAIAuthFileCandidates
var studioCatalogCache struct {
	sync.Mutex
	directory string
	modTime   time.Time
	images    []studioImageSummary
	videos    []studioImageSummary
	files     []string
	nextFile  int
	complete  bool
	ready     bool
}

type studioImageSummary struct {
	SourceID                 string            `json:"sourceId,omitempty"`
	ModelID                  string            `json:"modelId,omitempty"`
	Resolution               string            `json:"resolution,omitempty"`
	Quality                  string            `json:"quality,omitempty"`
	RequestedDurationSeconds *float64          `json:"requestedDurationSeconds,omitempty"`
	ID                       string            `json:"id"`
	Timestamp                string            `json:"timestamp"`
	Prompt                   string            `json:"prompt"`
	SourceService            string            `json:"sourceService"`
	AssetType                string            `json:"assetType,omitempty"`
	Dimensions               *studioDimensions `json:"dimensions,omitempty"`
	Duration                 *float64          `json:"duration,omitempty"`
	SourceType               string            `json:"sourceType,omitempty"`
	SourceAssetID            string            `json:"sourceAssetID,omitempty"`
	SourceProjectID          string            `json:"sourceProjectID,omitempty"`
	UsedInProjects           []string          `json:"usedInProjects"`
}

type studioAssetMetadata struct {
	SourceID                 string            `json:"sourceId,omitempty"`
	Resolution               string            `json:"resolution,omitempty"`
	Quality                  string            `json:"quality,omitempty"`
	PromptInfluence          *float64          `json:"promptInfluence,omitempty"`
	Loop                     bool              `json:"loop,omitempty"`
	ForceInstrumental        bool              `json:"forceInstrumental,omitempty"`
	AudioType                string            `json:"audioType,omitempty"`
	MimeType                 string            `json:"mimeType,omitempty"`
	Model                    string            `json:"model,omitempty"`
	VoiceID                  string            `json:"voiceId,omitempty"`
	OutputFormat             string            `json:"outputFormat,omitempty"`
	RequestedDurationSeconds *float64          `json:"requestedDurationSeconds,omitempty"`
	ID                       string            `json:"id"`
	Timestamp                string            `json:"timestamp"`
	MediaType                string            `json:"mediaType"`
	Prompt                   string            `json:"prompt"`
	SourceService            string            `json:"sourceService"`
	AssetType                string            `json:"assetType"`
	Dimensions               *studioDimensions `json:"dimensions,omitempty"`
	Duration                 *float64          `json:"duration,omitempty"`
	SourceType               string            `json:"sourceType"`
	SourceAssetID            string            `json:"sourceAssetID,omitempty"`
	SourceProjectID          string            `json:"sourceProjectID,omitempty"`
	UsedInProjects           []string          `json:"usedInProjects"`
}

type studioSaveOptions struct {
	SourceID                 string
	Resolution               string
	Quality                  string
	PromptInfluence          *float64
	Loop                     bool
	ForceInstrumental        bool
	AudioType                string
	MimeType                 string
	Model                    string
	VoiceID                  string
	OutputFormat             string
	RequestedDurationSeconds float64
	NewGeneration            bool
	GenerationID             string
	Prompt                   string
	DataURI                  string
	MediaType                string
	Source                   string
	AssetType                string
	SourceType               string
	AspectRatio              string
	Duration                 float64
	SourceAssetID            string
	SourceProjectID          string
	UsedInProjects           []string
	Dimensions               *studioDimensions
}

type studioImageRecord struct {
	SourceID                 string            `json:"sourceId,omitempty"`
	Resolution               string            `json:"resolution,omitempty"`
	Quality                  string            `json:"quality,omitempty"`
	PromptInfluence          *float64          `json:"promptInfluence,omitempty"`
	Loop                     bool              `json:"loop,omitempty"`
	ForceInstrumental        bool              `json:"forceInstrumental,omitempty"`
	AudioType                string            `json:"audioType,omitempty"`
	MimeType                 string            `json:"mimeType,omitempty"`
	Model                    string            `json:"model,omitempty"`
	VoiceID                  string            `json:"voiceId,omitempty"`
	OutputFormat             string            `json:"outputFormat,omitempty"`
	RequestedDurationSeconds *float64          `json:"requestedDurationSeconds,omitempty"`
	ID                       string            `json:"id"`
	Timestamp                string            `json:"timestamp"`
	MediaType                string            `json:"mediaType"`
	DataBase64               string            `json:"dataBase64"`
	ThumbnailBase64          *string           `json:"thumbnailBase64"`
	FileSize                 int64             `json:"fileSize"`
	Prompt                   string            `json:"prompt"`
	AssetType                string            `json:"assetType"`
	SourceService            string            `json:"sourceService"`
	Dimensions               *studioDimensions `json:"dimensions,omitempty"`
	Duration                 *float64          `json:"duration,omitempty"`
	SourceType               string            `json:"sourceType"`
	SourceAssetID            string            `json:"sourceAssetID,omitempty"`
	SourceProjectID          string            `json:"sourceProjectID,omitempty"`
	UsedInProjects           []string          `json:"usedInProjects"`
	Tags                     []string          `json:"tags"`
	IsFavorite               bool              `json:"isFavorite"`
	Notes                    string            `json:"notes"`
}

// studioDimensions matches the Mac app, which stores a CGSize as [width, height].
// Older records written elsewhere may use {"width":…,"height":…} instead.
type studioDimensions struct {
	Width  int
	Height int
}

func (d studioDimensions) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int{d.Width, d.Height})
}

func (d *studioDimensions) UnmarshalJSON(data []byte) error {
	var pair []float64
	if err := json.Unmarshal(data, &pair); err == nil {
		if len(pair) >= 2 {
			d.Width, d.Height = int(pair[0]), int(pair[1])
		}
		return nil
	}
	var object map[string]float64
	if err := json.Unmarshal(data, &object); err == nil {
		d.Width, d.Height = int(object["width"]), int(object["height"])
	}
	// An unfamiliar shape must never hide a saved asset.
	return nil
}

type xAIStoredCredential struct {
	Type    string          `json:"type"`
	Key     string          `json:"key"`
	Access  string          `json:"access"`
	Refresh string          `json:"refresh"`
	Expires json.RawMessage `json:"expires"`
}

type xAICredential struct {
	Bearer   string
	Kind     string
	AuthFile string
	Refresh  string
	Expires  int64
}

func studioAssetsDirectory() (string, error) {
	if override := strings.TrimSpace(os.Getenv("GLOWBOM_STUDIO_DIR")); override != "" {
		return filepath.Join(override, "Assets"), nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "Glowbom", "Studio", "Assets"), nil
}

func xAIAuthFileCandidates() []string {
	var paths []string
	if userPaths, err := userOpenCodeRuntimePaths(); err == nil {
		paths = append(paths, userPaths.AuthFile)
	}
	if glowbomPaths, err := glowbomOpenCodeRuntimePaths(); err == nil {
		paths = append(paths, glowbomPaths.AuthFile)
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path != "" && !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result
}

func readXAIStoredCredential(authFile string) (xAICredential, bool, error) {
	return readXAIStoredCredentialWithSubscription(authFile, grokSubscriptionMediaEnabled())
}

func readXAIStoredCredentialWithSubscription(authFile string, allowSubscription bool) (xAICredential, bool, error) {
	data, err := os.ReadFile(authFile)
	if err != nil {
		if os.IsNotExist(err) {
			return xAICredential{}, false, nil
		}
		return xAICredential{}, false, err
	}
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(data, &auth); err != nil {
		return xAICredential{}, false, err
	}
	raw, ok := auth["xai"]
	if !ok {
		return xAICredential{}, false, nil
	}
	var metadata struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return xAICredential{}, false, err
	}
	if strings.EqualFold(strings.TrimSpace(metadata.Type), "oauth") && !allowSubscription {
		return xAICredential{}, false, nil
	}
	var stored xAIStoredCredential
	if err := json.Unmarshal(raw, &stored); err != nil {
		return xAICredential{}, false, err
	}
	switch strings.ToLower(strings.TrimSpace(stored.Type)) {
	case "oauth":
		if strings.TrimSpace(stored.Access) == "" {
			return xAICredential{}, false, nil
		}
		return xAICredential{
			Bearer:   strings.TrimSpace(stored.Access),
			Kind:     "subscription",
			AuthFile: authFile,
			Refresh:  strings.TrimSpace(stored.Refresh),
			Expires:  int64(parseRawJSONNumber(stored.Expires)),
		}, true, nil
	case "api":
		if strings.TrimSpace(stored.Key) == "" {
			return xAICredential{}, false, nil
		}
		return xAICredential{Bearer: strings.TrimSpace(stored.Key), Kind: "api-key", AuthFile: authFile}, true, nil
	default:
		return xAICredential{}, false, nil
	}
}

func findXAICredential() (xAICredential, bool, error) {
	environmentKey := strings.TrimSpace(os.Getenv("XAI_API_KEY"))
	if !grokSubscriptionMediaEnabled() && environmentKey != "" {
		return xAICredential{Bearer: environmentKey, Kind: "api-key"}, true, nil
	}
	for _, path := range xAIMediaAuthFileCandidates() {
		credential, ok, err := readXAIStoredCredential(path)
		if err != nil {
			return xAICredential{}, false, err
		}
		if ok {
			return credential, true, nil
		}
	}
	if environmentKey != "" {
		return xAICredential{Bearer: environmentKey, Kind: "api-key"}, true, nil
	}
	return xAICredential{}, false, nil
}

func resolveXAICredential(forceRefresh bool) (xAICredential, error) {
	return resolveXAICredentialContext(context.Background(), forceRefresh)
}

func resolveXAICredentialContext(ctx context.Context, forceRefresh bool) (xAICredential, error) {
	if err := ctx.Err(); err != nil {
		return xAICredential{}, err
	}
	xAIAuthMu.Lock()
	defer xAIAuthMu.Unlock()
	if err := ctx.Err(); err != nil {
		return xAICredential{}, err
	}

	credential, ok, err := findXAICredential()
	if err != nil {
		return xAICredential{}, err
	}
	if !ok {
		if !grokSubscriptionMediaEnabled() {
			return xAICredential{}, errGrokSubscriptionMediaDisabled
		}
		return xAICredential{}, errors.New("Connect xAI in OpenCode first")
	}
	if credential.Kind != "subscription" {
		return credential, nil
	}
	if err := requireGrokSubscriptionMedia(); err != nil {
		return xAICredential{}, err
	}
	expiresSoon := credential.Expires > 0 && credential.Expires <= time.Now().Add(2*time.Minute).UnixMilli()
	if !forceRefresh && !expiresSoon {
		return credential, nil
	}
	if credential.Refresh == "" {
		if credential.Expires == 0 && !forceRefresh {
			return credential, nil
		}
		return xAICredential{}, errors.New("Reconnect xAI in OpenCode to refresh the subscription")
	}
	return refreshXAICredential(credential, xAIOAuthTokenURL, http.DefaultClient, ctx)
}

func refreshXAICredential(credential xAICredential, tokenURL string, client *http.Client, contexts ...context.Context) (xAICredential, error) {
	if err := requireGrokSubscriptionMedia(); err != nil {
		return xAICredential{}, err
	}
	return refreshXAICredentialForVideo(credential, tokenURL, client, contexts...)
}

func refreshXAICredentialForVideo(credential xAICredential, tokenURL string, client *http.Client, contexts ...context.Context) (xAICredential, error) {
	ctx, cancel := context.WithTimeout(imageRequestContext(contexts), 30*time.Second)
	defer cancel()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {credential.Refresh},
		"client_id":     {xAIOAuthClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return xAICredential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "glowbom-oss")
	resp, err := client.Do(req)
	if err != nil {
		return xAICredential{}, fmt.Errorf("could not refresh the xAI subscription: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil {
		return xAICredential{}, fmt.Errorf("could not read the xAI subscription refresh: %w", err)
	}
	if len(body) > 16<<10 {
		return xAICredential{}, errors.New("xAI returned an oversized subscription refresh")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// OpenCode may have rotated the refresh token concurrently. Prefer its
		// newly persisted credential when available.
		if latest, ok, readErr := readXAIStoredCredentialWithSubscription(credential.AuthFile, true); readErr == nil && ok && latest.Bearer != credential.Bearer {
			return latest, nil
		}
		return xAICredential{}, fmt.Errorf("xAI subscription refresh failed with status %d", resp.StatusCode)
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil || strings.TrimSpace(tokens.AccessToken) == "" {
		return xAICredential{}, errors.New("xAI returned an invalid subscription refresh")
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = credential.Refresh
	}
	if tokens.ExpiresIn <= 0 {
		tokens.ExpiresIn = 3600
	}
	refreshed := xAICredential{
		Bearer:   strings.TrimSpace(tokens.AccessToken),
		Kind:     "subscription",
		AuthFile: credential.AuthFile,
		Refresh:  strings.TrimSpace(tokens.RefreshToken),
		Expires:  time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).UnixMilli(),
	}
	if err := persistXAICredential(refreshed); err != nil {
		return xAICredential{}, fmt.Errorf("could not save the refreshed xAI subscription: %w", err)
	}
	return refreshed, nil
}

func persistXAICredential(credential xAICredential) error {
	data, err := os.ReadFile(credential.AuthFile)
	if err != nil {
		return err
	}
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(data, &auth); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"type": "oauth", "access": credential.Bearer, "refresh": credential.Refresh, "expires": credential.Expires,
	})
	if err != nil {
		return err
	}
	auth["xai"] = payload
	encoded, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(credential.AuthFile), ".auth-*.json")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, credential.AuthFile)
}

func studioImagesStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	credential, connected, err := findXAICredential()
	if err != nil {
		writeJSON(w, map[string]any{"connected": false, "credential": "", "provider": "xAI", "sourceSelectionSupported": true, "grokSubscriptionMediaEnabled": grokSubscriptionMediaEnabled(), "error": "Could not read the OpenCode xAI connection. Reconnect xAI to generate videos or use Grok images."})
		return
	}
	kind := ""
	if connected {
		kind = credential.Kind
	}
	status := map[string]any{"connected": connected, "credential": kind, "provider": "xAI", "sourceSelectionSupported": true, "grokSubscriptionMediaEnabled": grokSubscriptionMediaEnabled()}
	if !connected && !grokSubscriptionMediaEnabled() {
		status["error"] = errGrokSubscriptionMediaDisabled.Error()
	}
	writeJSON(w, status)
}

func studioImagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	assets, err := listStudioAssets("image", studioImageLimit)
	if err != nil {
		http.Error(w, "Could not load Studio images.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"images": assets})
}

func studioAssetsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	imageOffset := studioPageParameter(query.Get("imageOffset"), 0, int(^uint(0)>>1))
	videoOffset := studioPageParameter(query.Get("videoOffset"), 0, int(^uint(0)>>1))
	imageLimit := studioPageParameter(query.Get("imageLimit"), studioImageLimit, studioImageMaxPage)
	videoLimit := studioPageParameter(query.Get("videoLimit"), studioVideoLimit, studioVideoMaxPage)
	projectID := strings.TrimSpace(query.Get("projectId"))
	if projectID != "" && normalizedStudioUUID(projectID) == "" {
		http.Error(w, "Choose a valid Studio project.", http.StatusBadRequest)
		return
	}
	images, videos, hasMoreImages, hasMoreVideos, err := listStudioProjectCatalogPage(
		projectID, imageOffset, imageLimit, videoOffset, videoLimit,
	)
	if err != nil {
		http.Error(w, "Could not load Studio.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"images": images, "videos": videos,
		"hasMoreImages": hasMoreImages, "hasMoreVideos": hasMoreVideos, "projectsSupported": true,
	})
}

func studioPageParameter(raw string, fallback, maximum int) int {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return min(value, maximum)
}

func listStudioImages() ([]studioImageSummary, error) {
	return listStudioAssets("image", studioImageLimit)
}

func listStudioAssets(mediaType string, limit int) ([]studioImageSummary, error) {
	images, videos, err := listStudioCatalog()
	if err != nil {
		return nil, err
	}
	if mediaType == "video" {
		return videos[:min(limit, len(videos))], nil
	}
	return images[:min(limit, len(images))], nil
}

func listStudioCatalog() ([]studioImageSummary, []studioImageSummary, error) {
	images, videos, _, _, err := listStudioCatalogPage(0, studioImageLimit, 0, studioVideoLimit)
	return images, videos, err
}

func listStudioCatalogPage(imageOffset, imageLimit, videoOffset, videoLimit int) (
	[]studioImageSummary, []studioImageSummary, bool, bool, error,
) {
	dir, err := studioAssetsDirectory()
	if err != nil {
		return nil, nil, false, false, err
	}
	studioCatalogCache.Lock()
	defer studioCatalogCache.Unlock()

	info, statErr := os.Stat(dir)
	cacheCurrent := statErr == nil && studioCatalogCache.ready &&
		studioCatalogCache.directory == dir &&
		studioCatalogCache.modTime.Equal(info.ModTime())
	if !cacheCurrent {
		if err := resetStudioCatalogCache(dir, info, statErr); err != nil {
			return nil, nil, false, false, err
		}
	}

	imageTarget := imageOffset + imageLimit
	if imageLimit > 0 {
		imageTarget++
	}
	videoTarget := videoOffset + videoLimit
	if videoLimit > 0 {
		videoTarget++
	}
	for !studioCatalogCache.complete &&
		studioCatalogCache.nextFile < len(studioCatalogCache.files) &&
		(len(studioCatalogCache.images) < imageTarget || len(studioCatalogCache.videos) < videoTarget) {
		name := studioCatalogCache.files[studioCatalogCache.nextFile]
		studioCatalogCache.nextFile++
		record, err := readStudioAssetMetadata(filepath.Join(dir, name))
		if err != nil || record.ID == "" {
			studioCatalogCache.complete = studioCatalogCache.nextFile >= len(studioCatalogCache.files)
			continue
		}
		summary := summarizeStudioMetadata(record)
		switch record.MediaType {
		case "image":
			studioCatalogCache.images = append(studioCatalogCache.images, summary)
		case "video":
			studioCatalogCache.videos = append(studioCatalogCache.videos, summary)
		}
		studioCatalogCache.complete = studioCatalogCache.nextFile >= len(studioCatalogCache.files)
	}
	if studioCatalogCache.nextFile >= len(studioCatalogCache.files) {
		studioCatalogCache.complete = true
	}

	imageEnd := min(imageOffset+imageLimit, len(studioCatalogCache.images))
	videoEnd := min(videoOffset+videoLimit, len(studioCatalogCache.videos))
	var images, videos []studioImageSummary
	if imageOffset < imageEnd {
		images = append([]studioImageSummary(nil), studioCatalogCache.images[imageOffset:imageEnd]...)
	} else {
		images = []studioImageSummary{}
	}
	if videoOffset < videoEnd {
		videos = append([]studioImageSummary(nil), studioCatalogCache.videos[videoOffset:videoEnd]...)
	} else {
		videos = []studioImageSummary{}
	}
	hasMoreImages := imageLimit > 0 && len(studioCatalogCache.images) > imageEnd
	hasMoreVideos := videoLimit > 0 && len(studioCatalogCache.videos) > videoEnd
	return images, videos, hasMoreImages, hasMoreVideos, nil
}

func resetStudioCatalogCache(dir string, info os.FileInfo, statErr error) error {
	studioCatalogCache.directory = dir
	studioCatalogCache.images = nil
	studioCatalogCache.videos = nil
	studioCatalogCache.files = nil
	studioCatalogCache.nextFile = 0
	studioCatalogCache.complete = false
	studioCatalogCache.modTime = time.Time{}
	studioCatalogCache.ready = false
	if statErr != nil {
		if os.IsNotExist(statErr) {
			studioCatalogCache.complete = true
			studioCatalogCache.ready = true
			return nil
		}
		return statErr
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			files = append(files, entry.Name())
		}
	}
	sort.Slice(files, func(i, j int) bool {
		left, right := studioAssetEpoch(files[i]), studioAssetEpoch(files[j])
		if left != right {
			return left > right
		}
		return files[i] > files[j]
	})
	studioCatalogCache.modTime = info.ModTime()
	studioCatalogCache.files = files
	studioCatalogCache.complete = len(files) == 0
	studioCatalogCache.ready = true
	return nil
}

// studioAssetEpoch reads the time prefix of an asset filename. The Mac app writes
// seconds with a fraction, while earlier Glowbom OSS builds wrote milliseconds.
func studioAssetEpoch(name string) float64 {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0
	}
	value, err := strconv.ParseFloat(prefix, 64)
	if err != nil {
		return 0
	}
	if !strings.Contains(prefix, ".") && len(prefix) >= 13 {
		return value / 1000
	}
	return value
}

func readStudioAssetMetadata(path string) (studioAssetMetadata, error) {
	return readStudioClipEnvelope(context.Background(), path, nil)
}

func readStudioImageRecord(path string) (studioImageRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return studioImageRecord{}, err
	}
	var record studioImageRecord
	if err := json.Unmarshal(data, &record); err == nil {
		if record.MediaType == "" {
			record.MediaType = "image"
		}
		return record, nil
	}
	// Studio is shared with the Mac app, so a field written by another version
	// must not make the asset itself unreadable.
	var minimal struct {
		ID            string `json:"id"`
		Timestamp     string `json:"timestamp"`
		MediaType     string `json:"mediaType"`
		DataBase64    string `json:"dataBase64"`
		Prompt        string `json:"prompt"`
		SourceService string `json:"sourceService"`
	}
	if err := json.Unmarshal(data, &minimal); err != nil {
		return studioImageRecord{}, err
	}
	return studioImageRecord{
		ID: minimal.ID, Timestamp: minimal.Timestamp, MediaType: minimal.MediaType,
		DataBase64: minimal.DataBase64, Prompt: minimal.Prompt, SourceService: minimal.SourceService,
	}, nil
}

func findStudioAsset(id string) (studioImageRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, `/\`) {
		return studioImageRecord{}, os.ErrNotExist
	}
	dir, err := studioAssetsDirectory()
	if err != nil {
		return studioImageRecord{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return studioImageRecord{}, err
	}
	// The Mac app writes uppercase identifiers, so names and ids are compared
	// without case before any file is opened.
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if !strings.Contains(strings.ToLower(name), strings.ToLower(id)) {
			continue
		}
		record, readErr := readStudioImageRecord(filepath.Join(dir, name))
		if readErr != nil || !strings.EqualFold(record.ID, id) {
			continue
		}
		return record, nil
	}
	return studioImageRecord{}, os.ErrNotExist
}

func studioImageContentHandler(w http.ResponseWriter, r *http.Request) {
	writeStudioAssetContent(w, r, "image", "image/jpeg")
}

func studioAssetInfoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	record, err := findStudioAsset(strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		http.Error(w, "Asset not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"asset": summarizeStudioRecord(record)})
}

func writeStudioAssetContent(w http.ResponseWriter, r *http.Request, mediaType, fallbackMime string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	record, err := findStudioAsset(id)
	if err != nil || record.MediaType != mediaType {
		http.Error(w, "Asset not found", http.StatusNotFound)
		return
	}
	body, mimeType, decodeErr := decodeBase64Payload(record.DataBase64, fallbackMime)
	if decodeErr != nil {
		http.Error(w, "Stored asset is invalid", http.StatusInternalServerError)
		return
	}
	// Studio records can store bare base64 without the original content type.
	// Prefer a recognized media signature while preserving explicit types that
	// the standard sniffer does not recognize, such as AVIF.
	if detected := http.DetectContentType(body); strings.HasPrefix(detected, mediaType+"/") {
		mimeType = detected
	} else if !strings.HasPrefix(mimeType, mediaType+"/") {
		mimeType = fallbackMime
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(body)
}

func studioImageGenerateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		GenerationID   string `json:"generationId,omitempty"`
		Prompt         string `json:"prompt"`
		AspectRatio    string `json:"aspectRatio"`
		ReferenceID    string `json:"referenceId"`
		ReferenceImage string `json:"referenceImage"`
		SourceID       string `json:"sourceId,omitempty"`
		APIKey         string `json:"apiKey,omitempty"`
		ModelID        string `json:"modelId,omitempty"`
		Resolution     string `json:"resolution,omitempty"`
		Quality        string `json:"quality,omitempty"`
		UseSavedKey    bool   `json:"useSavedKey,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 12<<20)).Decode(&request); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	request.Prompt = strings.TrimSpace(request.Prompt)
	request.SourceID = strings.TrimSpace(request.SourceID)
	if request.Prompt == "" || len(request.Prompt) > 4000 {
		http.Error(w, "Enter an image prompt up to 4,000 characters.", http.StatusBadRequest)
		return
	}
	var sourceID, key string
	var err error
	options := studioImageOptions{SourceID: request.SourceID, ModelID: request.ModelID, AspectRatio: request.AspectRatio, Resolution: request.Resolution, Quality: request.Quality}
	if request.SourceID != "" || request.ModelID != "" || request.Resolution != "" || request.Quality != "" || request.APIKey != "" || request.UseSavedKey {
		options, err = normalizeStudioImageOptions(options)
		if err != nil {
			if errors.Is(err, errGrokSubscriptionMediaDisabled) {
				writeStudioProviderError(w, err, "The selected subscription image source is disabled.")
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request.AspectRatio = options.AspectRatio
		sourceID = options.SourceID
		if sourceID == "glowbom-api" && !authorizeGlowbomImage(w, r) {
			return
		}
		if request.UseSavedKey || sourceID == "xai-subscription" || sourceID == "openai-subscription" {
			if !authorizeVoiceKey(w, r) {
				return
			}
			if !isAllowedOrigin(r, glowbomAllowedOrigins()) {
				http.Error(w, "This request is not allowed.", http.StatusForbidden)
				return
			}
		}
		if studioVideoKeyAccount(studioProviderKeyStoreSource(sourceID)) != "" {
			key, err = resolveStudioProviderKey(r.Context(), sourceID, request.APIKey, request.UseSavedKey)
		} else {
			_, key, err = resolveProjectIconSource(projectIconRequest{SourceID: sourceID, APIKey: request.APIKey}, r.Context())
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		if !studioImageContains([]string{"", "1:1", "16:9", "9:16"}, request.AspectRatio) {
			http.Error(w, "Unsupported aspect ratio", http.StatusBadRequest)
			return
		}
		// Preserve the old default transport for clients that send no source settings.
		options.ModelID = xAIImageModel
		options.Resolution = xAIImageResolution
	}
	progress, ok := beginStudioProgress(w, r, request.GenerationID, "image")
	if !ok {
		return
	}
	defer progress.finish()
	if err := progress.configure(request.Prompt, request.AspectRatio, request.ReferenceID, sourceID, 0); err != nil {
		http.Error(w, "Could not save this request. No image generation was started.", http.StatusInternalServerError)
		return
	}
	if err := progress.imageOptions(options); err != nil {
		http.Error(w, "Could not save image settings. No generation was started.", http.StatusInternalServerError)
		return
	}
	timeout := 3 * time.Minute
	if sourceID == "openai-subscription" {
		timeout = codexImageTimeout
	}
	if sourceID == "glowbom-api" {
		timeout = glowbomImageTimeout
	}
	ctx, cancel := context.WithTimeout(progress.context(r.Context()), timeout)
	defer cancel()
	reference, sourceAssetID, err := persistStudioReference(request.ReferenceID, request.ReferenceImage, sourceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := progress.reference(sourceAssetID, nil); err != nil {
		http.Error(w, "Could not save the reference for this request. No image generation was started.", http.StatusInternalServerError)
		return
	}
	dataURI, sourceLabel := "", xAIImageSourceLabel
	if sourceID == "" {
		progress.stage("generating")
		dataURI, err = withXAIBearerContext(ctx, func(bearer string) (string, error) {
			if reference != "" {
				return callGrokImageGenerationWithReference(request.Prompt, reference, bearer, request.AspectRatio, ctx)
			}
			return callGrokImageGeneration(request.Prompt, bearer, request.AspectRatio, ctx)
		})
	} else {
		if reference != "" {
			reference, err = normalizeProjectIconReference(reference)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if sourceID == "glowbom-api" {
			if err := validateGlowbomImageReference(reference); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		progress.stage("generating")
		dataURI, sourceLabel, err = generateStudioSelectedImageWithOptions(ctx, options, key, request.Prompt, reference)
	}
	if err != nil {
		message, status := studioProviderError(err, "The selected source could not generate this image. Check its connection or API key, then try again.")
		progress.failed(ctx, message)
		http.Error(w, message, status)
		return
	}
	if ctx.Err() != nil {
		progress.failed(ctx, "The image request was stopped.")
		http.Error(w, "The image request was stopped.", http.StatusRequestTimeout)
		return
	}
	progress.stage("saving")
	data, _, decodeErr := decodeBase64Payload(dataURI, "image/png")
	if decodeErr == nil {
		data, decodeErr = normalizeStudioGeneratedImage(data)
	}
	if decodeErr != nil {
		http.Error(w, "The selected source returned an unsupported image. No retry was made.", http.StatusBadGateway)
		return
	}
	if err := validateGeneratedImageAspect(data, request.AspectRatio); err != nil {
		writeStudioProviderError(w, err, "The image source did not return the selected image shape.")
		return
	}
	config, _, _ := image.DecodeConfig(bytes.NewReader(data))
	dimensions := &studioDimensions{Width: config.Width, Height: config.Height}
	dataURI = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	record, err := saveStudioAsset(studioSaveOptions{
		Prompt: request.Prompt, DataURI: dataURI, MediaType: "image", Source: sourceLabel,
		SourceID: sourceID, Model: options.ModelID, Resolution: options.Resolution, Quality: options.Quality,
		GenerationID: progress.id,
		AspectRatio:  request.AspectRatio, SourceAssetID: sourceAssetID, Dimensions: dimensions,
	})
	if err != nil {
		http.Error(w, "The image was generated but could not be saved to Studio.", http.StatusInternalServerError)
		return
	}
	asset := summarizeStudioRecord(record)
	_ = progress.completed(asset)
	writeJSON(w, map[string]any{"image": asset})
}

func resolveStudioReferenceImage(id, inline string) (string, error) {
	id = strings.TrimSpace(id)
	inline = strings.TrimSpace(inline)
	if id == "" && inline == "" {
		return "", nil
	}
	if id != "" {
		record, err := findStudioAsset(id)
		if err != nil || record.MediaType != "image" || record.DataBase64 == "" {
			return "", errors.New("Choose a Studio image as the reference.")
		}
		return ensureImageDataURI(record.DataBase64, "image/jpeg"), nil
	}
	raw, mimeType, err := decodeBase64Payload(inline, "image/jpeg")
	if err != nil || len(raw) == 0 || len(raw) > 10<<20 {
		return "", errors.New("Use a PNG or JPEG reference smaller than 10 MB.")
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return "", errors.New("The reference must be an image.")
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(raw)), nil
}

func withXAIBearer[T any](fn func(bearer string) (T, error)) (T, error) {
	return withXAIBearerContext(context.Background(), fn)
}

func withXAIBearerContext[T any](ctx context.Context, fn func(bearer string) (T, error)) (T, error) {
	var zero T
	credential, err := resolveXAICredentialContext(ctx, false)
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if credential.Kind == "subscription" {
		if err := requireGrokSubscriptionMedia(); err != nil {
			return zero, err
		}
	}
	value, err := fn(credential.Bearer)
	if err != nil && credential.Kind == "subscription" && isXAIUnauthorized(err) {
		if refreshed, refreshErr := resolveXAICredentialContext(ctx, true); refreshErr == nil {
			credential = refreshed
			value, err = fn(credential.Bearer)
		} else if ctx.Err() != nil {
			return zero, ctx.Err()
		}
	}
	return value, err
}

func saveStudioImage(prompt, dataURI string) (studioImageRecord, error) {
	return saveStudioAsset(studioSaveOptions{Prompt: prompt, DataURI: dataURI, MediaType: "image", Source: xAIImageSourceLabel})
}

func saveStudioAsset(options studioSaveOptions) (studioImageRecord, error) {
	fallback := "image/jpeg"
	if options.MediaType == "video" {
		fallback = "video/mp4"
	} else if options.MediaType == "audio" {
		fallback = "audio/mpeg"
	}
	raw, mimeType, err := decodeBase64Payload(options.DataURI, fallback)
	if err != nil {
		return studioImageRecord{}, err
	}
	assetType := strings.TrimSpace(options.AssetType)
	if assetType == "" {
		assetType = "generated"
	}
	sourceType := strings.TrimSpace(options.SourceType)
	if sourceType == "" {
		sourceType = "generated"
	}
	// The Mac app reads these files with an ISO 8601 date strategy and an
	// uppercase identifier, so both are written in its format.
	id := strings.ToUpper(randomUUIDString())
	if generatedID := studioGenerationAssetID(options.GenerationID); generatedID != "" {
		id = generatedID
	}
	now := time.Now().UTC()
	record := studioImageRecord{
		SourceID: options.SourceID, Resolution: options.Resolution, Quality: options.Quality,
		PromptInfluence: options.PromptInfluence, Loop: options.Loop, ForceInstrumental: options.ForceInstrumental,
		AudioType: options.AudioType, MimeType: options.MimeType, Model: options.Model, VoiceID: options.VoiceID, OutputFormat: options.OutputFormat,
		ID: id, Timestamp: now.Format("2006-01-02T15:04:05Z"), MediaType: options.MediaType,
		DataBase64: base64.StdEncoding.EncodeToString(raw), ThumbnailBase64: nil,
		FileSize: int64(len(raw)), Prompt: options.Prompt, AssetType: assetType,
		SourceService: options.Source, Dimensions: studioDimensionsFor(options.MediaType, options.AspectRatio),
		SourceType: sourceType, SourceAssetID: strings.TrimSpace(options.SourceAssetID),
		SourceProjectID: normalizedStudioUUID(options.SourceProjectID),
		UsedInProjects:  normalizedStudioUUIDs(options.UsedInProjects), Tags: []string{}, Notes: "",
	}
	if options.Dimensions != nil {
		record.Dimensions = options.Dimensions
	}
	if options.Duration > 0 {
		duration := options.Duration
		record.Duration = &duration
	}
	if options.MediaType == "audio" {
		record.MimeType = mimeType
	}
	if options.MediaType == "audio" || options.MediaType == "video" {
		if options.RequestedDurationSeconds > 0 {
			duration := options.RequestedDurationSeconds
			record.RequestedDurationSeconds = &duration
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return studioImageRecord{}, err
	}
	dir, err := studioAssetsDirectory()
	if err != nil {
		return studioImageRecord{}, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return studioImageRecord{}, err
	}
	filename := fmt.Sprintf("%.6f_%s.json", float64(now.UnixNano())/1e9, id)
	if err := atomicChatFile(dir, filename, encoded); err != nil {
		return studioImageRecord{}, err
	}
	return record, nil
}

// Stable result IDs recover a saved file even if the final checkpoint write fails.
func studioGenerationAssetID(generationID string) string {
	if !studioGenerationID.MatchString(generationID) {
		return ""
	}
	digest := sha256.Sum256([]byte("glowbom-studio-generation:" + generationID))
	digest[6] = (digest[6] & 0x0f) | 0x40
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func studioDimensionsFor(mediaType, aspectRatio string) *studioDimensions {
	if mediaType == "audio" {
		return nil
	}
	if mediaType == "video" {
		switch aspectRatio {
		case "9:16":
			return &studioDimensions{Width: 720, Height: 1280}
		case "1:1":
			return &studioDimensions{Width: 1080, Height: 1080}
		default:
			return &studioDimensions{Width: 1280, Height: 720}
		}
	}
	switch aspectRatio {
	case "16:9":
		return &studioDimensions{Width: 1536, Height: 1024}
	case "9:16":
		return &studioDimensions{Width: 1024, Height: 1536}
	default:
		return &studioDimensions{Width: 1024, Height: 1024}
	}
}

func summarizeStudioMetadata(record studioAssetMetadata) studioImageSummary {
	return studioImageSummary{
		SourceID: record.SourceID, ModelID: studioVideoSummaryModel(record.MediaType, record.Model), Resolution: record.Resolution, Quality: record.Quality, RequestedDurationSeconds: record.RequestedDurationSeconds,
		ID: record.ID, Timestamp: record.Timestamp, Prompt: record.Prompt, SourceService: record.SourceService,
		AssetType: record.AssetType, Dimensions: record.Dimensions, Duration: record.Duration,
		SourceType: record.SourceType, SourceAssetID: record.SourceAssetID,
		SourceProjectID: record.SourceProjectID, UsedInProjects: normalizedStudioUUIDs(record.UsedInProjects),
	}
}

func summarizeStudioRecord(record studioImageRecord) studioImageSummary {
	return studioImageSummary{
		SourceID: record.SourceID, ModelID: studioVideoSummaryModel(record.MediaType, record.Model), Resolution: record.Resolution, Quality: record.Quality, RequestedDurationSeconds: record.RequestedDurationSeconds,
		ID: record.ID, Timestamp: record.Timestamp, Prompt: record.Prompt, SourceService: record.SourceService,
		AssetType: record.AssetType, Dimensions: record.Dimensions, Duration: record.Duration,
		SourceType: record.SourceType, SourceAssetID: record.SourceAssetID,
		SourceProjectID: record.SourceProjectID, UsedInProjects: normalizedStudioUUIDs(record.UsedInProjects),
	}
}

func writeStudioProviderError(w http.ResponseWriter, err error, fallback string) {
	message, status := studioProviderError(err, fallback)
	http.Error(w, message, status)
}

// Use vetted messages for both the response and saved generation status.
func studioProviderError(err error, fallback string) (string, int) {
	if errors.Is(err, errGrokSubscriptionMediaDisabled) {
		return errGrokSubscriptionMediaDisabled.Error(), http.StatusPreconditionFailed
	}
	var aspectFailure *imageAspectFailure
	if errors.As(err, &aspectFailure) {
		return aspectFailure.Error(), http.StatusBadGateway
	}
	var codexFailure *codexImageFailure
	if errors.As(err, &codexFailure) {
		status := http.StatusBadGateway
		if codexFailure.Code == "reconnect" || codexFailure.Code == "refresh" || codexFailure.Code == "save" {
			status = http.StatusPreconditionFailed
		} else if codexFailure.Code == "limits" {
			status = http.StatusTooManyRequests
		}
		return codexFailure.Error(), status
	}
	var appServerFailure *codexAppServerImageFailure
	if errors.As(err, &appServerFailure) {
		return appServerFailure.Error(), http.StatusBadGateway
	}
	var glowbomFailure *glowbomImageFailure
	if errors.As(err, &glowbomFailure) {
		return glowbomFailure.Error(), http.StatusBadGateway
	}
	if strings.Contains(strings.ToLower(err.Error()), "connect xai") {
		switch message := err.Error(); message {
		case "Connect xAI in OpenCode first", "Reconnect xAI in OpenCode to refresh the subscription", "Reconnect xAI in OpenCode.":
			return message, http.StatusPreconditionFailed
		default:
			return "Reconnect xAI in OpenCode.", http.StatusPreconditionFailed
		}
	}
	var failure *studioVideoFailure
	if errors.As(err, &failure) {
		return failure.message, http.StatusBadGateway
	}
	return fallback, http.StatusBadGateway
}

func isXAIUnauthorized(err error) bool {
	var providerErr *xAIAPIError
	return errors.As(err, &providerErr) && providerErr.Status == http.StatusUnauthorized
}

func studioVideoSummaryModel(mediaType, model string) string {
	if mediaType == "video" || mediaType == "image" {
		return model
	}
	return ""
}
