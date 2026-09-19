package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/zalando/go-keyring"
)

const credentialService = "Glowbom CLI"

var errAccountSignedOut = errors.New("you are not signed in; run glowbom login")
var errAccountInvalid = errors.New("saved sign-in is invalid; run glowbom login again")

type credentialStore struct {
	key  string
	file string
}

func newCredentialStore(apiURL string) (accountStore, error) {
	hash := sha256.Sum256([]byte(apiURL))
	store := &credentialStore{key: hex.EncodeToString(hash[:])}
	mode := os.Getenv("GLOWBOM_CREDENTIAL_STORE")
	if mode == "" || mode == "keyring" {
		return store, nil
	}
	if mode != "file" || runtime.GOOS == "windows" {
		return nil, errors.New("use the system keyring, or explicitly select file storage on macOS/Linux")
	}
	root := os.Getenv("GLOWBOM_CONFIG_DIR")
	if root == "" {
		var err error
		root, err = os.UserConfigDir()
		if err != nil {
			return nil, errors.New("could not find your account configuration directory")
		}
		root = filepath.Join(root, "glowbom")
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("GLOWBOM_CONFIG_DIR must be an absolute path outside your project")
	}
	store.file = filepath.Join(root, "accounts", store.key+".json")
	return store, nil
}

func (s *credentialStore) Load() (accountCredentials, error) {
	var value accountCredentials
	var data []byte
	if s.file == "" {
		text, err := keyring.Get(credentialService, s.key)
		if errors.Is(err, keyring.ErrNotFound) {
			return value, errAccountSignedOut
		}
		if err != nil {
			return value, errors.New("could not read the system keyring; unlock it and retry")
		}
		data = []byte(text)
	} else {
		if err := privateCredentialPath(s.file, false); err != nil {
			return value, err
		}
		var err error
		data, err = os.ReadFile(s.file)
		if err != nil {
			return value, errors.New("could not read saved credentials; run glowbom login")
		}
	}
	if len(data) > 65536 || json.Unmarshal(data, &value) != nil {
		return value, errors.New("saved credentials are invalid; run glowbom login again")
	}
	return value, nil
}

func (s *credentialStore) Save(value accountCredentials) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("could not prepare credentials for storage")
	}
	if s.file == "" {
		if err := keyring.Set(credentialService, s.key, string(data)); err != nil {
			return errors.New("could not save to the system keyring; on a headless macOS/Linux machine, explicitly set GLOWBOM_CREDENTIAL_STORE=file and log in again")
		}
		return nil
	}
	dir := filepath.Dir(s.file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("could not create the private credential directory")
	}
	if err := privateCredentialPath(s.file, true); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".login-*")
	if err != nil {
		return errors.New("could not create the private credential file")
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		return errors.New("could not save credentials")
	}
	if err := os.Rename(tmp.Name(), s.file); err != nil {
		return errors.New("could not replace the credential file")
	}
	return nil
}

func (s *credentialStore) Delete() error {
	if s.file == "" {
		err := keyring.Delete(credentialService, s.key)
		if err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return errors.New("could not remove credentials from the system keyring")
		}
		return nil
	}
	if err := os.Remove(s.file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not remove the local credential file")
	}
	return nil
}

func privateCredentialPath(path string, allowMissing bool) error {
	dir, err := os.Lstat(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return errAccountSignedOut
	}
	if err != nil {
		return errors.New("could not read the private credential directory")
	}
	if !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 || dir.Mode().Perm()&0o077 != 0 {
		return errors.New("the credential directory must be private (mode 0700) and must not be a symlink")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if allowMissing {
			return nil
		}
		return errAccountSignedOut
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("the credential file must be private (mode 0600) and must not be a symlink")
	}
	return nil
}
