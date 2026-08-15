# Workspace Instructions

## Public Repository Boundary

The complete oss folder is copied into the separate public Glowbom OSS repository.

- Keep this folder self-contained and independently buildable.
- Do not depend on files from the parent repository.
- Do not include private plans, production configuration, customer data, or internal-only assets.
- Keep public setup instructions, licenses, security guidance, and contributor information inside this folder.
- Use relative paths that continue to work when oss becomes the public repository root.
- Before a public sync, check tracked and untracked files for credentials, local logs, generated media, and build output.

## Commit Messages
- After making code changes, always suggest a one-line conventional commit message.
- Prefer the format `type: summary`.
