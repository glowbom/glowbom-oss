package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const studioClipToolsRelease = "b6.1.1"

var studioClipSetupMu sync.Mutex

func studioClipToolsDirectory() (string, error) {
	root, err := studioRootDirectory()
	return filepath.Join(root, "VideoTools", studioClipToolsRelease+"-"+runtime.GOOS+"-"+runtime.GOARCH), err
}

func studioClipTools() (string, string, error) {
	dir, _ := studioClipToolsDirectory()
	resolve := func(name, override string) string {
		if configured := strings.TrimSpace(os.Getenv(override)); configured != "" {
			if path, err := exec.LookPath(configured); err == nil {
				return path
			}
			return ""
		}
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
		candidates := []string{filepath.Join(dir, name), filepath.Join("/opt/homebrew/bin", name), filepath.Join("/usr/local/bin", name)}
		if executable, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(executable), name))
		}
		for _, path := range candidates {
			if path, err := exec.LookPath(path); err == nil {
				return path
			}
		}
		return ""
	}
	ffmpeg, ffprobe := resolve("ffmpeg", "GLOWBOM_FFMPEG_BIN"), resolve("ffprobe", "GLOWBOM_FFPROBE_BIN")
	if ffmpeg == "" || ffprobe == "" {
		return "", "", errors.New("Enable video editing to download the local video tools.")
	}
	return ffmpeg, ffprobe, nil
}

type studioClipToolDownload struct{ name, digest string }

func studioClipToolDownloads() []studioClipToolDownload {
	if runtime.GOOS != "darwin" {
		return nil
	}
	switch runtime.GOARCH {
	case "arm64":
		return []studioClipToolDownload{
			{"ffmpeg-darwin-arm64.gz", "8923876afa8db5585022d7860ec7e589af192f441c56793971276d450ed3bbfa"},
			{"ffprobe-darwin-arm64.gz", "d986a8ec7b030899fe66a8a288ed809a3543338705a3ce178cfb85869c5d80be"},
			{"darwin-arm64.LICENSE", "cb48bf09a11f5fb576cddb0431c8f5ed0a60157a9ec942adffc13907cbe083f2"},
			{"darwin-arm64.README", "05ba4b92c96605434b1aaae3eedf5a2c280c9607bf78ffca9a5b536d9af2dc6a"},
		}
	case "amd64":
		return []studioClipToolDownload{
			{"ffmpeg-darwin-x64.gz", "929b375c1182d956c51f7ac25e0b2b0411fb01f6f407aa15c9758efeb4242106"},
			{"ffprobe-darwin-x64.gz", "d4da574d6e2e197bd259b47d69cf262df9e312af24ad960444f6d806d3d4c186"},
			{"darwin-x64.LICENSE", "2e1d16c72fd74e12063776371da757322f8b77589386532f4fd8634bde7de1af"},
			{"darwin-x64.README", "e88a0325f8e5b75210355e37341824f074d3cd82def2125be54c914b62848a36"},
		}
	}
	return nil
}

func installStudioClipTools(ctx context.Context) error {
	if !studioClipSetupMu.TryLock() {
		return errors.New("Video tools are already being prepared. Try checking again shortly.")
	}
	defer studioClipSetupMu.Unlock()
	if _, _, err := studioClipTools(); err == nil {
		return nil
	}
	downloads := studioClipToolDownloads()
	if len(downloads) == 0 {
		return errors.New("Install FFmpeg and ffprobe on this computer, then choose Check again.")
	}
	dir, err := studioClipToolsDirectory()
	if err != nil {
		return errors.New("Could not locate the video tools folder.")
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return errors.New("Could not create the video tools folder.")
	}
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("The video tools folder is unavailable.")
	}
	if _, err := os.Lstat(dir); err == nil {
		return errors.New("The local video tools are incomplete. Install FFmpeg and ffprobe, then check again.")
	} else if !os.IsNotExist(err) {
		return errors.New("Could not inspect the video tools folder.")
	}
	temp, err := os.MkdirTemp(parent, ".video-tools-")
	if err != nil {
		return errors.New("Could not prepare the video tools download.")
	}
	defer os.RemoveAll(temp)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 4 || request.URL.Scheme != "https" || (request.URL.Hostname() != "github.com" && request.URL.Hostname() != "release-assets.githubusercontent.com") {
			return errors.New("Video tools download redirect is not allowed")
		}
		return nil
	}}
	for _, download := range downloads {
		if err := downloadStudioClipTool(ctx, client, temp, download); err != nil {
			return errors.New("Could not download and verify the video tools. Check your connection and try again.")
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.New("The video tools download was stopped.")
	}
	if err := os.Rename(temp, dir); err != nil {
		return errors.New("Could not finish installing the local video tools.")
	}
	return nil
}

func downloadStudioClipTool(ctx context.Context, client *http.Client, dir string, download studioClipToolDownload) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/eugeneware/ffmpeg-static/releases/download/"+studioClipToolsRelease+"/"+download.name, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Video tools download failed")
	}
	compressed, err := os.CreateTemp(dir, ".download-")
	if err != nil {
		return err
	}
	defer compressed.Close()
	defer os.Remove(compressed.Name())
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(compressed, hash), io.LimitReader(response.Body, (40<<20)+1))
	if err != nil || n > 40<<20 || hex.EncodeToString(hash.Sum(nil)) != download.digest {
		return errors.New("Video tools checksum did not match")
	}
	if _, err := compressed.Seek(0, io.SeekStart); err != nil {
		return err
	}
	name := download.name
	var reader io.Reader = compressed
	mode := os.FileMode(0600)
	if strings.HasSuffix(name, ".gz") {
		gz, err := gzip.NewReader(compressed)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader = gz
		name, _, _ = strings.Cut(name, "-")
		mode = 0700
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	n, err = io.Copy(file, io.LimitReader(&studioClipContextReader{ctx: ctx, reader: reader}, (100<<20)+1))
	if err != nil || n > 100<<20 {
		return errors.New("Video tools download is too large or incomplete")
	}
	return file.Sync()
}
