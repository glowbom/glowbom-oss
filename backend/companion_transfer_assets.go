package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type companionProjectImportAsset struct {
	ID              string `json:"id"`
	Filename        string `json:"filename"`
	MimeType        string `json:"mimeType"`
	DataBase64      string `json:"dataBase64"`
	Role            string `json:"role"`
	Prompt          string `json:"prompt"`
	SourceType      string `json:"sourceType,omitempty"`
	SourceService   string `json:"sourceService,omitempty"`
	SourceAssetID   string `json:"sourceAssetId,omitempty"`
	SourceProjectID string `json:"sourceProjectId,omitempty"`
}

type companionPreparedImportAsset struct {
	companionProjectImportAsset
	data       []byte
	media      string
	dimensions *studioDimensions
	aliasOf    string
}

type companionImportedAssetReceipt struct {
	Path      string `json:"path"`
	StudioID  string `json:"studioId"`
	SourceID  string `json:"sourceId,omitempty"`
	Hash      string `json:"hash"`
	ByteCount int    `json:"byteCount"`
}

func companionImportInputRole(role string) bool {
	return role == "input" || role == "reference" || role == "personalization"
}

func prepareCompanionImportAssets(request companionPrototypeImport) ([]companionPreparedImportAsset, int64, error) {
	fail := errors.New("An original input or saved picture is missing or invalid. Restore the project's complete media in Studio before sending.")
	if len(request.Assets) > 64 || len(request.OriginalHTML) > companionImportHTMLBytes || len(request.SourceSnapshot) > 1<<20 || request.SourceProjectID != "" && normalizedStudioUUID(request.SourceProjectID) == "" {
		return nil, 0, fail
	}
	total := int64(len(request.HTML) + len(request.OriginalHTML) + len(request.SourceSnapshot))
	seen := map[string]bool{}
	prepared := []companionPreparedImportAsset{}
	for _, asset := range request.Assets {
		id := normalizedStudioUUID(asset.ID)
		if id == "" || seen[id] || len(asset.Prompt) > 8000 || len(asset.Role) > 40 || len(asset.SourceType) > 40 || len(asset.SourceService) > 200 || len(asset.DataBase64) > 45<<20 || asset.SourceAssetID != "" && normalizedStudioUUID(asset.SourceAssetID) == "" || asset.SourceProjectID != "" && normalizedStudioUUID(asset.SourceProjectID) == "" {
			return nil, 0, fail
		}
		data, err := base64.StdEncoding.DecodeString(asset.DataBase64)
		if err != nil {
			return nil, 0, fail
		}
		mime, media, dimensions, err := companionImportMediaType(data)
		if err != nil || mime != asset.MimeType || asset.Filename != strings.ToLower(id)+companionMediaExtension(mime) {
			return nil, 0, fail
		}
		total += int64(len(data))
		if total > companionImportBundleBytes {
			return nil, 0, errors.New("The complete project contains more than 64 MB. Reduce its media before sending.")
		}
		asset.ID = id
		seen[id] = true
		prepared = append(prepared, companionPreparedImportAsset{companionProjectImportAsset: asset, data: data, media: media, dimensions: dimensions})
	}
	if total > companionImportBundleBytes {
		return nil, 0, errors.New("The complete project contains more than 64 MB.")
	}
	if request.SourceSnapshot != "" {
		var snapshot map[string]any
		if normalizedStudioUUID(request.SourceProjectID) == "" || json.Unmarshal([]byte(request.SourceSnapshot), &snapshot) != nil || snapshot == nil || normalizedStudioUUID(fmt.Sprint(snapshot["id"])) != normalizedStudioUUID(request.SourceProjectID) {
			return nil, 0, fail
		}
		var complete func(any, string) bool
		complete = func(value any, key string) bool {
			switch value := value.(type) {
			case map[string]any:
				for childKey, child := range value {
					if key == "videoLinks" && (!seen[normalizedStudioUUID(childKey)] || !seen[normalizedStudioUUID(fmt.Sprint(child))]) {
						return false
					}
					if !complete(child, childKey) {
						return false
					}
				}
			case []any:
				for _, child := range value {
					if !complete(child, key) {
						return false
					}
				}
			case string:
				if key == "referenceAssetIDs" || key == "generatedAssetIDs" || key == "inputAssetIDs" || key == "assetID" || key == "customIconAssetID" || key == "personalizationReferenceAssetID" || key == "videoLinks" {
					if !seen[normalizedStudioUUID(value)] {
						return false
					}
				}
			}
			return true
		}
		if !complete(snapshot, "") {
			return nil, 0, fail
		}
	}
	return prepared, total, nil
}

var companionImportAssetURL = regexp.MustCompile(`(?i)(?:file:[^\s"'<>]*?/)?assets/[a-z0-9_.%-]+`)
var companionImportTemporaryURL = regexp.MustCompile(`(?i)(?:src|poster)\s*=\s*["'](?:blob:|file:|https?://(?:localhost|127\.0\.0\.1))`)
var companionImportMetaTag = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
var companionImportCSPHeader = regexp.MustCompile(`(?i)\bhttp-equiv\s*=\s*["']content-security-policy["']`)
var companionImportImagePolicy = regexp.MustCompile(`(?i)img-src\s+data:\s*(;|"|>)`)

// Only broaden the iPhone's data-only image directive to allow the saved images.
func companionImportedImageCSP(html string) string {
	return companionImportMetaTag.ReplaceAllStringFunc(html, func(tag string) string {
		if !companionImportCSPHeader.MatchString(tag) {
			return tag
		}
		return companionImportImagePolicy.ReplaceAllString(tag, `img-src data: 'self'$1`)
	})
}

func completeCompanionImportAssets(request companionPrototypeImport, assets []companionPreparedImportAsset, html string, embedded map[string][]byte, parts []map[string]any, total int64) ([]companionPreparedImportAsset, string, int64, error) {
	hashes := map[string]string{}
	for _, asset := range assets {
		hashes[chatSourceHash(string(asset.data))] = asset.Filename
	}
	names := []string{}
	for name := range embedded {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := embedded[name]
		mime, media, dimensions, err := companionImportMediaType(data)
		if err != nil {
			return nil, "", 0, err
		}
		alias := hashes[chatSourceHash(string(data))]
		if alias == "" {
			total += int64(len(data))
			hashes[chatSourceHash(string(data))] = name
		}
		assets = append(assets, companionPreparedImportAsset{companionProjectImportAsset: companionProjectImportAsset{Filename: name, MimeType: mime, Role: "generated", Prompt: request.Prompt, SourceService: "Glowbom iPhone", SourceType: "extracted"}, data: data, media: media, dimensions: dimensions, aliasOf: alias})
	}
	for _, part := range parts {
		url, _ := part["url"].(string)
		filename, _ := part["filename"].(string)
		_, encoded, ok := strings.Cut(url, ",")
		if !ok {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, "", 0, err
		}
		mime, media, dimensions, err := companionImportMediaType(data)
		if err != nil {
			return nil, "", 0, err
		}
		assets = append(assets, companionPreparedImportAsset{companionProjectImportAsset: companionProjectImportAsset{Filename: filename, MimeType: mime, Role: "input", SourceService: "Glowbom iPhone", SourceType: "uploaded"}, data: data, media: media, dimensions: dimensions})
		total += int64(len(data))
	}
	count := 0
	filenames := map[string]string{}
	for _, asset := range assets {
		if asset.aliasOf == "" {
			count++
		}
		key := strings.ToLower(asset.Filename)
		if old := filenames[key]; old != "" && old != asset.Filename {
			return nil, "", 0, errors.New("Two saved assets use the same filename.")
		}
		filenames[key] = asset.Filename
		if asset.MimeType == "image/jpeg" {
			filenames[strings.TrimSuffix(key, ".jpg")+".jpeg"] = asset.Filename
		}
	}
	if count > 64 || total > companionImportBundleBytes {
		return nil, "", 0, errors.New("Send at most 64 original media files and 64 MB per project.")
	}
	var failure error
	html = companionImportAssetURL.ReplaceAllStringFunc(html, func(path string) string {
		name := strings.ToLower(path[strings.LastIndex(path, "/")+1:])
		if filename, ok := filenames[name]; ok {
			return "assets/" + filename
		}
		failure = errors.New("A prototype picture is missing from the saved media. Restore its original in Studio before sending.")
		return path
	})
	if companionImportTemporaryURL.MatchString(html) {
		failure = errors.New("The preview contains temporary media links. Save the original media in Studio before sending.")
	}
	if len(assets) > 0 {
		html = companionImportedImageCSP(html)
	}
	return assets, html, total, failure
}

func saveCompanionImportSources(root, record string, request companionPrototypeImport, assets []companionPreparedImportAsset) error {
	dir, err := chatWriteDirectory(root, ".glowbom/phone-import")
	if err != nil {
		return err
	}
	files := map[string][]byte{"saved-preview.html": []byte(request.HTML), "original.html": []byte(request.OriginalHTML), "source-project.json": []byte(request.SourceSnapshot), "prompt.txt": []byte(request.Prompt)}
	metadata := append([]companionProjectImportAsset{}, request.Assets...)
	for i := range metadata {
		metadata[i].DataBase64 = ""
	}
	files["assets.json"], err = json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	for name, data := range files {
		if len(data) > 0 {
			if err = atomicChatFile(dir, name, data); err != nil {
				return err
			}
		}
	}
	assetDir, err := chatWriteDirectory(root, "prototype/assets")
	if err != nil {
		return err
	}
	inputDir, err := chatWriteDirectory(root, "inputs")
	if err != nil {
		return err
	}
	var history map[string]json.RawMessage
	_, historyData, err := readStudioJSON(filepath.Join(record, "request.json"), 2<<20)
	if err != nil {
		return err
	}
	if json.Unmarshal(historyData, &history) != nil {
		return errors.New("Could not preserve prototype input history.")
	}
	attachments := []map[string]string{}
	_ = json.Unmarshal(history["attachments"], &attachments)
	for _, asset := range assets {
		path := filepath.Join(assetDir, asset.Filename)
		if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			if err = createChatImageFile(assetDir, asset.Filename, asset.data); err != nil {
				return err
			}
		} else {
			data, readErr := readCompanionProjectMedia(root, "prototype/assets/"+asset.Filename, companionImportVideoBytes)
			if readErr != nil || !bytes.Equal(data, asset.data) {
				return errors.New("A saved prototype image changed during transfer.")
			}
		}
		if !companionImportInputRole(asset.Role) {
			continue
		}
		if err = createChatImageFile(inputDir, asset.Filename, asset.data); err != nil {
			return err
		}
		inputs, err := chatWriteDirectory(root, filepath.ToSlash(filepath.Join(strings.TrimPrefix(record, root+string(filepath.Separator)), "inputs")))
		if err != nil {
			return err
		}
		if err = createChatImageFile(inputs, asset.Filename, asset.data); err != nil {
			return err
		}
		attachments = append(attachments, map[string]string{"filename": asset.Filename, "path": "inputs/" + asset.Filename})
	}
	history["attachments"], _ = json.Marshal(attachments)
	data, _ := json.MarshalIndent(history, "", "  ")
	return atomicChatFile(record, "request.json", data)
}

func readCompanionProjectMedia(root, relative string, limit int64) ([]byte, error) {
	if !filepath.IsLocal(relative) || filepath.ToSlash(filepath.Clean(relative)) != relative || strings.Contains(relative, `\`) {
		return nil, errors.New("Invalid saved media path.")
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	info, err := handle.Lstat(relative)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("The saved media is missing or changed.")
	}
	file, err := handle.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("The saved media is too large.")
	}
	return data, err
}

// The caller holds studioProjectMu. Preserve raw bytes instead of normalizing images.
func linkCompanionExactStudioAssetLocked(root, projectID, requestID string, asset companionPreparedImportAsset) (companionImportedAssetReceipt, error) {
	generationID := "phone-" + requestID + "-" + chatSourceHash(asset.Filename)[:32]
	id := studioGenerationAssetID(generationID)
	fields, _, err := readStudioProjectState(root)
	if err != nil {
		return companionImportedAssetReceipt{}, err
	}
	links := map[string]string{}
	if len(fields["assets"]) > 0 && json.Unmarshal(fields["assets"], &links) != nil {
		return companionImportedAssetReceipt{}, errors.New("The project image links could not be read.")
	}
	linked := links["prototype/assets/"+asset.Filename]
	if linked != "" {
		id = linked
	}
	candidate, err := readStudioAssetForLink(id)
	if err == nil {
		data, _, readErr := decodeBase64Payload(candidate.DataBase64, asset.MimeType)
		if readErr != nil || !bytes.Equal(data, asset.data) || candidate.MediaType != asset.media {
			return companionImportedAssetReceipt{}, errors.New("The existing Studio original changed. Its current content was kept.")
		}
	} else if errors.Is(err, os.ErrNotExist) && linked == "" {
		source := asset.SourceService
		if source == "" {
			source = "Glowbom iPhone"
		}
		sourceType := asset.SourceType
		if sourceType == "" {
			sourceType = "uploaded"
		}
		assetType := "generated"
		if companionImportInputRole(asset.Role) {
			assetType = "reference"
		}
		candidate, err = saveStudioAsset(studioSaveOptions{GenerationID: generationID, Prompt: asset.Prompt, DataURI: "data:" + asset.MimeType + ";base64," + base64.StdEncoding.EncodeToString(asset.data), MediaType: asset.media, MimeType: asset.MimeType, Source: source, AssetType: assetType, SourceType: sourceType, SourceAssetID: asset.SourceAssetID, SourceProjectID: projectID, UsedInProjects: []string{projectID}, Dimensions: asset.dimensions})
		if err != nil {
			return companionImportedAssetReceipt{}, err
		}
	} else {
		return companionImportedAssetReceipt{}, err
	}
	if !studioHasProject(append(append([]string{}, candidate.UsedInProjects...), candidate.SourceProjectID), projectID) {
		return companionImportedAssetReceipt{}, errors.New("The Studio original belongs to another project.")
	}
	return companionImportedAssetReceipt{Path: "prototype/assets/" + asset.Filename, StudioID: candidate.ID, SourceID: asset.ID, Hash: chatSourceHash(string(asset.data)), ByteCount: len(asset.data)}, nil
}

func saveCompanionImportStudioAssets(ctx context.Context, root, requestID string, assets []companionPreparedImportAsset, record *companionTransferRecord) error {
	studioProjectMu.Lock()
	defer studioProjectMu.Unlock()
	fields, previous, err := readStudioProjectState(root)
	if err != nil {
		return err
	}
	if studioJSONString(fields, "projectID") == "" {
		fields["projectID"], _ = json.Marshal(studioGenerationAssetID("phone-project-" + requestID))
		if err = saveStudioProjectState(root, fields, previous); err != nil {
			return err
		}
	}
	project, err := registerStudioProjectLocked(root)
	if err != nil {
		return err
	}
	fields, previous, err = readStudioProjectState(root)
	if err != nil {
		return err
	}
	links := map[string]string{}
	if json.Unmarshal(fields["assets"], &links) != nil && len(fields["assets"]) > 0 {
		return errors.New("The project image links could not be read.")
	}
	identities := map[string]string{}
	receipts := map[string]companionImportedAssetReceipt{}
	for _, asset := range assets {
		if err = ctx.Err(); err != nil {
			return err
		}
		var receipt companionImportedAssetReceipt
		if asset.aliasOf != "" {
			receipt = receipts[asset.aliasOf]
			if receipt.StudioID == "" {
				return errors.New("An embedded image could not be associated with its original.")
			}
			receipt.Path = "prototype/assets/" + asset.Filename
		} else {
			receipt, err = linkCompanionExactStudioAssetLocked(root, project.ID, requestID, asset)
			if err != nil {
				return err
			}
		}
		receipts[asset.Filename] = receipt
		links[receipt.Path] = receipt.StudioID
		if asset.ID != "" {
			identities[asset.ID] = receipt.StudioID
		}
		record.ProjectAssets = append(record.ProjectAssets, receipt)
	}
	fields["assets"], _ = json.Marshal(links)
	fields["phoneAssetIds"], _ = json.Marshal(identities)
	fields["phoneImportAssetsVersion"], _ = json.Marshal(2)
	return saveStudioProjectState(root, fields, previous)
}

func verifyCompanionImportedAssets(root string, receipts []companionImportedAssetReceipt) error {
	if len(receipts) == 0 {
		return nil
	}
	fields, _, err := readStudioProjectState(root)
	if err != nil {
		return err
	}
	projectID := studioJSONString(fields, "projectID")
	links := map[string]string{}
	if normalizedStudioUUID(projectID) == "" || json.Unmarshal(fields["assets"], &links) != nil {
		return errors.New("The saved project image links changed.")
	}
	for _, receipt := range receipts {
		data, err := readCompanionProjectMedia(root, receipt.Path, companionImportVideoBytes)
		if err != nil || len(data) != receipt.ByteCount || chatSourceHash(string(data)) != receipt.Hash {
			return errors.New("The saved original media changed.")
		}
		asset, err := readStudioAssetForLink(receipt.StudioID)
		if err != nil {
			return err
		}
		if links[receipt.Path] != asset.ID || !studioHasProject(normalizedStudioUUIDs(append(append([]string{}, asset.UsedInProjects...), asset.SourceProjectID)), projectID) {
			return errors.New("The original media is no longer linked to this project.")
		}
		raw, _, err := decodeBase64Payload(asset.DataBase64, http.DetectContentType(data))
		if err != nil || !bytes.Equal(raw, data) {
			return errors.New("The Studio original changed.")
		}
	}
	return nil
}

func writeCompanionProjectImportReceipt(w http.ResponseWriter, project companionProject, record companionTransferRecord) {
	receipt := map[string]any{"id": project.ID, "name": project.Name, "assetCount": project.AssetCount, "available": project.Available, "assetsSaved": true, "fullProjectTemplate": record.FullProjectTemplate, "suppliedAssetCount": record.SuppliedAssetCount, "inputCount": record.InputCount, "sourceProjectId": record.SourceProjectID, "savedPath": record.ProjectPath}
	if project.CreatedAt != "" {
		receipt["createdAt"] = project.CreatedAt
	}
	if record.DestinationID != "" {
		receipt["destinationId"], receipt["storagePath"] = record.DestinationID, record.ProjectPath
	}
	writeJSON(w, receipt)
}
