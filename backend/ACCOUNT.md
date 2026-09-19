<!-- Created by Codex under Jacob's direction, Glowbom Labs. -->

# Local Glowbom account bridge

This optional bridge lets a local client reuse the Glowbom CLI account. It does
not change the account requirements for the rest of OSS, OpenCode authentication,
Buzz identity, or generation billing.

Build or install the updated CLI and restart the updated backend. `glowbom start`
passes its own executable path as `GLOWBOM_CLI_BIN`. When starting the backend
manually, set that variable to an absolute path or put the updated `glowbom` on
PATH. The CLI and backend keep using the same account API configuration and
credential store. There are no new dependencies or hosted deployments.

All endpoints require the local backend bearer token, even when other development
routes have authentication disabled. The normal origin checks also apply. Responses
are marked `Cache-Control: no-store`. Account ID and refresh tokens remain in the
CLI credential store; the bridge never returns them, CLI stderr, or login output.

| Endpoint | Behavior |
| --- | --- |
| `GET /account/status` | Runs `glowbom account --json`, validates version 1, and returns only the allowed status fields. |
| `POST /account/login` | Starts `glowbom login` in the background; returns 202 with `state: pending`. Repeated requests during login join the same attempt. |
| `GET /account/login` | Returns `idle`, `pending`, `complete`, `failed`, or `canceled`. Completion means credentials were saved; read account status next. |
| `POST /account/login/cancel` | Requests cancellation of the active login. Poll until it settles before another credential operation. |
| `POST /account/logout` | Runs `glowbom logout`; returns `signed_out` only on success. This affects the shared local CLI account. |

Login opens the browser on the computer running the backend. It is intended for
a local desktop. On a remote machine, use the CLI's existing device login flow
instead. The bridge accepts no executable, command arguments, credential paths,
or arbitrary URLs from requests. It launches a fixed CLI command without a shell.

The bridge serializes its credential operations. Status or logout during login
returns 409 with `account_busy`; login itself is bounded to just over five minutes.
Status commands have a 25-second timeout and bounded output. Missing or old CLIs
produce `cli_unavailable` or `cli_update_required`; unknown output does not grant
access. CLI network or credential-store failures remain `unavailable`, distinct
from a confirmed `signed_out` result. The bridge does not cache account results.

Subscription categories are returned unchanged. A consumer must explicitly map
them to its content rules; neither remaining credits nor a browser login alone
proves Premium access. This bridge is not authorization for hosted purchases or
other paid services.

Run the backend tests with `go test ./...`, and concurrency checks with
`go test -race -run TestAccount ./...`. Tests use injected CLI runners and synthetic
responses; they never launch a real account login or read real credentials.
