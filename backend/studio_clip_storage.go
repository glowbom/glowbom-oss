package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const studioClipSourceLimit = 80 << 20
const studioClipRecordLimit = 120 << 20

// Read the shared native record without allocating its encoded video string.
func readStudioClipEnvelope(ctx context.Context, path string, output io.Writer) (studioAssetMetadata, error) {
	var metadata studioAssetMetadata
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > studioClipRecordLimit {
		return metadata, errors.New("The Studio file is unavailable or too large.")
	}
	file, err := os.Open(path)
	if err != nil {
		return metadata, errors.New("Could not read the Studio file.")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return metadata, errors.New("The Studio file changed while opening.")
	}
	r := bufio.NewReaderSize(&studioClipContextReader{ctx: ctx, reader: io.LimitReader(file, studioClipRecordLimit+1)}, 64<<10)
	if token, err := studioClipJSONToken(r); err != nil || token != '{' {
		return metadata, errors.New("The Studio record is invalid.")
	}
	fields := make(map[string]json.RawMessage)
	seen := make(map[string]bool)
	closed := false
	for count := 0; count < 64; count++ {
		token, err := studioClipJSONToken(r)
		if err != nil {
			return metadata, errors.New("The Studio record is incomplete.")
		}
		if token == '}' {
			if count > 0 {
				return metadata, errors.New("The Studio record has a trailing comma.")
			}
			closed = true
			break
		}
		if token != '"' {
			return metadata, errors.New("The Studio record is invalid.")
		}
		keyBytes, err := io.ReadAll(io.LimitReader(&studioClipJSONString{reader: r}, 129))
		if err != nil || len(keyBytes) > 128 || seen[string(keyBytes)] {
			return metadata, errors.New("The Studio record has invalid fields.")
		}
		key := string(keyBytes)
		seen[key] = true
		if token, err = studioClipJSONToken(r); err != nil || token != ':' {
			return metadata, errors.New("The Studio record is invalid.")
		}
		if key == "dataBase64" || key == "thumbnailBase64" {
			token, err = studioClipJSONToken(r)
			if err != nil {
				return metadata, err
			}
			if token == '"' {
				value := &studioClipJSONString{reader: r}
				if key == "dataBase64" && output != nil {
					encoded := bufio.NewReaderSize(value, 32<<10)
					prefix, _ := encoded.Peek(5)
					if string(prefix) == "data:" {
						var header []byte
						for len(header) < 128 {
							b, err := encoded.ReadByte()
							if err != nil {
								break
							}
							header = append(header, b)
							if b == ',' {
								break
							}
						}
						if string(header) != "data:video/mp4;base64," {
							return metadata, errors.New("The stored video encoding is unsupported.")
						}
					}
					n, err := io.CopyBuffer(output, io.LimitReader(base64.NewDecoder(base64.StdEncoding, encoded), studioClipSourceLimit+1), make([]byte, 64<<10))
					if err != nil || n == 0 || n > studioClipSourceLimit || !value.done {
						return metadata, errors.New("The stored video is invalid or exceeds 80 MB.")
					}
				} else if _, err := io.Copy(io.Discard, value); err != nil {
					return metadata, errors.New("The stored media is invalid.")
				}
			} else {
				_ = r.UnreadByte()
				raw, err := studioClipJSONValue(r)
				if err != nil || string(raw) != "null" || (key == "dataBase64" && output != nil) {
					return metadata, errors.New("The stored media is missing.")
				}
			}
		} else {
			raw, err := studioClipJSONValue(r)
			if err != nil {
				return metadata, err
			}
			fields[key] = raw
		}
		token, err = studioClipJSONToken(r)
		if err != nil {
			return metadata, errors.New("The Studio record is incomplete.")
		}
		if token == '}' {
			closed = true
			break
		}
		if token != ',' || count == 63 {
			return metadata, errors.New("The Studio record is invalid.")
		}
	}
	if !closed {
		return metadata, errors.New("The Studio record is incomplete.")
	}
	if _, err := studioClipJSONToken(r); err != io.EOF {
		return metadata, errors.New("The Studio record has trailing data.")
	}
	if output != nil && !seen["dataBase64"] {
		return metadata, errors.New("The Studio video is missing.")
	}
	data, err := json.Marshal(fields)
	if err != nil || len(data) > 64<<10 {
		return metadata, errors.New("The Studio metadata is too large.")
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return metadata, errors.New("The Studio metadata is invalid.")
	}
	if metadata.MediaType == "" {
		metadata.MediaType = "image"
	}
	return metadata, ctx.Err()
}

type studioClipContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *studioClipContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// JSONEncoder can escape '/' in base64. Decode escapes a buffer at a time.
type studioClipJSONString struct {
	reader *bufio.Reader
	done   bool
}

func (s *studioClipJSONString) Read(p []byte) (int, error) {
	if s.done {
		return 0, io.EOF
	}
	for i := range p {
		b, err := s.reader.ReadByte()
		if err != nil {
			return i, io.ErrUnexpectedEOF
		}
		if b == '"' {
			s.done = true
			return i, io.EOF
		}
		if b == '\\' {
			b, err = s.reader.ReadByte()
			if err != nil {
				return i, io.ErrUnexpectedEOF
			}
			switch b {
			case '"', '/', '\\':
			case 'n':
				b = '\n'
			case 'r':
				b = '\r'
			case 't':
				b = '\t'
			case 'b':
				b = '\b'
			case 'f':
				b = '\f'
			case 'u':
				var digits [4]byte
				if _, err := io.ReadFull(s.reader, digits[:]); err != nil {
					return i, err
				}
				value, err := strconv.ParseUint(string(digits[:]), 16, 16)
				if err != nil || value > 127 {
					return i, errors.New("Invalid encoded media escape")
				}
				b = byte(value)
			default:
				return i, errors.New("Invalid encoded media escape")
			}
		} else if b < 32 {
			return i, errors.New("Invalid encoded media string")
		}
		p[i] = b
	}
	return len(p), nil
}

func studioClipJSONToken(r *bufio.Reader) (byte, error) {
	for {
		b, err := r.ReadByte()
		if err != nil || !strings.ContainsRune(" \n\r\t", rune(b)) {
			return b, err
		}
	}
}

func studioClipJSONValue(r *bufio.Reader) ([]byte, error) {
	var value []byte
	depth, quoted, escaped := 0, false, false
	for len(value) <= 64<<10 {
		b, err := r.ReadByte()
		if err != nil {
			return nil, errors.New("The Studio record is incomplete.")
		}
		if !quoted && depth == 0 && (b == ',' || b == '}') {
			_ = r.UnreadByte()
			value = []byte(strings.TrimSpace(string(value)))
			if !json.Valid(value) {
				return nil, errors.New("The Studio record is invalid.")
			}
			return value, nil
		}
		value = append(value, b)
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
			}
		} else if b == '"' {
			quoted = true
		} else if b == '[' || b == '{' {
			depth++
		} else if b == ']' || b == '}' {
			depth--
		}
	}
	return nil, errors.New("The Studio metadata is too large.")
}

func studioClipRecordPath(id string) (string, error) {
	id = normalizedStudioUUID(id)
	if id == "" {
		return "", os.ErrNotExist
	}
	dir, err := studioAssetsDirectory()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(strings.ToUpper(entry.Name()), "_"+id+".JSON") {
			return filepath.Join(dir, entry.Name()), nil
		}
	}
	return "", os.ErrNotExist
}

// Keep the native Studio JSON format, but stream the encoded result to disk.
func saveStudioClipFile(ctx context.Context, jobID string, source studioAssetMetadata, inputPath string, media studioClipMediaInfo, mode string) (studioImageSummary, error) {
	return saveStudioClipFileWithCommit(ctx, jobID, source, inputPath, media, mode, nil)
}

func saveStudioClipFileWithCommit(ctx context.Context, jobID string, source studioAssetMetadata, inputPath string, media studioClipMediaInfo, mode string, commit func(studioImageSummary, func() error) error) (studioImageSummary, error) {
	var empty studioImageSummary
	input, err := os.Open(inputPath)
	if err != nil {
		return empty, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > studioClipSourceLimit {
		return empty, errors.New("The edited clip is unavailable or too large.")
	}
	now := time.Now().UTC()
	id := studioGenerationAssetID("clip_" + jobID)
	label := map[string]string{"trim": "Trimmed", "reverse": "Reversed", "loop": "Forward and back"}[mode]
	record := studioImageRecord{
		ID: id, Timestamp: now.Format("2006-01-02T15:04:05Z"), MediaType: "video", FileSize: info.Size(),
		Prompt: label + ": " + source.Prompt, AssetType: "generated", SourceService: "Glowbom video editor", SourceType: "derived",
		SourceAssetID: source.ID, Dimensions: &studioDimensions{Width: media.Width, Height: media.Height}, Duration: &media.DurationSeconds,
		UsedInProjects: []string{}, Tags: []string{},
	}
	data, err := json.Marshal(record)
	if err != nil {
		return empty, err
	}
	needle := []byte(`"dataBase64":""`)
	before, after, found := strings.Cut(string(data), string(needle))
	if !found {
		return empty, errors.New("Could not prepare the edited clip record.")
	}
	dir, err := studioAssetsDirectory()
	if err != nil {
		return empty, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return empty, err
	}
	temp, err := os.CreateTemp(dir, ".clip-*.tmp")
	if err != nil {
		return empty, err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	writer := bufio.NewWriterSize(temp, 64<<10)
	if _, err := io.WriteString(writer, before+`"dataBase64":"`); err != nil {
		return empty, err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, writer)
	if _, err := io.CopyBuffer(encoder, &studioClipContextReader{ctx: ctx, reader: input}, make([]byte, 64<<10)); err != nil {
		return empty, err
	}
	if err := encoder.Close(); err != nil {
		return empty, err
	}
	if _, err := io.WriteString(writer, `"`+after); err != nil {
		return empty, err
	}
	if err := writer.Flush(); err != nil {
		return empty, err
	}
	if err := temp.Sync(); err != nil {
		return empty, err
	}
	if err := temp.Close(); err != nil {
		return empty, err
	}
	name := fmt.Sprintf("%.6f_%s.json", float64(now.UnixNano())/1e9, id)
	asset := summarizeStudioRecord(record)
	publish := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return os.Rename(temp.Name(), filepath.Join(dir, name))
	}
	if commit == nil {
		commit = func(_ studioImageSummary, publish func() error) error { return publish() }
	}
	if err := commit(asset, publish); err != nil {
		return empty, err
	}
	return asset, nil
}
