package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	studioClipReverseLimit   = 10.0
	studioClipTrimLimit      = 10.0
	studioClipMinimumRange   = 0.1
	studioClipProbeTimeout   = 15 * time.Second
	studioClipRenderTimeout  = 3 * time.Minute
	studioClipProbeByteLimit = 64 << 10
)

type studioClipSelection struct {
	StartSeconds float64 `json:"startSeconds"`
	EndSeconds   float64 `json:"endSeconds"`
	Mode         string  `json:"mode"`
	Mute         bool    `json:"mute"`
}

type studioClipMediaInfo struct {
	DurationSeconds float64 `json:"durationSeconds"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	HasAudio        bool    `json:"hasAudio"`
	frameRate       float64
}

// A capped writer drains child output without retaining unbounded media or logs.
type studioClipBoundedOutput struct {
	data     []byte
	limit    int
	overflow bool
}

func (output *studioClipBoundedOutput) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := output.limit - len(output.data)
	if len(data) > remaining {
		output.overflow = true
		data = data[:remaining]
	}
	output.data = append(output.data, data...)
	return originalLength, nil
}

func probeStudioClip(ctx context.Context, ffprobePath, inputPath string) (studioClipMediaInfo, error) {
	var info studioClipMediaInfo
	file, err := os.Stat(inputPath)
	if err != nil || !file.Mode().IsRegular() || file.Size() <= 0 {
		return info, errors.New("The saved clip is unavailable.")
	}
	ctx, cancel := context.WithTimeout(ctx, studioClipProbeTimeout)
	defer cancel()
	cmd := studioClipCommand(ctx, ffprobePath,
		"-v", "error", "-threads", "2", "-protocol_whitelist", "file,pipe",
		"-show_entries", "stream=codec_type,width,height,duration,avg_frame_rate:stream_disposition=attached_pic:format=duration",
		"-of", "json", inputPath,
	)
	cmd.WaitDelay = time.Second
	configureCursorCancellation(cmd)
	output := &studioClipBoundedOutput{limit: studioClipProbeByteLimit}
	stderr := &studioClipBoundedOutput{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return info, fmt.Errorf("Clip inspection stopped: %w", ctx.Err())
		}
		return info, errors.New("Could not inspect the saved clip.")
	}
	if output.overflow {
		return info, errors.New("The clip has too many media tracks.")
	}
	var metadata struct {
		Streams []struct {
			CodecType   string `json:"codec_type"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			Duration    string `json:"duration"`
			AverageRate string `json:"avg_frame_rate"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output.data, &metadata); err != nil {
		return info, errors.New("The clip media information is invalid.")
	}
	var videoDuration float64
	for _, stream := range metadata.Streams {
		if stream.CodecType == "audio" {
			info.HasAudio = true
		}
		if stream.CodecType == "video" && stream.Disposition.AttachedPic == 0 && info.Width == 0 {
			info.Width, info.Height = stream.Width, stream.Height
			videoDuration, _ = strconv.ParseFloat(stream.Duration, 64)
			parts := strings.Split(stream.AverageRate, "/")
			if len(parts) == 2 {
				numerator, _ := strconv.ParseFloat(parts[0], 64)
				denominator, _ := strconv.ParseFloat(parts[1], 64)
				if denominator > 0 {
					info.frameRate = numerator / denominator
				}
			}
		}
	}
	info.DurationSeconds, _ = strconv.ParseFloat(metadata.Format.Duration, 64)
	if studioClipFinite(videoDuration) && videoDuration > 0 {
		info.DurationSeconds = videoDuration
	}
	if info.Width < 2 || info.Height < 2 || info.Width > 4096 || info.Height > 4096 ||
		int64(info.Width)*int64(info.Height) > 8_300_000 ||
		!studioClipFinite(info.DurationSeconds) || info.DurationSeconds <= 0 || info.DurationSeconds > 600 {
		return studioClipMediaInfo{}, errors.New("Choose a video with valid dimensions and duration.")
	}
	return info, nil
}

func studioClipFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateStudioClipSelection(selection studioClipSelection, info studioClipMediaInfo) error {
	if selection.Mode != "trim" && selection.Mode != "reverse" && selection.Mode != "loop" {
		return errors.New("Choose trim, reverse, or forward and back.")
	}
	if !studioClipFinite(selection.StartSeconds) || !studioClipFinite(selection.EndSeconds) ||
		!studioClipFinite(info.DurationSeconds) || info.DurationSeconds <= 0 ||
		selection.StartSeconds < 0 || selection.EndSeconds-selection.StartSeconds < studioClipMinimumRange-0.000001 ||
		selection.EndSeconds > info.DurationSeconds+0.001 {
		return errors.New("Choose at least 0.1 seconds within the saved clip.")
	}
	limit := studioClipTrimLimit
	if selection.Mode != "trim" {
		limit = studioClipReverseLimit
	}
	if selection.EndSeconds-selection.StartSeconds > limit+0.000001 {
		return fmt.Errorf("Select no more than %.0f seconds for this edit.", limit)
	}
	return nil
}

// Reverse processes one second at a time. A 720p, 30 fps YUV420 selection
// therefore buffers about 40 MiB of frames rather than the entire source clip.
func renderStudioClip(ctx context.Context, ffmpegPath, ffprobePath, inputPath, outputPath string, selection studioClipSelection, onProgress func(float64)) (studioClipMediaInfo, error) {
	var result studioClipMediaInfo
	inputPath, err := filepath.Abs(inputPath)
	if err != nil {
		return result, errors.New("The saved clip is unavailable.")
	}
	outputPath, err = filepath.Abs(outputPath)
	if err != nil || inputPath == outputPath {
		return result, errors.New("Choose a new file for the edited clip.")
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("The edited clip destination is already in use.")
	}
	ctx, cancel := context.WithTimeout(ctx, studioClipRenderTimeout)
	defer cancel()
	info, err := probeStudioClip(ctx, ffprobePath, inputPath)
	if err != nil {
		return result, err
	}
	if err := validateStudioClipSelection(selection, info); err != nil {
		return result, err
	}
	workDir, err := os.MkdirTemp(filepath.Dir(outputPath), ".studio-clip-")
	if err != nil {
		return result, errors.New("Could not prepare the edited clip.")
	}
	defer os.RemoveAll(workDir)
	finished := false
	defer func() {
		if !finished {
			os.Remove(outputPath)
		}
	}()
	lastProgress := -1.0
	report := func(progress float64) {
		progress = math.Max(0, math.Min(1, progress))
		if progress <= lastProgress {
			return
		}
		lastProgress = progress
		if onProgress != nil {
			onProgress(progress)
		}
	}
	report(0)
	duration := math.Round((selection.EndSeconds-selection.StartSeconds)*1_000_000) / 1_000_000
	outputDuration := duration
	if selection.Mode == "loop" {
		outputDuration *= 2
	}
	keepAudio := info.HasAudio && !selection.Mute
	if selection.Mode == "trim" {
		args := studioClipInputArgs(inputPath, selection.StartSeconds)
		args = append(args, studioClipNormalizeArgs(info, duration, keepAudio, false)...)
		args = append(args, studioClipMP4Args(keepAudio, false)...)
		args = append(args, "-t", studioClipSeconds(duration), outputPath)
		err = runStudioClipFFmpeg(ctx, ffmpegPath, args, duration, func(value float64) { report(value * 0.95) })
	} else {
		proxyPath := filepath.Join(workDir, "forward.mkv")
		args := studioClipInputArgs(inputPath, selection.StartSeconds)
		args = append(args, studioClipNormalizeArgs(info, duration, keepAudio, true)...)
		args = append(args, "-t", studioClipSeconds(duration), proxyPath)
		if err = runStudioClipFFmpeg(ctx, ffmpegPath, args, duration, func(value float64) { report(value * 0.25) }); err != nil {
			return result, err
		}
		segments := []string{}
		if selection.Mode == "loop" {
			segments = append(segments, "forward.mkv")
		}
		chunkCount := int(math.Ceil(duration))
		for index := chunkCount - 1; index >= 0; index-- {
			start := float64(index)
			chunkDuration := math.Min(1, duration-start)
			name := fmt.Sprintf("reverse-%02d.mkv", index)
			args = studioClipInputArgs(proxyPath, start)
			args = append(args, "-map", "0:v:0", "-vf", "trim=duration="+studioClipSeconds(chunkDuration)+",setpts=PTS-STARTPTS,reverse,setpts=PTS-STARTPTS")
			if keepAudio {
				args = append(args, "-map", "0:a:0", "-af", "atrim=duration="+studioClipSeconds(chunkDuration)+",asetpts=PTS-STARTPTS,areverse,asetpts=PTS-STARTPTS", "-c:a", "pcm_s16le", "-ac", "2", "-ar", "48000")
			} else {
				args = append(args, "-an")
			}
			args = append(args, studioClipVideoEncodeArgs(true)...)
			args = append(args, "-t", studioClipSeconds(chunkDuration), filepath.Join(workDir, name))
			chunkNumber := chunkCount - 1 - index
			if err = runStudioClipFFmpeg(ctx, ffmpegPath, args, chunkDuration, func(value float64) {
				report(0.25 + 0.65*(float64(chunkNumber)+value)/float64(chunkCount))
			}); err != nil {
				return result, err
			}
			segments = append(segments, name)
		}
		var manifest strings.Builder
		for _, name := range segments {
			fmt.Fprintf(&manifest, "file '%s'\n", name)
		}
		manifestPath := filepath.Join(workDir, "segments.txt")
		if err = os.WriteFile(manifestPath, []byte(manifest.String()), 0600); err != nil {
			return result, errors.New("Could not assemble the edited clip.")
		}
		args = []string{"-protocol_whitelist", "file,pipe", "-f", "concat", "-safe", "1", "-i", manifestPath, "-map", "0:v:0"}
		if keepAudio {
			args = append(args, "-map", "0:a:0")
		}
		args = append(args, studioClipMP4Args(keepAudio, true)...)
		args = append(args, "-t", studioClipSeconds(outputDuration), outputPath)
		err = runStudioClipFFmpeg(ctx, ffmpegPath, args, outputDuration, func(value float64) { report(0.9 + 0.05*value) })
	}
	if err != nil {
		return result, err
	}
	result, err = probeStudioClip(ctx, ffprobePath, outputPath)
	if err != nil {
		return studioClipMediaInfo{}, err
	}
	if result.Width > 1280 || result.Height > 1280 || result.Width > 720 && result.Height > 720 ||
		result.frameRate > 30.001 || result.HasAudio != keepAudio || math.Abs(result.DurationSeconds-outputDuration) > 0.1 {
		return studioClipMediaInfo{}, errors.New("The edited clip did not meet the output limits.")
	}
	if ctx.Err() != nil {
		return studioClipMediaInfo{}, fmt.Errorf("Clip editing stopped: %w", ctx.Err())
	}
	finished = true
	report(1)
	return result, nil
}

func studioClipSeconds(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 6, 64)
}

func studioClipInputArgs(inputPath string, start float64) []string {
	return []string{"-protocol_whitelist", "file,pipe", "-threads", "2", "-ss", studioClipSeconds(start), "-i", inputPath}
}

func studioClipNormalizeArgs(info studioClipMediaInfo, duration float64, keepAudio, intermediate bool) []string {
	scale := "scale=w='min(iw,if(gte(iw,ih),1280,720))':h='min(ih,if(gte(iw,ih),720,1280))':force_original_aspect_ratio=decrease:force_divisible_by=2"
	filter := "trim=duration=" + studioClipSeconds(duration) + ",setpts=PTS-STARTPTS," + scale + ",fps=30,setsar=1,format=yuv420p"
	args := []string{"-map", "0:V:0", "-vf", filter, "-map_metadata", "-1", "-map_chapters", "-1"}
	if keepAudio {
		audioFilter := "atrim=duration=" + studioClipSeconds(duration) + ",asetpts=PTS-STARTPTS,aresample=48000:async=1:first_pts=0,apad=whole_dur=" + studioClipSeconds(duration)
		args = append(args, "-map", "0:a:0", "-af", audioFilter, "-ac", "2", "-ar", "48000")
		if intermediate {
			args = append(args, "-c:a", "pcm_s16le")
		}
	} else {
		args = append(args, "-an")
	}
	if intermediate {
		args = append(args, studioClipVideoEncodeArgs(true)...)
	}
	return args
}

func studioClipVideoEncodeArgs(intermediate bool) []string {
	preset := "veryfast"
	if intermediate {
		preset = "ultrafast"
	}
	return []string{"-c:v", "libx264", "-preset", preset, "-crf", "20", "-pix_fmt", "yuv420p", "-threads", "2", "-g", "30", "-sc_threshold", "0"}
}

func studioClipMP4Args(keepAudio, copyVideo bool) []string {
	args := []string{"-map_metadata", "-1", "-map_chapters", "-1"}
	if copyVideo {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, studioClipVideoEncodeArgs(false)...)
	}
	if keepAudio {
		args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000")
	} else {
		args = append(args, "-an")
	}
	return append(args, "-movflags", "+faststart", "-f", "mp4")
}

type studioClipProgressOutput struct {
	line        []byte
	discardLine bool
	duration    float64
	onProgress  func(float64)
}

func (output *studioClipProgressOutput) Write(data []byte) (int, error) {
	for _, value := range data {
		if value == '\n' {
			if !output.discardLine {
				output.parseLine(string(output.line))
			}
			output.line = output.line[:0]
			output.discardLine = false
		} else if len(output.line) < 8<<10 && !output.discardLine {
			output.line = append(output.line, value)
		} else {
			output.discardLine = true
		}
	}
	return len(data), nil
}

func (output *studioClipProgressOutput) parseLine(line string) {
	key, value, found := strings.Cut(strings.TrimSpace(line), "=")
	if !found || output.onProgress == nil {
		return
	}
	if key == "progress" && value == "end" {
		output.onProgress(1)
		return
	}
	if key != "out_time_us" && key != "out_time_ms" {
		return
	}
	microseconds, err := strconv.ParseFloat(value, 64)
	if err == nil && studioClipFinite(microseconds) && microseconds >= 0 && output.duration > 0 {
		output.onProgress(math.Min(1, microseconds/1_000_000/output.duration))
	}
}

func runStudioClipFFmpeg(ctx context.Context, ffmpegPath string, args []string, duration float64, onProgress func(float64)) error {
	baseArgs := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-nostats", "-n", "-filter_threads", "2", "-filter_complex_threads", "2", "-progress", "pipe:1"}
	cmd := studioClipCommand(ctx, ffmpegPath, append(baseArgs, args...)...)
	cmd.WaitDelay = time.Second
	configureCursorCancellation(cmd)
	cmd.Stdout = &studioClipProgressOutput{duration: duration, onProgress: onProgress}
	cmd.Stderr = &studioClipBoundedOutput{limit: 8 << 10}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Clip editing stopped: %w", ctx.Err())
		}
		return errors.New("Could not render the edited clip with FFmpeg.")
	}
	return nil
}

func studioClipCommand(ctx context.Context, executable string, args ...string) *exec.Cmd {
	if runtime.GOOS != "windows" {
		if nicePath, err := exec.LookPath("nice"); err == nil {
			return exec.CommandContext(ctx, nicePath, append([]string{"-n", "10", executable}, args...)...)
		}
	}
	return exec.CommandContext(ctx, executable, args...)
}
