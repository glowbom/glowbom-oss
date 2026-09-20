## Jev decision tool

Glowbom OSS can use Jev as an optional decision tool for OpenCode agents. Keep your normal coding model as the main agent and let it call Jev for narrow choices such as build health, likely error class, or which action to try next.

Install the global OpenCode tool:

```bash
./scripts/install-jev-tool.sh
```

Then restart `glowbom start`.

See [`docs/jev/README.md`](./docs/jev/README.md) for setup, examples, and troubleshooting.
