package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxGeneratedImageBytes = 25 << 20
const maxGeneratedImagePixels = 32 << 20

type imageOutput struct {
	file        *os.File
	temporary   string
	destination string
	automatic   bool
}

// prepareImageOutput checks the output before a paid generation starts. It does
// not create directories or replace existing files, including symbolic links.
func prepareImageOutput(output string) (*imageOutput, error) {
	automatic := output == ""
	if automatic {
		output = "."
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return nil, errors.New("could not resolve the image output path")
	}
	info, err := os.Lstat(absolute)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("image output already exists; choose a new filename or a directory")
		}
		automatic = true
	} else if !os.IsNotExist(err) {
		return nil, errors.New("could not inspect the image output path")
	} else if automatic || strings.HasSuffix(output, string(os.PathSeparator)) {
		return nil, errors.New("the image output directory does not exist")
	}
	parent := filepath.Dir(absolute)
	if automatic {
		parent = absolute
	}
	info, err = os.Stat(parent)
	if err != nil || !info.IsDir() {
		return nil, errors.New("the image output parent must be an existing directory")
	}
	// Resolve directory aliases once so a later symlink change cannot redirect
	// the download into a different directory.
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, errors.New("could not resolve the image output directory")
	}
	file, err := os.CreateTemp(parent, ".glowbom-image-*")
	if err != nil {
		return nil, errors.New("the image output directory is not writable")
	}
	result := &imageOutput{file: file, temporary: file.Name(), automatic: automatic}
	probe, err := os.CreateTemp(parent, ".glowbom-image-check-*")
	if err != nil {
		result.Close()
		return nil, errors.New("could not check the image output directory")
	}
	probePath := probe.Name()
	defer os.Remove(probePath)
	closeErr := probe.Close()
	removeErr := os.Remove(probePath)
	if closeErr != nil || removeErr != nil {
		_ = os.Remove(probePath)
		result.Close()
		return nil, errors.New("could not check the image output directory")
	}
	if err := os.Link(result.temporary, probePath); err != nil {
		result.Close()
		return nil, errors.New("this output directory does not support saving images atomically; choose another directory")
	}
	if err := os.Remove(probePath); err != nil {
		result.Close()
		return nil, errors.New("could not clean up the image output directory check")
	}
	if !automatic {
		result.destination = filepath.Join(parent, filepath.Base(absolute))
	}
	return result, nil
}

func (output *imageOutput) Close() {
	if output == nil {
		return
	}
	if output.file != nil {
		_ = output.file.Close()
		output.file = nil
	}
	if output.temporary != "" {
		_ = os.Remove(output.temporary)
		output.temporary = ""
	}
}

func validImageDownloadURL(value *url.URL) bool {
	return value != nil && value.Scheme == "https" && value.Host != "" &&
		value.Hostname() != "" && value.User == nil && value.Fragment == "" && value.Opaque == ""
}

// Save downloads without account credentials and atomically publishes the
// original image bytes. A requested filename does not convert the image format.
func (output *imageOutput) Save(ctx context.Context, client *http.Client, rawURL string) (string, error) {
	if output == nil || output.file == nil {
		return "", errors.New("image output is no longer available")
	}
	defer output.Close()
	parsed, err := url.Parse(rawURL)
	if err != nil || !validImageDownloadURL(parsed) || strings.Contains(rawURL, "#") {
		return "", errors.New("the generated image must have an HTTPS download URL without credentials or fragments")
	}
	if client == nil {
		client = http.DefaultClient
	}
	downloadClient := *client
	downloadClient.Timeout = 60 * time.Second
	downloadClient.Jar = nil
	downloadClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		hasFragment := request.Response != nil && strings.Contains(request.Response.Header.Get("Location"), "#")
		if len(via) > 3 || !validImageDownloadURL(request.URL) || hasFragment {
			return errors.New("unsafe image download redirect")
		}
		request.Header.Del("Authorization")
		request.Header.Del("Cookie")
		request.Header.Del("Proxy-Authorization")
		request.Header.Del("Referer")
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", errors.New("could not create the image download request")
	}
	request.Header.Set("Accept", "image/png, image/jpeg, image/webp")
	response, err := downloadClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("image download stopped: %w", ctx.Err())
		}
		// Transport errors can contain a signed download URL. Do not expose it.
		return "", errors.New("could not download the generated image")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image download failed (HTTP %d)", response.StatusCode)
	}
	if response.ContentLength > maxGeneratedImageBytes {
		return "", errors.New("the generated image exceeds the 25 MiB download limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGeneratedImageBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("image download stopped: %w", ctx.Err())
		}
		return "", errors.New("the image download did not finish")
	}
	if len(data) > maxGeneratedImageBytes {
		return "", errors.New("the generated image exceeds the 25 MiB download limit")
	}
	format, err := imageFileFormat(data)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("image download stopped: %w", err)
	}
	if _, err := output.file.Write(data); err != nil {
		return "", errors.New("could not write the generated image")
	}
	if err := output.file.Sync(); err != nil {
		return "", errors.New("could not finish writing the generated image")
	}
	if err := output.file.Close(); err != nil {
		output.file = nil
		return "", errors.New("could not close the generated image file")
	}
	output.file = nil
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("image download stopped: %w", err)
	}
	if output.automatic {
		unique := strings.TrimPrefix(filepath.Base(output.temporary), ".glowbom-image-")
		output.destination = filepath.Join(filepath.Dir(output.temporary), "glowbom-"+unique+"."+format)
	}
	// Creating a hard link publishes a complete file and fails if the destination
	// was created during generation. Rename would overwrite that file on Unix.
	if err := os.Link(output.temporary, output.destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", errors.New("image output appeared during generation; the existing file was preserved")
		}
		return "", errors.New("could not save the generated image without replacing another file")
	}
	return output.destination, nil
}

func imageFileFormat(data []byte) (string, error) {
	invalid := errors.New("the download is not a complete supported PNG, JPEG, or WebP image")
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		if validWebPImage(data) {
			return "webp", nil
		}
		return "", invalid
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || !validImageDimensions(config.Width, config.Height) {
		return "", invalid
	}
	if format == "png" && (len(data) < 12 || !bytes.Equal(data[len(data)-12:], []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 174, 66, 96, 130})) {
		return "", invalid
	}
	if format == "jpeg" && (len(data) < 2 || data[len(data)-2] != 0xff || data[len(data)-1] != 0xd9) {
		return "", invalid
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return "", invalid
	}
	if format == "jpeg" {
		format = "jpg"
	}
	return format, nil
}

func validImageDimensions(width, height int) bool {
	return width > 0 && height > 0 && int64(width)*int64(height) <= maxGeneratedImagePixels
}

// The standard library has no WebP decoder. Validate its complete RIFF
// container, chunk bounds, and frame headers without adding a codec dependency.
func validWebPImage(data []byte) bool {
	if len(data) < 20 || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return false
	}
	frameSeen, extended := false, false
	canvasWidth, canvasHeight := 0, 0
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return false
		}
		kind := string(data[offset : offset+4])
		size := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := uint64(start) + size
		padded := end + size%2
		if padded > uint64(len(data)) || (size%2 == 1 && data[end] != 0) {
			return false
		}
		chunk := data[start:end]
		switch kind {
		case "VP8X":
			if offset != 12 || len(chunk) != 10 || chunk[0]&0xc3 != 0 || chunk[1] != 0 || chunk[2] != 0 || chunk[3] != 0 {
				return false
			}
			extended = true
			canvasWidth, canvasHeight = webPUint24(chunk[4:7])+1, webPUint24(chunk[7:10])+1
			if !validImageDimensions(canvasWidth, canvasHeight) {
				return false
			}
		case "VP8 ", "VP8L":
			if frameSeen {
				return false
			}
			width, height, ok := webPFrameDimensions(kind, chunk)
			if !ok || !validImageDimensions(width, height) || (extended && (width != canvasWidth || height != canvasHeight)) {
				return false
			}
			frameSeen = true
		case "ALPH":
			if !extended || frameSeen || len(chunk) < 2 || chunk[0]&0xc0 != 0 || chunk[0]&3 > 1 {
				return false
			}
		case "ICCP", "EXIF", "XMP ":
			if !extended || len(chunk) == 0 {
				return false
			}
		default:
			return false
		}
		offset = int(padded)
	}
	return frameSeen
}

func webPUint24(data []byte) int {
	return int(data[0]) | int(data[1])<<8 | int(data[2])<<16
}

func webPFrameDimensions(kind string, chunk []byte) (int, int, bool) {
	if kind == "VP8L" {
		if len(chunk) < 6 || chunk[0] != 0x2f || chunk[4]&0xe0 != 0 {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(chunk[1:5])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
	}
	if len(chunk) < 11 || chunk[0]&1 != 0 || chunk[0]>>1&7 > 3 || chunk[0]&0x10 == 0 || !bytes.Equal(chunk[3:6], []byte{0x9d, 0x01, 0x2a}) {
		return 0, 0, false
	}
	partitionLength := webPUint24(chunk[:3]) >> 5
	if partitionLength < 7 || partitionLength > len(chunk)-3 {
		return 0, 0, false
	}
	return int(binary.LittleEndian.Uint16(chunk[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(chunk[8:10]) & 0x3fff), true
}
