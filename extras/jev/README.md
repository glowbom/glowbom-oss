# Glowbom OSS Jev integration bundle

This archive contains repo-ready files for adding Jev as an OpenCode custom tool in Glowbom OSS.

Copy the contents of this archive into the root of `glowbom/glowbom-oss`.

Included:

- `.opencode/tools/jev.ts`
- `scripts/install-jev-tool.sh`
- `scripts/test-jev.mjs`
- `docs/jev/README.md`
- `docs/jev/README-SNIPPET.md`

After copying the files, run:

```bash
./scripts/install-jev-tool.sh
```

Then restart Glowbom OSS with `glowbom start`.

`docs/jev/README-SNIPPET.md` is a short section you can paste into the main project README.
