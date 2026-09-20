# Glowbom OSS

> Glowbom OSS was previously called Glowby OSS. Existing projects and transition commands remain supported.

**Build software like writing a book.**

**An open, local workflow where every project has a Project Book that people and coding agents can understand.**

Glowbom OSS helps you build production-ready software with coding agents. It is an open source coding agent workflow for real projects. It is built primarily for Glowbom projects, but the workflow can also work with other project structures.

## What It Does

- Make software projects and prototypes production-ready with coding agents
- Use the providers and models already configured in OpenCode
- Try Cursor CLI as an optional coding agent for local builds
- Preview your project locally and open it in a separate browser tab

See [all supported stacks](SUPPORTED_STACKS.md), why each is included, and which ones come in the standard Glowbom project. You can also add any custom stack.

Image placeholders in downloaded projects support both `glowbomimages:` and
legacy `glowbyimages:` prefixes. The singular forms `glowbomimage:` and
`glowbyimage:` also remain supported. They use the same approval and image
processing flow; existing projects do not need to be renamed.

## Vision

We believe that you should own your code and data. Every line of code Glowbom OSS generates lives on your machine, in standard project files you can open with any editor. No vendor lock-in.

## Glowbom Live

Glowbom Live is a 3D office for your Buzz channel. People and agents show up as
characters and react when messages come in. It is available now as a preview.

Download it from [glowbom.com/desktop](https://glowbom.com/desktop/#live), or
from the [v4.1.0 Glowbom OSS release](https://github.com/glowbom/glowbom-oss/releases/tag/v4.1.0).
Install OSS 4.1.0 or later, run `glowbom start`, connect a Buzz channel, then
paste the local backend access token into Live.

macOS is signed and notarized. Linux has portable x86_64 and ARM64 archives.
Windows is an unsigned x64 preview; run OSS in WSL.

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
- [OpenCode](https://opencode.ai/) or [Cursor CLI](https://cursor.com/docs/cli/installation)

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

Optional hosted account commands are described in the [CLI guide](cli/README.md). The local workflow does not require a Glowbom account.

## Cursor agent (preview)

Install [Cursor CLI](https://cursor.com/docs/cli/installation) on the computer
running the backend, then sign in using `cursor-agent login`. Check your login
with `cursor-agent status`.

In Glowbom OSS, open **Settings**, choose **Cursor (preview)** under **Coding
agent**, and refresh the checks. Leave the model blank to use Cursor's default,
or enter a model ID from `cursor-agent models`. Your Cursor account handles access
and usage. Glowbom does not copy its login tokens.

Glowbom looks for `cursor-agent` on `PATH` and in `~/.local/bin`. If your install
uses the command `agent`, or a different location, set `GLOWBOM_CURSOR_BIN` to its
full executable path before starting the backend.

This integration supports streamed build progress, staged instructions and
attachments, changed-file reporting, history, and continuing a Cursor session.
Switching agents starts a separate session. Stop cancels the running CLI process
group on macOS and Linux; edits already made remain in the project.

Build runs Cursor in headless mode with `--force`, allowing file edits and shell
commands without individual confirmation dialogs. Cursor's local permission and
workspace trust settings still apply. Open the project in Cursor CLI first if it
requires workspace trust. Automatic Glowbom media generation and interactive
OpenCode approval dialogs are not part of Cursor runs. Answer any questions with
a follow-up build.

The adapter is tested with a simulated CLI. A real Cursor account run still needs
verification before this preview can be considered dependable.

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

## Project previews

Load a project and click **Preview** beside the editor buttons. Choose
**Prototype** (`prototype/`) or **Web** (`web/`), then click **Start preview**.
For a Next.js or Vite app that needs packages, **Install & run** runs `bun install`
first. Bun must be available on the backend's `PATH`.

Use **Open in Browser** to open the same app in a separate browser tab. You can
interact with it there or inside Glowbom, switch between phone and desktop widths,
and see changes while an agent works. HTML previews refresh after file edits.
Next.js and Vite use their own live updates. Startup errors appear in the preview
logs.

The folder menu beside the stack tiles opens the selected stack in your browser,
terminal, file manager, or a detected local editor. It also lets you copy the
folder path. The project-level folder button still opens the whole project.

**+ Add stack** lets you describe what the agent should build. Start with
**React + Vite** (the Glowbom OSS frontend stack), **Tauri + React** (web and
desktop), or your own description. Choose a folder such as `apps/my-tool`, save,
then describe your app in the main editor and click **Build**. Saving a new stack
selects it as the next build target. Add other targets under **Project** when
needed. The stack description is included on every build that selects it.

You can also connect an existing folder. Static HTML, Next.js, and Vite are
detected automatically. Stack descriptions and preview settings are saved in
`.glowbom/previews.json`. See [custom stacks and existing apps](STACKS.md) for
examples, including how to work on Karaoke.

For another web server or an app with a special startup script, enter its launch
command. For example, a Vite script can use:

```text
bun run dev --host {host} --port {port}
```

The command must use both placeholders. Glowbom supplies `127.0.0.1` and an
available port. Commands run from the selected folder without a shell; put complex
startup steps in a project script. Install dependencies for custom commands first.
The embedded preview refreshes when the custom folder changes. A custom server
needs its own live reload support to update a separate browser tab automatically.

Previews use separate local ports. Built-in HTML previews have a separate access
token, while framework servers rely on loopback access. They do not receive the
Glowbom backend token or provider credentials. Starting a framework or custom
command runs project code on your computer, including package installation hooks.
Custom servers must honor the supplied host and port. A localhost preview is not
a sandbox or a public deployment.

Switching preview tabs keeps the project's running servers available. **Stop
preview**, switching projects, or shutting down the backend stops them. Hiding
the Preview panel keeps them running, so a separate browser tab remains usable.
Native Apple and Android apps still open in their platform tools. Tauri previews
show the web interface; native features and packaging require a separate desktop
run.

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

## Bring a saved Glowbom project to your computer

With the CLI built from the current source, sign in and export your saved project:

```bash
glowbom login
glowbom export --output ~/Downloads/my-glowbom-project
```

Finish saving in Glowbom before exporting and choose a new destination folder.
The command combines your saved generation with the current public starter,
placing available HTML, SwiftUI, Kotlin, and Next.js code in its platform
locations. It preserves original exports and existing local folders.

Use `glowbom pull` for only the saved generation or `glowbom template` for a
clean starter without signing in. See the [CLI guide](cli/README.md#export-a-complete-project-folder)
for options, file placement, and how the export is assembled.

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

An optional [Buzz member lookup preview](BUZZ.md) is available in Settings.

- `backend/` - Go backend
- `cli/` - Glowbom command-line tool
- `docs/` - documentation website
- `project/` - bundled default Glowbom project template
- `scripts/` - installation scripts
- `web/` - React + Vite web app
- `legacy/` - older Glowby code kept for historical reference
