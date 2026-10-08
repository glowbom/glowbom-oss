package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const folderPickerCanceledToken = "__GLOWBOM_PICKER_CANCELED__"
const folderPickerOpenPrompt = "Select local Glowbom project folder"
const folderPickerSavePrompt = "Choose where to save your Glowbom project"

var nativeProjectPickerMu sync.Mutex
var errNativeProjectPickerBusy = errors.New("A folder chooser is already open.")

func folderPickerPrompt(purpose string) string {
	if purpose == "save" {
		return folderPickerSavePrompt
	}
	return folderPickerOpenPrompt
}

type openCodeProjectPickResponse struct {
	Success  bool   `json:"success"`
	Path     string `json:"path,omitempty"`
	Canceled bool   `json:"canceled,omitempty"`
	Source   string `json:"source,omitempty"`
	Error    string `json:"error,omitempty"`
}

type openCodeInstructionPickedFile struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	MimeType  string `json:"mimeType,omitempty"`
}

type openCodeInstructionFilesPickResponse struct {
	Success  bool                            `json:"success"`
	Files    []openCodeInstructionPickedFile `json:"files,omitempty"`
	Canceled bool                            `json:"canceled,omitempty"`
	Source   string                          `json:"source,omitempty"`
	Error    string                          `json:"error,omitempty"`
}

type nativePickerCommand struct {
	Executable      string
	Args            []string
	CancelExitCodes []int
}

func openCodePickProjectFolderHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var (
		path     string
		canceled bool
		err      error
	)

	start, prompt := pickerStartPath(w, r)
	switch runtime.GOOS {
	case "darwin":
		path, canceled, err = pickProjectFolderMacOS(start, prompt)
	case "windows":
		path, canceled, err = pickProjectFolderWindows(prompt)
	case "linux":
		path, canceled, err = pickProjectFolderLinux(prompt)
	default:
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(openCodeProjectPickResponse{
			Success: false,
			Error:   "Native folder picker is only available on macOS, Windows, and Linux.",
		})
		return
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(openCodeProjectPickResponse{
			Success: false,
			Error:   "Failed to open native folder picker: " + err.Error(),
		})
		return
	}

	if canceled {
		_ = json.NewEncoder(w).Encode(openCodeProjectPickResponse{
			Success:  true,
			Canceled: true,
			Source:   "native",
		})
		return
	}

	info, statErr := os.Stat(path)
	if statErr != nil || !info.IsDir() {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(openCodeProjectPickResponse{
			Success: false,
			Error:   "Selected path is not a valid folder.",
		})
		return
	}

	_ = json.NewEncoder(w).Encode(openCodeProjectPickResponse{
		Success: true,
		Path:    canonicalDirectory(path),
		Source:  "native",
	})
}

func pickerStartPath(w http.ResponseWriter, r *http.Request) (string, string) {
	var req struct {
		Path    string `json:"path"`
		Purpose string `json:"purpose"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	}
	prompt := folderPickerPrompt(req.Purpose)
	if dir := existingDirectory(canonicalDirectory(req.Path)); dir != "" {
		return dir, prompt
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", prompt
	}
	return existingDirectory(canonicalDirectory(home)), prompt
}

func existingDirectory(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ""
	}
	return path
}

func canonicalDirectory(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.Clean(trimmed)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return cleaned
	}
	return resolved
}

func openCodePickInstructionFilesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var (
		paths    []string
		canceled bool
		err      error
	)

	switch runtime.GOOS {
	case "darwin":
		paths, canceled, err = pickInstructionFilesMacOS()
	case "windows":
		paths, canceled, err = pickInstructionFilesWindows()
	case "linux":
		paths, canceled, err = pickInstructionFilesLinux()
	default:
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(openCodeInstructionFilesPickResponse{
			Success: false,
			Error:   "Native file picker is only available on macOS, Windows, and Linux.",
		})
		return
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(openCodeInstructionFilesPickResponse{
			Success: false,
			Error:   "Failed to open native file picker: " + err.Error(),
		})
		return
	}

	if canceled {
		_ = json.NewEncoder(w).Encode(openCodeInstructionFilesPickResponse{
			Success:  true,
			Canceled: true,
			Source:   "native",
		})
		return
	}

	files := make([]openCodeInstructionPickedFile, 0, len(paths))
	for _, rawPath := range paths {
		path := strings.TrimSpace(rawPath)
		if path == "" {
			continue
		}

		info, statErr := os.Stat(path)
		if statErr != nil || info.IsDir() {
			continue
		}

		absPath, absErr := filepath.Abs(path)
		if absErr != nil {
			continue
		}

		files = append(files, openCodeInstructionPickedFile{
			Path:      absPath,
			Name:      filepath.Base(absPath),
			SizeBytes: info.Size(),
			MimeType:  inferMimeTypeForPath(absPath),
		})
	}

	_ = json.NewEncoder(w).Encode(openCodeInstructionFilesPickResponse{
		Success: true,
		Files:   files,
		Source:  "native",
	})
}

func pickProjectFolderMacOS(start, prompt string) (string, bool, error) {
	return pickProjectFolderMacOSWithContext(context.Background(), start, prompt)
}

func pickProjectFolderMacOSWithContext(ctx context.Context, start, prompt string, foreground ...bool) (string, bool, error) {
	if !nativeProjectPickerMu.TryLock() {
		return "", false, errNativeProjectPickerBusy
	}
	defer nativeProjectPickerMu.Unlock()
	if prompt != folderPickerSavePrompt {
		prompt = folderPickerOpenPrompt
	}
	args := []string{"-", start, prompt}
	if len(foreground) > 0 && foreground[0] {
		args = append(args, "foreground")
	}
	cmd := exec.CommandContext(ctx, "osascript", args...)
	cmd.Stdin = strings.NewReader(`on run argv
  set pickerPrompt to "Select local Glowbom project folder"
  try
    if (count of argv) > 2 then
      if item 3 of argv is "foreground" then activate
    end if
    if (count of argv) > 1 and item 2 of argv is not "" then
      set pickerPrompt to item 2 of argv
    end if
    if (count of argv) > 0 and item 1 of argv is not "" then
      set startFolder to POSIX file (item 1 of argv) as alias
      set selectedFolder to POSIX path of (choose folder with prompt pickerPrompt default location startFolder)
    else
      set selectedFolder to POSIX path of (choose folder with prompt pickerPrompt)
    end if
    return selectedFolder
  on error number -128
    return "__GLOWBOM_PICKER_CANCELED__"
  end try
end run
`)

	selected, err := runPickerCommand(cmd)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "user canceled") {
			return "", true, nil
		}
		return "", false, err
	}
	if selected == "" {
		return "", false, errors.New("no folder path returned")
	}

	if selected == folderPickerCanceledToken {
		return "", true, nil
	}

	return selected, false, nil
}

func pickInstructionFilesMacOS() ([]string, bool, error) {
	cmd := exec.Command(
		"osascript",
		"-e", "try",
		"-e", `set selectedFiles to choose file with prompt "Select local files for custom instructions" with multiple selections allowed`,
		"-e", "set outputPaths to {}",
		"-e", "repeat with selectedFile in selectedFiles",
		"-e", "set end of outputPaths to POSIX path of selectedFile",
		"-e", "end repeat",
		"-e", "set AppleScript's text item delimiters to linefeed",
		"-e", "set outputText to outputPaths as text",
		"-e", "set AppleScript's text item delimiters to \"\"",
		"-e", "return outputText",
		"-e", "on error number -128",
		"-e", `return "__GLOWBOM_PICKER_CANCELED__"`,
		"-e", "end try",
	)

	selected, err := runPickerCommand(cmd)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "user canceled") {
			return nil, true, nil
		}
		return nil, false, err
	}
	if selected == "" {
		return nil, false, errors.New("no file paths returned")
	}

	if selected == folderPickerCanceledToken {
		return nil, true, nil
	}

	lines := strings.Split(selected, "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, false, errors.New("no valid file paths returned")
	}

	return paths, false, nil
}

func pickProjectFolderLinux(prompt string) (string, bool, error) {
	if prompt != folderPickerSavePrompt {
		prompt = folderPickerOpenPrompt
	}
	startDir := defaultPickerStartDir()
	selected, canceled, err := runLinuxPicker([]nativePickerCommand{
		{
			Executable:      "zenity",
			Args:            []string{"--file-selection", "--directory", "--title=" + prompt},
			CancelExitCodes: []int{1},
		},
		{
			Executable:      "qarma",
			Args:            []string{"--file-selection", "--directory", "--title=" + prompt},
			CancelExitCodes: []int{1},
		},
		{
			Executable:      "yad",
			Args:            []string{"--file-selection", "--directory", "--title=" + prompt},
			CancelExitCodes: []int{1},
		},
		{
			Executable:      "kdialog",
			Args:            []string{"--title", prompt, "--getexistingdirectory", startDir},
			CancelExitCodes: []int{1},
		},
	}, "No supported Linux folder picker found. Install zenity, kdialog, yad, or qarma.")
	if err != nil || canceled {
		return "", canceled, err
	}
	if selected == "" {
		return "", false, errors.New("no folder path returned")
	}

	return selected, false, nil
}

func pickInstructionFilesLinux() ([]string, bool, error) {
	startDir := defaultPickerStartDir()
	selected, canceled, err := runLinuxPicker([]nativePickerCommand{
		{
			Executable:      "zenity",
			Args:            []string{"--file-selection", "--multiple", "--separator=\n", "--title=Select local files for custom instructions"},
			CancelExitCodes: []int{1},
		},
		{
			Executable:      "qarma",
			Args:            []string{"--file-selection", "--multiple", "--separator=\n", "--title=Select local files for custom instructions"},
			CancelExitCodes: []int{1},
		},
		{
			Executable:      "yad",
			Args:            []string{"--file-selection", "--multiple", "--separator=\n", "--title=Select local files for custom instructions"},
			CancelExitCodes: []int{1},
		},
		{
			Executable:      "kdialog",
			Args:            []string{"--getopenfilename", startDir, "", "--multiple", "--separate-output"},
			CancelExitCodes: []int{1},
		},
	}, "No supported Linux file picker found. Install zenity, kdialog, yad, or qarma.")
	if err != nil || canceled {
		return nil, canceled, err
	}
	if selected == "" {
		return nil, false, errors.New("no file paths returned")
	}

	lines := strings.Split(selected, "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, false, errors.New("no valid file paths returned")
	}

	return paths, false, nil
}

func pickProjectFolderWindows(prompt string) (string, bool, error) {
	if prompt != folderPickerSavePrompt {
		prompt = folderPickerOpenPrompt
	}
	newFolder := "$false"
	if prompt == folderPickerSavePrompt {
		newFolder = "$true"
	}
	cmd := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-STA",
		"-Command",
		fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = '%s'
$dialog.ShowNewFolderButton = %s
$result = $dialog.ShowDialog()
if ($result -ne [System.Windows.Forms.DialogResult]::OK -or [string]::IsNullOrWhiteSpace($dialog.SelectedPath)) {
  Write-Output '__GLOWBOM_PICKER_CANCELED__'
  exit 0
}
Write-Output $dialog.SelectedPath`, prompt, newFolder),
	)

	selected, err := runPickerCommand(cmd)
	if err != nil {
		return "", false, err
	}
	if selected == "" {
		return "", false, errors.New("no folder path returned")
	}
	if selected == folderPickerCanceledToken {
		return "", true, nil
	}

	return selected, false, nil
}

func pickInstructionFilesWindows() ([]string, bool, error) {
	cmd := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-STA",
		"-Command",
		`Add-Type -AssemblyName System.Windows.Forms
$dialog = New-Object System.Windows.Forms.OpenFileDialog
$dialog.Title = 'Select local files for custom instructions'
$dialog.Multiselect = $true
$dialog.CheckFileExists = $true
$dialog.CheckPathExists = $true
$result = $dialog.ShowDialog()
if ($result -ne [System.Windows.Forms.DialogResult]::OK -or $dialog.FileNames.Count -eq 0) {
  Write-Output '__GLOWBOM_PICKER_CANCELED__'
  exit 0
}
$dialog.FileNames | ForEach-Object { Write-Output $_ }`,
	)

	selected, err := runPickerCommand(cmd)
	if err != nil {
		return nil, false, err
	}
	if selected == "" {
		return nil, false, errors.New("no file paths returned")
	}
	if selected == folderPickerCanceledToken {
		return nil, true, nil
	}

	lines := strings.Split(selected, "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, false, errors.New("no valid file paths returned")
	}

	return paths, false, nil
}

func runPickerCommand(cmd *exec.Cmd) (string, error) {
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return "", errors.New(stderr)
			}
		}
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}

func runPickerCommandWithCancel(cmd *exec.Cmd, cancelExitCodes ...int) (string, bool, error) {
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			for _, code := range cancelExitCodes {
				if exitErr.ExitCode() == code {
					return "", true, nil
				}
			}

			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return "", false, errors.New(stderr)
			}
		}
		return "", false, err
	}

	selected := strings.TrimSpace(string(output))
	if selected == folderPickerCanceledToken {
		return "", true, nil
	}

	return selected, false, nil
}

func runLinuxPicker(commands []nativePickerCommand, missingMessage string) (string, bool, error) {
	for _, candidate := range commands {
		executable, err := exec.LookPath(candidate.Executable)
		if err != nil {
			continue
		}

		selected, canceled, runErr := runPickerCommandWithCancel(
			exec.Command(executable, candidate.Args...),
			candidate.CancelExitCodes...,
		)
		if runErr != nil || canceled {
			return "", canceled, runErr
		}
		return selected, false, nil
	}

	return "", false, errors.New(missingMessage)
}

func defaultPickerStartDir() string {
	homeDir, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(homeDir) != "" {
		return homeDir
	}
	return "."
}

func inferMimeTypeForPath(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return "application/octet-stream"
	}

	if mimeType := mime.TypeByExtension(ext); mimeType != "" {
		return mimeType
	}

	return "application/octet-stream"
}
