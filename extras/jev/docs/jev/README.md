# Jev with OpenCode and Glowbom OSS

Jev is useful as a small decision tool inside a normal coding-agent workflow.

Use a normal coding model as the main agent. Examples include GPT, Claude, Gemini, Kimi, Muse, or another OpenCode model that supports tools. The coding model reads and edits code, runs tests, and calls tools. Jev handles narrow choices when a structured judgment can save reasoning time.

## What Jev is good for

Examples:

- Are these build results healthy or unhealthy?
- Is this failure most likely code, config, or environment?
- Which of a few fixes should be tried first?
- Should the agent retry, continue, or stop?

Jev is not a replacement for the main coding model. It is not meant to write code or answer open-ended questions.

## Files

- `.opencode/tools/jev.ts` is the OpenCode custom tool.
- `scripts/install-jev-tool.sh` installs the tool globally for OpenCode.
- `scripts/test-jev.mjs` checks the free Jev endpoint directly.

## Quick setup

From the Glowbom OSS repo root:

```bash
./scripts/install-jev-tool.sh
```

Then restart Glowbom OSS:

```text
Ctrl+C
```

```bash
glowbom start
```

The global install is important for Glowbom OSS because the OpenCode server can work on projects outside the Glowbom OSS repo. OpenCode loads global custom tools from:

```text
~/.config/opencode/tools/
```

The repo also keeps `.opencode/tools/jev.ts` so developers running OpenCode directly inside the Glowbom OSS repo can use it locally.

## Test Jev directly

Requires a recent Node.js version with built-in `fetch`.

```bash
node scripts/test-jev.mjs
```

A successful response should include a choice, confidence, probabilities, usage, and a cost of `0` for `jev-1.13-free`.

## Use Jev in a prompt

You can force a Jev call:

```text
Use the jev tool to judge whether this build is healthy.
Use choices: healthy=All important checks pass; unhealthy=One or more important checks fail.
```

Or give the coding agent a standing instruction so it can decide when Jev is useful:

```text
Use Jev for narrow decisions with clear choices when it can save reasoning time.
Do not use Jev for writing code or open-ended reasoning.
```

## How it works

The flow is:

```text
User -> coding model -> Jev when useful -> coding model continues
```

The tool sends a structured request to:

```text
https://opencode.ai/zen/v1/systemone
```

using:

```text
jev-1.13-free
```

The current free endpoint does not require a separate Jev API key.

## Expected OpenCode activity

When the model calls Jev, OpenCode should show something like:

```text
Running: jev
Completed: jev
```

If you instead see the model searching for `jevctl`, the custom tool was not loaded. Check the global tool path and restart the OpenCode server.

## Troubleshooting

### OpenCode does not see `jev`

Check:

```bash
ls ~/.config/opencode/tools/jev.ts
```

Then restart Glowbom OSS.

### `Choice question must have at least one choice`

The caller did not provide usable choices. Pass at least two choices in this form:

```text
healthy=All important checks pass; unhealthy=One or more important checks fail
```

### `jevctl` asks for credentials

This integration does not use `jevctl`. It calls the free OpenCode Zen Jev endpoint directly.

## Notes

Jev can add a small network call, so it does not make every single action faster. Its value is reducing the amount of reasoning the main coding model needs for small, well-defined decisions. This can save tokens and reasoning time in larger agent workflows.
