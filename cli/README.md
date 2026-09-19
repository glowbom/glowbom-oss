# Glowbom CLI

The CLI starts the local Glowbom OSS workflow and checks its tools with `glowbom doctor`. See the [main README](../README.md) for setup.

## Optional Glowbom account

This source includes browser login, allowance reads, logout, hosted image generation, saved project downloads, a clean starter download, and combined project exports. A Glowbom account is not required for `doctor`, `start`, or `template`. Build from this source to try commands that may not yet be in your installed CLI release.

```sh
glowbom login
glowbom account
glowbom logout
```

Login opens the Glowbom sign-in page. Choose your account and approve the connection. The browser returns to a temporary localhost listener on this computer and shows **Build something great.** after the terminal saves the credentials. The listener closes when login finishes or expires. Account tokens never appear in browser URLs.

On a remote server, use `glowbom login --device-auth --no-browser` and open the printed link on another device. This mode asks you to enter the code from the terminal and uses polling instead of a localhost callback. Only approve a login you started. Both modes expire after five minutes.

`glowbom login --no-browser` alone still uses localhost: open its printed link in a browser on the same computer. Keep the login command running until it confirms the connection.

`glowbom account` displays your subscription category and remaining generation allowance. Credentials refresh automatically when needed; use `glowbom account --refresh` to refresh immediately. `glowbom logout` removes the credentials from the selected store on this machine. Your browser and other machines remain signed in.

### Structured account status

`glowbom account --json` returns a versioned status for local applications, with
no tokens or generation balances. The normal account command's text output is
unchanged. `--refresh` can be combined with `--json`.

```json
{"version":1,"status":"signed_in","uid":"example-user","email":"person@example.com","subscriptionStatus":"premium"}
```

`status` is `signed_in`, `signed_out`, or `unavailable`. Signed-in responses include
the verified identity and hosted subscription category. A verified account whose
billing record is not ready returns `subscriptionStatus: "unknown"` and
`code: "account_not_ready"`. Missing credentials or a rejected session return
`signed_out`; credential-store and network failures return `unavailable`, without
identity fields. Exit status is zero for signed-in or signed-out results and one
for unavailable results. Check the JSON version and status before using the fields.

The optional [local account bridge](../backend/ACCOUNT.md) lets Glowbom Live start
browser login and read this status through the authenticated OSS backend.

## Download a clean starter

Start a new project without signing in:

```sh
glowbom template
glowbom template --output ~/Downloads/my-new-project
```

The default creates a unique `glowbom-template-*` folder in the current directory. `--output` names a new folder whose parent must already exist. Existing files, folders, and symlinks are never replaced.

This downloads the current public starter from [audio.glowbom.com/starter.zip](https://audio.glowbom.com/starter.zip), checks the ZIP, and extracts the Apple, Android, and web projects together with their manifest and instructions. Finder metadata is removed, and the Android Gradle launcher stays executable. No code is run and no dependencies are installed. Platform builds still need their development tools.

Each run fetches the shared URL, so a replacement starter is available without rebuilding the CLI. Cloudflare may still serve a cached copy after an upload; purge that URL's cache when publishing an immediate update. Compatible ZIPs must stay within 8 MiB downloaded, 16 MiB unpacked, and 512 archive entries. Downloaded files are checked for unsafe paths and ZIP checksum errors before extraction.

`template` gives you a clean starter. `pull` downloads your account's saved generation and exports. Use `export` to combine the two in a new project folder. All three commands preserve existing local work.

From this source directory, use `go run . template --output ~/Downloads/my-new-project`. Run `go run . template --help` for the options. The command requires no Firebase or Cloudflare deployment.

## Pull your saved project

Finish saving your project in Glowbom, then download it with your existing login:

```sh
glowbom pull
glowbom pull --output ~/Downloads/my-glowbom-project
```

The default creates a unique `glowbom-project-*` folder in the current directory. `--output` names a new folder whose parent must already exist. Any existing file, directory, or symlink at that destination is rejected, including an empty directory. Choose a new folder for each pull.

The command downloads the current saved project from your account, checks the ZIP, and extracts its files. It preserves `glowbom.json`, the prompt, the saved source exports, and any drawing or icon listed in that manifest. This is the saved generation bundle; it does not assemble a complete platform project or install dependencies. No downloaded code is run. Only files included in the saved manifest are extracted.

Pull is a manual download. It does not upload local edits or keep the folder synchronized. Save first and wait for the save to finish before pulling. The API currently limits the bundle to 100 files and 9 MiB of unpacked content. An unfinished or missing save produces a message without replacing local work.

From this source directory, run `go run . pull --output ~/Downloads/my-glowbom-project`. `go run . pull --help` shows the options. Pull uses the existing API endpoint and requires no server deployment. Automated tests use simulated project responses and credentials; a real account project download remains a manual check.

## Export a complete project folder

Finish saving your project in Glowbom, wait for the save to complete, and sign in to the CLI:

```sh
glowbom login
glowbom export
glowbom export --output ~/Downloads/my-exported-project
```

`export` downloads your current saved generation from the account API and the public [starter ZIP](https://audio.glowbom.com/starter.zip), then combines them into one folder. It includes the starter's Apple, Android, and web projects with your available generated code placed inside them. The original source exports, prompt, and any saved drawing or icon remain included.

| Saved output | Placement in the exported folder |
| --- | --- |
| Nonempty processed HTML, otherwise raw HTML | `prototype/index.html` |
| SwiftUI | `apple/Custom/AiExtensions.swift` |
| Kotlin | `android/app/src/main/java/com/glowbom/custom/AiExtensions.kt` |
| Next.js | `web/src/app/components/AiExtensions.tsx` |

A nonempty HTML prototype is required. Platforms without saved generated code keep their starter files. Export does not generate missing translations, execute downloaded code, install dependencies, or build an app. Open the resulting folder with your development tools or the local Glowbom OSS workflow to continue.

Without `--output`, the command creates a unique `glowbom-export-*` folder in the current directory. With `--output`, choose a new destination whose parent already exists. Existing files, directories, and symlinks are never replaced, including empty directories. This is a manual export, so later cloud changes and local edits are not synchronized.

Use `pull` when you only need the saved generation files, `template` for a clean starter without an account, and `export` for the combined platform folders. Both `pull` and `export` require login and use the saved cloud version; they cannot retrieve unsaved browser changes.

Build the updated CLI source before trying this new command, or run `go run . export --output ~/Downloads/my-exported-project` from this directory. `go run . export --help` shows the options. Export reuses the existing `GET /project` API and public starter URL, so it requires no Firebase Functions or Cloudflare Worker redeployment. The command is available in source; a packaged release remains pending.

### How export assembles the folder

1. Check the saved CLI login and the destination. Refresh the account token when needed, then request the saved generation ZIP from `GET /project`.
2. Validate that bundle, then download the public starter. The starter request receives no account token or cookies.
3. Combine both archives locally in memory. Read `glowbom.json` to find the saved files by format, keep the originals at their saved paths, and place copies in the platform locations above. Prefer nonempty processed HTML, falling back to raw HTML.
4. Prepare the platform copies: add `import SwiftUI`, the Kotlin `com.glowbom.custom` package, or the Next.js `"use client"` directive when missing. Change supported `enabled = false` declarations to `true`. Original exported source files stay unchanged.
5. Copy a saved icon to `icon.png`, `web/src/app/icon.png`, and `web/public/icon.png`. Merge the saved manifest with the starter's platform targets and set its prototype and instructions paths. Remove the starter's sample prototype assets and dates that were not supplied by the saved project.
6. Validate the combined paths and sizes, then write one new project folder. The command rejects unsafe or conflicting paths, preserves the Android Gradle launcher's executable permission, and cleans up its incomplete output on failure or cancellation.

The result is an extracted folder. It does not leave two ZIP downloads to combine by hand. Available generated code is installed, but each platform still needs its normal dependencies, configuration, and build checks.

### Implementation files

| File | Responsibility |
| --- | --- |
| [main.go](main.go) | Register `export` and show it in CLI help. |
| [export.go](export.go) | Parse options, coordinate login and downloads, assemble the project, and report the destination. |
| [export_archive.go](export_archive.go) | Place generated files, prepare platform copies, merge the manifest, and validate the combined archive entries. |
| [pull.go](pull.go) | Share the existing authenticated project download and token refresh behavior with `pull`. |
| [template.go](template.go) | Reuse the public starter download and ZIP validation. |
| [pull_archive.go](pull_archive.go) | Reuse saved-bundle validation and extraction that protects existing local files. |
| [export_test.go](export_test.go) and [export_archive_test.go](export_archive_test.go) | Check command behavior, authentication, cancellation, file placement, and invalid archives. |

The implementation adds `export.go`, `export_archive.go`, and their tests, and updates `main.go` and `pull.go`. It reuses `template.go` and `pull_archive.go` without changing them for this command. Tests use synthetic archives and local test servers, with no hosted account required.

## Generate an image

Sign in, then describe an image:

```sh
glowbom generate-image "A friendly robot watering a garden"
glowbom generate-image "A watercolor version of this character" --ref character.png --output watercolor.png
glowbom generate-image "Combine this character and setting" --ref character.png --ref garden.jpg --output ./assets/
```

The command uses your Glowbom account allowance and downloads the result to the current directory with a unique filename. `--output` selects a new filename or an existing directory. It does not overwrite existing files or create missing directories. The terminal prints the saved path and the generation cost returned by the API, or says when accounting is still pending.

Repeat `--ref` to supply multiple local image files or HTTPS image URLs. References are read or downloaded and sent as image data. They are not uploaded to your saved Glowbom project. The default Flux fast model accepts up to five references; `--quality high` and `--source nano-banana` accept up to eight. References must fit within the API's combined seven-million-character data limit. Flux also limits each reference to four megapixels, with a combined nine-megapixel limit for high quality.

```sh
glowbom generate-image "Match the colors and composition" --ref palette.png --ref layout.jpg --quality high
glowbom generate-image "An illustrated scene with these objects" --ref object.png --source nano-banana
glowbom generate-image "A mountain landscape" --format webp --output mountains.webp
```

`--format` accepts `png`, `jpg`, or `webp` and defaults to PNG. The API may change the format for transparency or the selected model. Automatically named files use the actual returned format. An explicit output filename is kept as given; its extension does not convert the image. `--quality` selects the Flux reference model and has no effect on text-only Flux generation or Nano Banana.

PNG, JPEG, GIF, and WebP references are supported. Flux fast requires WebP files with an extended header that the API can measure; use PNG, JPEG, or high quality when the CLI reports an unsupported WebP reference. Invalid local inputs and output paths are checked before generation. Generated downloads are limited to 25 MiB. Downloads never receive your account token. An interrupted or failed generation is not automatically retried because it may already have used allowance.

From this source directory, use `go run . generate-image ...` in place of `glowbom generate-image ...`. Run `glowbom generate-image --help` for all options. Login, token refresh, account lookup, logout, and image generation with references using both Flux and Nano Banana have passed user-run production smoke tests. Automated tests use simulated API and image servers without spending account allowance.

## Storage and configuration

Credentials use the system keyring by default. On headless macOS or Linux, explicitly set `GLOWBOM_CREDENTIAL_STORE=file` to use a private file instead. There is no automatic fallback. Use the same setting for login, account, and logout.

The file store uses the operating system's user configuration directory, under `glowbom/accounts`, with directory mode 0700 and file mode 0600. `GLOWBOM_CONFIG_DIR` can select a different absolute directory. Keep it outside projects and source control. Never share credential files.

`GLOWBOM_ACCOUNT_API_URL` and `GLOWBOM_LOGIN_URL` override the hosted API and browser sign-in addresses. Only use endpoints you trust: they receive your account credentials. HTTPS is required. For a local test server, set `GLOWBOM_AUTH_LOCAL=1` and set both addresses to HTTP loopback URLs. Credentials for different API addresses are stored separately.

## Build and test

From this directory:

```sh
go build -o glowbom .
go test ./...
```

Tests use local HTTP servers and temporary credential files. They do not access a hosted Glowbom account. Real keyring access and production browser providers need manual release testing.
