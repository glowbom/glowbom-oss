# Check a public source copy

Run this with Python 3.9 or newer from the Glowbom OSS repository root before
publishing a source update:

```sh
python3 -B scripts/audit-publication.py
python3 -B scripts/test_audit_publication.py
```

To inspect another source directory, add `--root /absolute/path/to/candidate`.
Add `--json` for counts and findings that another script can read. Exit code 0
means no findings in the checked scope, 1 means findings need review, and 2 means
the root directory could not be read. The command reads files only. It does not
use Git, contact a service, install dependencies, change files, or publish.

Findings contain a relative file path, line number, and type. Matched credential
values and file contents are never printed. A JSON structure finding uses line
1 because it describes the parsed document. A Google API key finding requests
review; some Firebase client keys are public identifiers, so a match alone does
not establish that a credential is private or active.

The checks cover private key markers, recognizable provider and account token
formats, service-account JSON with key material, and Glowbom CLI credential-store
JSON under any filename. They also report sensitive filenames, local logs,
generated media directories, account/project state, compiled CLI/backend files,
and unsafe or uninspected symlinks. They do not skip test source: only explicit
fixture/example placeholders and runs of placeholder characters are exempt.
Public OAuth client IDs are not treated as credentials. The starter project's
empty `project/.glowbom/previews.json` is allowed; populated local state is not.
The known reference file `backend/node/local.ts` and private source directories
named `desktop`, `flutter`, `swiftui`, `site`, or `cloudflare` at the repository
root are rejected. The corresponding names inside portable templates remain
allowed. These explicit checks cannot identify every private feature by content.

The audit skips `.git`, installed dependencies, virtual environments, known
cache/build trees, and known binary media. It reports oversized text candidates
instead of silently reading an unbounded file. It cannot detect every secret,
encoded data, personal information, or private feature accidentally added to a
permitted source file. It does not inspect repository history or prove that a
deployed API enforces authentication. A clean result is one check, not a blanket
publication approval. Review the actual files being published, keep dependencies
and generated output out of the copy, and preserve server authorization checks.

The scanner's standard-library API is `scan(root: Path)`, returning a sorted list
of `Finding(path, line, kind)` objects and a dictionary of scan counts. It can be
used after preparing a filtered source export; any findings should stop that
export from being treated as ready to publish.
