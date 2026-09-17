package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const maxImageReferenceCharacters = 7_000_000

type imageOptions struct {
	Prompt, Output, Format, Source, Quality string
	References                              []string
}

func parseImageOptions(args []string) (imageOptions, error) {
	opts := imageOptions{Format: "png", Source: "flux", Quality: "fast"}
	seen := make(map[string]bool)
	var prompt []string
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if positionalOnly || !strings.HasPrefix(arg, "-") || arg == "-" {
			prompt = append(prompt, arg)
			continue
		}
		if arg == "--" {
			positionalOnly = true
			continue
		}
		if arg == "--help" || arg == "-h" {
			return opts, flag.ErrHelp
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--ref", "--output", "--format", "--source", "--quality":
		default:
			return opts, errors.New("unknown generate-image option; use --help for usage")
		}
		if name != "--ref" && seen[name] {
			return opts, fmt.Errorf("%s may only be provided once", name)
		}
		seen[name] = true
		if !hasValue {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return opts, fmt.Errorf("%s requires a value (use %s=value for a value starting with a dash)", name, name)
			}
			i++
			value = args[i]
		}
		if strings.TrimSpace(value) == "" {
			return opts, fmt.Errorf("%s requires a non-empty value", name)
		}
		switch name {
		case "--ref":
			opts.References = append(opts.References, value)
		case "--output":
			opts.Output = value
		case "--format":
			opts.Format = strings.ToLower(strings.TrimSpace(value))
		case "--source":
			opts.Source = strings.ToLower(strings.TrimSpace(value))
		case "--quality":
			opts.Quality = strings.ToLower(strings.TrimSpace(value))
		}
	}
	opts.Prompt = strings.TrimSpace(strings.Join(prompt, " "))
	return opts, validateImageOptions(opts)
}

func validateImageOptions(opts imageOptions) error {
	if strings.TrimSpace(opts.Prompt) == "" {
		return errors.New("an image prompt is required")
	}
	if !utf8.ValidString(opts.Prompt) || len(utf16.Encode([]rune(opts.Prompt))) > 10_000 {
		return errors.New("the image prompt must contain at most 10,000 UTF-16 characters")
	}
	if opts.Format != "png" && opts.Format != "jpg" && opts.Format != "webp" {
		return errors.New("image format must be png, jpg, or webp")
	}
	if opts.Source != "flux" && opts.Source != "nano-banana" {
		return errors.New("image source must be flux or nano-banana")
	}
	if opts.Quality != "fast" && opts.Quality != "high" {
		return errors.New("image quality must be fast or high")
	}
	limit := 8
	if opts.Source == "flux" && opts.Quality == "fast" {
		limit = 5
	}
	if len(opts.References) > limit {
		return fmt.Errorf("this image model supports at most %d reference images", limit)
	}
	for _, reference := range opts.References {
		if strings.TrimSpace(reference) == "" {
			return errors.New("reference image paths must not be empty")
		}
	}
	return nil
}

func prepareImageRequest(ctx context.Context, client *http.Client, opts imageOptions) (map[string]any, error) {
	if err := validateImageOptions(opts); err != nil {
		return nil, err
	}
	references := make([]string, 0, len(opts.References))
	characters := 0
	var totalPixels int64
	for i, reference := range opts.References {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// PNG and GIF have the shortest supported data URL prefix.
		remaining := maxImageReferenceCharacters - characters
		maxBytes := int64((remaining - len("data:image/png;base64,")) / 4 * 3)
		if maxBytes <= 0 {
			return nil, errors.New("reference images together exceed the 7,000,000 character upload limit")
		}
		data, err := readImageReference(ctx, client, reference, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("reference image %d: %w", i+1, err)
		}
		mime, width, height, extendedWebP, err := imageReferenceDimensions(data)
		if err != nil {
			return nil, fmt.Errorf("reference image %d: %w", i+1, err)
		}
		pixels := int64(width) * int64(height)
		if opts.Source == "flux" {
			if pixels > 4_194_304 {
				return nil, fmt.Errorf("reference image %d exceeds the Flux limit of 4,194,304 pixels; resize it first", i+1)
			}
			if opts.Quality == "fast" && mime == "image/webp" && !extendedWebP {
				return nil, fmt.Errorf("reference image %d: this WebP header is not supported by Flux fast; use PNG/JPEG or --quality high", i+1)
			}
			totalPixels += pixels
			if opts.Quality == "high" && totalPixels > 9*1024*1024 {
				return nil, errors.New("reference images together exceed the Flux high quality limit of 9,437,184 pixels; resize them first")
			}
		}
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		characters += len(dataURL)
		if characters > maxImageReferenceCharacters {
			return nil, errors.New("reference images together exceed the 7,000,000 character upload limit")
		}
		references = append(references, dataURL)
	}
	return map[string]any{
		"prompt": opts.Prompt, "outputFormat": opts.Format,
		"source": opts.Source, "refModelTier": opts.Quality, "imageRefs": references,
	}, nil
}

func readImageReference(ctx context.Context, client *http.Client, reference string, maxBytes int64) ([]byte, error) {
	if strings.Contains(reference, "://") || strings.HasPrefix(strings.ToLower(reference), "data:") {
		return fetchImageReference(ctx, client, reference, maxBytes)
	}
	info, err := os.Stat(reference)
	if err != nil {
		return nil, errors.New("could not access the local image file")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("reference must be a regular image file")
	}
	if info.Size() > maxBytes {
		return nil, errors.New("reference images together exceed the upload size limit")
	}
	file, err := os.Open(reference)
	if err != nil {
		return nil, errors.New("could not open the local image file")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("reference must be a regular image file")
	}
	return readBoundedImageReference(file, maxBytes)
}

func validImageReferenceURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && u.RawFragment == ""
}

func fetchImageReference(ctx context.Context, client *http.Client, reference string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(reference)
	if err != nil || !validImageReferenceURL(u) || strings.Contains(reference, "#") {
		return nil, errors.New("remote references must use HTTPS without URL credentials or fragments")
	}
	if client == nil {
		client = http.DefaultClient
	}
	downloadClient := *client
	downloadClient.Jar = nil
	if downloadClient.Timeout <= 0 || downloadClient.Timeout > 25*time.Second {
		downloadClient.Timeout = 25 * time.Second
	}
	downloadClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 3 || !validImageReferenceURL(request.URL) {
			return errors.New("unsupported reference image redirect")
		}
		// A reference URL can contain a signed query. Do not forward it as a
		// Referer, or forward account credentials or cookies to image hosts.
		request.Header = http.Header{"Accept": {"image/*"}}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("could not create the reference image download")
	}
	request.Header.Set("Accept", "image/*")
	response, err := downloadClient.Do(request)
	if err != nil {
		return nil, errors.New("could not download the reference image; check HTTPS, redirects, and connectivity")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reference image download failed (HTTP %d)", response.StatusCode)
	}
	if response.ContentLength > maxBytes {
		return nil, errors.New("reference images together exceed the upload size limit")
	}
	return readBoundedImageReference(response.Body, maxBytes)
}

func readBoundedImageReference(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, errors.New("could not read the reference image")
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("reference images together exceed the upload size limit")
	}
	return data, nil
}

func imageReferenceDimensions(data []byte) (mime string, width, height int, extendedWebP bool, err error) {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		width, height, extendedWebP, err = webPReferenceDimensions(data)
		return "image/webp", width, height, extendedWebP, err
	}
	config, format, decodeErr := image.DecodeConfig(bytes.NewReader(data))
	if decodeErr != nil || config.Width <= 0 || config.Height <= 0 {
		return "", 0, 0, false, errors.New("could not read the image dimensions; use a valid PNG, JPEG, GIF, or WebP image")
	}
	switch format {
	case "png", "jpeg", "gif":
		return "image/" + format, config.Width, config.Height, false, nil
	default:
		return "", 0, 0, false, errors.New("reference image format must be PNG, JPEG, GIF, or WebP")
	}
}

func webPReferenceDimensions(data []byte) (int, int, bool, error) {
	invalid := errors.New("could not read the WebP image dimensions; use a valid PNG, JPEG, GIF, or WebP image")
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" ||
		uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return 0, 0, false, invalid
	}
	// Validate all container bounds and required padding before slicing chunks.
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return 0, 0, false, invalid
		}
		size := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		end := uint64(offset) + 8 + size
		paddedEnd := end + size%2
		if paddedEnd > uint64(len(data)) || (size%2 != 0 && data[int(end)] != 0) {
			return 0, 0, false, invalid
		}
		offset = int(paddedEnd)
	}
	chunkSize := uint64(binary.LittleEndian.Uint32(data[16:20]))
	if chunkSize > uint64(len(data)-20) {
		return 0, 0, false, invalid
	}
	chunk := data[20 : 20+int(chunkSize)]
	var width, height int
	extended := false
	switch string(data[12:16]) {
	case "VP8 ":
		if len(chunk) < 10 || chunk[0]&1 != 0 || chunk[0]&0xe > 6 || chunk[0]&0x10 == 0 || string(chunk[3:6]) != "\x9d\x01\x2a" {
			return 0, 0, false, invalid
		}
		width = int(binary.LittleEndian.Uint16(chunk[6:8]) & 0x3fff)
		height = int(binary.LittleEndian.Uint16(chunk[8:10]) & 0x3fff)
	case "VP8L":
		if len(chunk) < 5 || chunk[0] != 0x2f || chunk[4]&0xe0 != 0 {
			return 0, 0, false, invalid
		}
		bits := binary.LittleEndian.Uint32(chunk[1:5])
		width = 1 + int(bits&0x3fff)
		height = 1 + int((bits>>14)&0x3fff)
	case "VP8X":
		if len(chunk) != 10 || chunk[0]&0xc1 != 0 || chunk[1] != 0 || chunk[2] != 0 || chunk[3] != 0 {
			return 0, 0, false, invalid
		}
		width = 1 + int(chunk[4]) + int(chunk[5])<<8 + int(chunk[6])<<16
		height = 1 + int(chunk[7]) + int(chunk[8])<<8 + int(chunk[9])<<16
		if uint64(width)*uint64(height) > 0xffffffff {
			return 0, 0, false, invalid
		}
		extended = true
	default:
		return 0, 0, false, invalid
	}
	if width <= 0 || height <= 0 {
		return 0, 0, false, invalid
	}
	return width, height, extended, nil
}
