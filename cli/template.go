package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"time"
)

const templateURL = "https://audio.glowbom.com/starter.zip"
const maxTemplateArchiveBytes = 8 * 1024 * 1024
const maxTemplateUncompressedBytes = 16 * 1024 * 1024
const maxTemplateEntries = 512

const templateUsage = `Usage: glowbom template [--output NEW_DIRECTORY]

Download a clean Glowbom starter with Apple, Android, and web project files.
No Glowbom login is required. This does not download your saved generation.
Without --output, creates a unique glowbom-template-* folder in the current directory.
The output's parent must already exist. Existing files and folders are never replaced.

The command fetches the current starter from https://audio.glowbom.com/starter.zip.
It checks the ZIP before extracting. It does not run downloaded code
or install development tools and dependencies. Use glowbom pull for your saved project.
`

func parseTemplateOptions(args []string) (pullOptions, error) {
	opts, err := parsePullOptions(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		return opts, errors.New(strings.ReplaceAll(err.Error(), "pull only accepts", "template only accepts"))
	}
	return opts, err
}

func validTemplateURL(address *url.URL) bool {
	if address == nil || address.Scheme != "https" || address.User != nil || address.Fragment != "" ||
		(address.Port() != "" && address.Port() != "443") {
		return false
	}
	return strings.EqualFold(address.Hostname(), "audio.glowbom.com") && address.EscapedPath() == "/starter.zip"
}

func downloadTemplate(ctx context.Context, client *http.Client) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	downloadClient := *client
	downloadClient.Timeout = 90 * time.Second
	downloadClient.Jar = nil
	downloadClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !validTemplateURL(request.URL) {
			return errors.New("the starter download returned an unsupported redirect")
		}
		request.Header.Del("Authorization")
		request.Header.Del("Cookie")
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, templateURL, nil)
	if err != nil {
		return nil, errors.New("the starter download address is invalid")
	}
	request.Header.Set("Accept", "application/zip, application/octet-stream")
	request.Header.Set("Cache-Control", "no-cache")
	response, err := downloadClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("starter download canceled")
		}
		return nil, errors.New("could not download the starter; check your connection and try again")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the starter could not be downloaded (HTTP %d); try again later", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/zip" && mediaType != "application/octet-stream") {
		return nil, errors.New("the starter download did not return a ZIP archive")
	}
	if response.ContentLength > maxTemplateArchiveBytes {
		return nil, errors.New("the starter archive exceeds the 8 MiB download limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTemplateArchiveBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("starter download canceled")
		}
		return nil, errors.New("the starter download was interrupted; try again")
	}
	if len(data) > maxTemplateArchiveBytes {
		return nil, errors.New("the starter archive exceeds the 8 MiB download limit")
	}
	return data, nil
}

func validateTemplateArchive(ctx context.Context, archive []byte) ([]projectArchiveEntry, error) {
	invalid := errors.New("the downloaded starter is not a valid supported ZIP archive")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(archive) > maxTemplateArchiveBytes {
		return nil, invalid
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxTemplateEntries {
		return nil, invalid
	}
	entries := make([]projectArchiveEntry, 0, len(reader.File))
	spellings := map[string]string{}
	kinds := map[string]bool{}
	total := 0
	manifestFound := false
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(file.Name, "/")
		isDirectory := file.Mode().IsDir()
		if !validProjectArchivePath(name) || (!isDirectory && !file.Mode().IsRegular()) ||
			file.Flags&(1|64) != 0 || (file.Method != zip.Store && file.Method != zip.Deflate) ||
			(strings.HasSuffix(file.Name, "/") != isDirectory) {
			return nil, invalid
		}
		if name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") || path.Base(name) == ".DS_Store" {
			continue
		}
		parts := strings.Split(name, "/")
		for index := 1; index <= len(parts); index++ {
			prefix := strings.Join(parts[:index], "/")
			folded := strings.ToLower(prefix)
			isFile := index == len(parts) && !isDirectory
			if previous, exists := spellings[folded]; exists && (previous != prefix || kinds[folded] || isFile) {
				return nil, invalid
			}
			spellings[folded] = prefix
			kinds[folded] = isFile
		}
		if isDirectory {
			continue
		}
		limit := maxTemplateUncompressedBytes - total
		if file.UncompressedSize64 > uint64(limit) {
			return nil, errors.New("the starter exceeds the supported unpacked size limit")
		}
		stream, err := file.Open()
		if err != nil {
			return nil, invalid
		}
		data, readErr := io.ReadAll(io.LimitReader(&projectContextReader{ctx: ctx, reader: stream}, int64(limit)+1))
		closeErr := stream.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil || closeErr != nil || len(data) > limit {
			return nil, invalid
		}
		total += len(data)
		mode := os.FileMode(0644)
		if name == "android/gradlew" && file.Mode()&0111 != 0 {
			mode = 0755
		}
		if name == "glowbom.json" {
			manifestFound = json.Valid(data)
		}
		entries = append(entries, projectArchiveEntry{name: name, data: data, mode: mode})
	}
	if !manifestFound || len(entries) == 0 {
		return nil, invalid
	}
	return entries, nil
}

func createTemplate(ctx context.Context, opts pullOptions, client *http.Client, out io.Writer) error {
	if ctx.Err() != nil {
		return errors.New("starter download canceled")
	}
	output, err := prepareDirectoryOutput(opts.Output, "glowbom-template")
	if err != nil {
		return err
	}
	defer output.Close()
	fmt.Fprintln(out, "Downloading clean starter project...")
	archive, err := downloadTemplate(ctx, client)
	if err != nil {
		return err
	}
	entries, err := validateTemplateArchive(ctx, archive)
	if err != nil {
		return err
	}
	path, count, err := output.extractEntries(ctx, entries)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved starter: %s\nDownloaded %d files. No account login required.\n", path, count)
	return nil
}

func runTemplateCommand(args []string) int {
	opts, err := parseTemplateOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(templateUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", err)
		fmt.Fprint(os.Stderr, templateUsage)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	err = createTemplate(ctx, opts, nil, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", err)
		return 1
	}
	return 0
}
