# Apple Intelligence for Glowbom OSS

Glowbom can add Apple's on-device Foundation Model to its OpenCode model list.
It uses the open-source [apfel](https://github.com/Arthur-Ficial/apfel) bridge
on a loopback-only port.

## One-click setup

On a supported Mac, open **Settings → Apple Intelligence** and turn on
**Use Apple Intelligence**. Glowbom:

1. Installs `apfel` with Homebrew when needed.
2. Starts `apfel` on `127.0.0.1:11435` and keeps it running while Glowbom runs.
3. Adds a managed provider to the OpenCode server used by Glowbom.
4. Reloads OpenCode so the model appears in Chat.

Glowbom does not replace or rewrite your normal OpenCode configuration. Turning
the setting off removes the provider from Glowbom and stops the `apfel` process
Glowbom started. It does not uninstall `apfel`.

If setup fails, the dialog shows the reason. Common causes are the Intel
version of Homebrew, Apple Intelligence turned off in System Settings, or a
model that is still downloading.

## Requirements

- An Apple Silicon Mac
- macOS 26 or later
- Apple Intelligence enabled in System Settings
- The Apple Silicon version of Homebrew, installed in `/opt/homebrew`

The model runs on the Mac and has a 4096-token context window. That window is
too small for OpenCode's instructions and tools, so Glowbom uses Apple
Intelligence for Chat only. Chat sends messages straight to `apfel` with a
short instruction, keeps only the most recent messages that fit, and does not
include project files. If `apfel` stops, Glowbom starts it again on the next
message. Build and drawing do not offer this model.

## Manual bridge setup

If the one-click installer cannot run, use:

```bash
chmod +x extras/apple-intelligence/setup.sh
./extras/apple-intelligence/setup.sh
```

Then return to Glowbom and enable the setting. The model ID is:

```text
apple-intelligence/apple-foundationmodel
```

The bridge stays bound to `127.0.0.1`. Do not expose it to the network.
