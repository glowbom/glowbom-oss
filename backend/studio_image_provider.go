package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

type studioImageOptions struct {
	SourceID    string `json:"sourceId"`
	ModelID     string `json:"modelId"`
	AspectRatio string `json:"aspectRatio"`
	Resolution  string `json:"resolution"`
	Quality     string `json:"quality"`
}

const studioGeneratedImageMaxBytes = 24 << 20
const studioImageProviderResponseMaxBytes = 40 << 20

// Generated 4K canvases can have a longer edge than a project icon.
func normalizeStudioGeneratedImage(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > studioGeneratedImageMaxBytes {
		return nil, errors.New("Use a generated image smaller than 24 MB.")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 6144 || config.Height > 6144 || int64(config.Width)*int64(config.Height) > 20_000_000 {
		return nil, errors.New("The provider returned unsupported image dimensions.")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("The provider returned incomplete image data.")
	}
	var output bytes.Buffer
	if png.Encode(&output, decoded) != nil || output.Len() > studioGeneratedImageMaxBytes {
		return nil, errors.New("The generated image could not be prepared within the 24 MB limit.")
	}
	return output.Bytes(), nil
}

type studioImageModelCapability struct {
	SourceID                       string                        `json:"sourceId"`
	ID                             string                        `json:"id"`
	Name                           string                        `json:"name"`
	AspectRatios                   []string                      `json:"aspectRatios"`
	DefaultAspectRatio             string                        `json:"defaultAspectRatio"`
	Resolutions                    []string                      `json:"resolutions"`
	DefaultResolution              string                        `json:"defaultResolution"`
	Qualities                      []string                      `json:"qualities"`
	DefaultQuality                 string                        `json:"defaultQuality"`
	PricesUSD                      map[string]float64            `json:"pricesUSD,omitempty"`
	PricesByQualityUSD             map[string]map[string]float64 `json:"pricesByQualityUSD,omitempty"`
	InputImageUSD                  float64                       `json:"inputImageUSD,omitempty"`
	TextInputUSDPerMillionTokens   float64                       `json:"textInputUSDPerMillionTokens,omitempty"`
	ImageInputUSDPerMillionTokens  float64                       `json:"imageInputUSDPerMillionTokens,omitempty"`
	ImageOutputUSDPerMillionTokens float64                       `json:"imageOutputUSDPerMillionTokens,omitempty"`
	PricingNotice                  string                        `json:"pricingNotice,omitempty"`
	PricingURL                     string                        `json:"pricingURL,omitempty"`
	PricingAsOf                    string                        `json:"pricingAsOf,omitempty"`
}

type studioImageSourceCapability struct {
	ID             string                       `json:"id"`
	Name           string                       `json:"name"`
	Provider       string                       `json:"provider"`
	Connected      bool                         `json:"connected"`
	RequiresAPIKey bool                         `json:"requiresApiKey"`
	Experimental   bool                         `json:"experimental,omitempty"`
	Notice         string                       `json:"notice,omitempty"`
	Models         []studioImageModelCapability `json:"models"`
}

type studioImageCatalogue struct {
	Sources []studioImageSourceCapability `json:"sources"`
}

func studioImageModels() []studioImageSourceCapability {
	ratios := []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"}
	google := func(id, name string, sizes []string, prices map[string]float64) studioImageModelCapability {
		return studioImageModelCapability{SourceID: "gemini-api", ID: id, Name: name, AspectRatios: ratios, DefaultAspectRatio: "1:1", Resolutions: sizes, DefaultResolution: "1K", Qualities: []string{}, PricesUSD: prices, PricingNotice: "Approximate output image cost. Input, text, and thinking tokens cost extra.", PricingURL: "https://ai.google.dev/gemini-api/docs/pricing", PricingAsOf: "2026-10-01"}
	}
	latest := studioImageModelCapability{SourceID: "xai-api", ID: "grok-imagine-image-2.0", Name: "Grok Imagine Image 2.0", AspectRatios: ratios, DefaultAspectRatio: "1:1", Resolutions: []string{"1k", "2k"}, DefaultResolution: "1k", Qualities: []string{"low", "medium"}, DefaultQuality: "low", PricesByQualityUSD: map[string]map[string]float64{"low": {"1k": .04, "2k": .06}, "medium": {"1k": .06, "2k": .08}}, InputImageUSD: .01, PricingNotice: "Approximate output cost. Each reference image adds its input charge.", PricingURL: "https://docs.x.ai/developers/models/grok-imagine-image-2.0", PricingAsOf: "2026-10-01"}
	older := studioImageModelCapability{SourceID: "xai-api", ID: "grok-imagine-image", Name: "Grok Imagine Image", AspectRatios: ratios, DefaultAspectRatio: "1:1", Resolutions: []string{"1k", "2k"}, DefaultResolution: "1k", Qualities: []string{}, PricesUSD: map[string]float64{"1k": .02, "2k": .02}, InputImageUSD: .002, PricingNotice: "Approximate output cost. Each reference image adds its input charge.", PricingURL: "https://docs.x.ai/developers/models/grok-imagine-image", PricingAsOf: "2026-10-01"}
	openai := func(id, name string, recent bool) studioImageModelCapability {
		m := studioImageModelCapability{SourceID: "openai-api", ID: id, Name: name, AspectRatios: ratios, DefaultAspectRatio: "1:1", Resolutions: []string{"1024x1024", "1536x1024", "1024x1536"}, DefaultResolution: "1024x1024", Qualities: []string{"low", "medium", "high"}, DefaultQuality: "low", PricingNotice: "Approximate output-only cost; prompt and reference inputs cost extra. The selected output size is billed before any local crop to the requested shape.", PricingURL: "https://developers.openai.com/api/docs/guides/image-generation#cost-and-latency", PricingAsOf: "2026-10-01"}
		if recent {
			m.AspectRatios = ratios
		}
		if strings.HasPrefix(id, "gpt-image-2.5-") {
			m.Qualities = []string{"low", "medium", "high", "xhigh", "max"}
			m.TextInputUSDPerMillionTokens = 5
			m.ImageInputUSDPerMillionTokens = 8
			m.ImageOutputUSDPerMillionTokens = 30
			m.PricesByQualityUSD = studioOpenAIImagePrices(id)
		}
		if m.PricesByQualityUSD == nil {
			m.PricesByQualityUSD = studioOpenAIImagePrices(id)
		}
		return m
	}
	sub := openai(openAIImageModelID, "GPT Image 2", true)
	sub.PricesByQualityUSD = nil
	sub.SourceID = "openai-subscription"
	sub.AspectRatios = []string{"1:1", "16:9", "9:16"}
	sub.Resolutions = []string{}
	sub.Qualities = []string{}
	sub.DefaultResolution = ""
	sub.DefaultQuality = ""
	sub.PricingURL = ""
	sub.PricingAsOf = ""
	sub.PricingNotice = "Uses your ChatGPT connection. Account allowance or credits may apply."
	flux := studioImageModelCapability{SourceID: "glowbom-api", ID: "flux", Name: "FLUX", AspectRatios: []string{}, Resolutions: []string{}, Qualities: []string{}}
	sources := []studioImageSourceCapability{
		{ID: "openai-subscription", Name: "ChatGPT", Provider: "OpenAI", Notice: "Your ChatGPT connection supports one image model. Use an OpenAI API key for other models.", Models: []studioImageModelCapability{sub}},
		{ID: "glowbom-api", Name: "Glowbom account", Provider: "Glowbom", Notice: "Your Glowbom account generates images with FLUX. Other models require a provider API key.", Models: []studioImageModelCapability{flux}},
		{ID: "openai-api", Name: "OpenAI API", Provider: "OpenAI", RequiresAPIKey: true, Models: []studioImageModelCapability{openai("gpt-image-2.5-flare", "GPT Image 2.5 Flare", true), openai("gpt-image-2.5-sunburst", "GPT Image 2.5 Sunburst", true), openai("gpt-image-2", "GPT Image 2", true), openai("gpt-image-1.5", "GPT Image 1.5", false), openai("gpt-image-1-mini", "GPT Image 1 Mini", false), openai("gpt-image-1", "GPT Image 1", false)}},
		{ID: "gemini-api", Name: "Google API", Provider: "Google", RequiresAPIKey: true, Models: []studioImageModelCapability{google("gemini-3.1-flash-lite-image", "Gemini 3.1 Flash Lite Image", []string{"1K"}, map[string]float64{"1K": .0336}), google("gemini-3.1-flash-image", "Gemini 3.1 Flash Image", []string{"512", "1K", "2K", "4K"}, map[string]float64{"512": .045, "1K": .067, "2K": .101, "4K": .151}), google("gemini-3-pro-image", "Gemini 3 Pro Image", []string{"1K", "2K", "4K"}, map[string]float64{"1K": .134, "2K": .134, "4K": .24})}},
		{ID: "xai-api", Name: "SpaceXAI API", Provider: "SpaceXAI", RequiresAPIKey: true, Models: []studioImageModelCapability{latest, older}},
	}
	if grokSubscriptionMediaEnabled() {
		latest.SourceID = "xai-subscription"
		older.SourceID = "xai-subscription"
		for _, m := range []*studioImageModelCapability{&latest, &older} {
			m.PricesUSD = nil
			m.PricesByQualityUSD = nil
			m.InputImageUSD = 0
			m.PricingURL = ""
			m.PricingAsOf = ""
			m.PricingNotice = "Media access and limits depend on your Grok account. Account allowance or purchased credits may apply."
		}
		sources = append(sources, studioImageSourceCapability{ID: "xai-subscription", Name: "Grok subscription", Provider: "SpaceXAI", Notice: "Media access and limits depend on your Grok account. Account allowance or purchased credits may apply.", Models: []studioImageModelCapability{latest, older}})
	}
	return sources
}

func studioImageCapabilities(ctx context.Context) studioImageCatalogue {
	sources := studioImageModels()
	for i := range sources {
		if sources[i].RequiresAPIKey {
			sources[i].Connected = studioVideoEnvironmentKey(studioProviderKeyStoreSource(sources[i].ID)) != ""
		}
		if sources[i].ID == "openai-subscription" {
			sources[i].Connected, _ = codexImageSourceAvailability(ctx)
		}
		if sources[i].ID == "xai-subscription" {
			_, sources[i].Connected = projectIconSubscription()
		}
	}
	return studioImageCatalogue{Sources: sources}
}

func studioImageCapabilitiesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	catalogue := studioImageCapabilities(r.Context())
	if bridge := glowbomImageAccount; bridge != nil && bridge.token != "" && hasValidGlowbomServerToken(r, bridge.token) && isAllowedOrigin(r, glowbomAllowedOrigins()) {
		for i := range catalogue.Sources {
			if catalogue.Sources[i].ID == "glowbom-api" {
				catalogue.Sources[i].Connected, _ = glowbomImagesAvailability(r.Context())
			}
		}
	}
	writeJSON(w, catalogue)
}

func normalizeStudioImageOptions(o studioImageOptions) (studioImageOptions, error) {
	o.SourceID = strings.TrimSpace(o.SourceID)
	o.ModelID = strings.TrimSpace(o.ModelID)
	o.AspectRatio = strings.TrimSpace(o.AspectRatio)
	o.Resolution = strings.TrimSpace(o.Resolution)
	o.Quality = strings.ToLower(strings.TrimSpace(o.Quality))
	if o.SourceID == "" {
		o.SourceID = "xai-api"
	}
	if o.SourceID == "xai-subscription" && !grokSubscriptionMediaEnabled() {
		return o, errGrokSubscriptionMediaDisabled
	}
	for _, s := range studioImageModels() {
		if s.ID != o.SourceID {
			continue
		}
		if o.ModelID == "" {
			o.ModelID = s.Models[0].ID
		}
		for _, m := range s.Models {
			if m.ID != o.ModelID {
				continue
			}
			if o.AspectRatio == "" {
				o.AspectRatio = m.DefaultAspectRatio
			}
			if !studioImageContains(m.AspectRatios, o.AspectRatio) && (o.AspectRatio != "" || len(m.AspectRatios) != 0) {
				return o, errors.New("Choose an aspect ratio supported by this image model.")
			}
			if o.Resolution == "" {
				o.Resolution = m.DefaultResolution
				if o.SourceID == "openai-api" {
					switch o.AspectRatio {
					case "16:9", "4:3":
						o.Resolution = "1536x1024"
					case "9:16", "3:4":
						o.Resolution = "1024x1536"
					case "3:2":
						o.Resolution = "1536x1024"
					case "2:3":
						o.Resolution = "1024x1536"
					}
				}
			}
			if o.SourceID == "gemini-api" {
				o.Resolution = strings.ToUpper(o.Resolution)
			}
			if o.SourceID == "xai-api" || o.SourceID == "xai-subscription" {
				o.Resolution = strings.ToLower(o.Resolution)
			}
			if o.Resolution != "" && !studioImageContains(m.Resolutions, o.Resolution) {
				return o, errors.New("Choose a resolution supported by this image model.")
			}
			if o.Quality == "" {
				o.Quality = m.DefaultQuality
			}
			if o.Quality != "" && !studioImageContains(m.Qualities, o.Quality) {
				return o, errors.New("Choose a quality supported by this image model.")
			}

			return o, nil
		}
		return o, errors.New("Choose a supported model for this image source.")
	}
	return o, errors.New("Choose a supported image source.")
}

func studioImageContains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func generateStudioSelectedImageWithOptions(ctx context.Context, o studioImageOptions, key, prompt, reference string) (string, string, error) {
	o, err := normalizeStudioImageOptions(o)
	if err != nil {
		return "", "", err
	}
	if err = ctx.Err(); err != nil {
		return "", "", err
	}
	timeout := 3 * time.Minute
	if o.SourceID == "openai-subscription" {
		timeout = codexImageTimeout
	}
	if o.SourceID == "glowbom-api" {
		timeout = glowbomImageTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	label := fmt.Sprintf("Glowbom Images (%s)", o.ModelID)
	switch o.SourceID {
	case "openai-subscription":
		value, e := callCodexImageGeneration(ctx, prompt, reference, o.AspectRatio)
		return value, codexImageSourceLabel, e
	case "glowbom-api":
		value, e := callGlowbomImageGeneration(ctx, prompt, reference)
		return value, glowbomImageSourceLabel, e
	case "xai-subscription":
		credential, e := resolveProjectIconSubscription(ctx, false)
		if e != nil {
			return "", label, e
		}
		key = credential.Bearer
	}
	if strings.TrimSpace(key) == "" || len(key) > 16384 {
		return "", label, errors.New("Add a valid provider API key.")
	}
	switch o.SourceID {
	case "xai-api", "xai-subscription":
		body := map[string]any{"model": o.ModelID, "prompt": imageAspectPrompt(prompt, o.AspectRatio, reference != ""), "n": 1, "resolution": o.Resolution, "aspect_ratio": o.AspectRatio}
		if o.Quality != "" {
			body["quality"] = o.Quality
		}
		endpoint := xAIImageGenerationURL
		if reference != "" {
			endpoint = xAIImageEditURL
			body["images"] = []map[string]any{{"type": "image_url", "url": ensureImageDataURI(reference, "image/png")}}
		}
		data, e := callStudioImageJSON(ctx, endpoint, body, "Bearer "+key, "Authorization")
		if e != nil {
			if ctx.Err() != nil {
				return "", label, ctx.Err()
			}
			return "", label, e
		}
		value, e := parseXAIImageGenerationResponse(data, key, ctx)
		if e != nil {
			return "", label, errors.New("The image provider could not finish this request. No other model was tried.")
		}
		return value, label, nil
	case "gemini-api":
		value, e := callStudioGeminiImage(ctx, o, key, prompt, reference)
		return value, label, e
	case "openai-api":
		value, e := callStudioOpenAIImage(ctx, o, key, prompt, reference)
		return value, label, e
	}
	return "", label, errors.New("Choose a supported image source.")
}

func callStudioImageJSON(ctx context.Context, endpoint string, body any, key, header string) ([]byte, error) {
	encoded, e := json.Marshal(body)
	if e != nil {
		return nil, e
	}
	r, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if e != nil {
		return nil, e
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(header, key)
	return callStudioImageRequest(r)
}

func callStudioImageRequest(r *http.Request) ([]byte, error) {
	client := *http.DefaultClient
	// A redirect must not replay a paid POST or forward provider credentials.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("The image provider redirected this generation request.")
	}
	resp, e := client.Do(r)
	if e != nil {
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		return nil, errors.New("Could not reach the image provider.")
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, studioImageProviderResponseMaxBytes+1))
	if e != nil {
		return nil, errors.New("Could not read the image provider response.")
	}
	if len(data) > studioImageProviderResponseMaxBytes {
		return nil, errors.New("The image provider returned a response larger than 40 MB.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Image provider rejected this request (status %d). No other model was tried.", resp.StatusCode)
	}
	return data, nil
}

func callStudioGeminiImage(ctx context.Context, o studioImageOptions, key, prompt, reference string) (string, error) {
	parts := []map[string]any{{"text": prompt}}
	if reference != "" {
		raw, mime, e := decodeBase64Payload(reference, "image/png")
		if e != nil {
			return "", errors.New("Choose a valid reference image.")
		}
		if detected := http.DetectContentType(raw); strings.HasPrefix(detected, "image/") {
			mime = detected
		}
		parts = append(parts, map[string]any{"inline_data": map[string]string{"mime_type": mime, "data": base64.StdEncoding.EncodeToString(raw)}})
	}
	body := map[string]any{"contents": []map[string]any{{"parts": parts}}, "generationConfig": map[string]any{"responseModalities": []string{"IMAGE"}, "imageConfig": map[string]string{"aspectRatio": o.AspectRatio, "imageSize": o.Resolution}}}
	data, e := callStudioImageJSON(ctx, "https://generativelanguage.googleapis.com/v1beta/models/"+o.ModelID+":generateContent", body, key, "x-goog-api-key")
	if e != nil {
		return "", e
	}
	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(data, &result) != nil {
		return "", errors.New("The image provider returned an unreadable response.")
	}
	for _, c := range result.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData.Data != "" {
				mime := p.InlineData.MimeType
				if !strings.HasPrefix(mime, "image/") {
					return "", errors.New("The provider returned unsupported image data.")
				}
				return "data:" + mime + ";base64," + p.InlineData.Data, nil
			}
		}
	}
	return "", errors.New("The provider did not return an image. No other model was tried.")
}

func callStudioOpenAIImage(ctx context.Context, o studioImageOptions, key, prompt, reference string) (string, error) {
	var data []byte
	var e error
	if reference == "" {
		data, e = callStudioImageJSON(ctx, "https://api.openai.com/v1/images/generations", map[string]any{"model": o.ModelID, "prompt": imageAspectPrompt(prompt, o.AspectRatio, false), "size": o.Resolution, "quality": o.Quality, "n": 1, "output_format": "png"}, "Bearer "+key, "Authorization")
	} else {
		raw, mime, decodeErr := decodeBase64Payload(reference, "image/png")
		if decodeErr != nil {
			return "", errors.New("Choose a valid reference image.")
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="image[]"; filename="reference.png"`)
		h.Set("Content-Type", mime)
		part, createErr := writer.CreatePart(h)
		if createErr != nil {
			return "", createErr
		}
		if _, e = part.Write(raw); e != nil {
			return "", e
		}
		for name, value := range map[string]string{"model": o.ModelID, "prompt": imageAspectPrompt(prompt, o.AspectRatio, true), "size": o.Resolution, "quality": o.Quality, "n": "1", "output_format": "png"} {
			if e = writer.WriteField(name, value); e != nil {
				return "", e
			}
		}
		if e = writer.Close(); e != nil {
			return "", e
		}
		r, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/images/edits", &body)
		if requestErr != nil {
			return "", requestErr
		}
		r.Header.Set("Content-Type", writer.FormDataContentType())
		r.Header.Set("Authorization", "Bearer "+key)
		data, e = callStudioImageRequest(r)
	}
	if e != nil {
		return "", e
	}
	var result struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Data) == 0 || result.Data[0].B64 == "" {
		return "", errors.New("The provider did not return an image. No other model was tried.")
	}
	return cropStudioOpenAIImage(result.Data[0].B64, o.AspectRatio)
}

func studioOpenAIImagePrices(id string) map[string]map[string]float64 {
	table := map[string][]float64{}
	switch id {
	case "gpt-image-2.5-flare", "gpt-image-2.5-sunburst":
		table = map[string][]float64{"low": {.00588, .00474}, "medium": {.01317, .01029}, "high": {.05268, .04116}, "xhigh": {.09366, .07377}, "max": {.21072, .16464}}
	case "gpt-image-2":
		table = map[string][]float64{"low": {.006, .005}, "medium": {.053, .041}, "high": {.211, .165}}
	case "gpt-image-1.5":
		table = map[string][]float64{"low": {.009, .013}, "medium": {.034, .05}, "high": {.133, .2}}
	case "gpt-image-1-mini":
		table = map[string][]float64{"low": {.005, .006}, "medium": {.011, .015}, "high": {.036, .052}}
	case "gpt-image-1":
		table = map[string][]float64{"low": {.011, .016}, "medium": {.042, .063}, "high": {.167, .25}}
	}
	if len(table) == 0 {
		return nil
	}
	result := map[string]map[string]float64{}
	for quality, prices := range table {
		result[quality] = map[string]float64{"1024x1024": prices[0], "1536x1024": prices[1], "1024x1536": prices[1]}
	}
	return result
}

// A selected API size can be cropped locally to the requested shape without another generation.
func cropStudioOpenAIImage(encoded, aspect string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) > projectIconMaxBytes {
		return "", errors.New("The image provider returned unsupported image data.")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return "", errors.New("The image provider returned unsupported image dimensions.")
	}
	first, last, found := strings.Cut(aspect, ":")
	if !found {
		return "data:image/png;base64," + encoded, nil
	}
	rw, e1 := strconv.Atoi(first)
	rh, e2 := strconv.Atoi(last)
	if e1 != nil || e2 != nil || rw < 1 || rh < 1 {
		return "", errors.New("Choose a supported image shape.")
	}
	if config.Width*rh == config.Height*rw {
		return "data:image/png;base64," + encoded, nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", errors.New("The image provider returned incomplete image data.")
	}
	w, h := config.Width, config.Height
	if w*rh > h*rw {
		w = h * rw / rh
	} else {
		h = w * rh / rw
	}
	if w < 1 || h < 1 {
		return "", errors.New("The image is too small for the selected shape.")
	}
	result := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(result, result.Bounds(), decoded, decoded.Bounds().Min.Add(image.Pt((config.Width-w)/2, (config.Height-h)/2)), draw.Src)
	var output bytes.Buffer
	if png.Encode(&output, result) != nil {
		return "", errors.New("Could not prepare the selected image shape.")
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(output.Bytes()), nil
}
