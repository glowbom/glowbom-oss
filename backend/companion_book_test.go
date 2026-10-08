package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompanionBookReadDoesNotCreateOrGenerate(t *testing.T) {
	path := bookTestProject(t)
	path, _ = filepath.EvalSymlinks(path)
	s := testCompanion(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("Book reading invoked an agent or another internal route")
	}))
	s.projects["shared"] = companionProject{ID: "shared", Name: "Garden", path: path}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/shared/book", ""))
	var book projectBook
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &book) != nil || len(book.Entries) != 0 {
		t.Fatalf("read empty Book: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(path, "project-book")); !os.IsNotExist(err) {
		t.Fatal("reading created a Book", err)
	}
	for _, route := range []string{"/projects/unshared/book", "/projects/shared/book/media/not-saved"} {
		w = httptest.NewRecorder()
		s.ServeHTTP(w, companionRequest(s, http.MethodGet, route, ""))
		if w.Code != http.StatusNotFound {
			t.Fatalf("unshared content accepted: %s %d", route, w.Code)
		}
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodPost, "/projects/shared/book", `{"action":"sync"}`))
	if w.Code != http.StatusNotFound {
		t.Fatal("phone can write Books", w.Code)
	}
}

func TestCompanionBookReadsSavedStoryAndOnlyItsImages(t *testing.T) {
	path := bookTestProject(t)
	path, _ = filepath.EvalSymlinks(path)
	bookTestBuild(t, path)
	response, saved := bookTestCall(t, path, "sync", nil)
	if response.Code != http.StatusOK || len(saved.Entries) == 0 || len(saved.Entries[0].Images) == 0 {
		t.Fatal("invalid fixture", response.Code)
	}
	manifest := filepath.Join(path, "project-book", "book.json")
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	s := testCompanion(t, http.NotFoundHandler())
	s.projects["shared"] = companionProject{ID: "shared", Name: "Garden", path: path}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/shared/book", ""))
	var book projectBook
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &book) != nil || len(book.Entries) != len(saved.Entries) || book.Entries[0].Body != saved.Entries[0].Body {
		t.Fatalf("saved story lost: %d %s", w.Code, w.Body.String())
	}
	image := book.Entries[0].Images[0]
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/shared/book/media/"+image.ID, ""))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), bookTestPNG(t)) {
		t.Fatal("saved image not returned", w.Code)
	}
	after, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("Book reading changed its files", err)
	}
	// An asset that exists on disk but is absent from the Book is inaccessible.
	bookTestWrite(t, path, "project-book/assets/unused.png", bookTestPNG(t))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, companionRequest(s, http.MethodGet, "/projects/shared/book/media/unused", ""))
	if w.Code != http.StatusNotFound {
		t.Fatal("unreferenced image exposed", w.Code)
	}
}
