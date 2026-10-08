package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const clipTestAsset = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
const clipTestJob = "11111111-2222-4333-8444-555555555555"

func clipTestService(t *testing.T) *studioClipService {
	t.Helper()
	t.Setenv("GLOWBOM_STUDIO_DIR", t.TempDir())
	s := newStudioClipService()
	s.tools = func() (string, string, error) { return "ffmpeg", "ffprobe", nil }
	s.probe = func(context.Context, string, string) (studioClipMediaInfo, error) {
		return studioClipMediaInfo{DurationSeconds: 8, Width: 640, Height: 360, HasAudio: true}, nil
	}
	t.Cleanup(s.Close)
	return s
}

func clipTestRecord(t *testing.T, id string, data []byte) string {
	t.Helper()
	dir, err := studioAssetsDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	record := studioImageRecord{ID: id, MediaType: "video", DataBase64: base64.StdEncoding.EncodeToString(data), Prompt: "ქართული / a song", SourceService: "Grok", AssetType: "generated"}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "100_"+id+".json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitClipCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("clip operation did not finish")
		case <-tick.C:
		}
	}
}

func TestStudioClipJobCancellationAndAdmission(t *testing.T) {
	s := clipTestService(t)
	clipTestRecord(t, clipTestAsset, []byte("ftyp test video"))
	started, stopped := make(chan struct{}), make(chan struct{})
	s.render = func(ctx context.Context, _, _, _, output string, _ studioClipSelection, progress func(float64)) (studioClipMediaInfo, error) {
		if err := os.WriteFile(output, []byte("partial"), 0600); err != nil {
			return studioClipMediaInfo{}, err
		}
		close(started)
		progress(.5)
		<-ctx.Done()
		close(stopped)
		return studioClipMediaInfo{}, ctx.Err()
	}
	selection := studioClipSelection{EndSeconds: 2, Mode: "reverse", Mute: true}
	state, status, err := s.start(clipTestJob, clipTestAsset, selection)
	if err != nil || status != 202 || state.Stage != "preparing" {
		t.Fatal(state, status, err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("render did not start")
	}
	if _, status, _ := s.start("66666666-2222-4333-8444-555555555555", clipTestAsset, selection); status != 409 {
		t.Fatal("parallel render admitted", status)
	}
	state, err = s.stop(clipTestJob)
	if err != nil || state.Stage != "stopped" {
		t.Fatal(state, err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not cancel renderer")
	}
	waitClipCondition(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.active == "" })
	entries, _ := os.ReadDir(s.cacheDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".render-") {
			t.Fatal("render intermediates retained")
		}
	}
	if _, status, _ := s.start(clipTestJob, clipTestAsset, selection); status != 409 {
		t.Fatal("cancelled ID reused")
	}
	id := "77777777-2222-4333-8444-555555555555"
	if _, err := s.stop(id); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := s.start(id, clipTestAsset, selection); status != 409 {
		t.Fatal("delayed POST escaped early Stop")
	}
	dir := s.cacheDir
	s.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("source cache was not removed at shutdown", err)
	}
}

func TestStudioClipSavedResultAndRestartRecovery(t *testing.T) {
	s := clipTestService(t)
	original := clipTestRecord(t, clipTestAsset, []byte("original bytes"))
	originalBytes, _ := os.ReadFile(original)
	s.render = func(_ context.Context, _, _, _, output string, _ studioClipSelection, progress func(float64)) (studioClipMediaInfo, error) {
		progress(1)
		return studioClipMediaInfo{DurationSeconds: 2, Width: 640, Height: 360}, os.WriteFile(output, []byte("edited bytes"), 0600)
	}
	if _, _, err := s.start(clipTestJob, clipTestAsset, studioClipSelection{EndSeconds: 2, Mode: "reverse", Mute: true}); err != nil {
		t.Fatal(err)
	}
	waitClipCondition(t, func() bool { state, _ := s.state(clipTestJob); return state.Stage == "complete" })
	state, err := s.stop(clipTestJob)
	if err != nil || state.Stage != "complete" || state.Asset == nil {
		t.Fatal("saved result lost to Stop", state, err)
	}
	path, err := studioClipRecordPath(state.Asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record studioImageRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal("native record is invalid", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(record.DataBase64)
	if err != nil || string(decoded) != "edited bytes" || record.SourceAssetID != clipTestAsset || record.Duration == nil || *record.Duration != 2 {
		t.Fatal("derived metadata or media changed", record, err)
	}
	if current, _ := os.ReadFile(original); !bytes.Equal(current, originalBytes) {
		t.Fatal("original was modified")
	}
	// Simulate a crash after the result rename but before the checkpoint update.
	job := &studioClipJob{State: studioClipState{Found: true, ID: clipTestJob, AssetID: clipTestAsset, Stage: "saving"}}
	if err := s.saveJob(job); err != nil {
		t.Fatal(err)
	}
	recovered := newStudioClipService()
	defer recovered.Close()
	state, err = recovered.state(clipTestJob)
	if err != nil || state.Stage != "complete" || state.Asset == nil || state.Asset.ID != record.ID {
		t.Fatal("saved result was not recovered", state, err)
	}
	interrupted := "88888888-2222-4333-8444-555555555555"
	if err := s.saveJob(&studioClipJob{State: studioClipState{Found: true, ID: interrupted, AssetID: clipTestAsset, Stage: "rendering"}, Selection: studioClipSelection{EndSeconds: 1, Mode: "loop"}}); err != nil {
		t.Fatal(err)
	}
	state, err = recovered.state(interrupted)
	if err != nil || state.Stage != "stopped" || !strings.Contains(state.Error, "restarted") {
		t.Fatal("interrupted render was restarted automatically", state, err)
	}
	for i := range 64 {
		recovered.jobs[fmt.Sprintf("old-%d", i)] = &studioClipJob{State: studioClipState{Stage: "complete"}, updated: time.Now()}
	}
	recovered.prune()
	if len(recovered.jobs) >= 64 {
		t.Fatal("terminal history blocked new work")
	}
}

func TestStudioClipScopedPlaybackAndRanges(t *testing.T) {
	s := clipTestService(t)
	payload := []byte("0123456789 video bytes")
	clipTestRecord(t, clipTestAsset, payload)
	previous := studioClips
	studioClips = s
	t.Cleanup(func() { studioClips = previous })
	t.Setenv("GLOWBOM_SERVER_TOKEN", "clip-test-bearer")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	mux := http.NewServeMux()
	mux.HandleFunc("/studio/clips/playback", studioClipPlaybackHandler)
	mux.HandleFunc("/studio/clips/source", studioClipSourceHandler)
	handler := withGlowbomSecurity(mux)
	request := httptest.NewRequest("POST", "/studio/clips/playback", strings.NewReader(`{"assetId":"`+clipTestAsset+`"}`))
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != 401 {
		t.Fatal("ticket minted without bearer", unauthorized.Code)
	}
	request.Header.Set("Authorization", "Bearer clip-test-bearer")
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, request)
	var ticket struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(record.Body.Bytes(), &ticket); err != nil || strings.Contains(ticket.URL, "clip-test-bearer") {
		t.Fatal("invalid or unsafe playback ticket", err)
	}
	sourceURL := strings.TrimPrefix(ticket.URL, "/api")
	stream := httptest.NewRequest("GET", sourceURL, nil)
	stream.Header.Set("Range", "bytes=3-6")
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, stream)
	if record.Code != 206 || record.Body.String() != "3456" || record.Header().Get("Content-Range") != fmt.Sprintf("bytes 3-6/%d", len(payload)) {
		t.Fatal("stream range failed", record.Code, record.Body.String(), record.Header())
	}
	stream.Header.Set("Sec-Fetch-Site", "cross-site")
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, stream)
	if record.Code != 401 {
		t.Fatal("cross-site playback allowed", record.Code)
	}
	stream.Header.Del("Sec-Fetch-Site")
	stream.Header.Set("Origin", "https://untrusted.example")
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, stream)
	if record.Code != 403 {
		t.Fatal("foreign origin playback allowed", record.Code)
	}
	parsed, _ := url.Parse(sourceURL)
	s.tickets[parsed.Query().Get("ticket")] = studioClipPlayback{assetID: clipTestAsset, expires: time.Now().Add(-time.Second)}
	stream.Header.Del("Origin")
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, stream)
	if record.Code != 401 {
		t.Fatal("expired ticket played", record.Code)
	}
}

func TestStudioClipStorageBoundedAndStrict(t *testing.T) {
	clipTestService(t)
	path := clipTestRecord(t, clipTestAsset, []byte{255, 255, 255, 255})
	data, _ := os.ReadFile(path)
	data = bytes.ReplaceAll(data, []byte("/////w=="), []byte(`data:video\/mp4;base64,\/\/\/\/\/w==`))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	metadata, err := readStudioClipEnvelope(context.Background(), path, &output)
	if err != nil || output.Len() != 4 || metadata.Prompt != "ქართული / a song" {
		t.Fatal(metadata, output.Len(), err)
	}
	for _, invalid := range []string{`{"dataBase64":"eA==",}`, `{"dataBase64":"eA=="} garbage`, `{"dataBase64":"eA==","dataBase64":"eA=="}`, `{"dataBase64":"data:` + strings.Repeat("x", 200) + `"}`} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readStudioClipEnvelope(context.Background(), path, io.Discard); err == nil {
			t.Fatal("invalid record accepted", invalid[:30])
		}
	}
	link := filepath.Join(t.TempDir(), "linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readStudioClipEnvelope(context.Background(), link, nil); err == nil {
		t.Fatal("symlink record accepted")
	}
	// A 16 MiB encoded source must use fixed buffers instead of media-sized allocations.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"id":"` + clipTestAsset + `","mediaType":"video","dataBase64":"`)
	encoder := base64.NewEncoder(base64.StdEncoding, f)
	zero := bytes.NewReader(make([]byte, 16<<20))
	if _, err := io.Copy(encoder, zero); err != nil {
		t.Fatal(err)
	}
	encoder.Close()
	f.WriteString(`"}`)
	f.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := readStudioClipEnvelope(context.Background(), path, io.Discard); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("media read allocated %d bytes", allocated)
	} else {
		t.Logf("16 MiB source read allocated %d bytes", allocated)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readStudioClipEnvelope(ctx, path, io.Discard); err == nil {
		t.Fatal("cancelled media read proceeded")
	}
}

func TestStudioClipSourceCachePinsAndCleanup(t *testing.T) {
	s := clipTestService(t)
	ids := []string{clipTestAsset, "BBBBBBBB-BBBB-4CCC-8DDD-EEEEEEEEEEEE", "CCCCCCCC-BBBB-4CCC-8DDD-EEEEEEEEEEEE"}
	for _, id := range ids {
		clipTestRecord(t, id, []byte("test clip"))
	}
	first, releaseFirst, err := s.source(context.Background(), ids[0], "ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	_, releaseSecond, err := s.source(context.Background(), ids[1], "ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.source(context.Background(), ids[2], "ffprobe"); err == nil {
		t.Fatal("pinned cache was evicted")
	}
	releaseFirst()
	_, releaseThird, err := s.source(context.Background(), ids[2], "ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.path); !os.IsNotExist(err) {
		t.Fatal("evicted source remained on disk", err)
	}
	releaseSecond()
	releaseThird()
	s.Close()
	if _, _, err := s.source(context.Background(), ids[0], "ffprobe"); err == nil {
		t.Fatal("closed service admitted media")
	}
}

func TestStudioClipShutdownWaitsForRenderer(t *testing.T) {
	s := clipTestService(t)
	clipTestRecord(t, clipTestAsset, []byte("test clip"))
	started, reaped := make(chan struct{}), make(chan struct{})
	s.render = func(ctx context.Context, _, _, _, output string, _ studioClipSelection, _ func(float64)) (studioClipMediaInfo, error) {
		close(started)
		<-ctx.Done()
		close(reaped)
		return studioClipMediaInfo{}, ctx.Err()
	}
	if _, _, err := s.start(clipTestJob, clipTestAsset, studioClipSelection{EndSeconds: 2, Mode: "reverse"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("renderer did not start")
	}
	s.Close()
	select {
	case <-reaped:
	default:
		t.Fatal("Close returned before renderer was reaped")
	}
	if _, err := os.Stat(s.cacheDir); !os.IsNotExist(err) {
		t.Fatal("shutdown cache retained", err)
	}
}

func TestStudioClipRequestCanonicalizesJobID(t *testing.T) {
	s := clipTestService(t)
	clipTestRecord(t, clipTestAsset, []byte("test clip"))
	previous := studioClips
	studioClips = s
	t.Cleanup(func() { studioClips = previous })
	t.Setenv("GLOWBOM_SERVER_TOKEN", "")
	t.Setenv("GLOWBY_SERVER_TOKEN", "")
	s.render = func(ctx context.Context, _, _, _, _ string, _ studioClipSelection, _ func(float64)) (studioClipMediaInfo, error) {
		<-ctx.Done()
		return studioClipMediaInfo{}, ctx.Err()
	}
	render := httptest.NewRecorder()
	studioClipRenderHandler(render, httptest.NewRequest("POST", "/studio/clips/render", strings.NewReader(`{"id":" `+clipTestJob+` ","assetId":"`+clipTestAsset+`","startSeconds":0,"endSeconds":1,"mode":"trim","mute":true}`)))
	if render.Code != 202 || render.Header().Get("Content-Type") != "application/json" {
		t.Fatal("render rejected", render.Code, render.Body.String())
	}
	var state studioClipState
	if err := json.Unmarshal(render.Body.Bytes(), &state); err != nil || state.ID != clipTestJob {
		t.Fatal("noncanonical job ID", state, err)
	}
	stop := httptest.NewRecorder()
	studioClipCancelHandler(stop, httptest.NewRequest("POST", "/studio/clips/cancel", strings.NewReader(`{"id":" `+clipTestJob+` "}`)))
	if err := json.Unmarshal(stop.Body.Bytes(), &state); err != nil || state.ID != clipTestJob || state.Stage != "stopped" {
		t.Fatal("cancel missed canonical job", state, err)
	}
}

func TestStudioClipCancellationBeforePublishKeepsOriginal(t *testing.T) {
	clipTestService(t)
	path := filepath.Join(t.TempDir(), "output.mp4")
	if err := os.WriteFile(path, []byte("edited media"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := saveStudioClipFileWithCommit(ctx, clipTestJob, studioAssetMetadata{ID: clipTestAsset}, path, studioClipMediaInfo{DurationSeconds: 1, Width: 640, Height: 360}, "trim", func(_ studioImageSummary, publish func() error) error {
		cancel()
		return publish()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled commit published", err)
	}
	dir, _ := studioAssetsDirectory()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("cancelled export left a result or partial asset")
	}
}
