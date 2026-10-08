# Local voice with KittenTTS Mini

This optional server lets Glowbom OSS read chat text aloud with
[KittenTTS Mini 0.8](https://huggingface.co/KittenML/kitten-tts-mini-0.8).
It offers eight preset English voices. It does not clone a voice. The server
speaks through a small subset of the VoiceStudio audio API, so Glowbom's
**Local voice** setting works with either service. Run only one on port 3900.

## Install and start

You need Python installed. On macOS or Linux, first open Terminal in the
Glowbom OSS source folder. This folder must contain
`extras/kitten-tts/server.py`. If you only installed the app, download and
extract the source first. On macOS, type `cd `, drag the source folder from
Finder into Terminal, and press Return.

Run this setup block once. It stops if any step fails and leaves your
terminal in the source folder:

```bash
(
  cd extras/kitten-tts &&
  python3 -m venv .venv &&
  .venv/bin/python -m pip install --upgrade pip &&
  .venv/bin/python -m pip install https://github.com/KittenML/KittenTTS/releases/download/0.8.1/kittentts-0.8.1-py3-none-any.whl
)
```

After setup succeeds, start the service from the same source folder:

```bash
(cd extras/kitten-tts && .venv/bin/python server.py)
```

If `cd` reports "no such file or directory", your terminal is in the wrong
folder. Locate the source folder before continuing. Installing the KittenTTS
package alone does not install Glowbom's `server.py`.

The first start downloads the Mini model to the normal Hugging Face cache.
The Python dependencies take more disk space than the model file itself.
The server listens only on `127.0.0.1:3900`. Keep this terminal open while
using speech in Glowbom. Press Ctrl+C to stop it. After the model is cached,
you can set `HF_HUB_OFFLINE=1` when starting the server if you want to avoid
checking the model hub again.

In Glowbom OSS, open **Voice settings**, choose **Local voice**, click
**Refresh voices** after Terminal shows `Local voice ready`, choose a voice,
and play a sample. The default voice is
Bella. You can check the server directly with:

```bash
curl http://127.0.0.1:3900/v1/audio/voices
```

Speech text stays on this computer. Model downloads need an internet
connection unless already cached. The server accepts up to 2,500 characters
per request and produces 24 kHz WAV audio. It has no API key because it is
bound to loopback; do not expose the port to another network.

KittenTTS is a separate project. Its
[code](https://github.com/KittenML/KittenTTS) and
[Mini weights](https://huggingface.co/KittenML/kitten-tts-mini-0.8) are marked
Apache-2.0. Check the terms of the exact version you install before business
use. VoiceStudio is a different application with different engine and weight
licenses. This server installs neither VoiceStudio nor its models.
