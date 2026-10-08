package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCompanionDiscoveryCommandsPublishOnlyPublicMetadata(t *testing.T) {
	advertisement := companionDiscoveryAdvertisement{ID: strings.Repeat("1", 32), Name: "Fixture Desktop", Host: "192.168.1.2:4443", CertificateSHA256: strings.Repeat("2", 64)}
	for _, platform := range []string{"darwin", "linux"} {
		commands, err := companionDiscoveryCommands(platform, advertisement)
		if err != nil {
			t.Fatal(err)
		}
		joined := ""
		for _, command := range commands {
			joined += strings.Join(command, " ") + "\n"
		}
		for _, expected := range []string{companionDiscoveryType, "id=" + advertisement.ID, "sha256=" + advertisement.CertificateSHA256, "path=" + companionPrefix, advertisement.Host[:11]} {
			if !strings.Contains(joined, expected) {
				t.Fatal("missing public metadata", expected)
			}
		}
		for _, forbidden := range []string{"token=", "challenge=", "Authorization", "/Projects/"} {
			if strings.Contains(joined, forbidden) {
				t.Fatal("private discovery metadata")
			}
		}
		if platform == "darwin" && commands[0][1] != "-P" {
			t.Fatal("selected address not explicitly advertised")
		}
	}
	if _, err := companionDiscoveryCommands("windows", advertisement); err == nil {
		t.Fatal("unsupported registrar promised discovery")
	}
	for _, host := range []string{"8.8.8.8:4443", "127.0.0.1:4443", "192.168.1.2:80", "bad"} {
		advertisement.Host = host
		if _, err := companionDiscoveryCommands("darwin", advertisement); err == nil {
			t.Fatal("unsafe endpoint advertised", host)
		}
	}
}

func TestCompanionDiscoveryFixtureCLIStopsWithRegistration(t *testing.T) {
	// This fixture never invokes Bonjour or opens a listener.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registration, err := startCompanionDiscoveryCommands(ctx, [][]string{{"/bin/sh", "-c", "exec sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	registration.Close()
	registration.Close()
	select {
	case <-registration.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery fixture process survived close")
	}
	if _, err := startCompanionDiscoveryCommands(ctx, [][]string{{"/not/a/bonjour/fixture"}}); err == nil {
		t.Fatal("missing discovery CLI accepted")
	}
	t.Setenv("GLOWBOM_SERVER_TOKEN", "fixture-private-desktop-token")
	clean, err := startCompanionDiscoveryCommands(ctx, [][]string{{"/bin/sh", "-c", "test -z \"$GLOWBOM_SERVER_TOKEN\""}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-clean.Done:
		if err != nil {
			t.Fatal("discovery process inherited Desktop credentials", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discovery environment fixture did not finish")
	}
}
