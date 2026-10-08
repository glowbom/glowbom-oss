package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const defaultLaunchInstance = "oss"

var launchInstanceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

type startOptions struct {
	WebDir        string
	BackendPort   int
	WebPort       int
	AgentPort     int
	Instance      string
	ProjectPath   string
	ShowLocalAuth bool
	NoBrowser     bool
	Help          bool
}

func parseStartOptions(args []string) (startOptions, error) {
	agentPort, _ := strconv.Atoi(localAgentPort())
	options := startOptions{BackendPort: backendPort, WebPort: webPort, AgentPort: agentPort, Instance: defaultLaunchInstance}
	var positional []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		switch arg {
		case "--show-local-auth":
			options.ShowLocalAuth = true
			continue
		case "--no-browser":
			options.NoBrowser = true
			continue
		case "-h", "--help":
			options.Help = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, value, inline := strings.Cut(arg, "=")
		switch name {
		case "--web-dir", "--backend-port", "--web-port", "--agent-port", "--instance":
		default:
			return options, fmt.Errorf("unknown start option %s", name)
		}
		if !inline {
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "--") {
				return options, fmt.Errorf("%s requires a value", name)
			}
			value = args[index]
		}
		if value == "" {
			return options, fmt.Errorf("%s requires a value", name)
		}
		switch name {
		case "--web-dir":
			if !filepath.IsAbs(value) {
				return options, fmt.Errorf("--web-dir requires an absolute directory")
			}
			options.WebDir = filepath.Clean(value)
		case "--instance":
			options.Instance = value
		default:
			port, err := strconv.Atoi(value)
			if err != nil || port < 1 || port > 65535 {
				return options, fmt.Errorf("%s requires a port from 1 to 65535", name)
			}
			switch name {
			case "--backend-port":
				options.BackendPort = port
			case "--web-port":
				options.WebPort = port
			case "--agent-port":
				options.AgentPort = port
			}
		}
	}
	if len(positional) > 1 {
		return options, fmt.Errorf("glowbom start accepts at most one project path")
	}
	if len(positional) == 1 {
		options.ProjectPath = positional[0]
	}
	if options.Help {
		return options, nil
	}
	if !launchInstanceName.MatchString(options.Instance) {
		return options, fmt.Errorf("--instance must start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens")
	}
	if options.AgentPort < 1 || options.AgentPort > 65535 {
		return options, fmt.Errorf("invalid GLOWBOM_AGENT_PORT; choose a port from 1 to 65535")
	}
	if options.BackendPort == options.WebPort || options.BackendPort == options.AgentPort || options.WebPort == options.AgentPort {
		return options, fmt.Errorf("backend, web, and agent ports must be different")
	}
	return options, nil
}

func (options startOptions) isolatedCredentials() bool {
	return options.Instance != defaultLaunchInstance || options.WebDir != "" || options.BackendPort != backendPort || options.WebPort != webPort
}

func requireLaunchPortsFree(options startOptions) error {
	for _, service := range []struct {
		name string
		port int
	}{{"backend", options.BackendPort}, {"web", options.WebPort}, {"agent", options.AgentPort}} {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", service.port))
		if err != nil {
			return fmt.Errorf("%s port %d is unavailable. Stop the existing launch or choose different ports; no existing process was stopped", service.name, service.port)
		}
		_ = listener.Close()
	}
	return nil
}

// Remove inherited routing and packaged-app settings before constructing a new
// source launch. Provider credentials and explicitly selected executables remain.
func sourceLaunchEnvironment(base []string) []string {
	var environment []string
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		switch upper {
		case "GLOWBOM_DESKTOP", "GLOWBY_DESKTOP", "GLOWBOM_WEB_DIR", "GLOWBY_WEB_DIR", "GLOWBOM_RESOURCE_DIR", "GLOWBY_RESOURCE_DIR",
			"GLOWBOM_PORT", "GLOWBY_PORT", "GLOWBOM_WEB_PORT", "GLOWBY_WEB_PORT", "GLOWBOM_AGENT_PORT", "GLOWBY_AGENT_PORT",
			"GLOWBOM_BIND_HOST", "GLOWBY_BIND_HOST", "GLOWBOM_SERVER_TOKEN", "GLOWBY_SERVER_TOKEN", "GLOWBOM_ALLOWED_ORIGINS", "GLOWBY_ALLOWED_ORIGINS",
			"GLOWBOM_INSTANCE", "GLOWBOM_LAUNCH_ID", "GLOWBOM_CLI_BIN", "GLOWBY_CLI_BIN", "GLOWBOM_RELEASE_BUILD",
			"OPENCODE_URL", "GLOWBOM_OPENCODE_URL", "GLOWBY_OPENCODE_URL", "OPENCODE_SERVER_HOSTNAME", "OPENCODE_SERVER_PASSWORD", "OPENCODE_PASSWORD", "OPENCODE_SERVER_USERNAME",
			"VITE_BACKEND_TARGET", "VITE_GLOWBOM_SERVER_TOKEN", "VITE_GLOWBY_SERVER_TOKEN":
			continue
		}
		environment = append(environment, entry)
	}
	return environment
}

func launchEnvironments(options startOptions, identity launchIdentity, serverToken, opencodePassword, cliPath string) ([]string, []string) {
	common := append(sourceLaunchEnvironment(os.Environ()),
		"GLOWBOM_INSTANCE="+identity.Instance,
		"GLOWBOM_LAUNCH_ID="+identity.LaunchID,
		"GLOWBOM_BIND_HOST=127.0.0.1", "GLOWBY_BIND_HOST=127.0.0.1",
		fmt.Sprintf("GLOWBOM_PORT=%d", options.BackendPort),
		fmt.Sprintf("GLOWBOM_WEB_PORT=%d", options.WebPort),
		fmt.Sprintf("GLOWBOM_AGENT_PORT=%d", options.AgentPort),
	)
	origins := fmt.Sprintf("http://127.0.0.1:%d,http://localhost:%d,http://127.0.0.1:%d,http://localhost:%d", options.WebPort, options.WebPort, options.BackendPort, options.BackendPort)
	backend := append(append([]string{}, common...),
		"GLOWBOM_SERVER_TOKEN="+serverToken, "GLOWBY_SERVER_TOKEN="+serverToken,
		"GLOWBOM_ALLOWED_ORIGINS="+origins, "GLOWBY_ALLOWED_ORIGINS="+origins,
		"OPENCODE_SERVER_HOSTNAME=127.0.0.1", "OPENCODE_SERVER_USERNAME="+localOpenCodeUsername(),
		"OPENCODE_SERVER_PASSWORD="+opencodePassword, "OPENCODE_PASSWORD="+opencodePassword,
	)
	if cliPath != "" {
		backend = append(backend, "GLOWBOM_CLI_BIN="+cliPath)
	}
	web := append(append([]string{}, common...),
		fmt.Sprintf("VITE_BACKEND_TARGET=http://127.0.0.1:%d", options.BackendPort),
		"VITE_GLOWBOM_SERVER_TOKEN="+serverToken, "VITE_GLOWBY_SERVER_TOKEN="+serverToken,
	)
	return backend, web
}
