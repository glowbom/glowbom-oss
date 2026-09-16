# Buzz connection preview

Open **Settings > Buzz connection**. Enter an HTTPS relay URL, channel UUID,
and your Buzz identity private key (hex or nsec). For a personal identity,
leave owner-auth tag blank. Agent identities may need a NIP-OA auth tag.
Click **Connect**, then use **Refresh members** without re-entering the key.
The relay URL and channel ID are remembered in this browser across reloads
and backend restarts. Private keys, owner-auth tags, and member lists are not
saved with this setup. After a backend restart, enter the private key again.
Channel and member IDs display their first eight and last four characters.
Focus the disconnected channel field to edit its full value. Use **Copy channel
ID**, or click a member's abbreviated ID, to copy the complete identifier.
The panel shows **Connected** or **Disconnected**, and checks the shared local
status every five seconds while Settings is open and the page is visible.
If the backend cannot be reached or its response is invalid, it shows
**Status unavailable** with a **Check connection** action.

Once connected, the main actions are **Copy local backend access token** and
**Disconnect**. Copy puts the token already used by OSS onto your clipboard
without displaying it. Paste it into Glowbom Live on the same computer.
This is the local OSS access token, not your Buzz private key. It grants access
to the local backend, not just the Buzz endpoints. No extra token endpoint or
credential storage is added. Clipboard permissions must allow the copy.

Member rows show Buzz profile pictures, with initials when no supported image
is available. **Refresh members** reloads profile metadata and retries pictures.
Relay-hosted pictures use the connected identity through `buzz media get` on
the backend. Public HTTPS picture URLs load in the browser without the local
backend token or Buzz credentials. Private image bytes stay in browser memory
while the rows are mounted; closing Settings or disconnecting releases them.

The connection belongs to the local backend and is shared by OSS and the
Glowbom Live game. Closing Settings or the browser does not disconnect.
**Disconnect** cancels an outstanding lookup and drops the stored credentials
and roster. Stopping the backend also ends the session. Credentials are kept
in process memory only, never deliberately written to disk or browser storage.
This is not guaranteed secure memory erasure. Your personal key retains its
account permissions even though this connector performs only reads.

## Local setup

Use `glowbom start`, or `go run . start` from `cli/`, to configure matching
local backend and web access tokens. The backend must bind to loopback and
have `GLOWBOM_SERVER_TOKEN` configured. The existing legacy token name works.
A missing token disables the Buzz endpoints without changing other routes.

If the panel says this backend does not support Buzz connections, stop and
restart OSS with the updated code, then reload the web page. Updating the web
interface alone does not restart an older Go backend. If access is denied,
restart through the launcher and reload the page to use matching tokens.

The CLI is discovered through `GLOWBOM_BUZZ_CLI`, then `buzz` on PATH, then
`/Applications/Buzz.app/Contents/MacOS/buzz` on macOS. OSS starts normally
without Buzz installed. Message subscriptions use the Go Nostr signing library;
roster and portrait lookup still use the CLI.

## Local API

All routes require the existing backend authentication and origin checks:

- `POST /buzz/session`: connect using `relayUrl`, `channelId`, `privateKey`,
  and optional `authTag` (a string containing a JSON array).
- `GET /buzz/session`: current connection status and cached roster, without
  contacting Buzz or returning credentials.
- `POST /buzz/session/refresh`: refresh the roster using the in-memory key.
- `DELETE /buzz/session`: disconnect and cancel outstanding session work.
- `GET /buzz/session/avatar?pubkey=<member-key>`: read a connected member's
  relay-hosted profile image using the same backend token.

Responses contain `connected`, `connecting`, `relayUrl`, `channelId`, and
`members`. Each member has `pubkey`, `role`, `displayName`, and an optional
`pictureUrl` from the Buzz profile's `picture` field. Connecting
while already connected is rejected; disconnect before changing identities
or channels. A failed refresh retains the previous snapshot and connection.
The original request-scoped `POST /buzz/members` remains available.

The session permits one lookup at a time, with a 20-second deadline, 16 KiB
request limit, 2 MiB output limit per CLI invocation, and 1,000-member roster
limit. Profile queries use batches of 200. CLI credentials are supplied via
its environment, not command arguments. Other application credentials are
excluded; CLI stderr and raw errors are not exposed. Responses are no-store.

The avatar route accepts a member public key, not a caller-supplied URL. It
downloads only that member's profile picture from the connected relay's
`/media/` path. Downloads use a 15-second deadline, at most four concurrent
CLI calls, and a 2 MiB output limit. PNG, JPEG, GIF, and WebP responses are
accepted; other formats fall back to initials. Disconnect cancels outstanding
avatar work. The CLI media command supplies Blossom get authentication and
does not follow redirects. No image proxy is provided for other origins.

Membership remains a snapshot. Message capture uses a separate persistent
Buzz WebSocket subscription described below. No message posting or project
updates are implemented. Empty rosters cannot distinguish missing access from an
empty or incorrect channel.

## Using the game

Connect in OSS first, then click **Copy local backend access token** in
**Settings > Buzz connection**. Run Glowbom Live from the Godot editor and
choose the Glowbom Live experience. Paste into its **Local backend access
token** field and press **New Game**.
The game reads `http://127.0.0.1:4569/buzz/session` and chooses the smallest
4-, 6-, or 8-person office that fits. Empty and larger rosters are rejected.
Characters walk to furnished desks and show roster names. Typing is ambient
animation, not evidence that a real member is working. Restart the office to
apply a refreshed roster. Disconnecting OSS returns the game to setup within
its next status check (normally three seconds, plus any request timeout).
Glowbom Live can also show these pictures on character shirts and beside their names.

The game can inherit `GLOWBOM_SERVER_TOKEN` from its launch environment.
For manual setup, the launcher's `--show-local-auth` option displays local
credentials in your terminal; enter the backend token locally in the game.
Do not put credentials in a Project Book, screenshot, commit, or chat.

## Compatibility and verification

The command contract was checked against installed CLI help and
[Buzz source revision 3c7f288](https://github.com/block/buzz/tree/3c7f288c60d67df78577b237e27c3dfc8831aaa1/crates/buzz-cli/src).
The original member lookup was also verified by Jacob against a real channel.
The new session and game integration are tested with a fake CLI and public
fixture identities, without accessing personal credentials or changing Buzz.


## Live messages and optional speech

After connecting, the Buzz settings panel starts an authenticated WebSocket listener
for new messages in that channel. Its status is separate from roster connectivity.
Glowbom Live also starts the listener when it requests the feed. Reconnect uses
backoff; initial and recovered history are not eligible for speech. This observer
does not publish channel messages.

In **Settings > Buzz connection**, enter an **ElevenLabs API key for Live** and
choose **Use key for Live**, or use the existing ElevenLabs key when offered.
Alternatively, set `ELEVENLABS_API_KEY` in the backend environment before starting
OSS. UI-supplied keys are now saved privately on the backend computer and restored
on new connections. Clearing or changing the key turns current speech off.
See remembered-key details below for storage and environment fallback behavior.

Open Glowbom Live. **Speak new messages** starts enabled when a key is available and uses
the existing default voice and multilingual model. Enabled speech sends
message text to ElevenLabs and uses quota. The game receives MP3 audio, never the
ElevenLabs or Buzz private key. Generation writes no audio files and logs no
conversation text. The existing general-purpose `/audio` route is unchanged.

Only one Live client may enable speech at a time. Its lease expires after 15 seconds
without feed requests. Each message is attempted at most once per backend session;
failed generations are not retried. In default limited mode, the client queues up to five messages, skips
speech older than 30 seconds, and speaks at most 500 characters per message. Full
message text remains available in the feed, up to the 32 KiB message limit. Muting
stops local playback and cancels generation where possible; requests already
accepted by the provider may still use quota.

The authenticated local endpoints are `GET /buzz/session/messages` and
`POST /buzz/session/speech`. The first returns a bounded 128-message buffer, sequence
numbers, listener status, and speech availability. The second configures the
session key, claims/releases speech for a client, or generates audio for an eligible
buffered event ID. It does not accept arbitrary speech text. Disconnect cancels the
listener and speech work and clears session data. No message history is persisted.

The relay connection uses kind 9 messages scoped by the channel `h` tag, NIP-42
authentication, and optional owner authorization. Event IDs and signatures are
verified before messages enter the local feed. See the
[Buzz authentication protocol](https://github.com/block/buzz/blob/main/docs/nips/NIP-AA.md)
and [ElevenLabs speech endpoint](https://elevenlabs.io/docs/api-reference/text-to-speech/convert).

### Message mentions and replies

Each message includes two additive fields for local conversation animation:

- `mentions`: an array of public identity keys from explicit `p` tags. Keys must
  contain exactly 64 hexadecimal characters. They are lowercased and deduplicated
  in tag order. Self-mentions remain in the data; a client excludes the sender
  before counting other people. Missing or malformed identity tags add no target.
- `replyTo`: the immediate parent event ID from an `e` tag whose fourth value is
  `reply`. It is normalized to lowercase after 64-character hexadecimal
  validation. When several valid reply markers exist, the last wins, matching
  Buzz. Invalid markers do not replace an earlier valid marker.

Buzz emits a reply marker alone for direct replies, and root plus reply markers
for nested replies. A bare `e` tag or a root marker alone is not a reply. No names,
mentions, or reply links are inferred from message text. These conventions were
checked against [Buzz's message builders](https://github.com/block/buzz/blob/3c7f288c60d67df78577b237e27c3dfc8831aaa1/crates/buzz-sdk/src/builders.rs)
and [shared thread parser](https://github.com/block/buzz/blob/3c7f288c60d67df78577b237e27c3dfc8831aaa1/crates/buzz-core/src/nip10.rs).

An ordinary message has `mentions: []` and `replyTo: ""`. References are extracted
only after the existing event signature, ID, kind, channel, and time checks.
They do not prove the referenced identity is in the office or the parent event is
available. The client matches them against its current roster and conversation.
Recovered messages keep `live: false`; metadata does not make history eligible
for playback. Pending full-message speech retains the same reference fields.


## Local character preferences

Glowbom Live can edit each roster member's character and speaking voice through
its Customize team screen or a portrait click. OSS persists body, accessory,
voice IDs, and label preferences in `Glowbom/live-profiles.json` under the OS user configuration
directory. Preferences are keyed by public identity, survive restarts, and do not
modify Buzz. Only current roster entries are returned or writable through
`GET/PUT /buzz/session/profiles`. Writes replace the saved file through a temporary
file; a corrupt file is reported rather than overwritten.

`GET /buzz/session/voices` reads the account's voice list using the configured
ElevenLabs key. `POST /buzz/session/voice-preview` generates a short fixed sample
for a known voice, only on an explicit Preview action. Previews use credits. These
routes keep the same local authentication and origin checks as other Buzz routes.
The game and preference file never receive the ElevenLabs key. Speech uses each
sender's assigned voice, or the shared default when no voice is assigned.

### Optional full-message speech queue

The Godot Live client can opt in with `readAll: true` when enabling speech through
`POST /buzz/session/speech`. The default remains limited speech. The feed reports
`readAllSupported`, the current owner's `readAll` state, and `speechOverflow`.
For that owner, queued events are merged with recent events and sorted by
sequence, so ordinary feed eviction does not lose queued speech.

Full-message speech requests use a zero-based `chunk` field. The server chooses
segments of up to 500 Unicode characters from verified channel events. A
successful audio response includes `X-Buzz-Speech-More: true` when another segment
remains. Only the next segment is accepted, and a generation attempt is consumed
before contacting ElevenLabs. Failed synthesis is not automatically retried.
The client never submits arbitrary replacement speech text.

This opt-in queue retains up to 1,024 messages in memory while the speaker lease
is active. It removes the 30-second speech expiry for those messages, but keeps
existing event validation, size limits, authentication, one-speaker ownership,
request cancellation, and duplicate protection. Muting, changing mode/key, lease
expiry, or disconnect clears the queue. Overflow is reported explicitly.
No React UI changes or new dependencies are required.

### Remembered ElevenLabs key

The Live settings UI now sends `remember: true` with speech-key updates. This
saves the key under the OS user configuration directory at
`Glowbom/live-private/elevenlabs-key`, using a private directory and file
(0700/0600 on macOS/Linux), temporary writes, and rename. This is local plaintext,
not encrypted storage. Save failures leave the active key unchanged. The status
API reports availability without returning the key.

A listener restores the saved key on startup. If no saved setting exists, the
`ELEVENLABS_API_KEY` environment value is the fallback. Clearing stores an empty
setting, removing the saved secret and suppressing that fallback. The browser's
provider-key storage is independent and is not cleared by this action.
Previously entered memory-only keys must be saved once through the updated UI.
New Godot sessions automatically enable limited speech when a key is available;
read-all mode stays off by default, and manual mute is respected for that session.

### Editable nameplates and Buzz metadata

Roster responses include `description` from the public profile's `about` field,
normalized to a single line and bounded to 280 Unicode characters. The original
`defaultLabel` remains an 80-character version for older single-line clients.

Optional authenticated reads through Buzz's `POST /query` bridge supply `harness`
and `model`, each bounded to 80 characters. OSS verifies signed kind-0 profiles,
NIP-OA owner attestations, same-owner kind-30177 managed-agent records, and linked
kind-30175 definitions. Definitions provide `runtime` and `model`; standalone
managed agents can provide model only. No values are inferred from descriptions.
The protocol was checked against
[Buzz NIP-AP](https://github.com/block/buzz/blob/3c7f288c60d67df78577b237e27c3dfc8831aaa1/docs/nips/NIP-AP.md)
and [NIP-OA](https://github.com/block/buzz/blob/3c7f288c60d67df78577b237e27c3dfc8831aaa1/docs/nips/NIP-OA.md).

Both optional queries share a two-second deadline per profile batch inside the
existing overall request deadline. Metadata failure, missing records, or missing
proof leaves these fields empty and preserves roster loading. These are published
configuration values. Local harness overrides and inherited defaults that have
not been published are unavailable; the fields do not report current execution.

Local presentation profiles add `labelLayout` (`legacy` or `structured`),
`customName`, `customHarness`, `customModel`, `customDescription`, `hideHarness`,
and `hideModel`. Empty custom values follow Buzz. Name and harness allow 80 Unicode
characters, model 120, and description 280; control/format characters are rejected.
The original `customLabel` (80 characters) and `hideLabel` remain supported.
Absent layout preserves legacy custom or hidden labels in Godot; other profiles
use separate harness and model lines. Switching layouts retains both sets of text.

These preferences save with appearance and voice choices. No Buzz writes occur.
Restart the updated backend and Godot, then refresh members in OSS to fetch the
new fields. No React changes or new dependencies are required.
