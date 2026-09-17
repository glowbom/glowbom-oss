package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

const pullUsage = `Usage: glowbom pull [--output NEW_DIRECTORY]

Download your current saved Glowbom project into a new local folder.
Without --output, creates a unique glowbom-project-* folder in the current directory.
The output's parent directory must already exist. Existing files and folders are never replaced.

Finish saving in Glowbom before pulling. This downloads the saved project once;
it does not synchronize changes or upload local edits.
`

type pullOptions struct {
	Output string
}

func parsePullOptions(args []string) (pullOptions, error) {
	var opts pullOptions
	seen := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--help" || args[i] == "-h" {
			return opts, flag.ErrHelp
		}
		name, value, hasValue := strings.Cut(args[i], "=")
		if name != "--output" {
			return opts, errors.New("pull only accepts --output NEW_DIRECTORY; use --help for usage")
		}
		if seen {
			return opts, errors.New("--output may only be provided once")
		}
		seen = true
		if !hasValue {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return opts, errors.New("--output requires a new directory path")
			}
			i++
			value = args[i]
		}
		if strings.TrimSpace(value) == "" {
			return opts, errors.New("--output requires a non-empty directory path")
		}
		opts.Output = value
	}
	return opts, nil
}

type projectAPIError struct {
	status int
	code   string
}

func (e *projectAPIError) Error() string {
	switch {
	case e.status == 401:
		return "your sign-in was not accepted; run glowbom login again"
	case e.status == 404 && e.code == "project_not_found":
		return "no saved project was found; save a project in Glowbom first, then run glowbom pull again"
	case e.status == 409 && e.code == "project_incomplete":
		return "the saved project is incomplete; wait for saving to finish in Glowbom, then run glowbom pull again"
	case e.status == 413:
		return "the saved project exceeds the API's 9 MiB download limit"
	case e.status == 422 && e.code == "invalid_project":
		return "the saved project could not be read; save it again in Glowbom before pulling"
	case e.status == 429:
		return "too many requests; wait a minute before pulling again"
	case e.status == 403:
		return "project download was denied; check your sign-in and API access"
	default:
		return fmt.Sprintf("the project could not be downloaded (HTTP %d); try again later", e.status)
	}
}

func (c *accountClient) requestProject(ctx context.Context, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.APIURL+"/project", nil)
	if err != nil {
		return nil, errors.New("invalid project API address")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/zip")
	client := *c.http
	client.Timeout = 150 * time.Second
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("project download canceled")
		}
		return nil, errors.New("could not download the project; check your connection and try again")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Code string `json:"code"`
		}
		// Only recognized codes affect the message. Never echo server bodies,
		// redirect locations, or transport errors containing credentials.
		_ = json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&failure)
		return nil, &projectAPIError{status: response.StatusCode, code: failure.Code}
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/zip" && mediaType != "application/octet-stream") {
		return nil, errors.New("the project API did not return a ZIP archive")
	}
	if response.ContentLength > maxProjectArchiveBytes {
		return nil, errors.New("the project archive exceeds the download size limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxProjectArchiveBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("project download canceled")
		}
		return nil, errors.New("the project download was interrupted; try again")
	}
	if len(data) > maxProjectArchiveBytes {
		return nil, errors.New("the project archive exceeds the download size limit")
	}
	return data, nil
}

func (c *accountClient) pullProject(ctx context.Context, opts pullOptions) error {
	if ctx.Err() != nil {
		return errors.New("project download canceled")
	}
	credentials, err := c.store.Load()
	if err != nil {
		return err
	}
	if !credentials.valid() {
		return errors.New("you are not signed in; run glowbom login first")
	}
	output, err := prepareProjectOutput(opts.Output)
	if err != nil {
		return err
	}
	defer output.Close()
	archive, err := c.downloadSavedProject(ctx, credentials)
	if err != nil {
		return err
	}
	path, count, err := output.Extract(ctx, archive)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Saved project: %s\nDownloaded %d files.\n", path, count)
	return nil
}

func (c *accountClient) downloadSavedProject(ctx context.Context, credentials accountCredentials) ([]byte, error) {
	var err error
	refreshed := credentials.ExpiresAt <= time.Now().Add(time.Minute).Unix()
	if refreshed {
		credentials, err = c.refresh(ctx, credentials)
		if err != nil {
			return nil, err
		}
	}
	fmt.Fprintln(c.out, "Downloading saved project...")
	archive, err := c.requestProject(ctx, credentials.IDToken)
	var failure *projectAPIError
	if !refreshed && errors.As(err, &failure) && failure.status == http.StatusUnauthorized {
		credentials, err = c.refresh(ctx, credentials)
		if err == nil {
			archive, err = c.requestProject(ctx, credentials.IDToken)
		}
	}
	return archive, err
}

func runPullCommand(args []string) int {
	opts, err := parsePullOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(pullUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", err)
		fmt.Fprint(os.Stderr, pullUsage)
		return 2
	}
	c, err := newAccountClient()
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		err = c.pullProject(ctx, opts)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", err)
		return 1
	}
	return 0
}
