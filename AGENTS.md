# Glowbom OSS instructions

## Public source boundary

This tree supplies the separate public Glowbom OSS repository. Keep the backend,
CLI, minimal Agent + Buzz browser interface, project templates, extras, and
API documentation self-contained and independently buildable.

- Do not import files from the parent repository or the private Desktop client.
- The full Chat, Draw, Studio, Book, and onboarding interfaces and the Tauri shell
  belong outside this public tree. Open backend features and portable project
  records remain available to clients through their existing APIs.
- Preserve the public Agent and Buzz layout and interactions unless the user
  explicitly approves a change.
- Keep public setup instructions, licenses, security guidance, and contributor
  information here. Relative paths must work when this becomes the repository root.
- Do not include credentials, production configuration, customer data, private
  plans, local logs, generated media, installed dependencies, or release output.
- Review the selected source and export inventory before a public sync. A local
  build is not a public release, and filename exclusions cannot prove a file is safe.

## Checks

Run focused Go checks from backend or cli. From web, run bun run typecheck and
bun run build. The CLI source entry remains go run . start from cli.
Do not require any private client files to run those commands.

## Commits

After changes, suggest one Conventional Commit subject. Do not run Git commands
or publish unless the user explicitly requests those actions.
