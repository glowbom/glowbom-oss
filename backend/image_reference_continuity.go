package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	htmlnode "golang.org/x/net/html"
)

const maxPreviousImageSnapshotBytes = 64 << 20

// The snapshot keeps prior pixels in memory. A later coding pass cannot change
// the reference by overwriting or deleting the project image.
type prototypeImageReferenceSnapshot struct {
	byKey        map[string][]*frozenPrototypeImage
	captureError error
}

type frozenPrototypeImage struct {
	data     []byte
	assetURL string
	err      error
}

type prototypeImageReferenceMatch struct {
	ReferenceImage string
	AssetURL       string
}

type prototypeImageSlot struct {
	source      string
	keyGroups   [][]string
	noReference bool
}

var inlineBackgroundImagePattern = regexp.MustCompile(`(?i)\bbackground(?:-image)?\s*:\s*([^;]+)`)
var inlineImageURLPattern = regexp.MustCompile(`(?i)url\(\s*(?:"([^"]+)"|'([^']+)'|([^\s)]+))\s*\)`)

// Only identifiable image slots are captured. Image order and prompt similarity
// never establish identity between two pictures.
func capturePrototypeImageReferences(root, previousHTML string) (*prototypeImageReferenceSnapshot, error) {
	snapshot := &prototypeImageReferenceSnapshot{byKey: make(map[string][]*frozenPrototypeImage)}
	if strings.TrimSpace(previousHTML) == "" {
		return snapshot, nil
	}
	slots := prototypeImageSlots(previousHTML)
	anchor, err := os.OpenRoot(root)
	if err != nil {
		snapshot.captureError = fmt.Errorf("The previous project images could not be opened safely.")
		return snapshot, snapshot.captureError
	}
	defer anchor.Close()
	bySource := make(map[string]*frozenPrototypeImage)
	totalBytes := 0
	for _, slot := range slots {
		if len(slot.keyGroups) == 0 || isPrototypeImagePlaceholder(slot.source) {
			continue
		}
		frozen := bySource[slot.source]
		if frozen == nil {
			frozen = capturePrototypeImage(anchor, slot.source, &totalBytes)
			bySource[slot.source] = frozen
		}
		for _, group := range slot.keyGroups {
			for _, key := range group {
				snapshot.byKey[key] = append(snapshot.byKey[key], frozen)
			}
		}
	}
	return snapshot, nil
}

func captureExistingPrototypeImageReferences(root string) (*prototypeImageReferenceSnapshot, error) {
	snapshot := &prototypeImageReferenceSnapshot{byKey: make(map[string][]*frozenPrototypeImage)}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		snapshot.captureError = fmt.Errorf("The previous project images could not be opened safely.")
		return snapshot, snapshot.captureError
	}
	defer anchor.Close()
	info, err := anchor.Lstat("prototype/index.html")
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxChatResultBytes {
		snapshot.captureError = fmt.Errorf("The previous prototype could not be captured safely.")
		return snapshot, snapshot.captureError
	}
	file, err := anchor.Open("prototype/index.html")
	if err != nil {
		snapshot.captureError = fmt.Errorf("The previous prototype could not be captured safely.")
		return snapshot, snapshot.captureError
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxChatResultBytes+1))
	if err != nil || len(data) > maxChatResultBytes {
		snapshot.captureError = fmt.Errorf("The previous prototype could not be captured safely.")
		return snapshot, snapshot.captureError
	}
	return capturePrototypeImageReferences(root, string(data))
}

func capturePrototypeImage(anchor *os.Root, source string, totalBytes *int) *frozenPrototypeImage {
	frozen := &frozenPrototypeImage{}
	parsed, err := url.Parse(strings.TrimSpace(source))
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" || parsed.User != nil {
		frozen.err = fmt.Errorf("The previous image must be a local project image. Add a reference photo or choose a fresh image.")
		return frozen
	}
	path := parsed.Path
	if strings.ContainsAny(path, "\\\x00") {
		frozen.err = fmt.Errorf("The previous image path is not a safe project asset.")
		return frozen
	}
	var relative string
	if strings.HasPrefix(path, "/prototype/assets/") {
		relative = strings.TrimPrefix(path, "/")
	} else if strings.HasPrefix(path, "/assets/") {
		relative = "prototype" + path
	} else if strings.HasPrefix(path, "/") {
		frozen.err = fmt.Errorf("The previous image must stay in the project asset folder.")
		return frozen
	} else {
		relative = filepath.ToSlash(filepath.Clean(filepath.Join("prototype", path)))
	}
	relative = filepath.ToSlash(filepath.Clean(relative))
	if !filepath.IsLocal(relative) || !strings.HasPrefix(relative, "prototype/assets/") {
		frozen.err = fmt.Errorf("The previous image must stay in the project asset folder.")
		return frozen
	}
	frozen.assetURL = strings.TrimPrefix(relative, "prototype/")
	info, err := anchor.Lstat(relative)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxGeneratedImageBytes {
		frozen.err = fmt.Errorf("The previous image could not be captured. Add a reference photo or choose a fresh image.")
		return frozen
	}
	if int64(*totalBytes)+info.Size() > maxPreviousImageSnapshotBytes {
		frozen.err = fmt.Errorf("The previous images are too large to keep safely. Add a smaller reference photo.")
		return frozen
	}
	file, err := anchor.Open(relative)
	if err != nil {
		frozen.err = fmt.Errorf("The previous image could not be captured safely.")
		return frozen
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxGeneratedImageBytes+1))
	if err != nil || len(data) > maxGeneratedImageBytes {
		frozen.err = fmt.Errorf("The previous image could not be captured safely.")
		return frozen
	}
	*totalBytes += len(data)
	frozen.data = data
	return frozen
}

func resolvePreviousPrototypeImageReference(snapshot *prototypeImageReferenceSnapshot, currentHTML, placeholderToken, sourceID string) (string, error) {
	match, _, err := matchPreviousPrototypeImageReference(snapshot, currentHTML, placeholderToken, sourceID)
	return match.ReferenceImage, err
}

// A caller can retain AssetURL on failure, or save ReferenceImage under a content
// hash when it needs the frozen picture rather than the mutable old file.
func matchPreviousPrototypeImageReference(snapshot *prototypeImageReferenceSnapshot, currentHTML, placeholderToken, sourceID string) (prototypeImageReferenceMatch, bool, error) {
	if snapshot == nil {
		return prototypeImageReferenceMatch{}, false, nil
	}
	token := html.UnescapeString(strings.TrimSpace(placeholderToken))
	slots := prototypeImageSlots(currentHTML)
	for _, slot := range slots {
		if slot.source == token && slot.noReference {
			return prototypeImageReferenceMatch{}, false, nil
		}
	}
	if snapshot.captureError != nil {
		return prototypeImageReferenceMatch{}, false, snapshot.captureError
	}
	var selected *frozenPrototypeImage
	foundSlot := false
	for _, slot := range slots {
		if slot.source != token {
			continue
		}
		foundSlot = true
		if slot.noReference {
			return prototypeImageReferenceMatch{}, false, nil
		}
		candidate := matchFrozenPrototypeImage(snapshot, slot)
		if candidate == nil {
			return prototypeImageReferenceMatch{}, false, nil
		}
		if selected != nil && selected != candidate {
			return prototypeImageReferenceMatch{}, false, nil
		}
		selected = candidate
	}
	if !foundSlot || selected == nil {
		return prototypeImageReferenceMatch{}, false, nil
	}
	match := prototypeImageReferenceMatch{AssetURL: selected.assetURL}
	if selected.err != nil {
		return match, true, selected.err
	}
	data, err := normalizePreviousPrototypeImage(selected.data, sourceID)
	if err != nil {
		return match, true, err
	}
	match.ReferenceImage = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	return match, true, nil
}

func matchFrozenPrototypeImage(snapshot *prototypeImageReferenceSnapshot, slot prototypeImageSlot) *frozenPrototypeImage {
	for _, group := range slot.keyGroups {
		var selected *frozenPrototypeImage
		for _, key := range group {
			matches := snapshot.byKey[key]
			if len(matches) > 1 {
				return nil
			}
			if len(matches) == 0 {
				continue
			}
			if selected != nil && selected != matches[0] {
				return nil
			}
			selected = matches[0]
		}
		if selected != nil {
			return selected
		}
	}
	return nil
}

func prototypeImageSlots(document string) []prototypeImageSlot {
	doc, err := htmlnode.Parse(strings.NewReader(document))
	if err != nil {
		return nil
	}
	ids, alts := map[string]int{}, map[string]int{}
	walkPrototypeNodes(doc, func(node *htmlnode.Node) {
		if node.Type != htmlnode.ElementNode {
			return
		}
		if id := prototypeNodeAttribute(node, "id"); id != "" {
			ids[id]++
		}
		if node.Data == "img" {
			if alt := prototypeNodeAttribute(node, "alt"); alt != "" {
				alts[alt]++
			}
		}
	})
	var slots []prototypeImageSlot
	walkPrototypeNodes(doc, func(node *htmlnode.Node) {
		if node.Type != htmlnode.ElementNode {
			return
		}
		noReference := prototypeNodeReferenceOptOut(node)
		if node.Data == "img" {
			source := prototypeNodeAttribute(node, "src")
			if source != "" {
				slot := prototypeImageSlot{source: source, noReference: noReference}
				if id := prototypeNodeAttribute(node, "id"); id != "" && ids[id] == 1 {
					slot.keyGroups = append(slot.keyGroups, []string{"image-id:" + id})
				}
				ancestor := node.Parent
				for ancestor != nil {
					id := prototypeNodeAttribute(ancestor, "id")
					if id != "" && ids[id] == 1 {
						var keys []string
						for _, attr := range []string{"data-image-id", "data-role", "name", "alt", "aria-label"} {
							if value := prototypeNodeAttribute(node, attr); value != "" {
								keys = append(keys, "ancestor:"+id+"|"+attr+":"+value)
							}
						}
						if len(keys) > 0 {
							slot.keyGroups = append(slot.keyGroups, keys)
						}
						break
					}
					ancestor = ancestor.Parent
				}
				if alt := prototypeNodeAttribute(node, "alt"); alt != "" && alts[alt] == 1 {
					slot.keyGroups = append(slot.keyGroups, []string{"image-alt:" + alt})
				}
				slots = append(slots, slot)
			}
		}
		id := prototypeNodeAttribute(node, "id")
		if id == "" || ids[id] != 1 {
			return
		}
		for _, declaration := range inlineBackgroundImagePattern.FindAllStringSubmatch(prototypeNodeAttribute(node, "style"), -1) {
			for _, value := range inlineImageURLPattern.FindAllStringSubmatch(declaration[1], -1) {
				source := ""
				for _, candidate := range value[1:] {
					if candidate != "" {
						source = candidate
						break
					}
				}
				if source != "" {
					slots = append(slots, prototypeImageSlot{source: source, keyGroups: [][]string{{"image-background:" + id}}, noReference: noReference})
				}
			}
		}
	})
	keyCounts := map[string]int{}
	for _, slot := range slots {
		for _, group := range slot.keyGroups {
			for _, key := range group {
				keyCounts[key]++
			}
		}
	}
	for index := range slots {
		var groups [][]string
		for _, group := range slots[index].keyGroups {
			var unique []string
			for _, key := range group {
				if keyCounts[key] == 1 {
					unique = append(unique, key)
				}
			}
			if len(unique) > 0 {
				groups = append(groups, unique)
			}
		}
		slots[index].keyGroups = groups
	}
	return slots
}

func prototypeNodeReferenceOptOut(node *htmlnode.Node) bool {
	for _, attr := range node.Attr {
		if attr.Key == "data-glowbom-reference" && strings.EqualFold(strings.TrimSpace(attr.Val), "none") {
			return true
		}
	}
	return false
}

func prototypeNodeAttribute(node *htmlnode.Node, name string) string {
	if node == nil || node.Type != htmlnode.ElementNode {
		return ""
	}
	value := ""
	found := false
	for _, attr := range node.Attr {
		if attr.Key != name {
			continue
		}
		if found {
			return ""
		}
		found = true
		value = strings.TrimSpace(attr.Val)
	}
	return value
}

func walkPrototypeNodes(node *htmlnode.Node, visit func(*htmlnode.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walkPrototypeNodes(child, visit)
	}
}

func isPrototypeImagePlaceholder(source string) bool {
	return strings.HasPrefix(source, "glowbomimage:") || strings.HasPrefix(source, "glowbomimages:") || strings.HasPrefix(source, "glowbyimage:") || strings.HasPrefix(source, "glowbyimages:")
}

func normalizePreviousPrototypeImage(data []byte, sourceID string) ([]byte, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > 16384 || config.Height > 16384 || int64(config.Width)*int64(config.Height) > 64_000_000 {
		return nil, fmt.Errorf("The previous image is not a supported photo. Add a PNG or JPEG reference, or choose a fresh image.")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("The previous image could not be opened. Add a reference photo or choose a fresh image.")
	}
	maxPixels := int64(4096 * 4096)
	maxEncoded := projectIconMaxBytes
	if sourceID == "glowbom-api" {
		maxPixels = 4_194_304
		maxEncoded = 5_200_000
	}
	factor := math.Min(1, math.Min(4096/float64(config.Width), 4096/float64(config.Height)))
	factor = math.Min(factor, math.Sqrt(float64(maxPixels)/float64(int64(config.Width)*int64(config.Height))))
	width, height := max(1, int(float64(config.Width)*factor)), max(1, int(float64(config.Height)*factor))
	for {
		output := decoded
		if width != config.Width || height != config.Height {
			output = resizePreviousPrototypeImage(decoded, width, height)
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, output); err != nil {
			return nil, fmt.Errorf("The previous image could not be prepared as a reference.")
		}
		if encoded.Len() <= maxEncoded {
			if sourceID == "glowbom-api" {
				if err := validateGlowbomImageReference(base64.StdEncoding.EncodeToString(encoded.Bytes())); err != nil {
					return nil, err
				}
			}
			return encoded.Bytes(), nil
		}
		if width <= 1 && height <= 1 {
			return nil, fmt.Errorf("The previous image is too large to use as a reference.")
		}
		width, height = max(1, width*3/4), max(1, height*3/4)
	}
}

// Bilinear sampling preserves the full frame while fitting provider limits.
func resizePreviousPrototypeImage(source image.Image, width, height int) image.Image {
	bounds := source.Bounds()
	output := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		sy := (float64(y)+.5)*float64(bounds.Dy())/float64(height) - .5
		y0 := max(0, int(math.Floor(sy)))
		y1 := min(bounds.Dy()-1, y0+1)
		fy := math.Max(0, sy-float64(y0))
		for x := 0; x < width; x++ {
			sx := (float64(x)+.5)*float64(bounds.Dx())/float64(width) - .5
			x0 := max(0, int(math.Floor(sx)))
			x1 := min(bounds.Dx()-1, x0+1)
			fx := math.Max(0, sx-float64(x0))
			pixels := []color.NRGBA{color.NRGBAModel.Convert(source.At(bounds.Min.X+x0, bounds.Min.Y+y0)).(color.NRGBA), color.NRGBAModel.Convert(source.At(bounds.Min.X+x1, bounds.Min.Y+y0)).(color.NRGBA), color.NRGBAModel.Convert(source.At(bounds.Min.X+x0, bounds.Min.Y+y1)).(color.NRGBA), color.NRGBAModel.Convert(source.At(bounds.Min.X+x1, bounds.Min.Y+y1)).(color.NRGBA)}
			interpolate := func(values [4]uint8) uint8 {
				top := float64(values[0])*(1-fx) + float64(values[1])*fx
				bottom := float64(values[2])*(1-fx) + float64(values[3])*fx
				return uint8(math.Round(top*(1-fy) + bottom*fy))
			}
			output.SetNRGBA(x, y, color.NRGBA{R: interpolate([4]uint8{pixels[0].R, pixels[1].R, pixels[2].R, pixels[3].R}), G: interpolate([4]uint8{pixels[0].G, pixels[1].G, pixels[2].G, pixels[3].G}), B: interpolate([4]uint8{pixels[0].B, pixels[1].B, pixels[2].B, pixels[3].B}), A: interpolate([4]uint8{pixels[0].A, pixels[1].A, pixels[2].A, pixels[3].A})})
		}
	}
	return output
}

func previousImageContinuityPrompt(prompt string) string {
	return prompt + " Use the supplied previous image as the visual reference for this asset. Preserve the person's recognizable identity and the subject's distinctive appearance unless the requested changes specify otherwise."
}

// Source changes may impose tighter image bounds. Resize a supplied default
// reference in place, while an explicitly empty selection stays empty.
func preparePreviousImageApprovalReferences(items []OpenCodeMediaApprovalItem) error {
	for index, item := range items {
		if item.Excluded || item.MediaType != "image" || item.ReferenceOrigin != "previous-image" || len(item.ReferenceImages) != 1 {
			continue
		}
		data, _, err := decodeBase64Payload(item.ReferenceImages[0], "image/png")
		if err != nil {
			return fmt.Errorf("The previous image reference could not be opened.")
		}
		normalized, err := normalizePreviousPrototypeImage(data, item.SourceID)
		if err != nil {
			return err
		}
		items[index].ReferenceImages = []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(normalized)}
	}
	return nil
}
