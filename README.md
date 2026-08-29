# Glowbom OSS

> Glowbom OSS was previously called Glowby OSS. Existing projects and transition commands remain supported.

**Build software like writing a book.**

**An open, local workflow where every project has a Project Book that people and coding agents can understand.**

Glowbom OSS helps you build production-ready software with coding agents. It is an open source coding agent workflow for real projects. It is built primarily for Glowbom projects, but the workflow can also work with other project structures.

## What It Does

- Make software projects and prototypes production-ready with coding agents
- Use the providers and models already configured in OpenCode

## Vision

We believe that you should own your code and data. Every line of code Glowbom OSS generates lives on your machine, in standard project files you can open with any editor. No vendor lock-in.

## Install

Install the Glowbom CLI:

```bash
curl -fsSL https://raw.githubusercontent.com/glowbom/glowbom-oss/main/scripts/install.sh | sudo sh
```

For Windows, we recommend using WSL and running the install command inside Ubuntu.

Then clone the repo and enter it:

```bash
git clone https://github.com/glowbom/glowbom-oss.git
cd glowbom-oss
```

## Quickstart

Glowbom OSS needs these tools available on your `PATH`:

- [Go](https://go.dev/)
- [Bun](https://bun.sh/)
- [OpenCode](https://opencode.ai/)

Run the built-in environment check and launch Glowbom OSS:

```bash
glowbom doctor
glowbom start
```

Glowbom OSS uses your local OpenCode setup for provider access. We recommend signing in to ChatGPT through OpenCode:

```bash
opencode auth login
```

Choose OpenAI, then choose ChatGPT Plus/Pro and finish the browser login. You can use the same OpenCode command to configure another provider instead.

Run those commands from the Glowbom OSS repo root, where `backend/` and `web/` live side by side.

## Security Defaults

`glowbom start` hardens the local stack by default:

- Glowbom OSS services bind to loopback (`127.0.0.1`) instead of all interfaces
- the backend API requires a per-run bearer token
- the OpenCode bridge runs with `OPENCODE_SERVER_PASSWORD`

To view the generated credentials for the current session, start Glowbom OSS with `glowbom start --show-local-auth`.

If you launch the stack manually, set equivalent env vars yourself:

```bash
export GLOWBOM_BIND_HOST=127.0.0.1
export GLOWBOM_SERVER_TOKEN="$(openssl rand -hex 32)"
export OPENCODE_SERVER_PASSWORD="$(openssl rand -hex 32)"
```

Then run the backend with those env vars, and start the web app with:

```bash
export VITE_GLOWBOM_SERVER_TOKEN="$GLOWBOM_SERVER_TOKEN"
```

The previous `GLOWBY_*` variables remain accepted during the rename transition.

## Start Using Glowbom OSS

1. Open `http://localhost:4572`
2. Load a local project
3. Choose the model from your OpenCode setup, or keep its configured default
4. Start a refine run

## Cost

You can build with Glowbom OSS for free. OpenCode can use local models on your computer, free cloud models, or a paid provider account that you configure directly in OpenCode.

## Requirements And Setup

If `glowbom doctor` reports missing tools, install them first and confirm they are available on your `PATH`:

```bash
go version
bun --version
opencode --version
```

If any command is not found, restart your terminal first. If it still does not work, add the tool's install location to your `PATH` or reinstall it using the tool's recommended installer.

On macOS, a common fix is to add the tool's bin directory to your shell profile (usually `~/.zshrc`) and then reload it:

```bash
# Common PATH fixes on macOS
echo 'export PATH="/usr/local/go/bin:$PATH"' >> ~/.zshrc
echo 'export BUN_INSTALL="$HOME/.bun"' >> ~/.zshrc
echo 'export PATH="$BUN_INSTALL/bin:$PATH"' >> ~/.zshrc

# Add the directory that contains the opencode binary
echo 'export PATH="/path/to/opencode/bin:$PATH"' >> ~/.zshrc

source ~/.zshrc
```

If you use Bash instead of zsh, update `~/.bash_profile` or `~/.bashrc` instead.

### Manual fallback

If you prefer to launch the stack without the CLI, run the backend and web app separately:

#### Backend

```bash
cd backend
go run .
```
The backend runs on `http://localhost:4569`.

#### Web app

```bash
cd web
bun install
bun run dev
```

The web app runs on `http://localhost:4572`.

## Using the Bundled Default Project

This repo includes a ready-to-use Glowbom default project in `project/`.

You can use `project/` as your main starting template without logging in to Glowbom.com or downloading a project export first. Just copy the folder, rename it if you want, and start customizing it locally.

The bundled project includes:

- `project/prototype/` - reference design and assets
- `project/apple/` - Apple app project
- `project/android/` - Android app project
- `project/web/` - web app project
- `project/glowbom.json` - project manifest

If you only need some targets, remove the platform folders you do not want:

- Delete `project/apple/` if you do not need Apple platforms
- Delete `project/android/` if you do not need Android
- Delete `project/web/` if you do not need web
- Keep all of them if you want to build every platform in sync from one Glowbom project

## Project Structure

- `backend/` - Go backend
- `cli/` - Glowbom command-line tool
- `docs/` - documentation website
- `project/` - bundled default Glowbom project template
- `scripts/` - installation scripts
- `web/` - React + Vite web app
- `legacy/` - older Glowby code kept for historical reference
