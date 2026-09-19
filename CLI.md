# glowbom CLI

Terminal-first CLI for Glowbom OSS. Starts the Go backend and web UI, opens the browser, and manages the full local dev workflow from one command.

## Install

### From GitHub Releases

```sh
curl -fsSL https://raw.githubusercontent.com/glowbom/glowbom-oss/main/scripts/install.sh | sh
```

Or set a custom install directory:

```sh
curl -fsSL https://raw.githubusercontent.com/glowbom/glowbom-oss/main/scripts/install.sh | GLOWBOM_INSTALL_DIR="$HOME/.local/bin" sh
```

### Build from source

```sh
cd cli
go build -o glowbom .
```

## Commands

### `glowbom start`

Start the backend, web UI, and open the browser from a local Glowbom OSS checkout.

```sh
glowbom start                    # Start Glowbom OSS from the current checkout
glowbom start /path/to/project   # Start Glowbom OSS and print a project path hint
```

What it does:
1. Starts the Go backend (`go run .` in `backend/`)
2. Runs `bun install` in `web/` if `node_modules/` is missing
3. Reclaims ports `4569` and `4572` if they are already occupied by a previous Glowbom OSS run
4. Starts the web dev server (`bun run dev` in `web/`)
5. Waits for the web server to be ready, then opens the browser
6. If a project path is given, prints the path so you can load it in the UI

Port checks consider only listening servers. Browser or editor connections left
over from a previous run do not block startup. An existing Glowbom server is
restarted automatically; an unrecognized server is left running.

Press Ctrl+C to stop both servers.

**Argument parsing:**
- If a positional arg is given and it is an existing directory, it is treated as the project path
- If it is not an existing directory, the command exits with an error

**Finding the Glowbom OSS root:** The CLI looks for sibling `backend/` and `web/` directories relative to the binary location or the current working directory and its parent directories.

### `glowbom doctor`

Check that required tools are installed.

```sh
glowbom doctor
```

Checks for: `go` (required), `bun` (required), `opencode` (required), and a local Glowbom OSS checkout with sibling `backend/` and `web/` directories. Returns exit code 1 if required dependencies are missing.

### `glowbom version`

```sh
glowbom version
```

Prints version, commit hash, and build date. These are injected at build time via ldflags.

## Local Development

```sh
cd cli

# Build
go build -o glowbom .

# Build with version info
go build -ldflags "-s -w \
  -X main.version=v0.1.0 \
  -X main.commit=$(git rev-parse --short HEAD) \
  -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o glowbom .

# Run
./glowbom version
./glowbom doctor
./glowbom start

# Vet
go vet ./...
```

## Rename compatibility

`glowbom code` remains available as a deprecated alias for `glowbom start`. Existing `GLOWBY_*` configuration variables remain accepted, but new setup should use `GLOWBOM_*`. Release archives ship only the `glowbom` binary.

## Releases

Releases are built automatically by GitHub Actions when a tag matching `v*` is pushed.

```sh
git tag v0.1.0
git push origin v0.1.0
```

The workflow builds binaries for:
- macOS (amd64, arm64)
- Linux (amd64, arm64)
- Windows (amd64, arm64)

Archives are uploaded to the GitHub Release page.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Command or dependency failure |
| 2 | Usage error (unknown command, bad args) |
