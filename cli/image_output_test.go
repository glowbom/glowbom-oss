package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func outputImageFixture(t *testing.T, format string) []byte {
	t.Helper()
	if format == "webp" {
		// A complete 48x48 WebP from the portable Android template, embedded so
		// these tests run independently of other folders and external tools.
		data, err := base64.StdEncoding.DecodeString("UklGRs4DAABXRUJQVlA4WAoAAAAQAAAALwAALwAAQUxQSKoAAAABgFvb1rLo8r4Pd3d3qwJS2vMOHKYqXMJRXN63FtkEETEBeJdCsovHiKAg0QMEuGNR5pgboDsEU3O+YZ83TaB77aOSPc0tAc/y+PnL/nlcBSGuJLK74++R/fe4L0Heym0VKT+wU6Tyj9FWkfK9wk6R6i2ByPr4+cv+edykboEM/aOSYyPhlijPtof9gXV/2H5UxR2QMVlrsNeTRsJ90plt7GYd4WGpZZd4l1ZQOCD+AgAA0A8AnQEqMAAwAD4tEoZCoaEONbcADAFiWwAtSEAOa4NX8VebV9Q7v5L/y32S9pjbX+KN+pPvM88B1oHoAeV57Hn7P+lBFX67alT5nPoj/l+4T+qP+R/NTuzH5jmS8EfoWft9h4LXEngz9aJFqqfwVhfoz3atOPU1jqEu+ua6CIVfEgMYtadIAP77qa8EeSEIymjGC3wOJ+Tta6SUuhpL39i0XmAku/QdSnQr+2ayoXOvOEWBGrLaIJh5HxLQqx/rRX9eU3/c8GZyPI37+E/tfgnwn75E2X8TsG//UNqr/lSIwnVi+DLJ/j5Q1zHyElKV7hh7Jw17W0oa0l/SqxXqiQVqeR28BlbTIR3NvN4hLM/5nw20QpOUcJcf9OSY2qd58sEmpYG4jjirSxXZEco3FMkN1G4ros/67aDUnLvkucUdbuDjj1esX+J26agHYszL9uSK0ZYxAb6y5QkQgYnjQz6k68itx8//In8vu9CujXnlfjO6aKP2ootDnZr5AqPVA6QPEk1M6RXZl9t6cMPD2TgMvSPInDyf0JyqmBGmDzHLQ9UI/00XrppOIrQUyg+KVuOe/vOu/IiJHfSx8Ji7Yu1q+Mmb/OE2jUsM7XjDp2FMiWTQLZiDa6WSXF9ar+TC11DEdqrfPK/k61Bak9wV5aizsLL7pnTTntGYiYJCi2JuZSXH4h31BkLZxiwRsCUAApjv+XBIaK8HhUdA1n/wUO393siURHr/5pxW/u4mjzYoXRV9kaUgDS3u4FDN3lfAMuNdH4L9v8lfE/fyKdo3+Z9L6E8auQnTea/OSv7/KAUpKMW5/z03XcXxsAcTN1Cddlocca//HP899Ost0eqQ5vtvb9lgn6dcvMxuKvlGWgWptHUhEXHm4IdAZmKRRn19HDsTB5pd8uO6nDWbbpsyWVH6XgtTJF8LLEPR4yRPHX/cffHjRR/YHUAeaYpdc143ah3s9ONoV4c1Lbl8H5+1yFY/LpDLqK4SjvzeIv1JjY/jhqOa4D3Zzcz3gCoAAA==")
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	picture := image.NewRGBA(image.Rect(0, 0, 2, 3))
	picture.Set(0, 0, color.RGBA{R: 19, G: 130, B: 84, A: 255})
	var out bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&out, picture)
	} else {
		err = jpeg.Encode(&out, picture, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func assertImageOutputDirectory(t *testing.T, directory string, expected int) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != expected {
		t.Fatalf("output directory has %d files, expected %d: %v", len(entries), expected, entries)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".glowbom-image-") {
			t.Fatalf("temporary image was not removed: %s", entry.Name())
		}
	}
}

func TestImageOutputPrepareProtectsFilesAndCleansUp(t *testing.T) {
	directory := t.TempDir()
	existing := filepath.Join(directory, "existing.png")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.png")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(directory, "dangling.png")
	if err := os.Symlink(filepath.Join(directory, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{existing, link, dangling, filepath.Join(directory, "missing", "file.png"), existing + string(os.PathSeparator) + "file.png"} {
		if output, err := prepareImageOutput(path); err == nil {
			output.Close()
			t.Fatalf("accepted invalid destination %s", path)
		}
	}
	missingDirectory := filepath.Join(directory, "missing") + string(os.PathSeparator)
	if output, err := prepareImageOutput(missingDirectory); err == nil {
		output.Close()
		t.Fatal("accepted a missing directory")
	}
	output, err := prepareImageOutput(filepath.Join(directory, "new.png"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output.temporary)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("temporary output permissions: %v, %v", info, err)
	}
	output.Close()
	output.Close()
	assertImageOutputDirectory(t, directory, 3)
}

func TestImageOutputDefaultUsesCurrentDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	output, err := prepareImageOutput("")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cwd, _ := os.Getwd()
	cwd, _ = filepath.EvalSymlinks(cwd)
	if !output.automatic || filepath.Dir(output.temporary) != cwd {
		t.Fatalf("unexpected default output: %#v", output)
	}
}

func TestImageOutputSaveFormatsAndNoCredentials(t *testing.T) {
	for _, format := range []string{"png", "jpg", "webp"} {
		t.Run(format, func(t *testing.T) {
			fixture := outputImageFixture(t, format)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("account credentials were sent to the image host")
				}
				if r.URL.Path == "/start" {
					http.Redirect(w, r, "/image", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(fixture)
			}))
			defer server.Close()
			client := server.Client()
			client.Timeout = time.Second
			client.Jar, _ = cookiejar.New(nil)
			parsed, _ := url.Parse(server.URL)
			client.Jar.SetCookies(parsed, []*http.Cookie{{Name: "session", Value: "do-not-send"}})
			client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
				t.Error("download reused the account redirect handler")
				return nil
			}
			directory := t.TempDir()
			output, err := prepareImageOutput(directory)
			if err != nil {
				t.Fatal(err)
			}
			path, err := output.Save(context.Background(), client, server.URL+"/start?token=private")
			if err != nil {
				t.Fatal(err)
			}
			if !filepath.IsAbs(path) || filepath.Ext(path) != "."+format {
				t.Fatalf("unexpected saved image path: %s", path)
			}
			stored, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(stored, fixture) {
				t.Fatalf("saved image differs from download: %v", err)
			}
			if client.Jar == nil || client.Timeout != time.Second {
				t.Fatal("shared HTTP client was modified")
			}
			assertImageOutputDirectory(t, directory, 1)
		})
	}
}

func TestImageOutputExplicitPathDoesNotConvertAndCannotOverwrite(t *testing.T) {
	fixture := outputImageFixture(t, "png")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(fixture) }))
	defer server.Close()
	for _, collision := range []string{"none", "file", "symlink"} {
		directory := t.TempDir()
		destination := filepath.Join(directory, "requested.jpg")
		output, err := prepareImageOutput(destination)
		if err != nil {
			t.Fatal(err)
		}
		if collision == "file" {
			if err := os.WriteFile(destination, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
		} else if collision == "symlink" {
			target := filepath.Join(t.TempDir(), "protected")
			if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, destination); err != nil {
				t.Fatal(err)
			}
		}
		path, err := output.Save(context.Background(), server.Client(), server.URL)
		stored, readErr := os.ReadFile(destination)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if collision != "none" {
			if err == nil || string(stored) != "keep" {
				t.Fatalf("overwrote a competing file: %v", err)
			}
		} else {
			resolved, _ := filepath.EvalSymlinks(destination)
			if err != nil || path != resolved || !bytes.Equal(stored, fixture) {
				t.Fatalf("explicit filename or image bytes changed: %s, %v", path, err)
			}
		}
		assertImageOutputDirectory(t, directory, 1)
	}
}

func TestImageOutputRejectsInvalidAndTruncatedImages(t *testing.T) {
	pngBytes := outputImageFixture(t, "png")
	jpegBytes := outputImageFixture(t, "jpg")
	webpBytes := outputImageFixture(t, "webp")
	corruptPNG := append([]byte(nil), pngBytes...)
	corruptPNG[len(corruptPNG)/2] ^= 0xff
	corruptWebP := append([]byte(nil), webpBytes...)
	binary.LittleEndian.PutUint32(corruptWebP[16:20], ^uint32(0))
	for name, fixture := range map[string][]byte{
		"HTML": []byte("<html>not an image</html>"), "empty": nil,
		"truncated PNG": pngBytes[:len(pngBytes)-1], "corrupt PNG": corruptPNG,
		"truncated JPEG": jpegBytes[:len(jpegBytes)-2],
		"truncated WebP": webpBytes[:len(webpBytes)-1], "corrupt WebP": corruptWebP,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(fixture)
			}))
			defer server.Close()
			directory := t.TempDir()
			output, err := prepareImageOutput(directory)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := output.Save(context.Background(), server.Client(), server.URL); err == nil {
				t.Fatal("saved an invalid image")
			}
			assertImageOutputDirectory(t, directory, 0)
		})
	}
}

func TestImageOutputRejectsUnsafeURLsAndRedirects(t *testing.T) {
	var server *httptest.Server
	fixture := outputImageFixture(t, "png")
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/http":
			http.Redirect(w, r, "http://127.0.0.1:1/image?secret=private", http.StatusFound)
		case "/credentials":
			http.Redirect(w, r, strings.Replace(server.URL, "https://", "https://secret:private@", 1), http.StatusFound)
		case "/fragment":
			http.Redirect(w, r, server.URL+"/#private", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop?secret=private", http.StatusFound)
		case "/three", "/two", "/one":
			next := map[string]string{"/three": "/two", "/two": "/one", "/one": "/image"}[r.URL.Path]
			http.Redirect(w, r, next, http.StatusFound)
		default:
			_, _ = w.Write(fixture)
		}
	}))
	defer server.Close()
	for _, rawURL := range []string{"http://127.0.0.1:1/image?secret=private", "data:image/png,private", server.URL + "/http", server.URL + "/credentials", server.URL + "/fragment", server.URL + "/loop", server.URL + "/#private"} {
		directory := t.TempDir()
		output, err := prepareImageOutput(directory)
		if err != nil {
			t.Fatal(err)
		}
		_, err = output.Save(context.Background(), server.Client(), rawURL)
		if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), server.URL) {
			t.Fatalf("unsafe URL accepted or leaked: %v", err)
		}
		assertImageOutputDirectory(t, directory, 0)
	}
	output, err := prepareImageOutput(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.Save(context.Background(), server.Client(), server.URL+"/three"); err != nil {
		t.Fatalf("three HTTPS redirects should succeed: %v", err)
	}
}

func TestImageOutputRejectsOversizedAndCanceledTransfers(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/length":
			w.Header().Set("Content-Length", "26214401")
		case "/stream":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			_, _ = io.CopyN(w, zeroImageReader{}, maxGeneratedImageBytes+1)
		case "/cancel":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	for _, path := range []string{"/length", "/stream", "/cancel"} {
		directory := t.TempDir()
		output, err := prepareImageOutput(directory)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if path == "/cancel" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
		}
		if _, err := output.Save(ctx, server.Client(), server.URL+path); err == nil {
			t.Fatalf("accepted invalid transfer %s", path)
		}
		assertImageOutputDirectory(t, directory, 0)
	}
}

type zeroImageReader struct{}

func (zeroImageReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}
