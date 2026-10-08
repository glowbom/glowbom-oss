package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var companionLegacyInlineName = regexp.MustCompile(`^phone-inline-([0-9a-f]{24})\.(png|jpg|webp|gif)$`)

// Called with studioProjectMu held. Never acquire companionTransferMu here:
// imports acquire the transfer lock before entering Studio.
func repairCompanionPhoneImagesLocked(path, projectID string) error {
	studio, err := studioRootDirectory()
	if err != nil {
		return err
	}
	canonicalStudio, err := filepath.EvalSymlinks(studio)
	if err != nil {
		return nil
	}
	parent := filepath.Join(canonicalStudio, "PhoneProjects")
	if filepath.Dir(path) != parent {
		return nil
	}
	requestID := normalizedStudioUUID(filepath.Base(path))
	if requestID == "" || path != filepath.Join(parent, strings.ToLower(requestID)) {
		return nil
	}
	receiptPath := filepath.Join(canonicalStudio, "PhoneImports", strings.ToLower(requestID)+".json")
	_, data, err := readStudioJSON(receiptPath, 128<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var record companionTransferRecord
	if json.Unmarshal(data, &record) != nil || record.Kind != "project" || record.Project == nil || record.ProjectPath != path || record.Project.ID != companionProjectID(path) {
		return nil
	}
	canonical, err := chatProjectRoot(path)
	if err != nil || canonical != path {
		return errors.New("The saved phone project is unavailable.")
	}
	fields, previous, err := readStudioProjectState(path)
	if err != nil {
		return err
	}
	var version int
	_ = json.Unmarshal(fields["phoneImportAssetsVersion"], &version)
	if version >= 2 {
		return nil
	}
	if studioJSONString(fields, "projectID") != projectID {
		return errors.New("The saved phone project identity changed.")
	}
	handle, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	folder, err := handle.Open("prototype/assets")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	files, readErr := folder.ReadDir(129)
	folder.Close()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if len(files) > 128 {
		return errors.New("The saved phone project has too many files to repair safely.")
	}
	assets := []companionPreparedImportAsset{}
	total := 0
	for _, file := range files {
		name := file.Name()
		match := companionLegacyInlineName.FindStringSubmatch(name)
		if len(match) == 0 {
			continue
		}
		image, err := readCompanionProjectMedia(path, "prototype/assets/"+name, companionImportImageBytes)
		if err != nil {
			return err
		}
		mime, media, dimensions, err := companionImportMediaType(image)
		if err != nil || media != "image" || companionMediaExtension(mime) != "."+match[2] || !strings.HasPrefix(chatSourceHash(string(image)), match[1]) {
			return errors.New("A saved phone image changed. Its current content was kept.")
		}
		total += len(image)
		if len(assets) >= 64 || total > companionImportBundleBytes {
			return errors.New("The saved phone images exceed the safe repair limit.")
		}
		assets = append(assets, companionPreparedImportAsset{companionProjectImportAsset: companionProjectImportAsset{Filename: name, MimeType: mime, Role: "generated", Prompt: "Saved iPhone prototype image", SourceService: "Glowbom iPhone", SourceType: "extracted"}, data: image, media: media, dimensions: dimensions})
	}
	if len(assets) == 0 {
		return nil
	}
	// Validate every source before writing any registration or image policy.
	links := map[string]string{}
	if len(fields["assets"]) > 0 && json.Unmarshal(fields["assets"], &links) != nil {
		return errors.New("The saved phone image links could not be read.")
	}
	receipts := []companionImportedAssetReceipt{}
	for _, asset := range assets {
		receipt, err := linkCompanionExactStudioAssetLocked(path, projectID, requestID, asset)
		if err != nil {
			return err
		}
		links[receipt.Path] = receipt.StudioID
		receipts = append(receipts, receipt)
	}
	if err = repairCompanionPhoneCSP(path); err != nil {
		return err
	}
	fields["assets"], _ = json.Marshal(links)
	fields["phoneImportAssetsVersion"], _ = json.Marshal(2)
	fields["phoneImportedAssets"], _ = json.Marshal(receipts)
	return saveStudioProjectState(path, fields, previous)
}

func repairCompanionPhoneCSP(path string) error {
	chatResultMu.Lock()
	defer chatResultMu.Unlock()
	current, err := readChatProjectFile(path, "prototype/index.html", maxChatResultBytes)
	if err != nil {
		return err
	}
	updated := companionImportedImageCSP(current)
	if updated == current {
		return nil
	}
	source, err := chatWriteDirectory(path, ".glowbom/phone-import")
	if err != nil {
		return err
	}
	backup := filepath.Join(source, "legacy-preview.html")
	file, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, writeErr := file.Write([]byte(current))
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	latest, err := readChatProjectFile(path, "prototype/index.html", maxChatResultBytes)
	if err != nil || !bytes.Equal([]byte(current), []byte(latest)) {
		return errors.New("The prototype changed while checking its saved images. Its newer content was kept.")
	}
	directory, err := chatWriteDirectory(path, "prototype")
	if err != nil {
		return err
	}
	return atomicChatFile(directory, "index.html", []byte(updated))
}
