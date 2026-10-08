package main

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStudioClipSelectionRequiresBoundedFiniteRange(t *testing.T) {
	info := studioClipMediaInfo{DurationSeconds: 30, Width: 1920, Height: 1080}
	for _, mode := range []string{"trim", "reverse", "loop"} {
		if err := validateStudioClipSelection(studioClipSelection{StartSeconds: 2, EndSeconds: 12, Mode: mode}, info); err != nil {
			t.Fatalf("valid %s rejected: %v", mode, err)
		}
	}
	cases := []studioClipSelection{
		{StartSeconds: math.NaN(), EndSeconds: 2, Mode: "trim"},
		{StartSeconds: 1, EndSeconds: math.Inf(1), Mode: "trim"},
		{StartSeconds: -1, EndSeconds: 2, Mode: "trim"},
		{StartSeconds: 2, EndSeconds: 2, Mode: "trim"},
		{StartSeconds: 0, EndSeconds: 0.09, Mode: "trim"},
		{StartSeconds: 28, EndSeconds: 31, Mode: "trim"},
		{StartSeconds: 0, EndSeconds: 10.001, Mode: "trim"},
		{StartSeconds: 0, EndSeconds: 11, Mode: "reverse"},
		{StartSeconds: 0, EndSeconds: 11, Mode: "loop"},
		{StartSeconds: 0, EndSeconds: 1, Mode: "speed"},
	}
	for index, selection := range cases {
		if validateStudioClipSelection(selection, info) == nil {
			t.Fatalf("invalid selection %d accepted", index)
		}
	}
	info.DurationSeconds = math.NaN()
	if validateStudioClipSelection(studioClipSelection{EndSeconds: 1, Mode: "trim"}, info) == nil {
		t.Fatal("nonfinite source duration accepted")
	}
}

func TestStudioClipOutputBuffersDrainWithFixedMemory(t *testing.T) {
	output := &studioClipBoundedOutput{limit: 32}
	for range 3 {
		data := []byte(strings.Repeat("x", 10_000))
		if length, err := output.Write(data); err != nil || length != len(data) {
			t.Fatal("bounded output failed to drain child output")
		}
	}
	if len(output.data) != 32 || !output.overflow {
		t.Fatal("child output exceeded its memory limit")
	}
	progress := []float64{}
	parser := &studioClipProgressOutput{duration: 2, onProgress: func(value float64) { progress = append(progress, value) }}
	for _, part := range []string{"out_time_", "us=500000\nout_time_ms=1000000\n", "out_time_us=NaN\n", strings.Repeat("z", 100_000), "\nout_time_us=1500000\nprogress=end\n"} {
		parser.Write([]byte(part))
		if len(parser.line) > 8<<10 {
			t.Fatal("progress parsing exceeded its memory limit")
		}
	}
	if len(progress) != 4 || progress[0] != 0.25 || progress[1] != 0.5 || progress[2] != 0.75 || progress[3] != 1 {
		t.Fatalf("incremental progress was not parsed: %v", progress)
	}
}

type studioClipRendererFixture struct {
	dir     string
	input   string
	output  string
	log     string
	ffmpeg  string
	ffprobe string
}

func newStudioClipRendererFixture(t *testing.T) studioClipRendererFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock executable fixtures use the Unix shell")
	}
	dir := t.TempDir()
	fixture := studioClipRendererFixture{
		dir: dir, input: filepath.Join(dir, "source clip.mp4"), output: filepath.Join(dir, "edited clip.mp4"),
		log: filepath.Join(dir, "arguments.txt"), ffmpeg: filepath.Join(dir, "ffmpeg"), ffprobe: filepath.Join(dir, "ffprobe"),
	}
	if err := os.WriteFile(fixture.input, []byte("fixture media"), 0600); err != nil {
		t.Fatal(err)
	}
	probe := `#!/bin/sh
for arg do last=$arg; done
if [ "$last" = "$GLOWBOM_CLIP_TEST_INPUT" ]; then
    printf '%s' "$GLOWBOM_CLIP_TEST_SOURCE_JSON"
else
    printf '%s' "$GLOWBOM_CLIP_TEST_OUTPUT_JSON"
fi
`
	renderer := `#!/bin/sh
printf 'CALL\n' >> "$GLOWBOM_CLIP_TEST_LOG"
printf '%s\n' "$@" >> "$GLOWBOM_CLIP_TEST_LOG"
for arg do last=$arg; done
printf 'fixture result' > "$last"
if [ "$GLOWBOM_CLIP_TEST_BEHAVIOR" = 'block' ]; then
    sleep 30 &
    wait
fi
if [ "$GLOWBOM_CLIP_TEST_BEHAVIOR" = 'fail' ]; then
    printf 'private path /Users/private and token fixture-secret\n' >&2
    exit 1
fi
printf 'out_time_us=500000\nprogress=continue\nout_time_us=1000000\nprogress=end\n'
`
	for path, script := range map[string]string{fixture.ffprobe: probe, fixture.ffmpeg: renderer} {
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GLOWBOM_CLIP_TEST_INPUT", fixture.input)
	t.Setenv("GLOWBOM_CLIP_TEST_LOG", fixture.log)
	t.Setenv("GLOWBOM_CLIP_TEST_SOURCE_JSON", `{"streams":[{"codec_type":"video","width":1920,"height":1080,"avg_frame_rate":"60/1"},{"codec_type":"audio"}],"format":{"duration":"12"}}`)
	t.Setenv("GLOWBOM_CLIP_TEST_OUTPUT_JSON", `{"streams":[{"codec_type":"video","width":1280,"height":720,"avg_frame_rate":"30/1"}],"format":{"duration":"2.4"}}`)
	t.Setenv("GLOWBOM_CLIP_TEST_BEHAVIOR", "")
	return fixture
}

func TestStudioClipProbeValidatesActualBoundedMetadata(t *testing.T) {
	fixture := newStudioClipRendererFixture(t)
	info, err := probeStudioClip(context.Background(), fixture.ffprobe, fixture.input)
	if err != nil || info.DurationSeconds != 12 || info.Width != 1920 || !info.HasAudio {
		t.Fatalf("valid media probe failed: %+v %v", info, err)
	}
	for _, metadata := range []string{
		`{"streams":[],"format":{"duration":"12"}}`,
		`{"streams":[{"codec_type":"video","width":20000,"height":1080}],"format":{"duration":"12"}}`,
		`{"streams":[{"codec_type":"video","width":1280,"height":720}],"format":{"duration":"NaN"}}`,
		`{"streams":[{"codec_type":"video","width":1280,"height":720}],"format":{"duration":"601"}}`,
		`{"streams":[{"codec_type":"video","width":1280,"height":720,"disposition":{"attached_pic":1}}],"format":{"duration":"12"}}`,
		strings.Repeat("x", studioClipProbeByteLimit+1),
	} {
		t.Setenv("GLOWBOM_CLIP_TEST_SOURCE_JSON", metadata)
		if _, err := probeStudioClip(context.Background(), fixture.ffprobe, fixture.input); err == nil {
			t.Fatal("invalid or oversized media metadata accepted")
		}
	}
}

func TestStudioClipRenderCommandsKeepReverseBufferBounded(t *testing.T) {
	for _, mode := range []string{"trim", "reverse", "loop"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newStudioClipRendererFixture(t)
			if mode == "loop" {
				t.Setenv("GLOWBOM_CLIP_TEST_OUTPUT_JSON", `{"streams":[{"codec_type":"video","width":1280,"height":720,"avg_frame_rate":"30/1"}],"format":{"duration":"4.8"}}`)
			}
			progress := []float64{}
			info, err := renderStudioClip(context.Background(), fixture.ffmpeg, fixture.ffprobe, fixture.input, fixture.output,
				studioClipSelection{StartSeconds: 3, EndSeconds: 5.4, Mode: mode, Mute: true}, func(value float64) { progress = append(progress, value) })
			if err != nil || info.Width != 1280 || info.HasAudio {
				t.Fatalf("render failed: %+v %v", info, err)
			}
			if len(progress) < 2 || progress[0] != 0 || progress[len(progress)-1] != 1 {
				t.Fatalf("render never completed progress: %v", progress)
			}
			for index := 1; index < len(progress); index++ {
				if progress[index] <= progress[index-1] {
					t.Fatal("progress decreased or repeated")
				}
			}
			data, err := os.ReadFile(fixture.log)
			if err != nil {
				t.Fatal(err)
			}
			calls := strings.Split(strings.TrimPrefix(string(data), "CALL\n"), "CALL\n")
			wantCalls := 5
			if mode == "trim" {
				wantCalls = 1
			}
			if len(calls) != wantCalls {
				t.Fatalf("got %d child processes, want %d", len(calls), wantCalls)
			}
			if !strings.Contains(calls[0], "-ss\n3.000000\n-i\n"+fixture.input) || !strings.Contains(calls[0], "fps=30") || !strings.Contains(calls[0], "force_divisible_by=2") {
				t.Fatal("forward rendering did not seek and normalize before editing")
			}
			for _, call := range calls {
				if !strings.Contains(call, "-filter_threads\n2\n") || !strings.Contains(call, "-filter_complex_threads\n2\n") {
					t.Fatal("child process filter threads are unbounded")
				}
			}
			if mode != "trim" {
				for index, name := range []string{"reverse-02.mkv", "reverse-01.mkv", "reverse-00.mkv"} {
					call := calls[index+1]
					if !strings.Contains(call, name) || !strings.Contains(call, ".studio-clip-") || !strings.Contains(call, "forward.mkv\n") || !strings.Contains(call, ",setpts=PTS-STARTPTS,reverse,setpts=PTS-STARTPTS") {
						t.Fatal("reverse did not use the normalized disk proxy in reverse chunk order")
					}
					if !strings.Contains(call, "trim=duration=1.000000") && !strings.Contains(call, "trim=duration=0.400000") {
						t.Fatal("reverse buffered more than one second")
					}
				}
				if !strings.Contains(calls[len(calls)-1], "-safe\n1\n") || !strings.Contains(calls[len(calls)-1], "-c:v\ncopy\n") {
					t.Fatal("final assembly unnecessarily re-encoded the video")
				}
			}
			assertStudioClipTemporaryFilesRemoved(t, fixture)
		})
	}
}

func TestStudioClipRenderPreservesOrMutesAudio(t *testing.T) {
	fixture := newStudioClipRendererFixture(t)
	t.Setenv("GLOWBOM_CLIP_TEST_OUTPUT_JSON", `{"streams":[{"codec_type":"video","width":1280,"height":720},{"codec_type":"audio"}],"format":{"duration":"2"}}`)
	_, err := renderStudioClip(context.Background(), fixture.ffmpeg, fixture.ffprobe, fixture.input, fixture.output,
		studioClipSelection{StartSeconds: 1, EndSeconds: 2, Mode: "loop"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	commands := string(data)
	for _, fragment := range []string{"atrim=duration=1.000000", "areverse,asetpts=PTS-STARTPTS", "-c:a\npcm_s16le\n", "-c:a\naac\n", "-movflags\n+faststart\n"} {
		if !strings.Contains(commands, fragment) {
			t.Fatalf("audio handling missing %s", fragment)
		}
	}
}

func TestStudioClipCancellationKillsProcessAndRemovesPartialFiles(t *testing.T) {
	fixture := newStudioClipRendererFixture(t)
	t.Setenv("GLOWBOM_CLIP_TEST_BEHAVIOR", "block")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := renderStudioClip(ctx, fixture.ffmpeg, fixture.ffprobe, fixture.input, fixture.output,
			studioClipSelection{EndSeconds: 2, Mode: "trim", Mute: true}, nil)
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(fixture.output); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child process did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not kill and reap the child process")
	}
	if _, err := os.Stat(fixture.output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancellation left a partial output file")
	}
	assertStudioClipTemporaryFilesRemoved(t, fixture)
}

func TestStudioClipFailureKeepsPrivateDiagnosticsOutOfErrors(t *testing.T) {
	fixture := newStudioClipRendererFixture(t)
	t.Setenv("GLOWBOM_CLIP_TEST_BEHAVIOR", "fail")
	_, err := renderStudioClip(context.Background(), fixture.ffmpeg, fixture.ffprobe, fixture.input, fixture.output,
		studioClipSelection{EndSeconds: 2, Mode: "reverse", Mute: true}, nil)
	if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("private diagnostics exposed: %v", err)
	}
	if _, err := os.Stat(fixture.output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed render left a partial output")
	}
	assertStudioClipTemporaryFilesRemoved(t, fixture)
}

func TestStudioClipRenderRefusesToOverwriteExistingFile(t *testing.T) {
	fixture := newStudioClipRendererFixture(t)
	if err := os.WriteFile(fixture.output, []byte("previous edit"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := renderStudioClip(context.Background(), fixture.ffmpeg, fixture.ffprobe, fixture.input, fixture.output,
		studioClipSelection{EndSeconds: 2, Mode: "trim", Mute: true}, nil)
	if err == nil {
		t.Fatal("existing clip overwritten")
	}
	data, err := os.ReadFile(fixture.output)
	if err != nil || string(data) != "previous edit" {
		t.Fatal("previous clip changed")
	}
}

func TestStudioClipRenderWithFFmpeg(t *testing.T) {
	ffmpeg, ffprobe := os.Getenv("GLOWBOM_TEST_FFMPEG"), os.Getenv("GLOWBOM_TEST_FFPROBE")
	if ffmpeg == "" || ffprobe == "" {
		t.Skip("set GLOWBOM_TEST_FFMPEG and GLOWBOM_TEST_FFPROBE to test actual media")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "colors.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := []string{
		"-threads", "2", "-f", "lavfi", "-i", "color=red:s=64x64:r=30:d=1",
		"-f", "lavfi", "-i", "color=lime:s=64x64:r=30:d=1",
		"-f", "lavfi", "-i", "color=blue:s=64x64:r=30:d=1",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3",
		"-filter_complex", "[0:v][1:v][2:v]concat=n=3:v=1:a=0[v]", "-map", "[v]", "-map", "3:a:0",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-threads", "2", "-c:a", "aac", "-t", "3", input,
	}
	if err := runStudioClipFFmpeg(ctx, ffmpeg, args, 3, nil); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"trim", "reverse", "loop"} {
		for _, mute := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "-audio", true: "-muted"}[mute], func(t *testing.T) {
				output := filepath.Join(dir, mode+map[bool]string{false: "-audio", true: "-muted"}[mute]+".mp4")
				selection := studioClipSelection{StartSeconds: 0.37, EndSeconds: 2.78, Mode: mode, Mute: mute}
				info, err := renderStudioClip(ctx, ffmpeg, ffprobe, input, output, selection, nil)
				if err != nil {
					t.Fatal(err)
				}
				wantDuration := 2.41
				if mode == "loop" {
					wantDuration *= 2
				}
				if math.Abs(info.DurationSeconds-wantDuration) > 0.06 || info.HasAudio == mute {
					t.Fatalf("unexpected duration or audio: %+v", info)
				}
				first, last := "red", "blue"
				if mode == "reverse" {
					first, last = "blue", "red"
				} else if mode == "loop" {
					last = "red"
				}
				assertStudioClipFrameColor(t, ctx, ffmpeg, output, 0.05, first)
				assertStudioClipFrameColor(t, ctx, ffmpeg, output, wantDuration-0.08, last)
				if mode == "loop" {
					assertStudioClipFrameColor(t, ctx, ffmpeg, output, 2.46, "blue")
				}
			})
		}
	}
}

func TestStudioClipRenderTenSecond720p(t *testing.T) {
	ffmpeg, ffprobe := os.Getenv("GLOWBOM_TEST_FFMPEG"), os.Getenv("GLOWBOM_TEST_FFPROBE")
	if ffmpeg == "" || ffprobe == "" || os.Getenv("GLOWBOM_TEST_CLIP_PERFORMANCE") != "1" {
		t.Skip("set FFmpeg paths and GLOWBOM_TEST_CLIP_PERFORMANCE=1 for the 720p render check")
	}
	dir := t.TempDir()
	input, output := filepath.Join(dir, "source.mp4"), filepath.Join(dir, "reverse.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := []string{"-threads", "2", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=10", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-pix_fmt", "yuv420p", "-t", "10", input}
	if err := runStudioClipFFmpeg(ctx, ffmpeg, args, 10, nil); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	info, err := renderStudioClip(ctx, ffmpeg, ffprobe, input, output, studioClipSelection{EndSeconds: 10, Mode: "reverse", Mute: true}, nil)
	if err != nil || info.Width != 1280 || info.Height != 720 || math.Abs(info.DurationSeconds-10) > 0.06 {
		t.Fatalf("10 second 720p render failed: %+v %v", info, err)
	}
	t.Logf("10 second 720p reverse finished in %s", time.Since(started).Round(time.Millisecond))
}

func assertStudioClipFrameColor(t *testing.T, ctx context.Context, ffmpeg, input string, seconds float64, color string) {
	t.Helper()
	args := append(studioClipInputArgs(input, seconds), "-v", "error", "-frames:v", "1", "-an", "-vf", "scale=2:2", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
	cmd := studioClipCommand(ctx, ffmpeg, args...)
	cmd.WaitDelay = time.Second
	configureCursorCancellation(cmd)
	output, stderr := &studioClipBoundedOutput{limit: 12}, &studioClipBoundedOutput{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil || len(output.data) != 12 || output.overflow {
		t.Fatalf("could not inspect output frame: %v", err)
	}
	channel := map[string]int{"red": 0, "lime": 1, "blue": 2}[color]
	for pixel := range 4 {
		if output.data[pixel*3+channel] < 220 {
			t.Fatalf("frame at %.2fs expected %s, got %v", seconds, color, output.data[:3])
		}
	}
}

func assertStudioClipTemporaryFilesRemoved(t *testing.T, fixture studioClipRendererFixture) {
	t.Helper()
	entries, err := os.ReadDir(fixture.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".studio-clip-") {
			t.Fatal("render left temporary proxy files")
		}
	}
}
