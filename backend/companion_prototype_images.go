package main

import (
	"context"
	"strings"
)

type companionPrototypeImageContextKey struct{}

type companionPrototypeImage struct {
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Prompt        string `json:"prompt"`
	SourceService string `json:"sourceService"`
}

// The image pipeline calls this only after the project file and Studio link
// are saved. A model response cannot publish arbitrary image IDs as progress.
func recordCompanionPrototypeImage(ctx context.Context, root string, asset studioImageRecord) {
	job, _ := ctx.Value(companionPrototypeImageContextKey{}).(*companionDesktopPrototypeJob)
	if job == nil || ctx.Err() != nil || asset.MediaType != "image" {
		return
	}
	id := normalizedStudioUUID(asset.ID)
	if id == "" {
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.status != "running" || job.project == nil || job.projectPath != root || job.request.Images == nil {
		return
	}
	for _, existing := range job.generatedImages {
		if strings.EqualFold(existing.ID, id) {
			return
		}
	}
	if len(job.generatedImages) >= maxChatImages {
		return
	}
	job.generatedImages = append(job.generatedImages, companionPrototypeImage{
		ID: id, Timestamp: companionProjectDate(asset.Timestamp),
		Prompt:        companionPublicRunText(companionPrototypeDataURL.ReplaceAllString(asset.Prompt, "[embedded media]"), 2000),
		SourceService: companionPublicRunText(asset.SourceService, 200),
	})
}
