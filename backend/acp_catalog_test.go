package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestACPCatalogRemainsAvailableWhenNativeRuntimeTimesOut(t *testing.T) {
	acpSettingsFixture(t)
	t.Setenv("GLOWBOM_CURSOR_BIN", filepath.Join(t.TempDir(), "missing-cursor"))
	t.Setenv("GLOWBOM_CLAUDE_CODE_BIN", filepath.Join(t.TempDir(), "missing-claude"))
	// Isolate the cached native catalog without starting an installed agent.
	cursorModelCache.mu.Lock()
	previousModels, previousAt := cursorModelCache.models, cursorModelCache.at
	cursorModelCache.models, cursorModelCache.at = nil, time.Now()
	cursorModelCache.mu.Unlock()
	t.Cleanup(func() {
		cursorModelCache.mu.Lock()
		cursorModelCache.models, cursorModelCache.at = previousModels, previousAt
		cursorModelCache.mu.Unlock()
	})
	withCodexModels(t, nil)
	release := make(chan struct{})
	var pending sync.WaitGroup
	t.Cleanup(func() {
		close(release)
		pending.Wait()
	})
	codexLoadChatModels = func(context.Context) ([]chatModel, error) {
		defer pending.Done()
		<-release
		return nil, errors.New("native runtime unavailable")
	}
	service := &chatService{prepare: func() error { return errors.New("OpenCode unavailable") }}
	profile := acpProfile{ID: "acp-1", Name: "Available agent", Command: "do-not-launch", Args: []string{"acp"}}
	for _, test := range []struct {
		name       string
		configured bool
		cancel     bool
	}{
		{name: "saved connection survives deadline", configured: true},
		{name: "no saved connections keeps native deadline error"},
		{name: "explicit cancellation is respected", configured: true, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			profiles := []acpProfile{}
			if test.configured {
				profiles = append(profiles, profile)
			}
			if err := saveACPProfiles(profiles); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if test.cancel {
				cancel()
			}
			pending.Add(1)
			models, err := service.connectedModels(ctx)
			if test.cancel {
				if !errors.Is(err, context.Canceled) || len(models) != 0 {
					t.Fatalf("cancellation returned models=%+v err=%v", models, err)
				}
			} else if !test.configured {
				if !errors.Is(err, context.DeadlineExceeded) || len(models) != 0 {
					t.Fatalf("native behavior changed: models=%+v err=%v", models, err)
				}
			} else if err != nil || len(models) != 1 || models[0].ID != "acp/acp-1" || models[0].Name != profile.Name || !models[0].Build {
				t.Fatalf("native timeout hid a configured ACP connection: models=%+v err=%v", models, err)
			}
		})
	}
}
