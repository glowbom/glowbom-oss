# Glowbom OSS

Glowbom OSS is the open local backend, command-line tool, and Agent + Buzz
browser workspace for Glowbom projects. Choose a project, give a coding agent
instructions, review its work, and preview the result. Your project files stay
on your computer.

The public browser interface focuses on Agent and Buzz. The full Glowbom Desktop
interface, including Chat, Draw, Studio, Project Book screens, onboarding, and
the Tauri shell, is developed separately. It uses this open backend. Its UI
source is not included here. The backend APIs, portable Book format, and project
templates remain available for other clients and tools.

Glowbom OSS was previously called Glowby OSS. Existing projects and transition
commands remain supported.

## Start locally

Install [Go](https://go.dev/), [Bun](https://bun.sh/), and the coding agent you
intend to use. For OpenCode, install it and connect a provider using
`opencode auth login`. The local workflow does not require a Glowbom account.

From the source checkout:

```sh
cd cli
go run . doctor
go run . start
```

The launcher starts the local backend and public browser interface. Choose a
project, select a configured agent/model, enter instructions, and select
**Build**. Review permission requests and generated assets before approving them.
Use **Preview** for supported project stacks and **Settings** for agent and
Buzz connections. Follow the [Buzz guide](BUZZ.md) for a channel and Glowbom Live.

A packaged CLI can also be installed with:

```sh
curl -fsSL https://raw.githubusercontent.com/glowbom/glowbom-oss/main/scripts/install.sh | sudo sh
```

Run `glowbom doctor` and `glowbom start` from a source checkout containing sibling
`backend/` and `web/` directories. The installer selects the latest published CLI
release; it may differ from changes in this checkout. On Windows, use WSL for
this local workflow. See the [CLI guide](cli/README.md) for native account and
export commands.

## Agents, projects, and previews

OpenCode uses your configured providers and models. Cursor requires its own CLI
and login. Backend adapters for additional agents retain their own credentials,
permissions, and model access; installing an agent does not connect its account.
See [Connect AI](docs/content/docs/connect-ai.mdx) for roles and limits. Screen
instructions there identify the full Desktop client where applicable.

Agent builds can edit files and run commands in the selected project. Cursor's
headless build mode uses its local permission and trust rules. Review the
selected agent's behavior before granting access to a project.

Use an existing project or copy the bundled `project/` template. Supported Apple,
Android, and web targets are described in [Supported stacks](SUPPORTED_STACKS.md).
[Custom stacks](STACKS.md) explains previews and connecting another folder.
Starting a preview can run project code and install dependencies. Native builds
still require the relevant development tools.

## Open backend and project records

The backend includes agent execution, provider connections, local project and
media operations, Project Book records, and companion APIs. The public browser
shell does not expose every endpoint as a screen. These guides describe the
protocols and, where stated, the separate Desktop UI:

- [Project Book](PROJECT_BOOK.md): portable history, evidence, and generation.
- [Buzz and Glowbom Live](BUZZ.md): channel connection and local messages.
- [Account API](backend/ACCOUNT.md): optional Glowbom account access.
- [CLI](CLI.md): source and packaged command-line use.
- [Documentation portal](docs/README.md): public API and product guides.

## Glowbom Live

[Glowbom Live](https://glowbom.com/desktop/#live) is a separate 3D office for a
Buzz channel. The current 4.1.1 packages are hosted as versioned assets on the
existing [OSS v4.1.0 release](https://github.com/glowbom/glowbom-oss/releases/tag/v4.1.0).
This hosting choice does not make the OSS source version 4.1.1.

The signed Mac Desktop app can launch Live with its local connection. When using
OSS in a browser, follow the manual connection steps in [BUZZ.md](BUZZ.md).
Mac Live is signed and notarized. Windows is an unsigned x64 preview; Linux has
x86_64 and ARM64 archives. Keep the backend running while Live is connected.

## Security and model costs

`glowbom start` binds services to loopback, creates a per-run backend bearer token,
and authenticates the OpenCode bridge. Preserve those checks when adding clients.
Use `glowbom start --show-local-auth` only when you need the local connection
values, and never share their output. Manual backend launches need equivalent
local authentication configuration; the CLI is the recommended entry point.
The backend refuses a network-facing bind address when no bearer token is set.
Optional file-based account credentials must stay outside project and source
directories, including paths reached through symlinks. The system keyring remains
the default.

Local project storage does not mean every model runs offline. Cloud requests use
your selected provider and account, and media generation can incur charges.
Keep credentials outside project files, source, logs, and release archives.

Before publishing source, run `python3 -B scripts/audit-publication.py`. It checks
for known credential patterns, private source directories, and local files.
Read the [publication check guide](scripts/PUBLICATION_AUDIT.md) for its scope and
limits. Review the changes being published as well as the automated result.

## Development checks

```sh
(cd backend && go test ./...)
(cd cli && go test ./...)
(cd web && bun install && bun run typecheck && bun run build)
```

The public build must work without files from the private Desktop client or any
other repository. Run checks for the area you change.

## Repository layout

- `backend/`: open Go backend and APIs
- `cli/`: local launcher and account commands
- `web/`: minimal Agent + Buzz React interface
- `project/`: portable project template
- `extras/`: optional agent and local-model integrations
- `docs/`: public API and product documentation
- `scripts/`: installation scripts

Historical public checkouts may also contain `legacy/`. The current source
export does not refresh that older application code.
