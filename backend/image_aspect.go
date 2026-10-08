package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

func imageGenerationSize(aspect string) string {
	switch strings.TrimSpace(aspect) {
	case "1:1":
		return "1024x1024"
	case "16:9":
		return "1536x864"
	case "9:16":
		return "864x1536"
	default:
		return "auto"
	}
}

func imageAspectPrompt(prompt, aspect string, hasReference bool) string {
	var canvas string
	switch strings.TrimSpace(aspect) {
	case "1:1":
		canvas = "square, 1:1 width-to-height aspect ratio; width must equal height"
	case "16:9":
		canvas = "landscape, 16:9 width-to-height aspect ratio; width must be greater than height"
	case "9:16":
		canvas = "portrait, 9:16 width-to-height aspect ratio; height must be greater than width"
	case "4:3", "3:2":
		canvas = "landscape, " + strings.TrimSpace(aspect) + " width-to-height aspect ratio; width must be greater than height"
	case "3:4", "2:3":
		canvas = "portrait, " + strings.TrimSpace(aspect) + " width-to-height aspect ratio; height must be greater than width"
	default:
		return prompt
	}
	instruction := "Final output canvas: " + canvas + "."
	if hasReference {
		instruction += " Expand or reframe the reference image to fill this output canvas even if the input image has a different aspect ratio. Preserve the subject without stretching or distortion."
	}
	return prompt + "\n\n" + instruction
}

type imageAspectFailure struct {
	Width  int
	Height int
	Aspect string
}

func (e *imageAspectFailure) Error() string {
	if e.Width < 1 || e.Height < 1 {
		return "The image source returned an image whose size could not be verified. Try again or choose another image source."
	}
	shape := map[string]string{"1:1": "square", "16:9": "landscape", "9:16": "portrait", "4:3": "landscape", "3:4": "portrait", "3:2": "landscape", "2:3": "portrait"}[e.Aspect]
	return fmt.Sprintf("The image source returned a %d×%d image instead of the selected %s (%s) shape. Try again or choose another image source.", e.Width, e.Height, shape, e.Aspect)
}

func validateGeneratedImageAspect(data []byte, aspect string) error {
	aspect = strings.TrimSpace(aspect)
	var ratioWidth, ratioHeight int64
	switch aspect {
	case "1:1":
		ratioWidth, ratioHeight = 1, 1
	case "16:9":
		ratioWidth, ratioHeight = 16, 9
	case "9:16":
		ratioWidth, ratioHeight = 9, 16
	case "4:3":
		ratioWidth, ratioHeight = 4, 3
	case "3:4":
		ratioWidth, ratioHeight = 3, 4
	case "3:2":
		ratioWidth, ratioHeight = 3, 2
	case "2:3":
		ratioWidth, ratioHeight = 2, 3
	default:
		return nil
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	width, height := config.Width, config.Height
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		width, height, err = bookWebPSize(data)
	}
	if err != nil || width < 1 || height < 1 {
		return &imageAspectFailure{Aspect: aspect}
	}
	expected := int64(height) * ratioWidth
	difference := int64(width)*ratioHeight - expected
	if difference < 0 {
		difference = -difference
	}
	if difference*100 > expected*2 {
		return &imageAspectFailure{Width: width, Height: height, Aspect: aspect}
	}
	return nil
}
