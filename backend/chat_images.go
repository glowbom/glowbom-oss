package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"image"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxChatImages = 4
const maxChatImageStorage = 32 << 20

var chatImageSlots = make(chan struct{}, 2)
var chatImageProjectsMu sync.Mutex
var chatImageProjects = map[string]*chatImageProjectLock{}

type chatImageProjectLock struct {
	token chan struct{}
	users int
}

func lockChatImageProject(ctx context.Context, root string) (func(), error) {
	chatImageProjectsMu.Lock()
	lock := chatImageProjects[root]
	if lock == nil {
		lock = &chatImageProjectLock{token: make(chan struct{}, 1)}
		chatImageProjects[root] = lock
	}
	lock.users++
	chatImageProjectsMu.Unlock()
	releaseUser := func() {
		chatImageProjectsMu.Lock()
		defer chatImageProjectsMu.Unlock()
		lock.users--
		if lock.users == 0 {
			delete(chatImageProjects, root)
		}
	}
	select {
	case lock.token <- struct{}{}:
		return func() { <-lock.token; releaseUser() }, nil
	case <-ctx.Done():
		releaseUser()
		return nil, ctx.Err()
	}
}

type chatImageOptions struct {
	SourceID        string `json:"sourceId"`
	APIKey          string `json:"apiKey,omitempty"`
	Personalization bool   `json:"personalization,omitempty"`
	ReferencePath   string `json:"referencePath,omitempty"`
	referencePNG    string
	previousImages  *prototypeImageReferenceSnapshot
}

func validateChatImageOptions(options chatImageOptions) error {
	if options.SourceID == "xai-subscription" {
		if err := requireGrokSubscriptionMedia(); err != nil {
			return err
		}
	}
	switch options.SourceID {
	case "picsum", "glowbom-api", "openai-api", "openai-subscription", "gemini-api", "xai-api", "xai-subscription":
	default:
		return errors.New("Choose a supported prototype image source.")
	}
	if len(options.APIKey) > 16384 {
		return errors.New("The image API key is too long.")
	}
	if len(options.ReferencePath) > 4096 {
		return errors.New("Choose an uploaded reference photo for personalization.")
	}
	if options.Personalization {
		if options.SourceID == "picsum" {
			return errors.New("Personalization needs an image generation source. Choose one instead of Lorem Picsum.")
		}
		if strings.TrimSpace(options.ReferencePath) == "" {
			return errors.New("Add a reference photo or sketch before enabling personalization.")
		}
	}
	return nil
}

func chatImageInstructions(options chatImageOptions) string {
	description := "Glowbom will generate images using the selected image source after saving the HTML."
	if options.SourceID == "picsum" {
		description = "Glowbom will add placeholder photos from Lorem Picsum after saving the HTML. These are sample photos, not exact matches to the description."
	}
	if options.Personalization {
		description += " Image personalization is enabled: the selected reference photo or sketch identifies the person, pet, or object to feature in newly generated images. When the request is about that subject, you MUST create at least one image placeholder describing the referenced subject in the requested setting and action, not just scenery. For example, for a travel app about the person visiting Brazil, use glowbomimages:The person from the reference photo exploring Rio de Janeiro, overlooking the bay at sunset, preserving their recognizable face. Write a complete visual scene for every personalized image and explicitly mention the person or subject from the reference photo in those prompts only. Plan a varied set within the four-image budget. By default, when multiple images suit the page, use one or two personalized scenes and use the remaining images for relevant places, food, objects, or background details without the reference subject. Personalization is permission to include the subject where relevant, not an instruction to include them in every picture. For a Brazil travel page, combine the referenced person exploring Rio with a Rio skyline, Brazilian food, or a rainforest view. Only feature the subject in every image if the user explicitly requests that. Do not force extra images when the page only needs one. Reusing the uploaded photo as an avatar or repeating it across the page does not fulfill a request for personalized scenes. Do not use the uploaded photo path or an older local image in place of a requested new personalized scene; create a new image placeholder there. Other images, such as scenery, food, or interface artwork, should describe only their actual content without adding the referenced person. Glowbom detects identity cues in each placeholder and attaches the reference only to personalized images."
	}
	return " " + description + " For each new image use a quoted URL placeholder such as src=\"glowbomimages:a quiet seaside harbor\" or url('glowbomimages:a quiet seaside harbor'). Use at most four distinct, short image descriptions. Reuse the same placeholder for the same image. Keep existing local asset URLs for layout, text, or color changes unless the user asks to replace an image. When replacing the same existing image, preserve its stable element ID, unique alt text, or stable parent ID so Glowbom can use the previous image as a visual reference. Do not transfer an existing image's ID or alt text to an unrelated new image. If the user asks to start fresh or use a different person or subject, mark that replacement img or inline background element with data-glowbom-reference=\"none\" to disable the previous-image reference. An explicitly selected personalization photo still takes precedence. Do not invent remote image URLs or embed image data."
}

// Match identity cues as words so scenery such as a mangrove or monument does
// not accidentally receive a private reference photo.
var chatImageIdentityPattern = regexp.MustCompile(`(?i)\b(same\s+(?:person|people|character|face)|this\s+(?:person|man|woman|guy|girl)|portrait|selfie|headshot|avatar|resembling|looks?\s+like|me|myself|personaliz(?:e|ed|ation|ing)|reference\s+(?:photo|image|subject)|referenced\s+(?:subject|pet|product|object)|same\s+(?:pet|product|object|look)|preserve\s+(?:the\s+)?identity)\b`)
var chatImageAbsentPeoplePattern = regexp.MustCompile(`(?i)\b(?:no|without)\s+(?:people|persons?|humans?|men|women|characters?|faces?)\b`)

func isPersonalizedChatImage(prompt string) bool {
	prompt = chatImageAbsentPeoplePattern.ReplaceAllString(prompt, "")
	return chatImageIdentityPattern.MatchString(prompt)
}

// Match complete quoted URLs so punctuation in descriptions stays intact and
// a JavaScript template string cannot consume the code after its closing quote.
var chatImagePattern = regexp.MustCompile(`"(?:glowby|glowbom)images?:(?:\\.|[^"\\\r\n])*"|'(?:glowby|glowbom)images?:(?:\\.|[^'\\\r\n])*'|` + "`(?:glowby|glowbom)images?:(?:\\\\.|[^`\\\\\\r\\n])*`")

type chatImagePlaceholder struct {
	prompt string
	tokens []string
}

func chatImagePlaceholders(document string) []chatImagePlaceholder {
	result := []chatImagePlaceholder{}
	indices := map[string]int{}
	for _, token := range chatImagePattern.FindAllString(document, -1) {
		_, payload, _ := strings.Cut(token[1:len(token)-1], ":")
		prompt := html.UnescapeString(strings.TrimSpace(payload))
		prompt = strings.NewReplacer(`\"`, `"`, `\'`, `'`, "\\`", "`", `\\`, `\`).Replace(prompt)
		if decoded, err := url.PathUnescape(prompt); err == nil {
			prompt = decoded
		}
		prompt = strings.Join(strings.Fields(prompt), " ")
		if prompt == "" {
			continue
		}
		if index, ok := indices[prompt]; ok {
			result[index].tokens = append(result[index].tokens, token)
			continue
		}
		indices[prompt] = len(result)
		result = append(result, chatImagePlaceholder{prompt: prompt, tokens: []string{token}})
	}
	return result
}

// Only the explicitly selected image source is contacted. Failed requests are never retried.
func generateChatImage(ctx context.Context, source, key, prompt, reference string) (string, string, error) {
	if reference != "" {
		prompt += " Use the supplied reference photo or sketch as the visual reference for the requested person or object. Keep the subject recognizable and preserve its distinctive appearance."
	}
	switch source {
	case "openai-subscription":
		value, err := callCodexImageGeneration(ctx, prompt, reference, "16:9")
		return value, codexImageSourceLabel, err
	case "glowbom-api":
		value, err := callGlowbomImageGeneration(ctx, prompt, reference)
		return value, glowbomImageSourceLabel, err
	case "openai-api":
		if reference != "" {
			value, err := callOpenAIImageGenerationWithReference(prompt, reference, "16:9", "png", key, ctx)
			return value, openAIImageSourceLabel, err
		}
		value, err := callOpenAIImageGeneration(prompt, "16:9", "png", key, ctx)
		return value, openAIImageSourceLabel, err
	case "gemini-api":
		if reference != "" {
			value, err := callGeminiImageGenerationWithReference(prompt, reference, "16:9", "png", key, ctx)
			return value, "Glowbom Images (Nano Banana 2)", err
		}
		value, err := callGeminiImageGeneration(prompt, "16:9", "png", key, ctx)
		return value, "Glowbom Images (Nano Banana 2)", err
	case "xai-api", "xai-subscription":
		if source == "xai-subscription" {
			credential, err := resolveProjectIconSubscription(ctx, false)
			if err != nil {
				return "", xAIImageSourceLabel, err
			}
			key = credential.Bearer
		}
		if reference != "" {
			value, err := callGrokImageGenerationWithReference(prompt, "data:image/png;base64,"+reference, key, "16:9", ctx)
			return value, xAIImageSourceLabel, err
		}
		value, err := callGrokImageGeneration(prompt, key, "16:9", ctx)
		return value, xAIImageSourceLabel, err
	case "picsum":
		if reference != "" {
			return "", "Lorem Picsum", errors.New("Lorem Picsum does not support personalization.")
		}
		hash := sha256.Sum256([]byte(prompt))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://picsum.photos/seed/%x/1024/768", hash[:12]), nil)
		if err != nil {
			return "", "Lorem Picsum", err
		}
		client := *http.DefaultClient
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 || req.URL.Scheme != "https" || (req.URL.Host != "picsum.photos" && req.URL.Host != "fastly.picsum.photos") || req.URL.User != nil {
				return errors.New("Unexpected photo redirect")
			}
			return nil
		}
		response, err := client.Do(req)
		if err != nil {
			return "", "Lorem Picsum", err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return "", "Lorem Picsum", errors.New("Photo service unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, projectIconMaxBytes+1))
		if err != nil || len(data) > projectIconMaxBytes {
			return "", "Lorem Picsum", errors.New("Photo is too large")
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), "Lorem Picsum", nil
	}
	return "", "", errors.New("Unsupported image source")
}

func chatImageFilename(source, prompt string, references ...string) string {
	identity := source + "\x00" + prompt
	if len(references) > 0 && references[0] != "" {
		digest := sha256.Sum256([]byte(references[0]))
		identity += fmt.Sprintf("\x00reference:%x", digest)
	}
	hash := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("glowbom-image-%x.png", hash[:16])
}

func readChatImage(root, filename string) ([]byte, error) {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	relative := filepath.Join("prototype", "assets", filename)
	info, err := handle.Lstat(relative)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > projectIconMaxBytes {
		return nil, errors.New("Image asset is not a regular bounded file")
	}
	file, err := handle.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, projectIconMaxBytes+1))
	if err != nil {
		return nil, err
	}
	return normalizeProjectIcon(data)
}

// Check the saved HTML before spending another request or replacing its assets.
func checkChatImageRevision(ctx context.Context, root, expected string) error {
	if ctx.Err() != nil {
		return errors.New("Prototype saved. Image generation stopped or timed out. Images already added were kept.")
	}
	current, err := readChatProjectFile(root, "prototype/index.html", maxChatResultBytes)
	if err != nil {
		return err
	}
	if current != expected {
		return errors.New("The prototype changed while adding images. The newer prototype was kept.")
	}
	return nil
}

func createChatImageFile(dir, filename string, data []byte) error {
	temp, err := os.CreateTemp(dir, ".glowbom-image-*.png")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0644); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	// Linking a complete file is atomic and fails if an image appeared meanwhile.
	if err := os.Link(temp.Name(), filepath.Join(dir, filename)); err != nil {
		return errors.New("An image could not be added safely. Any existing image was kept.")
	}
	return nil
}

func commitChatImage(ctx context.Context, root, record, expected, next, filename string, data []byte, generated bool) error {
	chatResultMu.Lock()
	defer chatResultMu.Unlock()
	if err := checkChatImageRevision(ctx, root, expected); err != nil {
		return err
	}
	assets, err := chatWriteDirectory(root, "prototype/assets")
	if err != nil {
		return err
	}
	current, err := readChatImage(root, filename)
	if err != nil {
		return err
	}
	if generated {
		if len(current) > 0 {
			return errors.New("An image changed while generating. The newer image was kept.")
		}
		if err := createChatImageFile(assets, filename, data); err != nil {
			return err
		}
	} else if !bytes.Equal(current, data) {
		return errors.New("An image changed while updating the prototype. The newer image was kept.")
	}
	if err := checkChatImageRevision(ctx, root, expected); err != nil {
		return err
	}
	dir, err := chatWriteDirectory(root, "prototype")
	if err != nil {
		return err
	}
	if err := atomicChatFile(dir, "index.html", []byte(next)); err != nil {
		return err
	}
	if record != "" {
		canonical, err := filepath.EvalSymlinks(record)
		if err != nil || !isPathWithin(root, canonical) {
			return errors.New("The prototype was saved, but its history folder moved.")
		}
		if err := atomicChatFile(canonical, "result.html", []byte(next)); err != nil {
			return errors.New("The prototype was saved, but its history could not be updated.")
		}
	}
	return nil
}

func previousChatImageReference(options chatImageOptions, document string, placeholder chatImagePlaceholder) (prototypeImageReferenceMatch, bool, error) {
	var previous prototypeImageReferenceMatch
	found := false
	for _, token := range placeholder.tokens {
		match, matched, err := matchPreviousPrototypeImageReference(options.previousImages, document, html.UnescapeString(token[1:len(token)-1]), options.SourceID)
		if err != nil {
			return match, matched, err
		}
		if !matched {
			// Aliases share one generated image only when every slot agrees.
			return prototypeImageReferenceMatch{}, false, nil
		}
		if found && (match.ReferenceImage != previous.ReferenceImage || match.AssetURL != previous.AssetURL) {
			// A shared prompt cannot choose between different previous subjects.
			return prototypeImageReferenceMatch{}, false, nil
		}
		previous, found = match, true
	}
	return previous, found, nil
}

func previousChatImageAsset(root, prompt string, previous prototypeImageReferenceMatch) (string, []byte, bool, error) {
	data, _, err := decodeBase64Payload(previous.ReferenceImage, "image/png")
	if err != nil {
		return "", nil, false, err
	}
	data, err = normalizeProjectIcon(data)
	if err != nil {
		return "", nil, false, err
	}
	if strings.HasPrefix(previous.AssetURL, "assets/") {
		filename := strings.TrimPrefix(previous.AssetURL, "assets/")
		current, readErr := readChatImage(root, filename)
		if readErr == nil && bytes.Equal(current, data) {
			return filename, data, false, nil
		}
	}
	// Preserve the captured pixels without overwriting a changed project image.
	filename := chatImageFilename("previous-project-image", prompt, previous.ReferenceImage)
	current, err := readChatImage(root, filename)
	if err != nil {
		return "", nil, false, err
	}
	if len(current) > 0 && !bytes.Equal(current, data) {
		return "", nil, false, errors.New("The previous image copy changed. The newer image was kept.")
	}
	return filename, data, len(current) == 0, nil
}

func materializeChatImages(parent context.Context, root, record, document string, options chatImageOptions, emit func(map[string]any)) (string, []string, error) {
	warnings := []string{}
	if err := validateChatImageOptions(options); err != nil {
		return document, warnings, err
	}
	reference := ""
	if options.Personalization {
		reference = options.referencePNG
		if reference == "" {
			return document, warnings, errors.New("The personalization reference is unavailable. Attach the photo or sketch again.")
		}
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return document, warnings, err
	}
	defer anchor.Close()
	checkRoot := func() error {
		opened, err := anchor.Stat(".")
		current, pathErr := os.Stat(root)
		if err != nil || pathErr != nil || !os.SameFile(opened, current) {
			return errors.New("The project folder moved while adding images. Reopen it to continue.")
		}
		return nil
	}
	warn := func(message string) { warnings = append(warnings, message); emit(map[string]any{"warning": message}) }
	placeholders := chatImagePlaceholders(document)
	personalized := []chatImagePlaceholder{}
	if options.Personalization {
		for _, placeholder := range placeholders {
			if len(placeholder.prompt) <= 2000 && isPersonalizedChatImage(placeholder.prompt) {
				personalized = append(personalized, placeholder)
			}
		}
		if len(personalized) == 0 {
			warn("The prototype was saved, but it did not include any personalized image prompts. Ask for a new scene featuring the person or subject in your reference photo.")
		}
	}
	if len(placeholders) == 0 {
		return document, warnings, nil
	}
	if len(placeholders) > maxChatImages {
		if len(personalized) > 0 {
			for _, placeholder := range placeholders {
				if len(placeholder.prompt) > 2000 || !isPersonalizedChatImage(placeholder.prompt) {
					personalized = append(personalized, placeholder)
				}
			}
			placeholders = personalized
			emit(map[string]any{"notice": "Creating up to four images, with personalized scenes first."})
		} else {
			emit(map[string]any{"notice": "Creating up to four images for this prototype."})
		}
		placeholders = placeholders[:maxChatImages]
	}
	timeout := 5 * time.Minute
	if options.SourceID == "openai-subscription" {
		timeout = codexImageTimeout * time.Duration(len(placeholders))
	}
	if options.SourceID == "glowbom-api" {
		if err := validateGlowbomImageReference(reference); err != nil {
			return document, warnings, err
		}
		timeout = glowbomImageTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	unlock, err := lockChatImageProject(ctx, root)
	if err != nil {
		return document, warnings, errors.New("Prototype saved. Image generation stopped before it could start.")
	}
	defer unlock()
	key := ""
	providerReady := true
	if options.SourceID != "picsum" {
		_, resolved, err := resolveProjectIconSource(projectIconRequest{SourceID: options.SourceID, APIKey: options.APIKey}, ctx)
		if err != nil {
			warn("The prototype was saved, but images need a working connection or API key for the selected source.")
			providerReady = false
		}
		key = resolved
	}
	storage := 0
	for index, placeholder := range placeholders {
		if err := checkRoot(); err != nil {
			return document, warnings, err
		}
		if err := checkChatImageRevision(ctx, root, document); err != nil {
			return document, warnings, err
		}
		if len(placeholder.prompt) > 2000 {
			warn(fmt.Sprintf("Image %d was skipped because its description is too long.", index+1))
			continue
		}
		previous, matchedPrevious, previousErr := previousChatImageReference(options, document, placeholder)
		imageReference := ""
		if isPersonalizedChatImage(placeholder.prompt) {
			imageReference = reference
		}
		if imageReference == "" && matchedPrevious {
			if previousErr != nil {
				warn(fmt.Sprintf("Image %d could not use its previous image as a reference. No new image was requested.", index+1))
				continue
			}
			imageReference, err = normalizeProjectIconReference(previous.ReferenceImage)
			if err != nil {
				warn(fmt.Sprintf("Image %d could not use its previous image as a reference. No new image was requested.", index+1))
				continue
			}
		}
		emit(map[string]any{
			"status": fmt.Sprintf("Creating image %d of %d", index+1, len(placeholders)),
			"imagePrompt": map[string]any{
				"index":        index + 1,
				"total":        len(placeholders),
				"prompt":       placeholder.prompt,
				"personalized": imageReference != "",
			},
		})
		filename := chatImageFilename(options.SourceID, placeholder.prompt, imageReference)
		data, err := readChatImage(root, filename)
		if err != nil {
			warn(fmt.Sprintf("Image %d could not be saved safely. Its placeholder was kept.", index+1))
			continue
		}
		generated := len(data) == 0
		newGeneration := generated
		source := openAIImageSourceLabel
		switch options.SourceID {
		case "openai-subscription":
			source = codexImageSourceLabel
		case "glowbom-api":
			source = glowbomImageSourceLabel
		case "gemini-api":
			source = "Glowbom Images (Nano Banana 2)"
		case "xai-api", "xai-subscription":
			source = xAIImageSourceLabel
		case "picsum":
			source = "Lorem Picsum"
		}
		if matchedPrevious && previousErr == nil && (options.SourceID == "picsum" || !providerReady) {
			filename, data, generated, err = previousChatImageAsset(root, placeholder.prompt, previous)
			if err != nil {
				warn(fmt.Sprintf("Image %d could not preserve its previous image. No new image was requested.", index+1))
				continue
			}
			source, newGeneration = "Previous project image", false
			if options.SourceID == "picsum" {
				warn(fmt.Sprintf("Image %d kept its previous image because sample photos cannot use a reference.", index+1))
			}
		} else if !providerReady && generated {
			continue
		}
		if generated && newGeneration {
			if storage+projectIconMaxBytes > maxChatImageStorage {
				warn("The prototype was saved. The image size limit was reached; remaining placeholders were kept.")
				break
			}
			// Check output folders before an image request that may incur a charge.
			if _, err := chatWriteDirectory(root, "prototype/assets"); err != nil {
				warn("The prototype was saved, but its image folder is unavailable.")
				break
			}
			imageTimeout := 2 * time.Minute
			if options.SourceID == "openai-subscription" {
				imageTimeout = codexImageTimeout
			}
			if options.SourceID == "glowbom-api" {
				imageTimeout = glowbomImageTimeout
			}
			imageCtx, stop := context.WithTimeout(ctx, imageTimeout)
			select {
			case chatImageSlots <- struct{}{}:
			case <-imageCtx.Done():
				stop()
				return document, warnings, errors.New("Prototype saved. Image generation stopped before it could start.")
			}
			if err := checkRoot(); err != nil {
				<-chatImageSlots
				stop()
				return document, warnings, err
			}
			if err := checkChatImageRevision(imageCtx, root, document); err != nil {
				<-chatImageSlots
				stop()
				return document, warnings, err
			}
			var value string
			value, source, err = generateChatImage(imageCtx, options.SourceID, key, placeholder.prompt, imageReference)
			<-chatImageSlots
			stop()
			if ctx.Err() != nil {
				return document, warnings, errors.New("Prototype saved. Image generation stopped or timed out. Images already added were kept.")
			}
			if err == nil {
				data, _, err = decodeBase64Payload(value, "image/png")
			}
			if err == nil {
				data, err = normalizeProjectIcon(data)
			}
			if err != nil {
				if matchedPrevious && previousErr == nil {
					filename, data, generated, err = previousChatImageAsset(root, placeholder.prompt, previous)
					if err == nil {
						source, newGeneration = "Previous project image", false
						warn(fmt.Sprintf("Image %d could not be regenerated. Its previous image was kept; no retry was made.", index+1))
					}
				}
				if err != nil {
					var failure *glowbomImageFailure
					var codexFailure *codexImageFailure
					if errors.As(err, &failure) {
						warn(fmt.Sprintf("Image %d: %s Its placeholder was kept.", index+1, failure.Error()))
					} else if errors.As(err, &codexFailure) {
						warn(fmt.Sprintf("Image %d: %s Its placeholder was kept.", index+1, codexFailure.Error()))
					} else {
						warn(fmt.Sprintf("Image %d could not be created with the selected source. Its placeholder was kept; no retry was made.", index+1))
					}
					continue
				}
			}
		}
		if storage+len(data) > maxChatImageStorage {
			warn("The prototype was saved. The image size limit was reached; remaining placeholders were kept.")
			break
		}
		storage += len(data)
		tokens := map[string]bool{}
		for _, token := range placeholder.tokens {
			tokens[token] = true
		}
		assetURL := (&url.URL{Path: "assets/" + filepath.ToSlash(filename)}).EscapedPath()
		next := chatImagePattern.ReplaceAllStringFunc(document, func(token string) string {
			if tokens[token] {
				return token[:1] + assetURL + token[len(token)-1:]
			}
			return token
		})
		if err := checkRoot(); err != nil {
			return document, warnings, err
		}
		if err := commitChatImage(ctx, root, record, document, next, filename, data, generated); err != nil {
			return document, warnings, err
		}
		document = next
		emit(map[string]any{"text": document})
		config, _, _ := image.DecodeConfig(bytes.NewReader(data))
		asset, err := linkStudioProjectImage(root, "prototype/assets/"+filename, data, studioSaveOptions{Prompt: placeholder.prompt, DataURI: "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), MediaType: "image", Source: source, NewGeneration: newGeneration, Dimensions: &studioDimensions{Width: config.Width, Height: config.Height}})
		if err != nil {
			warn(fmt.Sprintf("Image %d could not be added to Studio. Its project copy was saved.", index+1))
		} else {
			recordCompanionPrototypeImage(ctx, root, asset)
		}
	}
	return document, warnings, nil
}
