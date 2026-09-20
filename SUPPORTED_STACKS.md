# Supported stacks

Glowbom OSS offers these 14 stack presets. Each saves build instructions and preview settings for your coding agent.

**Standard project** means the stack already has a folder in the bundled [Glowbom starter](project/). Choose **Add stack** for the others.

| Category | Stack | Why we support it | Standard project | Preview |
| --- | --- | --- | --- | --- |
| Web | HTML + CSS | Simple websites and a quick visual reference for other versions of your app. | Yes: `prototype/` | Browser |
| Web | Next.js | React apps with routing and server features built in. | Yes: `web/` | Browser |
| Web | React + Vite | Flexible interactive web apps using the same frontend stack as Glowbom OSS. | No | Browser |
| Web | PHP | Straightforward websites that render pages on the server. | No | Browser |
| Mobile & desktop | SwiftUI | Native Apple apps using Apple's interface tools. | Yes: `apple/` | Xcode/device/simulator |
| Mobile & desktop | Kotlin + Compose | Native Android apps using Android's interface tools. | Yes: `android/` | Android Studio/device/emulator |
| Mobile & desktop | React Native + Expo | React-based mobile apps; Expo provides a shared setup for development, routing, and compatible packages. | No | Web interface |
| Mobile & desktop | Flutter | One Dart codebase for web, mobile, and desktop. | No | Web interface |
| Mobile & desktop | Tauri + React | Desktop apps with a React interface and access to native features. | No | Web interface |
| Games & simulations | Godot 2D | Games and simulations built around 2D scenes and interaction. | No | Web export |
| Games & simulations | Godot 3D | Games and simulations with 3D scenes, cameras, and movement. | No | Web export |
| Backends & tools | Python + FastAPI | Python services with input validation and interactive API documentation. | No | API documentation |
| Backends & tools | Go HTTP service | Small web services using Go's standard HTTP library. | No | Web/API page |
| Backends & tools | Command-line tool | Automation and utilities that belong in a terminal. Choose a language; Python is the default. | No | Terminal |

Presets guide generation; required tools and dependencies still need to be installed. Browser previews cover web-compatible features, not native behavior. Godot needs a web export and matching export templates.

## Why these standard folders?

The starter keeps **HTML as the design reference**, **Next.js as the web app**, and **SwiftUI and Kotlin + Compose as native destinations**. This keeps existing Glowbom exports compatible. You can also add a separate HTML website without replacing the prototype.

The web starter includes React, TypeScript, Tailwind, and reusable UI components. The project also includes a shared icon, `glowbom.json` for project identity and destinations, and `AGENTS.md` with instructions to match the prototype and check builds.

## Any custom stack

You can add **any custom stack** through **Add stack → Describe your own**. Specify its language, framework, libraries, folder, and optional preview command. This lets you keep your preferred tools or work on an existing app beyond the preset catalog.

Glowbom saves those choices with the project. A custom stack needs the appropriate local tools and a coding agent that can build it; a browser preview is optional.

See [stack setup and examples](STACKS.md) for instructions.
