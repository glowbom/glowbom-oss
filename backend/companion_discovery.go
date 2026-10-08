package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const companionDiscoveryType = "_glowbom._tcp"

type companionDiscoveryAdvertisement struct {
	ID, Name, Host, CertificateSHA256 string
}

type companionDiscoveryRegistration struct {
	Close func()
	Done  <-chan error
}

type companionDiscoveryRegister func(context.Context, companionDiscoveryAdvertisement) (*companionDiscoveryRegistration, error)

// Only public connection metadata crosses Bonjour. The selected IPv4 address
// is published explicitly so another interface cannot point at a closed port.
func companionDiscoveryCommands(platform string, advertisement companionDiscoveryAdvertisement) ([][]string, error) {
	address, port, err := net.SplitHostPort(advertisement.Host)
	if err != nil || !companionPrivateIPv4(net.ParseIP(address)) || !companionHex(advertisement.ID, 32) || !companionHex(advertisement.CertificateSHA256, 64) {
		return nil, errors.New("Invalid discovery metadata.")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1024 || portNumber > 65535 {
		return nil, errors.New("Invalid discovery port.")
	}
	name := companionPairingDeviceName(advertisement.Name)
	if name == "" {
		name = "Glowbom Desktop"
	}
	instance := "Glowbom " + advertisement.ID[:8]
	hostname := "glowbom-" + advertisement.ID + ".local"
	txt := []string{"v=1", "id=" + advertisement.ID, "name=" + name, "sha256=" + advertisement.CertificateSHA256, "path=" + companionPrefix}
	switch platform {
	case "darwin":
		return [][]string{append([]string{"/usr/bin/dns-sd", "-P", instance, companionDiscoveryType, "local", port, hostname, address}, txt...)}, nil
	case "linux":
		return [][]string{
			{"avahi-publish-address", "-a", hostname, address},
			append([]string{"avahi-publish-service", "-s", "--host=" + hostname, instance, companionDiscoveryType, port}, txt...),
		}, nil
	default:
		return nil, errors.New("Nearby discovery is unavailable on this computer.")
	}
}

func registerCompanionDiscovery(parent context.Context, advertisement companionDiscoveryAdvertisement) (*companionDiscoveryRegistration, error) {
	commands, err := companionDiscoveryCommands(runtime.GOOS, advertisement)
	if err != nil {
		return nil, err
	}
	return startCompanionDiscoveryCommands(parent, commands)
}

func startCompanionDiscoveryCommands(parent context.Context, commands [][]string) (*companionDiscoveryRegistration, error) {
	if len(commands) == 0 {
		return nil, errors.New("No discovery command is available.")
	}
	ctx, cancel := context.WithCancel(parent)
	var processes []*exec.Cmd
	for _, args := range commands {
		if len(args) == 0 || strings.ContainsRune(args[0], '\x00') {
			cancel()
			return nil, errors.New("Invalid discovery command.")
		}
		command := exec.CommandContext(ctx, args[0], args[1:]...)
		command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C"}
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if err := command.Start(); err != nil {
			cancel()
			for _, started := range processes {
				_ = started.Wait()
			}
			return nil, fmt.Errorf("Nearby discovery is unavailable: %w", err)
		}
		processes = append(processes, command)
	}
	done := make(chan error, 1)
	var once sync.Once
	closeRegistration := func() { once.Do(cancel) }
	go func() {
		results := make(chan error, len(processes))
		for _, process := range processes {
			go func(command *exec.Cmd) { results <- command.Wait() }(process)
		}
		first := <-results
		closeRegistration()
		for i := 1; i < len(processes); i++ {
			<-results
		}
		done <- first
		close(done)
	}()
	return &companionDiscoveryRegistration{Close: closeRegistration, Done: done}, nil
}
