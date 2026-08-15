# Workspace Instructions

## Bundled Project Template

When working in `project/`, use `project/prototype/index.html` as the reference design.

### Primary Goal

All destination projects in `project/` must look and function exactly like the prototype. This includes:
- Identical visual appearance: colors, fonts, spacing, and layout
- Same functionality and user interactions
- All prototype images included and displayed correctly
- App icon matching `project/icon.png`

### App Metadata

- Display Name: `Glowbom`
- Bundle ID: `app.glowbom.glowby`
- Version: `1.0`
- Build Number: `1`

Update these values in each platform output:

#### iOS
- Display Name: Xcode target -> General -> Display Name, or `CFBundleDisplayName` in `Info.plist`
- Bundle ID: Xcode target -> Signing & Capabilities -> Bundle Identifier
- Version: Xcode target -> General -> Version, or `CFBundleShortVersionString` in `Info.plist`
- Build: Xcode target -> General -> Build, or `CFBundleVersion` in `Info.plist`

#### Android
- Display Name: `project/android/app/src/main/res/values/strings.xml` -> `app_name`
- Bundle ID: `project/android/app/build.gradle` -> `applicationId`
- Version: `project/android/app/build.gradle` -> `versionName`
- Build: `project/android/app/build.gradle` -> `versionCode`

#### Games
- Display Name: update in the engine project settings
- Bundle ID: update in the engine export settings
- Version: update in the engine project/export settings

### Default Tasks

#### 0. Production Readiness
- Review the prototype and ensure all UI elements are properly implemented
- Verify colors, typography, and spacing match the prototype
- Check that user interactions work as expected
- Full screen layout: the app or game should fill the screen with no unnecessary empty white space
- Sprite transparency for games: sprites must use transparent backgrounds

#### 1. Copy Images from Prototype
- Copy all media assets from `project/prototype/assets/` to platform-specific asset directories
- Reference `project/prototype/assets.json` for asset metadata and original prompts
- Ensure image and video formats are compatible with each platform

#### 2. Copy and Resize App Icon
- Source icon: `project/icon.png`
- Resize and place icons in the correct locations for each platform

#### Apple (SwiftUI)
- Single SwiftUI project under `project/apple/` targets iOS, iPadOS, macOS, visionOS, watchOS, and tvOS
- Update the SwiftUI extension file such as `project/apple/Custom/AiExtensions.swift`
- Copy images into the asset catalog under `project/apple/Custom/Assets.xcassets/`
- Update `Contents.json` in the AppIcon asset with the correct filenames

#### Android (Jetpack Compose)
- Update the Android extension file such as `AiExtensions.kt`
- Copy images into Android drawable resources
- Name icon files `ic_launcher.png` and `ic_launcher_round.png`

#### Web (Next.js)
- Place the app icon in `project/web/src/app/icon.png`
- Also copy the icon to `project/web/public/icon.png` for static references
- Keep `project/web/src/app/favicon.ico` consistent if it exists
- Ensure web metadata and title reflect the app identity

#### 3. Verify No Compile Errors
- Build each platform project and fix any compilation errors
- For Apple: open the destination project in Xcode and build
- For Android: open the destination in Android Studio and sync/build
- For games: open the project and run the main scene

### Target Destinations

iOS (SwiftUI), Android (Jetpack Compose), Web (Next.js)

### Reference Files

- Prototype HTML: `project/prototype/index.html`
- Assets: `project/prototype/assets/`
- Asset metadata: `project/prototype/assets.json`
- App Icon: `project/icon.png`
- Project Manifest: `project/glowbom.json`

### Handling Files Downloaded from Glowbom.com

Users can build a prototype on the Glowbom desktop app and then download files to feed into this project. They may attach some or all of the following when running a build:

#### File Types the User May Attach

| File pattern | What it is |
|---|---|
| `*.zip` | Glowbom default project template (this boilerplate) as an archive |
| `AiExtensions.swift` | iOS/SwiftUI AI extension — generated UI code for `apple/Custom/AiExtensions.swift` |
| `AiExtensions.kt` | Android/Kotlin AI extension — generated UI code for `android/app/src/main/java/com/glowbom/custom/AiExtensions.kt` |
| `AiExtensions.tsx` | Web/Next.js AI extension — generated UI code for `web/src/app/components/AiExtensions.tsx` |
| Larger `.html` file (contains `data:image/...;base64,`) | HTML prototype with images embedded as base64 |
| Smaller `.html` file (no base64 images) | HTML prototype with image prompts in `alt`/`title` attributes but no image data |
| `*.html` with `preview` in name | Preview variant of the prototype (typically the one with base64 images) |

Not all file types will be present every time. The user may attach just one file or several. Handle whatever is provided.

#### What To Do When Attachments Are Present

1. **Check what this project already has.** The Glowbom project structure (`glowbom.json`, `prototype/`, `apple/`, `android/`, `web/`) may already be in place. If so, work with what exists. If the project folder is empty or missing the structure and a `.zip` template is attached, unarchive it first.

2. **Place AI extension files** into their correct locations:
   - `AiExtensions.swift` → `apple/Custom/AiExtensions.swift`
   - `AiExtensions.kt` → `android/app/src/main/java/com/glowbom/custom/AiExtensions.kt`
   - `AiExtensions.tsx` → `web/src/app/components/AiExtensions.tsx`
   - Set `enabled = true` and update `title` if not already set
   - Only update platforms for which an extension file was attached

3. **Extract images from the HTML prototype.** If an attached HTML file contains base64-encoded images (`data:image/(png|jpeg|jpg|gif|webp);base64,...`):
   - Decode each base64 image and save it to `prototype/assets/image-NNN.png` (numbered sequentially: `image-001.png`, `image-002.png`, etc.)
   - Replace the data URIs in the HTML with relative paths (`assets/image-NNN.png`)
   - Save the cleaned HTML as `prototype/index.html`

4. **Extract image prompts.** If a second smaller HTML file is attached (without base64 images), or if the HTML has `alt` or `title` attributes describing images:
   - Collect the image descriptions and create or update `prototype/assets.json`:
     ```json
     {
       "assets": [
         { "filename": "image-001.png", "prompt": "description from the HTML", "sourceService": "Glowbom Images" }
       ],
       "exportedAt": "<current ISO timestamp>",
       "version": "1.0"
     }
     ```
   - If only one HTML file is attached and it has no base64 images, it is still the prototype — save it as `prototype/index.html`

5. **Copy extracted assets to platform directories** following the rules in "Copy Images from Prototype" above, then proceed with the default tasks (production readiness, icon setup, build verification).

#### Tips

- The user may provide no instructions at all — the attachments themselves are the instructions
- If the HTML references images but none are embedded, check whether they already exist in `prototype/assets/`
- Merge attachments into the existing project rather than overwriting existing work
- Be resourceful: figure out what the user intends from the combination of files they attached

### Notes

- If the user gives different instructions, follow those instead of the defaults above
- Keep the prototype as the source of truth for visual design
- Test on actual devices when possible
- For games, ensure sprite assets keep transparent backgrounds
