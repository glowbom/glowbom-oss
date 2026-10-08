package main

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
)

// Only the installer's creation inventory is owned by this rollback. Later
// prototype files, replacements, and local edits remain in the partial folder.
func cleanupCompanionStarter(parent *os.Root, name string, original os.FileInfo, created []starterCreatedPath) bool {
	if parent == nil || original == nil || !original.IsDir() || original.Mode()&os.ModeSymlink != 0 || name == "." || name != filepath.Base(name) || !filepath.IsLocal(name) {
		return false
	}
	for _, entry := range created {
		if entry.info == nil || entry.name == "." || !filepath.IsLocal(entry.name) || filepath.Clean(entry.name) != entry.name {
			return false
		}
	}
	currentScope := func() bool {
		openedParent, err := parent.Stat(".")
		if err != nil {
			return false
		}
		visibleParent, err := os.Stat(parent.Name())
		if err != nil || !os.SameFile(openedParent, visibleParent) {
			return false
		}
		current, err := parent.Lstat(name)
		return err == nil && current.IsDir() && current.Mode() == original.Mode() && os.SameFile(current, original)
	}
	if !currentScope() {
		return false
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return false
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, original) {
		return false
	}
	for index := len(created) - 1; index >= 0; index-- {
		entry := created[index]
		if !currentScope() {
			return false
		}
		current, err := root.Lstat(entry.name)
		if err != nil || current.Mode() != entry.info.Mode() || !os.SameFile(current, entry.info) {
			continue
		}
		if entry.info.IsDir() {
			// Remove only an unchanged directory that is now empty.
			_ = root.Remove(entry.name)
			continue
		}
		if !current.Mode().IsRegular() || current.Size() != entry.size || !current.ModTime().Equal(entry.info.ModTime()) {
			continue
		}
		file, err := root.Open(entry.name)
		if err != nil {
			continue
		}
		fileInfo, statErr := file.Stat()
		digest := sha256.New()
		count, readErr := io.Copy(digest, io.LimitReader(file, entry.size+1))
		closeErr := file.Close()
		if statErr != nil || !os.SameFile(fileInfo, entry.info) || readErr != nil || closeErr != nil || count != entry.size || !bytes.Equal(digest.Sum(nil), entry.hash[:]) {
			continue
		}
		current, err = root.Lstat(entry.name)
		if err != nil || !os.SameFile(current, entry.info) || current.Mode() != entry.info.Mode() || current.Size() != entry.size || !current.ModTime().Equal(entry.info.ModTime()) {
			continue
		}
		if !currentScope() {
			return false
		}
		_ = root.Remove(entry.name)
	}
	if !currentScope() {
		return false
	}
	return parent.Remove(name) == nil
}
