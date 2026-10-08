package main

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

// Only forward known account messages, never raw provider output or credentials.
func companionStudioFailure(body []byte) string {
	message := strings.TrimSpace(string(body))
	for _, code := range []string{"sign_in_required", "allowance_required", "rate_limited", "invalid_image_request", "cli_update_required", "image_generation_failed"} {
		if message == glowbomImageError(&accountCommandFailure{code: code}).Error() {
			return message
		}
	}
	for _, safe := range []string{
		"Update or install the Glowbom CLI to use Glowbom images.",
		"Restart Glowbom with its secure local connection to use account images.",
		"Sign in from Account to use Glowbom images.",
		"Could not check your Glowbom account. Open Account and try again.",
		"Could not check your Glowbom account. Update the Glowbom CLI and try again.",
		"Your Glowbom account is busy. Wait for its current operation to finish.",
		"The Glowbom reference photo is too large. Use a smaller photo.",
		"Choose a valid PNG or JPEG reference photo.",
		"Glowbom reference photos must be PNG or JPEG images up to 4 megapixels. Use a smaller photo.",
		"Choose an aspect ratio supported by this image model.",
	} {
		if message == safe {
			return message
		}
	}
	return "Desktop could not finish this request. Check its connection and try again."
}

func validateCompanionStudioReference(id, inline string) error {
	if id != "" && (normalizedStudioUUID(id) == "" || inline != "") {
		return errors.New("Choose one Studio image or upload one reference.")
	}
	if inline == "" {
		return nil
	}
	if len(inline) > base64.StdEncoding.EncodedLen(companionAttachmentMaxBytes)+64 {
		return errors.New("Choose a reference smaller than 2 MB.")
	}
	var mime, payload string
	for _, candidate := range []string{"image/jpeg", "image/png"} {
		prefix := "data:" + candidate + ";base64,"
		if strings.HasPrefix(inline, prefix) {
			mime, payload = candidate, strings.TrimPrefix(inline, prefix)
			break
		}
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || mime == "" {
		return errors.New("Choose a JPEG or PNG reference.")
	}
	_, actualMIME, err := companionAttachmentImage(data)
	if err != nil {
		return err
	}
	if actualMIME != mime {
		return errors.New("This reference image could not be read.")
	}
	return nil
}

func (s *companionSession) startStudioVideo(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Prompt          string `json:"prompt"`
		SourceID        string `json:"sourceId"`
		ModelID         string `json:"modelId"`
		Resolution      string `json:"resolution"`
		AspectRatio     string `json:"aspectRatio"`
		DurationSeconds int    `json:"durationSeconds"`
		ReferenceID     string `json:"referenceId,omitempty"`
		ReferenceImage  string `json:"referenceImage,omitempty"`
	}
	if !companionDecode(w, r, &request, 3<<20) {
		return
	}
	if strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > 4000 || request.SourceID == "" {
		http.Error(w, "Choose a Desktop video source and enter a description up to 4,000 characters.", http.StatusBadRequest)
		return
	}
	if err := validateCompanionStudioReference(request.ReferenceID, request.ReferenceImage); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	options, err := normalizeStudioVideoOptions(studioVideoOptions{SourceID: request.SourceID, ModelID: request.ModelID, Resolution: request.Resolution, AspectRatio: request.AspectRatio, DurationSeconds: request.DurationSeconds})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Credentials stay on Desktop. Only the selected provider's saved key is used.
	payload := map[string]any{"prompt": request.Prompt, "sourceId": options.SourceID, "modelId": options.ModelID,
		"resolution": options.Resolution, "aspectRatio": options.AspectRatio, "durationSeconds": options.DurationSeconds,
		"referenceId": request.ReferenceID, "referenceImage": request.ReferenceImage, "useSavedKey": true}
	s.startJob(w, r, "video", companionProject{}, "/studio/videos/generate", payload)
}
