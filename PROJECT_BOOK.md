# Project Book

Project Book turns saved project work into portable history. Its backend,
generation APIs, and file format are open. The full reader, editing, and drawing
screens below belong to the separate private Desktop client, not the public
Agent + Buzz browser. In Desktop, the book icon is available beside chat, in
Result, and in the full workspace.

The reader follows the app's light or dark appearance. Opening it gathers saved
prototype and build records. It does not call a model or change the project code.

## Open backend APIs

The routes are implemented in `backend/main.go` and the `project_book*.go` files:

| Route | Methods | Purpose |
| --- | --- | --- |
| `/project-book` | GET, POST | Read and revise project entries |
| `/project-book/story` | POST | Generate or rewrite a story |
| `/project-book/visual` | POST | Generate a result drawing |
| `/project-book/models` | GET | List compatible connected models |
| `/settings/project-book` | GET, POST, PATCH | Read or change generation preferences |
| `/project-book/media` | GET | Read saved illustrations |

Browser clients reach these through the local `/api` proxy. Preserve bearer-token,
origin, project-path, and media validation. See the handler request structs for
payloads and the adjacent tests for conflict and revision behavior. A custom
client can use these APIs without importing Desktop UI code.

## Desktop reader: read and correct an entry

Select a story to read a plain-language account of what changed, how it was
made, and why it matters. When available, the run's visual sketch illustrates
the story. Open Sources and contribution details for the original request,
recorded model, contributor, changed files, and run outline. Older records may
not contain all of this information.

Use the pencil to edit the title, story, and image captions. The image icon adds
a PNG, JPEG, or WebP image. Remove an illustration from its caption controls.
To replace an illustration, add the new image and remove the old one.

An older, untouched entry may offer **Write story**. This asks the selected
Project Book model to draft an article from its saved evidence. It does not
run when you merely open the Book, and it does not replace a story you edited.

Edits create saved revisions. Removing an image from a story leaves its original
build input and earlier Book revisions intact. New history does not replace your
existing stories. If another window changed an entry, reload it before saving
again so you can keep both sets of changes.

## Listen to a story

Use the speaker in an entry's controls to hear its title and story. It uses the
same saved speech voice as chat. Select it again to stop. Playback also stops
when you open another entry, edit the story, or close the Book.

## Choose a model and style

Open **Settings > Project Book > Model** to choose a model for both stories and
vector sketches. This setting is independent of your Build and Chat selections.
**Automatic** first uses the build's model if it is connected and supported,
then the selected Chat model. If neither can do the work, choose a connected
Book model. An explicit choice stays selected if it becomes unavailable.

The picker includes compatible connected OpenCode and Codex models. Build-only
connections such as Cursor, Claude Code, and Big Pickle can still produce Book
entries through this separate model. Apple Intelligence and local MiMo are not
available for Book generation in this version.

An entry shows **Using** and a **Change** control. Choose a model there to retry
missing content or preview another writing voice for that entry. This override
does not change the saved default. Existing stories and drawings remain intact
when the selected model changes. The original builder stays credited, with
separate model details for generated stories and drawings.

Open **Settings > Project Book** to choose a voice. Announcement is the default.
Builder's Journal, Changelog, Keynote, Indie Launch, Documentary, Epic, Roast,
Unhinged Dev, Standup, and Super Funny each pair a writing voice with a different
sketch treatment. Unhinged Dev asks for chaotic developer comedy with punchlines.
Standup asks for a short comedy routine with setups, jokes, and a closing
callback about the actual change. Both offer **Allow swearing in stories**.
With it on, the model is asked for occasional swearing; with it off, the jokes
stay clean. Drawing labels stay free of profanity.

Super Funny uses clean, rapid-fire jokes, escalating silly metaphors, and a
closing callback that anyone can follow. Its drawings add playful cartoon
details around a clear sketch of the actual interface. It has no swearing switch.

Choose **Custom** to write your own style prompt, up to 4000 characters. Describe
your tone, preferred length, structure, and illustration direction. For example:
"Write a warm, direct social post under 280 characters. Lead with what changed.
Draw a minimal notebook sketch with one playful arrow." Save the custom style
to use it for future generation. Length requests guide the model; review the
result before posting.

Preferences are saved on the backend device in
`~/.glowbom/project-book-writing.json`. They apply to new stories and newly
generated sketches, including automatic results. Existing stories, drawings,
and source records stay intact. Every style keeps the same evidence and vector
drawing limits. Custom guidance is sent to the selected Book model along with
the saved evidence.

For a completed entry with an available Book model, **Try another voice**
drafts a story preview. Use this to rewrite an existing story after changing
your style. You can choose a voice, change the swearing switch for Standup or
Unhinged Dev, or provide custom guidance for just that preview. These choices
start from your saved preferences and do not change them.
Review and edit the draft, then choose **Save entry** to keep it as a new revision,
or cancel to leave the saved story intact. The drawing stays unchanged during
a story preview. Saving checks for changes made in another window or editor.
Nothing is posted to a social network automatically.

## Open a result sketch

Completed results with a saved run identity have a small sketch icon. It opens
that result's visual sketch directly when one exists. You can switch to the
recorded run outline or open either view from the story.

After a successful prototype or an OpenCode, Codex, Cursor, or Claude Code build,
the selected Book model reads the saved request, result, and a bounded selection
of changed source files. A separate request drafts a short story and draws a hand-style vector sketch
of a supported screen or feature. The Book saves the story as editable Markdown
and the sketch marks as editable JSON coordinates. The
sketch is an interpretation of saved work, not an automatically verified capture
of a running app. If model output does not match the required format, Glowbom
makes one automatic repair request with the same model and keeps any valid
story or drawing. Generation has a three-minute limit, including the repair.
Codex uses a separate request with tools disabled and low thinking effort when
the selected Book model supports it. The Build keeps its selected thinking effort.
Drawing styles use the same supported vector marks. If drawing still fails or
no model is connected, the result still completes and the Book keeps a factual
record. The reader shows when an entry has no drawing. Choose **Drawing and text**,
**Drawing only**, or **Text only**, then select **Retry selected**.
A retry keeps any already saved story or drawing, including owner edits, and
fills the missing part. When only one part succeeds, the reader shows that saved
part and a warning so you can retry the other.
You can close the Book while generating text or a drawing. Keep Glowbom open
while it runs, then reopen the Book to see its progress or saved result. Closing
an unsaved voice preview cancels that preview.

The run outline remains available. It draws the recorded request, work, and
outcome as vector nodes and connections. A completed build does not mean all its
behavior was tested.

New prototypes created from Draw also keep the original drawing's points,
shapes, text, and optional background image with the saved prototype. Existing
PNG inputs remain images; their lost vector data cannot be recovered automatically.

## Continue a drawing in Magic Window

Use the small window icon beside **View drawing**, or in the result sketch view,
to open an editable copy in Magic Window. The original Book drawing stays intact.
An existing Magic Window draft is saved in local history before the copy opens.

Adjust the drawing or add notes, then use Magic Window to generate a browser
prototype. The Book drawing is a reference interpretation of saved work. Opening
it does not change the project's code or verify the running app.

## Files you own

The Book lives inside the selected project:

```text
project-book/
  README.md
  book.json
  intent.md
  current-result.md
  decisions.md
  next.md
  PROJECT_BRIEF.md
  assets/
  history/
    <entry-id>/
      <revision>/
        entry.md
        source.json
        sketch.json
        ready
```

The latest complete revision of each entry is current. The `ready` file marks a
complete revision so an interrupted save does not replace a previous one.
The story comes from `entry.md`, including edits made in a text editor.
`source.json` keeps the links to recorded work and `sketch.json` keeps the
outline and optional visual sketch.

The overview notes are starting points for the owner. The initial Project Brief
points to these files; it is not a generated synthesis or automatically inserted
into every coding session. The project manifest is left unchanged.

Original source records stay in `history/` and `.glowbom/prototypes/`. New
prototype records include immutable input images and, when supplied,
`input-sketch.json`. Keep these files with the project when moving it.

## Boundaries of this version

- Reading and editing are local. The Book is not published automatically.
- Changing a story or illustration does not rebuild the software.
- Visual sketches are AI interpretations and may omit or misplace details.
- Glowbom Live contributions are not connected. The paired companion can read
  saved Books; native editing remains separate work.
- Books in an unrecognized format are left untouched.
- Images are limited to 8 MiB, 32 megapixels, and 8192 pixels per side.
- Each entry can contain up to 32 images. A Book can contain up to 1000 entries.

The files and backend are open; the full reader is part of the private Desktop React client.
This document describes the source implementation, not availability in older releases.
