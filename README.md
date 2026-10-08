# Glowbom OSS

**Build local projects with the coding agents you choose.**

Open a project, describe a change, review the agent's work, and preview the result.
Glowbom OSS includes the open backend, CLI, project templates, and a simple
browser interface for coding agents and Buzz connections. Your project files
stay on your computer.

## Choose how to use Glowbom

| Product | Get started |
| --- | --- |
| **Glowbom OSS** | Open-source backend, CLI, and local coding interface. [Run from source](#run-glowbom-oss); no Glowbom account required. |
| **Glowbom Live** | A separate, closed-source 3D office for your Buzz channel. [Download the app from GitHub release assets](https://github.com/glowbom/glowbom-oss/releases/tag/v4.1.0). |
| **Glowbom Desktop** | The full Mac app, available through early access. [Sign up at glowbom.com/desktop](https://glowbom.com/desktop). |

Desktop and Live use the open backend. Their app source is not included in
this repository. Live's downloadable apps are hosted here as release assets.

## Connect a coding agent

Install and sign in to at least one agent before your first Build. Each agent
uses its own account, available models, and usage limits.

| Agent | Install | Sign in |
| --- | --- | --- |
| **OpenCode** | [OpenCode CLI](https://opencode.ai/docs/) | `opencode auth login` |
| **Codex** | [Codex CLI](https://developers.openai.com/codex/cli) | `codex login`, or **Sign in with ChatGPT** in Glowbom's Codex settings. |
| **Claude Code** | [Claude Code CLI](https://code.claude.com/docs/en/setup) | `claude auth login` |
| **Cursor** | [Cursor CLI](https://cursor.com/docs/cli/installation) | `cursor-agent login`; installations with only `agent` need the [path setup](docs/content/docs/connect-ai.mdx#connect-cursor). |

Glowbom manages Codex through **Codex App Server**. You do not need to start
that server yourself. For OpenCode, choose a provider during login.

### More agents through ACP

Agent Client Protocol (ACP) lets Glowbom connect to other installed coding
agents. In OSS, open **Settings → Coding agent → ACP connection**. Add the
executable and its arguments, test the connection, choose a model, and save.
You can save up to three connections.

| Agent | Executable | Arguments | Account setup |
| --- | --- | --- | --- |
| [Cline](https://docs.cline.bot/usage/acp) | `cline` | `--acp` | `cline auth` |
| [Goose](https://goose-docs.ai/docs/gdk/acp/) | `goose` | `acp` | `goose configure`; enable the Developer extension. |
| [Grok Build](docs/content/docs/connect-ai.mdx#grok-build) | `grok` | `agent`, then `stdio` on separate lines | `grok login` |
| [Hermes](docs/content/docs/connect-ai.mdx#hermes) | `hermes` | `acp` | `hermes model`, then `hermes acp --check` |

The [connection guide](docs/content/docs/connect-ai.mdx#add-an-acp-agent) also
covers Kilo and OpenClaw. Test each manual connection with a small Build;
capabilities depend on the installed agent. Grok Build and Hermes are documented
for testing; successful builds with your accounts still need to be checked.

## Run Glowbom OSS

Install [Go](https://go.dev/doc/install), [Bun](https://bun.sh/), and an agent
from the list above. Use macOS or Linux, or WSL on Windows.

Get this repository and start from source:

```sh
git clone https://github.com/glowbom/glowbom-oss.git
cd glowbom-oss/cli
go run . doctor
go run . start
```

Already have the checkout? Run the last two commands from its `cli/` folder.
The launcher opens the browser. Choose a project, select your agent and model
in **Settings**, enter instructions, and click **Build**. The OSS interface runs
one agent at a time. Press Ctrl+C in the terminal to stop.

Prefer the installed `glowbom` command? Follow the [CLI installation guide](cli/README.md).
It still needs this checkout, Go, and Bun. Published CLI releases can differ
from the source on `main`.

## Projects

Use an existing project or copy the bundled `project/` starter. See
[supported stacks](SUPPORTED_STACKS.md) and [previews](STACKS.md). Native builds
need the relevant platform tools, such as Xcode or Android Studio.

## Glowbom Live

Glowbom Live shows people and agents from your Buzz channel in a 3D office.
It is a separate, closed-source app for Mac, Windows, and Linux. Download the
matching `Glowbom-Live-4.1.1-*` package from the
[GitHub release assets](https://github.com/glowbom/glowbom-oss/releases/tag/v4.1.0).
The release tag is `v4.1.0`; the Live app downloads on it are version `4.1.1`.

Start OSS and click **Buzz** to connect your channel, then open the installed
Live app. Keep the local backend running. See the
[Live installation guide](docs/content/docs/glowbom-live.mdx) for downloads
and connection steps, or the [Buzz guide](BUZZ.md) for the open backend API.

## Glowbom Desktop

Glowbom Desktop is the separate, closed-source Mac app with Chat, Draw, Studio,
Project Book, and iOS/Vision Pro companion connections. It uses the open backend
and is available through early access.

**[Sign up for early access at glowbom.com/desktop](https://glowbom.com/desktop).**
See the [Desktop guide](docs/content/docs/desktop.mdx) for setup and companion pairing.

## Permissions and privacy

Agents edit files and run commands. Review permission requests; Cursor
uses its CLI's automatic tool behavior and deny rules. Project previews can
also run code and install dependencies.

The launcher keeps services on loopback and protects the backend with a local
access token. Cloud models send requests to the selected provider, whose charges
and limits apply. Keep credentials out of project files and logs. See
[security defaults](docs/content/docs/glowbom-oss.mdx#security-defaults).

## Documentation and development

- [Agents](docs/content/docs/connect-ai.mdx): installation, login, and ACP setup.
- [CLI](cli/README.md): launch, account, template, and export commands.
- [Project Book](PROJECT_BOOK.md): portable history and project records.
- [Companion guide](docs/content/docs/companion.mdx): connect to Desktop from iOS or Vision Pro.
- [Documentation portal](docs/README.md): build the public guides.
- [Publication checks](scripts/PUBLICATION_AUDIT.md): review source before sharing it.

```sh
(cd backend && go test ./...)
(cd cli && go test ./...)
(cd web && bun install && bun run typecheck && bun run build)
```

The public source builds independently of Desktop. Glowbom OSS was previously
Glowby OSS; existing projects and compatibility commands remain supported.
