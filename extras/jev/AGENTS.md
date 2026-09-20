# Jev integration notes for coding agents

This directory contains an optional OpenCode custom-tool integration for Jev.

## Purpose

Keep Jev outside Glowbom OSS core code. The provider, endpoint, free tier, model ID, or installation method may change independently of Glowbom OSS.

## Runtime behavior

- Tool source: `extras/jev/jev.ts`
- Installer copies it to: `~/.config/opencode/tools/jev.ts`
- OpenCode global tools are needed because Glowbom OSS can run OpenCode sessions in generated or downloaded projects outside the `glowbom-oss` repository.
- Restart Glowbom OSS after installing or changing the global tool.
- Current endpoint: `https://opencode.ai/zen/v1/systemone`
- Current model: `jev-1.13-free`
- This integration calls the endpoint directly. It does not use `jevctl`.

## Recommended standing instruction

```text
Use Jev for narrow decisions with clear choices when it can save reasoning time.
```

Jev is best for small structured judgments such as:

- healthy vs unhealthy
- code vs config vs environment
- retry vs continue vs stop
- choosing among a small number of proposed fixes

Do not use Jev as the main coding model or for open-ended code generation.

## Tool arguments

The OpenCode tool accepts:

- `state`: evidence or facts to judge
- `question`: the decision to make
- `choices`: at least two `key=description` choices separated by semicolons or newlines

Example:

```text
healthy=All important checks pass; unhealthy=One or more important checks fail
```

## Troubleshooting

If the activity log shows `Running: jev`, the custom tool is loaded.

If the model searches for or installs `jevctl`, the custom tool was not available to that OpenCode session. Verify:

```bash
ls ~/.config/opencode/tools/jev.ts
```

Then restart Glowbom OSS.

If Jev returns `Choice question must have at least one choice`, the caller did not provide at least two valid choices in `key=description` form.

The current free OpenCode Zen endpoint has worked without a separate Jev API key. Treat that as provider behavior that may change.
