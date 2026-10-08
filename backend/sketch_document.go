package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"regexp"
	"strings"
)

const maxSketchDocumentBytes = 6 << 20
const maxSketchBackgroundCharacters = 4 << 20

type sketchDocument struct {
	Version     int               `json:"version"`
	Width       float64           `json:"width"`
	Height      float64           `json:"height"`
	Annotations []json.RawMessage `json:"annotations"`
	Background  string            `json:"background,omitempty"`
}

type sketchPoint struct {
	X *float64 `json:"x"`
	Y *float64 `json:"y"`
}

type sketchAnnotation struct {
	Kind     string        `json:"kind"`
	Color    string        `json:"color"`
	Width    *float64      `json:"width"`
	Points   []sketchPoint `json:"points"`
	Erase    bool          `json:"erase"`
	Shape    string        `json:"shape"`
	Start    *sketchPoint  `json:"start"`
	End      *sketchPoint  `json:"end"`
	Text     string        `json:"text"`
	X        *float64      `json:"x"`
	Y        *float64      `json:"y"`
	FontSize *float64      `json:"fontSize"`
}

var sketchColorPattern = regexp.MustCompile(`^#(?:[a-fA-F0-9]{3}|[a-fA-F0-9]{6}|[a-fA-F0-9]{8})$`)

func sketchCoordinate(value *float64, positive bool) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= 1 && (!positive || *value > 0)
}

func validSketchPoint(point *sketchPoint) bool {
	return point != nil && sketchCoordinate(point.X, false) && sketchCoordinate(point.Y, false)
}

func validateSketchDocument(document *sketchDocument) error {
	if document == nil {
		return nil
	}
	invalid := errors.New("The sketch contains invalid drawing data. Try drawing it again.")
	if document.Version != 1 || math.IsNaN(document.Width) || math.IsNaN(document.Height) || document.Width < 1 || document.Height < 1 || document.Width > 4096 || document.Height > 4096 || document.Width*document.Height > 8_000_000 || len(document.Annotations) > 2000 {
		return invalid
	}
	data, err := json.Marshal(document)
	if err != nil || len(data) > maxSketchDocumentBytes {
		return errors.New("The sketch is too large to save. Use fewer drawing marks or a smaller image.")
	}
	pointCount, textCount := 0, 0
	for _, raw := range document.Annotations {
		var annotation sketchAnnotation
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&annotation) != nil || !sketchColorPattern.MatchString(annotation.Color) || (annotation.Width != nil && !sketchCoordinate(annotation.Width, true)) {
			return invalid
		}
		switch annotation.Kind {
		case "stroke":
			pointCount += len(annotation.Points)
			if len(annotation.Points) == 0 || pointCount > 100000 {
				return invalid
			}
			for i := range annotation.Points {
				if !validSketchPoint(&annotation.Points[i]) {
					return invalid
				}
			}
		case "shape":
			if (annotation.Shape != "rectangle" && annotation.Shape != "arrow") || !validSketchPoint(annotation.Start) || !validSketchPoint(annotation.End) {
				return invalid
			}
		case "text":
			textCount += len(annotation.Text)
			if !sketchCoordinate(annotation.X, false) || !sketchCoordinate(annotation.Y, false) || !sketchCoordinate(annotation.Width, true) || !sketchCoordinate(annotation.FontSize, true) || len(annotation.Text) > 16000 || textCount > 100000 || strings.ContainsRune(annotation.Text, '\x00') {
				return invalid
			}
		default:
			return invalid
		}
	}
	if document.Background != "" {
		if len(document.Background) > maxSketchBackgroundCharacters {
			return invalid
		}
		pieces := strings.SplitN(document.Background, ",", 2)
		if len(pieces) != 2 || (pieces[0] != "data:image/png;base64" && pieces[0] != "data:image/jpeg;base64") {
			return invalid
		}
		bitmap, err := base64.StdEncoding.Strict().DecodeString(pieces[1])
		if err != nil || "data:"+http.DetectContentType(bitmap)+";base64" != pieces[0] {
			return invalid
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(bitmap))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 || int64(config.Width)*int64(config.Height) > 8_000_000 {
			return invalid
		}
	}
	return nil
}
