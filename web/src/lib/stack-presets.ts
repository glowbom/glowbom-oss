import ossPackage from '../../package.json';

export const STACK_CATEGORIES = [
  { id: 'web', name: 'Web' },
  { id: 'apps', name: 'Mobile & desktop' },
  { id: 'games', name: 'Games & simulations' },
  { id: 'services', name: 'Backends & tools' },
] as const;
export type StackCategory = typeof STACK_CATEGORIES[number]['id'];

export interface StackPreset {
  id: string;
  name: string;
  directory: string;
  description: string;
  category: StackCategory;
  summary: string;
  preview: string;
  requirements: string;
  previewMode?: 'auto' | 'command' | 'none';
  command?: string[];
  previewNotes?: string;
}

// Keep the web preset aligned with the app's actual dependency declarations.
const reactDescription = `Build with the same frontend stack as Glowbom OSS:
- React ${ossPackage.dependencies.react} and React DOM ${ossPackage.dependencies['react-dom']}.
- TypeScript ${ossPackage.devDependencies.typescript} with strict checking, ES2022, bundler module resolution, and react-jsx.
- Vite ${ossPackage.devDependencies.vite} with @vitejs/plugin-react ${ossPackage.devDependencies['@vitejs/plugin-react']}.
- Plain CSS, small typed components, and React hooks. Add a router or UI library only when the app needs it or the user requests it.
- Use Bun for new projects, with dev, typecheck, build, and test scripts. Build should run tsc --noEmit and vite build.
- If Markdown rendering is needed, use markdown-it ${ossPackage.dependencies['markdown-it']} with @types/markdown-it ${ossPackage.devDependencies['@types/markdown-it']}.
Match the existing prototype's appearance and behavior when one exists. Keep the app self-contained and preserve an existing app's package manager, data formats, and working dependencies. Use the project identity rather than copying Glowbom's name or backend settings.`;

export const STACK_PRESETS: StackPreset[] = [
  { category: 'web', summary: 'A flexible web app using the same frontend stack as Glowbom OSS.', preview: 'Browser preview', requirements: 'Bun', id: 'react-vite', name: 'React + Vite', directory: 'react', description: reactDescription },
  {
    category: 'apps', summary: 'A desktop app with a React web interface.', preview: 'Web interface preview', requirements: 'Bun; Rust and platform tools for native builds', previewNotes: 'Preview shows the web interface only. Test native features and packaging separately with the project’s tauri script.', id: 'tauri-react', name: 'Tauri + React', directory: 'desktop',
    description: `${reactDescription}

Add a Tauri 2 desktop wrapper around the React/Vite app, following the same web-and-desktop pattern as Glowbom's small internal apps:
- Put the Rust wrapper and Tauri configuration in src-tauri/. Keep app logic in typed modules that can be tested independently of the UI.
- Use @tauri-apps/api and @tauri-apps/cli v2. Add the dialog and filesystem plugins only when native file access is needed, with narrowly scoped capabilities.
- Isolate native integration in a module such as src/lib/desktop.ts. Guard native calls with isTauri() and provide browser file-input, download, and fullscreen fallbacks.
- Use Lucide React for icons and Vitest for behavior tests when needed.
- Keep user files local unless an external service is explicitly part of the app.
- Provide a tauri script, beforeDevCommand, beforeBuildCommand, devUrl, and frontendDist pointing to ../dist. Make the native devUrl match the Vite development port, bind to loopback, and ignore src-tauri in Vite's watcher.
- The Glowbom browser preview runs only the Vite frontend on an assigned port. Native dialogs, Rust commands, permissions, and installers must be checked separately with tauri dev/build.
- Document the Rust and platform prerequisites and the commands for running, testing, and packaging. Report native checks that cannot run; do not claim browser testing verifies desktop behavior.`,
  },
  {
    id: "html",
    name: "HTML + CSS",
    directory: "website",
    category: "web",
    summary: "A simple website without a framework.",
    preview: "Browser preview",
    requirements: "No additional runtime",
    description: `Create index.html with semantic HTML, responsive CSS, and JavaScript modules only where needed. Use
local assets and accessible controls. Match the prototype where one exists. Keep the app self-
contained. Preserve existing dependencies, package manager, and data formats when refining an
existing app. Document setup and run behavior tests and build checks; report unavailable checks. Do
not claim a browser preview verifies native behavior.`,
    previewMode: "auto",
    previewNotes: "",
  },
  {
    id: "nextjs",
    name: "Next.js",
    directory: "next-app",
    category: "web",
    summary: "A React framework with routing and server features.",
    preview: "Browser preview",
    requirements: "Bun",
    description: `Build a TypeScript Next.js app using the App Router and a compatible stable React version. Use
server components where appropriate and client components for interactivity. Include dev, build, and
test scripts. Keep secrets on the server. For local development only, allow embedding from
http://127.0.0.1:4572 and http://localhost:4572; preserve production frame restrictions. Honor the
assigned loopback host and port. Keep the app self-contained. Preserve existing dependencies,
package manager, and data formats when refining an existing app. Document setup and run behavior
tests and build checks; report unavailable checks. Do not claim a browser preview verifies native
behavior.`,
    previewMode: "auto",
    previewNotes: "",
  },
  {
    id: "php",
    name: "PHP",
    directory: "php",
    category: "web",
    summary: "A server-rendered website using plain PHP.",
    preview: "Browser preview",
    requirements: "PHP",
    description: `Build with plain PHP and no framework unless requested. Put the entry point at public/index.php,
static assets under public/assets, and application code outside public. Use sessions, CSRF
protection for mutations, escaped output, and server-side validation. Include PHP lint checks. For
local development only, allow embedding from http://127.0.0.1:4572 and http://localhost:4572;
preserve production frame restrictions. Honor the assigned loopback host and port. Keep the app
self-contained. Preserve existing dependencies, package manager, and data formats when refining an
existing app. Document setup and run behavior tests and build checks; report unavailable checks. Do
not claim a browser preview verifies native behavior.`,
    previewMode: "command",
    previewNotes: "Requires PHP. The preset serves public/ and includes local embedding instructions. Existing apps may need a different document root.",
    command: ["php", "-S", "{host}:{port}", "-t", "public"],
  },
  {
    id: "expo",
    name: "React Native + Expo",
    directory: "expo",
    category: "apps",
    summary: "An app for iOS, Android, and the web.",
    preview: "Web interface preview",
    requirements: "Node.js and npm; Expo-compatible native tools",
    description: `Build with React Native, TypeScript, Expo, and Expo Router. Use the stable Expo SDK and its
compatible React and React Native versions; use expo install for SDK dependencies rather than
choosing versions independently. Include react-native-web and the SDK-compatible web dependencies.
Provide browser alternatives for native-only APIs. Add tools/preview.mjs accepting --host and
--port, validating a loopback host, and spawning the installed Expo CLI with start --web --localhost
--port, EXPO_PACKAGER_PROXY_URL set to the supplied http host and port, and CI=1. Forward
termination signals and do not open a browser automatically. Use npm install for a new project.
Document device/simulator setup and web export checks. For local development only, allow embedding
from http://127.0.0.1:4572 and http://localhost:4572; preserve production frame restrictions. Honor
the assigned loopback host and port. Keep the app self-contained. Preserve existing dependencies,
package manager, and data formats when refining an existing app. Document setup and run behavior
tests and build checks; report unavailable checks. Do not claim a browser preview verifies native
behavior.`,
    previewMode: "command",
    previewNotes: "Build creates tools/preview.mjs. Install dependencies with npm install. Preview covers web-compatible features; use a device or simulator for native features.",
    command: ["node", "tools/preview.mjs", "--host", "{host}", "--port", "{port}"],
  },
  {
    id: "flutter",
    name: "Flutter",
    directory: "flutter-app",
    category: "apps",
    summary: "One Dart app for web, mobile, and desktop.",
    preview: "Web interface preview",
    requirements: "Flutter SDK",
    description: `Build a Flutter app with Dart, including web support. Use web-compatible packages and conditional
imports for native integrations. Provide responsive layouts and accessible widgets. Include widget
tests, flutter analyze, and flutter build web. Document flutter pub get and platform setup. Browser
preview uses flutter run -d web-server. For local development only, allow embedding from
http://127.0.0.1:4572 and http://localhost:4572; preserve production frame restrictions. Honor the
assigned loopback host and port. Keep the app self-contained. Preserve existing dependencies,
package manager, and data formats when refining an existing app. Document setup and run behavior
tests and build checks; report unavailable checks. Do not claim a browser preview verifies native
behavior.`,
    previewMode: "command",
    previewNotes: "Run flutter pub get first. Native-only plugins need platform testing. Some web changes require a hot restart or restarting the preview.",
    command: ["flutter", "run", "-d", "web-server", "--web-hostname", "{host}", "--web-port", "{port}"],
  },
  {
    id: "swiftui",
    name: "SwiftUI",
    directory: "apple-app",
    category: "apps",
    summary: "A native Apple app.",
    preview: "Native tools",
    requirements: "Xcode on macOS",
    description: `Build a SwiftUI app with a valid Xcode project. Match the requested Apple destinations, preserve app
identity, and include accessibility and focused tests. Document Xcode run and build commands. Keep
the app self-contained. Preserve existing dependencies, package manager, and data formats when
refining an existing app. Document setup and run behavior tests and build checks; report unavailable
checks. Do not claim a browser preview verifies native behavior.`,
    previewMode: "none",
    previewNotes: "Open the generated Xcode project to run on an Apple device or simulator. No browser preview.",
  },
  {
    id: "kotlin",
    name: "Kotlin + Compose",
    directory: "android-app",
    category: "apps",
    summary: "A native Android app.",
    preview: "Native tools",
    requirements: "Android Studio and Android SDK",
    description: `Build a Kotlin Android app using Jetpack Compose, a compatible Gradle setup, and the Gradle wrapper.
Preserve package identity and include accessibility and focused tests. Document emulator/device
setup and build commands. Keep the app self-contained. Preserve existing dependencies, package
manager, and data formats when refining an existing app. Document setup and run behavior tests and
build checks; report unavailable checks. Do not claim a browser preview verifies native behavior.`,
    previewMode: "none",
    previewNotes: "Open the generated project in Android Studio. No browser preview.",
  },
  {
    id: "godot-2d",
    name: "Godot 2D",
    directory: "godot-2d",
    category: "games",
    summary: "A 2D game or interactive simulation.",
    preview: "Web export preview",
    requirements: "Godot 4, matching export templates, Python 3",
    description: `Build a Godot 4 2D project with GDScript, project.godot, appropriate scenes, cameras, and input
controls. Use the Compatibility renderer and single-threaded web export. Include a Web preset in
export_presets.cfg and document installing matching Godot export templates. Add tools/preview.py
accepting --host and --port. It should run Godot headless export to build/web/index.html, fail
clearly on export errors, and serve only build/web on the supplied loopback address. Support .wasm
MIME types. Re-export on source changes with debouncing, excluding .godot and build output, without
overlapping exports. Clean up subprocesses on shutdown. Keep gameplay separate from UI and document
editor/native testing. For local development only, allow embedding from http://127.0.0.1:4572 and
http://localhost:4572; preserve production frame restrictions. Honor the assigned loopback host and
port. Keep the app self-contained. Preserve existing dependencies, package manager, and data formats
when refining an existing app. Document setup and run behavior tests and build checks; report
unavailable checks. Do not claim a browser preview verifies native behavior.`,
    previewMode: "command",
    previewNotes: "Build creates an export-and-serve script. Install Godot and matching export templates first. Preview updates require an export; native graphics/features need editor testing.",
    command: ["python3", "tools/preview.py", "--host", "{host}", "--port", "{port}"],
  },
  {
    id: "godot-3d",
    name: "Godot 3D",
    directory: "godot-3d",
    category: "games",
    summary: "A 3D game or interactive simulation.",
    preview: "Web export preview",
    requirements: "Godot 4, matching export templates, Python 3",
    description: `Build a Godot 4 3D project with GDScript, project.godot, appropriate scenes, cameras, and input
controls. Use the Compatibility renderer and single-threaded web export. Include a Web preset in
export_presets.cfg and document installing matching Godot export templates. Add tools/preview.py
accepting --host and --port. It should run Godot headless export to build/web/index.html, fail
clearly on export errors, and serve only build/web on the supplied loopback address. Support .wasm
MIME types. Re-export on source changes with debouncing, excluding .godot and build output, without
overlapping exports. Clean up subprocesses on shutdown. Keep gameplay separate from UI and document
editor/native testing. For local development only, allow embedding from http://127.0.0.1:4572 and
http://localhost:4572; preserve production frame restrictions. Honor the assigned loopback host and
port. Keep the app self-contained. Preserve existing dependencies, package manager, and data formats
when refining an existing app. Document setup and run behavior tests and build checks; report
unavailable checks. Do not claim a browser preview verifies native behavior.`,
    previewMode: "command",
    previewNotes: "Build creates an export-and-serve script. Install Godot and matching export templates first. Preview updates require an export; native graphics/features need editor testing.",
    command: ["python3", "tools/preview.py", "--host", "{host}", "--port", "{port}"],
  },
  {
    id: "fastapi",
    name: "Python + FastAPI",
    directory: "api",
    category: "services",
    summary: "A Python API with interactive documentation.",
    preview: "API preview",
    requirements: "Python 3 and a project virtual environment",
    description: `Build a FastAPI service in main.py with an app object. Redirect GET / to /docs so preview opens
interactive API documentation. Use typed request/response models and explicit validation. Add a
requirements.txt, setup instructions for a .venv environment, and pytest tests. Do not expose
secrets in API responses. Start with .venv/bin/python -m uvicorn main:app --host and --port. For
local development only, allow embedding from http://127.0.0.1:4572 and http://localhost:4572;
preserve production frame restrictions. Honor the assigned loopback host and port. Keep the app
self-contained. Preserve existing dependencies, package manager, and data formats when refining an
existing app. Document setup and run behavior tests and build checks; report unavailable checks. Do
not claim a browser preview verifies native behavior.`,
    previewMode: "command",
    previewNotes: "Create .venv and install requirements first. The command uses the macOS/Linux virtual environment path; adjust for Windows. Preview shows API documentation.",
    command: ["./.venv/bin/python", "-m", "uvicorn", "main:app", "--host", "{host}", "--port", "{port}"],
  },
  {
    id: "go-http",
    name: "Go HTTP service",
    directory: "go-service",
    category: "services",
    summary: "A small Go web service or backend.",
    preview: "Browser or API preview",
    requirements: "Go",
    description: `Build a Go module with an HTTP server at the module root, using net/http. Accept --host and --port
flags and bind exactly to their values. Provide a useful HTML status/API page at / for browser
preview. Include graceful shutdown, request validation, timeouts, and go test coverage. Document go
run and go build. For local development only, allow embedding from http://127.0.0.1:4572 and
http://localhost:4572; preserve production frame restrictions. Honor the assigned loopback host and
port. Keep the app self-contained. Preserve existing dependencies, package manager, and data formats
when refining an existing app. Document setup and run behavior tests and build checks; report
unavailable checks. Do not claim a browser preview verifies native behavior.`,
    previewMode: "command",
    previewNotes: "The generated service accepts --host and --port. Preview shows its web interface or API status page; restart preview after code changes.",
    command: ["go", "run", ".", "--host", "{host}", "--port", "{port}"],
  },
  {
    id: "cli",
    name: "Command-line tool",
    directory: "cli",
    category: "services",
    summary: "A terminal program in the language you choose.",
    preview: "Terminal only",
    requirements: "Depends on the chosen language",
    description: `Build a command-line tool in the language requested by the user; use Python 3 if unspecified.
Provide --help, useful exit codes, argument validation, stdout/stderr separation, focused tests, and
documented installation and usage. Avoid destructive defaults. Do not add a web server just to
provide a preview. Keep the app self-contained. Preserve existing dependencies, package manager, and
data formats when refining an existing app. Document setup and run behavior tests and build checks;
report unavailable checks. Do not claim a browser preview verifies native behavior.`,
    previewMode: "none",
    previewNotes: "Open this folder in your system terminal and use the commands in its README. No browser preview.",
  },
];

export function nextStackDirectory(base: string, directories: string[]): string {
  const used = new Set(directories.map((path) => path.replace(/^\.\//, '').replace(/\/$/, '')));
  if (!used.has(base)) return base;
  let suffix = 2;
  while (used.has(`${base}-${suffix}`)) suffix += 1;
  return `${base}-${suffix}`;
}
