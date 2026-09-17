package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseImageOptionsInterspersed(t *testing.T) {
	opts, err := parseImageOptions([]string{"--ref=first.png", "a", "small", "--format", "JPG", "boat", "--ref", "second.png", "--output=boat.jpg", "--source=nano-banana", "--quality", "high"})
	if err != nil {
		t.Fatal(err)
	}
	want := imageOptions{Prompt: "a small boat", Output: "boat.jpg", Format: "jpg", Source: "nano-banana", Quality: "high", References: []string{"first.png", "second.png"}}
	if !reflect.DeepEqual(opts, want) {
		t.Fatalf("options = %#v, want %#v", opts, want)
	}
	opts, err = parseImageOptions([]string{"--", "--dash", "prompt"})
	if err != nil || opts.Prompt != "--dash prompt" || opts.Format != "png" || opts.Source != "flux" || opts.Quality != "fast" {
		t.Fatalf("defaults or -- parsing failed: %#v, %v", opts, err)
	}
	for _, format := range []string{"png", "jpg", "webp"} {
		if _, err := parseImageOptions([]string{"a boat", "--format=" + format}); err != nil {
			t.Errorf("format %s: %v", format, err)
		}
	}
}

func TestParseImageOptionsRejectInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		nil, {" \t "}, {"boat", "--ref"}, {"boat", "--output", "--quality=high"},
		{"boat", "--ref="}, {"boat", "--quality=nope"}, {"boat", "--source=nope"},
		{"boat", "--format=gif"}, {"boat", "--unknown=secret"},
		{"boat", "--format=png", "--format=jpg"}, {"boat", "--output=one", "--output=two"},
		{"boat", "--source=flux", "--source=flux"}, {"boat", "--quality=high", "--quality=high"},
		{strings.Repeat("x", 10_001)}, {strings.Repeat("\U0001f600", 5_001)},
	} {
		if _, err := parseImageOptions(args); err == nil {
			t.Errorf("accepted invalid arguments of length %d", len(args))
		}
	}
	if _, err := parseImageOptions([]string{strings.Repeat("\U0001f600", 5_000)}); err != nil {
		t.Fatalf("rejected 10,000 UTF-16 units: %v", err)
	}
	for _, help := range []string{"-h", "--help"} {
		if _, err := parseImageOptions([]string{help}); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s: expected help, got %v", help, err)
		}
	}
}

func TestImageReferenceCountsBeforeIO(t *testing.T) {
	for _, tc := range []struct {
		source, quality string
		limit           int
	}{{"flux", "fast", 5}, {"flux", "high", 8}, {"nano-banana", "fast", 8}} {
		opts := imageOptions{Prompt: "boat", Format: "png", Source: tc.source, Quality: tc.quality}
		for i := 0; i < tc.limit; i++ {
			opts.References = append(opts.References, "file-that-does-not-exist")
		}
		if err := validateImageOptions(opts); err != nil {
			t.Fatal(err)
		}
		opts.References = append(opts.References, "file-that-does-not-exist")
		if _, err := prepareImageRequest(context.Background(), nil, opts); err == nil || !strings.Contains(err.Error(), "at most") {
			t.Errorf("count must be checked before file I/O: %v", err)
		}
	}
}

func TestPrepareImageRequestPreservesReferenceOrderAndFormat(t *testing.T) {
	dir := t.TempDir()
	var refs []string
	var want []string
	for _, format := range []string{"png", "jpeg", "gif"} {
		var encoded bytes.Buffer
		img := image.NewRGBA(image.Rect(0, 0, 2, 3))
		var err error
		switch format {
		case "png":
			err = png.Encode(&encoded, img)
		case "jpeg":
			err = jpeg.Encode(&encoded, img, nil)
		case "gif":
			err = gif.Encode(&encoded, img, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "reference."+format)
		if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, path)
		want = append(want, "data:image/"+format+";base64,"+base64.StdEncoding.EncodeToString(encoded.Bytes()))
	}
	opts, err := parseImageOptions([]string{"boat", "--ref", refs[0], "--ref", refs[1], "--ref", refs[2]})
	if err != nil {
		t.Fatal(err)
	}
	body, err := prepareImageRequest(context.Background(), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body["imageRefs"], want) || body["prompt"] != "boat" || body["outputFormat"] != "png" || body["source"] != "flux" || body["refModelTier"] != "fast" {
		t.Fatal("request did not preserve ordered data URLs and model options")
	}
	opts.References = nil
	body, err = prepareImageRequest(context.Background(), nil, opts)
	if err != nil || body["imageRefs"] == nil || len(body["imageRefs"].([]string)) != 0 {
		t.Fatalf("empty references must be an array: %v", err)
	}
}

type imageInputRoundTripper func(*http.Request) (*http.Response, error)

func (f imageInputRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRemoteImageReferenceDoesNotSendCredentials(t *testing.T) {
	data := imageInputPNG(t, 2, 3)
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://images.example")
	jar.SetCookies(u, []*http.Cookie{{Name: "account", Value: "secret"}})
	calls := 0
	client := &http.Client{Jar: jar, Timeout: time.Minute, Transport: imageInputRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Referer"} {
			if r.Header.Get(header) != "" {
				t.Errorf("download forwarded %s", header)
			}
		}
		if calls == 1 {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://other.example/result"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, nil
	})}
	got, err := fetchImageReference(context.Background(), client, "https://images.example/input?token=private", 1000)
	if err != nil || !bytes.Equal(got, data) || calls != 2 {
		t.Fatalf("download = %d calls, %v", calls, err)
	}
	if client.Jar != jar || client.Timeout != time.Minute || client.CheckRedirect != nil {
		t.Fatal("download mutated the account HTTP client")
	}
}

func TestRemoteImageReferenceRejectsUnsafeURLsAndRedirects(t *testing.T) {
	for _, address := range []string{"http://images.example/secret", "https://user:private@images.example/image", "https://images.example/image#private", "https://images.example/image#", "data:image/png;base64,private", "https:///missing-host"} {
		calls := 0
		client := &http.Client{Transport: imageInputRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("should not run")
		})}
		_, err := fetchImageReference(context.Background(), client, address, 1000)
		if err == nil || calls != 0 || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), address) {
			t.Errorf("unsafe URL not rejected privately: calls=%d err=%v", calls, err)
		}
	}
	for _, location := range []string{"http://other.example/private", "https://user:private@other.example/image", "https://other.example/image#private", "https://images.example/loop?secret=private"} {
		calls := 0
		client := &http.Client{Transport: imageInputRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {location}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		})}
		_, err := fetchImageReference(context.Background(), client, "https://images.example/input?token=private", 1000)
		if err == nil || calls > 4 || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "images.example") {
			t.Errorf("unsafe redirect not rejected privately: calls=%d err=%v", calls, err)
		}
		if !strings.Contains(location, "/loop") && calls != 1 {
			t.Errorf("unsafe redirect was followed: %d calls", calls)
		}
	}
}

func TestReferenceImageDownloadBoundsAndErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		length int64
		body   string
	}{{200, 101, ""}, {200, -1, strings.Repeat("x", 101)}, {403, 0, "private server error"}} {
		client := &http.Client{Transport: imageInputRoundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, ContentLength: tc.length, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header), Request: r}, nil
		})}
		_, err := fetchImageReference(context.Background(), client, "https://images.example/private?token=secret", 100)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Errorf("download failure not sanitized: %v", err)
		}
	}
	reader := bytes.NewReader(make([]byte, 200))
	if _, err := readBoundedImageReference(reader, 100); err == nil || reader.Len() != 99 {
		t.Fatalf("read must stop at limit + 1: remaining=%d error=%v", reader.Len(), err)
	}
}

func TestImageReferenceRegularFilesAndCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "original.png")
	data := imageInputPNG(t, 2, 3)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.png")
	if err := os.Symlink(path, link); err == nil {
		got, err := readImageReference(context.Background(), nil, link, 1000)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("regular image symlink was rejected: %v", err)
		}
	}
	if _, err := readImageReference(context.Background(), nil, os.DevNull, 1000); err == nil {
		t.Fatal("accepted a device as an image file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := imageOptions{Prompt: "boat", Format: "png", Source: "flux", Quality: "fast", References: []string{path}}
	if _, err := prepareImageRequest(ctx, nil, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled input preparation = %v", err)
	}
	for _, invalid := range [][]byte{nil, []byte("not an image"), imageInputPNG(t, 0, 1)} {
		if _, _, _, _, err := imageReferenceDimensions(invalid); err == nil {
			t.Fatal("accepted an invalid image header")
		}
	}
}

func TestImageReferenceDimensionAndUploadLimits(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	large := write("large.png", imageInputPNG(t, 2049, 2048))
	valid := write("valid.png", imageInputPNG(t, 2048, 2048))
	oversized := write("oversized.png", append(imageInputPNG(t, 1, 1), make([]byte, 5_250_000)...))
	combined := write("combined.png", append(imageInputPNG(t, 1, 1), make([]byte, 2_700_000)...))
	for _, tc := range []struct {
		name, source, quality string
		refs                  []string
		wantError             string
	}{
		{"flux fast dimensions", "flux", "fast", []string{large}, "4,194,304"},
		{"flux high dimensions", "flux", "high", []string{large}, "4,194,304"},
		{"flux high total", "flux", "high", []string{valid, valid, valid}, "9,437,184"},
		{"one upload", "flux", "fast", []string{oversized}, "size limit"},
		{"combined upload", "flux", "fast", []string{combined, combined}, "size limit"},
		{"directory", "flux", "fast", []string{dir}, "regular image file"},
		{"missing", "flux", "fast", []string{filepath.Join(dir, "private-name")}, "could not access"},
		{"nano dimension", "nano-banana", "fast", []string{large}, ""},
		{"exact cap", "flux", "fast", []string{valid}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := imageOptions{Prompt: "boat", Format: "png", Source: tc.source, Quality: tc.quality, References: tc.refs}
			_, err := prepareImageRequest(context.Background(), nil, opts)
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error = %v, want %s", err, tc.wantError)
			}
		})
	}
}

func imageInputPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()
	// DecodeConfig checks the IHDR header and CRC without decoding pixel data.
	binary.BigEndian.PutUint32(data[16:20], uint32(width))
	binary.BigEndian.PutUint32(data[20:24], uint32(height))
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func imageInputWebP(kind string, width, height int) []byte {
	var payload []byte
	switch kind {
	case "VP8 ":
		payload = []byte{0x10, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
		binary.LittleEndian.PutUint16(payload[6:8], uint16(width))
		binary.LittleEndian.PutUint16(payload[8:10], uint16(height))
	case "VP8L":
		payload = make([]byte, 5)
		payload[0] = 0x2f
		binary.LittleEndian.PutUint32(payload[1:5], uint32(width-1)|uint32(height-1)<<14)
	case "VP8X":
		payload = make([]byte, 10)
		for i := 0; i < 3; i++ {
			payload[4+i] = byte((width - 1) >> (8 * i))
			payload[7+i] = byte((height - 1) >> (8 * i))
		}
	}
	data := make([]byte, 20+len(payload)+len(payload)%2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WEBP")
	copy(data[12:16], kind)
	binary.LittleEndian.PutUint32(data[16:20], uint32(len(payload)))
	copy(data[20:], payload)
	return data
}

func TestWebPReferenceHeadersAndModelCompatibility(t *testing.T) {
	for _, kind := range []string{"VP8 ", "VP8L", "VP8X"} {
		data := imageInputWebP(kind, 1234, 2345)
		mime, width, height, extended, err := imageReferenceDimensions(data)
		if err != nil || mime != "image/webp" || width != 1234 || height != 2345 || extended != (kind == "VP8X") {
			t.Fatalf("%s dimensions = %d x %d, extended=%t, %v", kind, width, height, extended, err)
		}
		path := filepath.Join(t.TempDir(), "ref.webp")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, model := range []struct{ source, quality string }{{"flux", "fast"}, {"flux", "high"}, {"nano-banana", "fast"}} {
			opts := imageOptions{Prompt: "boat", Format: "png", Source: model.source, Quality: model.quality, References: []string{path}}
			_, err := prepareImageRequest(context.Background(), nil, opts)
			wantErr := kind != "VP8X" && model.source == "flux" && model.quality == "fast"
			if (err != nil) != wantErr {
				t.Errorf("%s / %s / %s: %v", kind, model.source, model.quality, err)
			}
		}
		for cut := 0; cut < len(data); cut++ {
			if _, _, _, err := webPReferenceDimensions(data[:cut]); err == nil {
				t.Errorf("accepted truncated %s header at %d bytes", kind, cut)
			}
		}
		broken := append([]byte(nil), data...)
		binary.LittleEndian.PutUint32(broken[16:20], 0xffffffff)
		if _, _, _, err := webPReferenceDimensions(broken); err == nil {
			t.Errorf("accepted oversized %s chunk", kind)
		}
		if kind == "VP8L" {
			badPadding := append([]byte(nil), data...)
			badPadding[len(badPadding)-1] = 1
			if _, _, _, err := webPReferenceDimensions(badPadding); err == nil {
				t.Error("accepted a WebP chunk with invalid padding")
			}
		}
	}
}
