# Glowbom documentation portal

This directory is the source for the public documentation at `glowbom.com/docs/`.
It uses React Router, Fumadocs, and MDX. It is separate from the marketing site.
For the local Glowbom product setup, see the [repository README](../README.md).

## Edit the content

Public pages live in `content/docs/`. Their titles and descriptions come from
MDX frontmatter. `content/docs/meta.json` controls sidebar order. When adding a
page, also add its route to the prerender list in `react-router.config.ts`.
Search records are generated from the same content.

Use Glowbom for public names. Describe available behavior from the implementation
and distinguish it from planned work. Desktop 4.1.1 has a Mac early-access download;
Windows and Linux Desktop packages are not released. The documented OSS setup uses macOS, Linux,
or WSL on Windows. Do not describe the whole workflow as offline when it uses a
cloud model. Keep this directory independent of files outside the OSS repository.
The public browser interface is Agent + Buzz. Label Chat, Draw, Studio, Book,
onboarding, and native shell instructions as the separate Desktop client. Open
backend/API documentation stays here even when no public screen exposes it.

## Develop

From this directory:

```bash
bun install
bun run dev
```

Development serves the portal at `/`. To preview the built production URL layout:

```bash
bun run build
PORT=3005 HOST=127.0.0.1 bun run start
```

Open `http://127.0.0.1:3005/docs/`.

## Validate and build

```bash
bun run typecheck
bun run build
```

The production base path is `/docs`. Static files are generated under
`build/client/`; `build/server/` contains server output and is not needed when
publishing the prerendered portal to static hosting.

## Publish with the existing website

The portal has its own build. Rebuilding the marketing website does not update
these docs.

The contents of `build/client/docs/` belong in the existing hosting release's
`production/docs/` directory. Merge matching files and preserve the other app
folders in the combined release. Do not copy `build/server/` or replace the
hosting project's configuration with a new one.

Before publishing, verify the docs homepage and direct loads of `/docs/quickstart`,
`/docs/glowbom-oss`, `/docs/project-book`, and `/docs/desktop`. Check sidebar links,
search, styles, and mobile navigation. Confirm that the existing `/docs/glowby-oss`
compatibility route still resolves through the deployment's routing.

Check direct loads of `/docs/connect-ai` and `/docs/companion` too. Keep the
companion guide marked as a preview requiring matching Desktop and app builds.
Installation, agent login, and successful model discovery are separate steps.

A generic static file server does not reproduce Firebase rewrite behavior.
Use a local emulator to check the combined routing. Jacob alone publishes to
Firebase; agents prepare local output and verify it read-only after publication.
Do not upload through the CLI, API, browser, preview channels, or a workflow.
