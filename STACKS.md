# Custom stacks and existing apps

A stack has a name, a folder, and a description of the language, framework, and
libraries the agent should use. Glowbom saves these choices with the project and
includes them whenever you build that target, including follow-up edits.

## Build a new app

1. Load a Glowbom project and open **Preview**.
2. Click **+ Add stack**. Choose a category and preset, search across the catalog,
   or choose **Describe your own**.
3. Edit the description and choose a folder, such as `apps/my-tool`.
4. Click **Save stack**. The new stack becomes the selected build target.
5. Describe the app or change in the main editor and click **Build**.
6. After the agent creates a working app, click **Start preview**, then
   **Open in Browser** to use it in a separate browser tab.

Saving the stack stores instructions; **Build** asks the selected coding agent to
create or change the app. The agent can use an existing prototype as a reference.
An existing target is edited in place. Check **Project > Build targets** to select
several targets or change which one the next build updates. Switching preview
tabs does not change the build targets.

The React preset follows the dependency declarations of Glowbom OSS: React,
TypeScript, Vite, plain CSS, and Bun for a new app. The Tauri preset adds a Tauri 2
Rust wrapper, browser fallbacks for native features, and separate desktop checks.
Both descriptions are editable. Existing apps keep their package manager and
working dependencies unless you request a change.

For example, after choosing **Tauri + React**, a build request could be:

> Build a local karaoke editor. Let me open an audio file, type and time lyrics,
> play the song with highlighted words, and save a portable project. Support a
> browser preview and native file dialogs in the desktop app. Add behavior tests
> for timing and file import/export, and document the run and packaging commands.

## Work on an existing app

Glowbom currently needs a `glowbom.json` file in the folder you load. For an app
that does not have one, add this small manifest alongside its `package.json`:

```json
{
  "name": "My app",
  "version": "0.1.0",
  "targets": {}
}
```

Keep an existing manifest if one is already present. Load that folder in Glowbom,
add the appropriate stack, and set **Project folder** to `.`. This points the
stack at the existing app. It does not move or replace source files.

For a repository containing several apps, put the manifest at the repository
root instead, then add each app with its own folder, such as `apps/karaoke`.
Tell the agent to read the repository's instructions and the selected app's
README before making changes.

### Karaoke example

[Karaoke](https://github.com/glowbom/Glowbom/tree/master/apps/karaoke) already
uses React, TypeScript, Vite, and Tauri. To refine a local copy:

1. Add the manifest above to `apps/karaoke`, with the name `Karaoke`, if it does
   not already have a manifest. Load that local folder in Glowbom.
2. Add **Tauri + React**, name it **Karaoke**, and set the folder to `.`.
3. Keep its existing npm setup. Run `npm install` in the app folder before
   starting the preview. Glowbom's **Install & run** uses Bun, so install with
   the existing package manager yourself when preserving a lockfile.
4. Save the stack and send a specific change request. For example:

> Read ../AGENTS.md, README.md, and package.json. Add a search field to the song
> list. Preserve the existing .karaoke project format and LRC, SRT, and TTML
> exports. Keep the browser and desktop behavior working. Run npm test and
> npm run build, and report any desktop checks that could not run.

Use **Start preview > Open localhost** for the browser version. Karaoke's
`npm run tauri dev` starts the native app, and `npm run tauri build` packages it.
These native commands require Rust and the relevant platform tools. See the
[Karaoke README](https://github.com/glowbom/Glowbom/blob/master/apps/karaoke/README.md)
for its setup and the [Tauri prerequisites](https://v2.tauri.app/start/prerequisites/)
for platform requirements.

## Preview and saved instructions

Glowbom detects HTML, Next.js, and Vite from the selected folder. Other servers
can use **Preview settings > Launch command**, with `{host}` and `{port}`
placeholders. For PHP with `index.php` in the selected folder, for example:

```text
php -S {host}:{port}
```

Install the runtime and dependencies first. Put framework and library choices
in **Describe your stack**; the launch command only starts its web server.

Stack descriptions, folders, and launch commands are stored in
`.glowbom/previews.json`. You can edit the description in Glowbom before the next
build. It is resolved again for each run, so resumed agent sessions also receive
the updated choices. These are instructions for the coding agent, not a file
access sandbox.

## Preset catalog

Existing `prototype/` and `web/` targets keep their HTML and Next.js behavior.
Adding a preset creates another target. Editing a saved stack does not show the
preset picker, so choosing a new framework cannot replace that stack by accident.

| Category | Presets |
| --- | --- |
| Web | React + Vite, HTML + CSS, Next.js, PHP |
| Mobile & desktop | Tauri + React, React Native + Expo, Flutter, SwiftUI, Kotlin + Compose |
| Games & simulations | Godot 2D, Godot 3D |
| Backends & tools | Python + FastAPI, Go HTTP service, Command-line tool |

Each card describes its intended preview and required tools. Presets save the
build instructions, folder, preview mode, command, and setup notes with the
project. These generation recipes do not install SDKs or certify that every
generated app works on every destination.

**Preview settings** offers automatic detection, a saved web server command,
or no browser preview. PHP includes its `public/` launch command. Flutter uses
its web server. FastAPI expects a project virtual environment and an API page at
`/`; its preset command uses the macOS/Linux virtual environment path. Go expects
the generated service to accept `--host` and `--port`.

Expo instructions ask the agent to create `tools/preview.mjs` to start the local
Expo web server. Godot instructions ask for `tools/preview.py` to export and serve
the game, with matching Godot export templates installed. Those scripts must
exist before preview can start. Godot uses GDScript, the Compatibility renderer,
and single-threaded web export. Exported games update after rebuilding; browser
preview is not a replacement for native or editor testing.

Native and command-line targets remain build targets without an iframe.
**Open in Terminal** opens their folder in a separate system terminal without
executing a project command. It supports Terminal on macOS, Windows Terminal,
and installed GNOME, Konsole, or Xfce terminals on Linux. This is not an embedded
terminal. Use the generated README for run commands. If no supported terminal is
installed, open the folder manually in your preferred terminal.

The preset catalog and explicit preview modes require the updated backend.
Restart Glowbom OSS after updating. An older backend is detected before saving,
so it cannot silently discard the new settings.

Tauri's browser preview runs the Vite frontend. Browser testing does not verify
Rust commands, native dialogs, permissions, or installers. Verify those with the
app's native run and build commands before distributing it.

### Find apps already in your project

**Add stack** opens the categorized preset catalog. Already connected stacks remain in the preview tiles; select a custom stack and choose **Edit** to change its instructions or preview settings. Glowbom reads common project markers, such as `package.json`, `project.godot`, PHP entry points, Flutter metadata, Gradle files, and Xcode projects. It also checks folders named in `glowbom.json` and saved stack settings. Folder names alone do not establish a framework.

Unconnected folders appear in a collapsed **Found other apps** section below the catalog. Expand it and choose **Add to project** to review settings. Saving a connection does not generate source files or start a server. Preview commands and build instructions stay in `.glowbom/previews.json`.

The catalog contains categorized presets. Badges show which presets already appear in the project. You can still add another copy; the suggested folder avoids detected and connected app folders. An independent HTML app stays separate from the built-in Prototype target. The built-in Web target remains unchanged.

Discovery is a bounded scan, not a check that every app builds or its tools are installed. It skips dependency, build, hidden, and archive metadata folders, and does not follow folders outside the project. Use **Scan for existing apps** after adding files. For apps it does not recognize, choose **Describe your own** and enter their folder and preview settings. Native apps and command-line tools can be connected without a browser preview.
