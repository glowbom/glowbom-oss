package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const studioGenerationCheckpointLimit = 64 << 10

// Provider operation identifiers and signed URLs remain private on this computer.
type studioGenerationCheckpoint struct {
	State       studioProgressState `json:"state"`
	OperationID string              `json:"operationId,omitempty"`
	VideoURL    string              `json:"videoUrl,omitempty"`
}

func studioGenerationRecordsDirectory(create bool) (string, bool, error) {
	assets, err := studioAssetsDirectory()
	if err != nil {
		return "", false, errors.New("Could not locate saved Studio generations.")
	}
	root := filepath.Dir(assets)
	if create {
		if err := os.MkdirAll(root, 0700); err != nil {
			return "", false, errors.New("Could not create the Studio generation directory.")
		}
	}
	info, err := os.Lstat(root)
	if !create && os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("The Studio generation directory is unavailable.")
	}
	dir := filepath.Join(root, "Generations")
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return "", false, errors.New("Could not create the Studio generation directory.")
		}
	}
	info, err = os.Lstat(dir)
	if !create && os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("The Studio generation directory is unavailable.")
	}
	if create {
		if err := os.Chmod(dir, 0700); err != nil {
			return "", false, errors.New("Could not protect saved Studio generations.")
		}
	}
	return dir, true, nil
}

func saveStudioGenerationCheckpoint(checkpoint studioGenerationCheckpoint) error {
	if !checkpoint.State.Found || !studioGenerationID.MatchString(checkpoint.State.ID) {
		return errors.New("Invalid Studio generation identifier.")
	}
	data, err := json.Marshal(checkpoint)
	if err != nil || len(data) > studioGenerationCheckpointLimit {
		return errors.New("The Studio generation record is too large or invalid.")
	}
	dir, _, err := studioGenerationRecordsDirectory(true)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, checkpoint.State.ID+".json")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("The Studio generation record is not a regular file.")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("Could not inspect the Studio generation record.")
	}
	temp, err := os.CreateTemp(dir, ".generation-*.json")
	if err != nil {
		return errors.New("Could not save the Studio generation record.")
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err := temp.Chmod(0600); err != nil {
		return errors.New("Could not protect the Studio generation record.")
	}
	if _, err := temp.Write(data); err != nil {
		return errors.New("Could not save the Studio generation record.")
	}
	if err := temp.Sync(); err != nil {
		return errors.New("Could not save the Studio generation record.")
	}
	if err := temp.Close(); err != nil {
		return errors.New("Could not save the Studio generation record.")
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return errors.New("Could not save the Studio generation record.")
	}
	return nil
}

func loadStudioGenerationCheckpoint(id string) (studioGenerationCheckpoint, bool, error) {
	var checkpoint studioGenerationCheckpoint
	if !studioGenerationID.MatchString(id) {
		return checkpoint, false, errors.New("Invalid Studio generation identifier.")
	}
	dir, exists, err := studioGenerationRecordsDirectory(false)
	if err != nil || !exists {
		return checkpoint, false, err
	}
	path := filepath.Join(dir, id+".json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return checkpoint, false, nil
	}
	if err != nil {
		return checkpoint, false, errors.New("Could not read the Studio generation record.")
	}
	if !info.Mode().IsRegular() || info.Size() > studioGenerationCheckpointLimit {
		return checkpoint, false, errors.New("The Studio generation record is not a regular bounded file.")
	}
	file, err := os.Open(path)
	if err != nil {
		return checkpoint, false, errors.New("Could not read the Studio generation record.")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return checkpoint, false, errors.New("The Studio generation record changed while reading.")
	}
	data, err := io.ReadAll(io.LimitReader(file, studioGenerationCheckpointLimit+1))
	if err != nil || len(data) > studioGenerationCheckpointLimit {
		return checkpoint, false, errors.New("Could not read the Studio generation record.")
	}
	if err := json.Unmarshal(data, &checkpoint); err != nil || !checkpoint.State.Found || checkpoint.State.ID != id {
		return studioGenerationCheckpoint{}, false, errors.New("The saved Studio generation record is invalid.")
	}
	return checkpoint, true, nil
}
