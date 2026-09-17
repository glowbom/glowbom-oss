package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"time"
)

const imageUsage = `Usage: glowbom generate-image [options] "Describe the image"

Options may appear before or after the prompt:
  --ref FILE_OR_HTTPS_URL  Add a reference image; repeat for multiple images
  --output PATH           Save to a new file or an existing directory
  --format png|jpg|webp   Requested format (default: png)
  --source flux|nano-banana
                         Image source (default: flux)
  --quality fast|high    Flux reference quality (default: fast)

Saves to the current directory by default, without overwriting existing files.
Flux fast accepts up to 5 references; Flux high and Nano Banana accept up to 8.
Generation uses your Glowbom account allowance. Sign in with glowbom login first.
`

type imageGenerationResult struct {
	Image string `json:"image"`
	Usage struct {
		Status string   `json:"status"`
		Cost   *float64 `json:"cost"`
	} `json:"usage"`
}

type imageGenerationError struct {
	status int
}

func (e *imageGenerationError) Error() string {
	switch e.status {
	case http.StatusUnauthorized:
		return "your sign-in was not accepted; run glowbom login again"
	case http.StatusForbidden:
		return "image generation was denied; check your account allowance with glowbom account"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "the image request was rejected; check the prompt, reference images, and selected options"
	case http.StatusRequestEntityTooLarge:
		return "the image request is too large; use fewer or smaller reference images"
	case http.StatusTooManyRequests:
		return "image generation was rate limited; wait before trying again"
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return "the image generation endpoint is unavailable"
	default:
		return "could not confirm image generation; allowance may have been used. Check glowbom account before trying again"
	}
}

// Generation may spend credits. Never replay a request after an uncertain result.
func (c *accountClient) requestImage(ctx context.Context, token string, body map[string]any) (imageGenerationResult, error) {
	var result imageGenerationResult
	data, err := json.Marshal(body)
	if err != nil {
		return result, errors.New("could not prepare the image request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.APIURL+"/generateImage", bytes.NewReader(data))
	if err != nil {
		return result, errors.New("invalid image API address")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := *c.http
	client.Timeout = 10 * time.Minute
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return result, &imageGenerationError{}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, &imageGenerationError{status: response.StatusCode}
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 || json.Unmarshal(data, &result) != nil || result.Image == "" {
		return result, &imageGenerationError{}
	}
	return result, nil
}

func (c *accountClient) generateImage(ctx context.Context, opts imageOptions) error {
	credentials, err := c.store.Load()
	if err != nil {
		return err
	}
	if !credentials.valid() {
		return errors.New("you are not signed in; run glowbom login first")
	}
	output, err := prepareImageOutput(opts.Output)
	if err != nil {
		return err
	}
	defer output.Close()
	body, err := prepareImageRequest(ctx, c.http, opts)
	if err != nil {
		return err
	}
	refreshed := credentials.ExpiresAt <= time.Now().Add(time.Minute).Unix()
	if refreshed {
		credentials, err = c.refresh(ctx, credentials)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.New("image generation canceled before sending the request")
	}
	fmt.Fprintln(c.out, "Generating image...")
	result, err := c.requestImage(ctx, credentials.IDToken, body)
	var failure *imageGenerationError
	// This API verifies authentication before reserving allowance or generating.
	if !refreshed && errors.As(err, &failure) && failure.status == http.StatusUnauthorized {
		credentials, err = c.refresh(ctx, credentials)
		if err == nil {
			result, err = c.requestImage(ctx, credentials.IDToken, body)
		}
	}
	if err != nil {
		return err
	}
	if cost := result.Usage.Cost; result.Usage.Status == "settled" && cost != nil && *cost >= 0 && !math.IsNaN(*cost) && !math.IsInf(*cost, 0) {
		fmt.Fprintf(c.out, "Generation cost: $%.4f\n", *cost)
	} else {
		fmt.Fprintln(c.out, "Generation cost is pending confirmation; check glowbom account for your remaining allowance.")
	}
	path, err := output.Save(ctx, c.http, result.Image)
	if err != nil {
		return fmt.Errorf("image generated, but saving failed: %w. Generation was not retried", err)
	}
	fmt.Fprintf(c.out, "Saved image: %s\n", path)
	return nil
}

func runImageCommand(args []string) int {
	opts, err := parseImageOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(imageUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", err)
		fmt.Fprint(os.Stderr, imageUsage)
		return 2
	}
	c, err := newAccountClient()
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		err = c.generateImage(ctx, opts)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Glowbom:", err)
		return 1
	}
	return 0
}
