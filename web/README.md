# Glowbom OSS interface

This React interface opens the local Agent workspace directly. It supports
OpenCode, Cursor, Claude Code, Codex, and configured ACP agents, with project and
stack selection, previews, build history, permissions, questions, media review,
and the Buzz connection to Glowbom Live. No Glowbom account is required to enter.

Run the complete local workflow with `glowbom start` from the OSS checkout, or
`go run . start` from `cli/`. For frontend checks, run these commands here:

```sh
bun install
bun run typecheck
bun test
bun run build
```

The source and dependencies are self-contained in the OSS checkout. The full
Desktop creation interface is maintained separately. Shared provider settings
and API types support agent media review without including the standalone
Studio, Draw, Chat, Project Book, or phone pairing screens.

The development server uses loopback port 4572 by default. The launcher can set
`GLOWBOM_WEB_PORT` and `VITE_BACKEND_TARGET`. Its local readiness endpoint,
`GET /__glowbom/launch`, returns only `GLOWBOM_INSTANCE` and
`GLOWBOM_LAUNCH_ID`; authentication tokens are never included in that response.
