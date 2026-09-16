# CLI installer follow-up

Status: proposed improvements, not implemented yet.

Keep the public installation flow to one command. The current installer defaults
to `/usr/local/bin`, so the documentation uses `sudo sh` to write there.

## Next improvement

Make `install.sh` handle installation location and PATH guidance itself:

- Prefer a writable location without administrator access when practical.
- Preserve `GLOWBOM_INSTALL_DIR` and the existing compatibility variables.
- Create the destination directory and explain where the binary was installed.
- Detect whether the destination is on PATH. Show the exact next step for the
  user's shell when needed; do not silently rewrite shell profiles.
- Use elevated permissions only for installation steps that require them,
  rather than running the entire download and extraction process as root.
- Keep dependency and repository requirements clear. Installing the CLI alone
  does not install Go, Bun, OpenCode, or a Glowbom OSS checkout.

Before changing the recommended command, verify fresh installs and upgrades on
macOS, Linux, and WSL, including paths with spaces, unwritable destinations, and
missing PATH entries. Verify that download failures leave an existing binary
intact. Update the repository README and portal install instructions together.

The public portal source is in `../docs/content/docs/glowbom-oss.mdx`.
