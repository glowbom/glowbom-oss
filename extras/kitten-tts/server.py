"""Optional loopback speech API for KittenTTS Mini 0.8."""

import contextlib
import io
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


HOST = "127.0.0.1"
PORT = 3900
MODEL = "KittenML/kitten-tts-mini-0.8"
DEFAULT_VOICE = "Bella"
SAMPLE_RATE = 24000
MAX_REQUEST_BYTES = 16 * 1024
MAX_TEXT_LENGTH = 2500
MAX_AUDIO_BYTES = 32 * 1024 * 1024


class VoiceServer(HTTPServer):
    def __init__(self, address, model):
        super().__init__(address, VoiceHandler)
        self.model = model
        self.voices = tuple(model.available_voices)
        if DEFAULT_VOICE not in self.voices:
            raise ValueError("KittenTTS Mini does not provide the default voice")


class VoiceHandler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(30)

    def log_message(self, _format, *_args):
        # Do not log user text or untrusted request paths.
        pass

    def _check_local_request(self):
        port = self.server.server_address[1]
        if self.headers.get("Host") not in (f"127.0.0.1:{port}", f"localhost:{port}"):
            self._error(403, "Use the loopback address.")
            return False
        if self.headers.get("Origin"):
            self._error(403, "Use the Glowbom local voice connection.")
            return False
        return True

    def _respond(self, status, content_type, body):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status, message):
        body = json.dumps({"error": message}).encode("utf-8")
        self._respond(status, "application/json", body)

    def do_GET(self):
        if not self._check_local_request():
            return
        if self.path != "/v1/audio/voices":
            self._error(404, "Not found.")
            return
        voices = [{"id": "default", "name": f"Default ({DEFAULT_VOICE})"}]
        voices.extend({"id": name, "name": name} for name in self.server.voices)
        self._respond(200, "application/json", json.dumps({"voices": voices}).encode("utf-8"))

    def do_POST(self):
        if not self._check_local_request():
            return
        if self.path != "/v1/audio/speech":
            self._error(404, "Not found.")
            return
        if self.headers.get("Content-Type", "").split(";", 1)[0].strip().lower() != "application/json":
            self._error(415, "Send JSON.")
            return
        try:
            length = int(self.headers.get("Content-Length", ""))
        except ValueError:
            self._error(411, "Content-Length is required.")
            return
        if length < 1 or length > MAX_REQUEST_BYTES:
            self._error(413, "Request is too large.")
            return
        try:
            data = json.loads(self.rfile.read(length))
        except (ValueError, UnicodeDecodeError):
            self._error(400, "Invalid JSON.")
            return
        if not isinstance(data, dict):
            self._error(400, "Invalid JSON.")
            return
        prompt = data.get("input")
        voice = data.get("voice", "default")
        audio_format = data.get("response_format", "wav")
        if not isinstance(prompt, str) or not prompt.strip() or len(prompt) > MAX_TEXT_LENGTH:
            self._error(400, "Enter up to 2500 characters of text.")
            return
        if not isinstance(voice, str) or (voice != "default" and voice not in self.server.voices):
            self._error(400, "Choose an available voice.")
            return
        if audio_format != "wav":
            self._error(400, "Only WAV audio is supported.")
            return
        try:
            import soundfile as sf

            # KittenTTS 0.8.1 prints the text passed to generate. Keep it out of logs.
            with contextlib.redirect_stdout(io.StringIO()):
                audio = self.server.model.generate(
                    prompt.strip(), voice=DEFAULT_VOICE if voice == "default" else voice
                )
            output = io.BytesIO()
            sf.write(output, audio, SAMPLE_RATE, format="WAV", subtype="PCM_16")
            wav = output.getvalue()
            if len(wav) > MAX_AUDIO_BYTES:
                self._error(500, "Generated audio is too large.")
                return
        except Exception:
            self._error(500, "Could not generate local speech.")
            return
        self._respond(200, "audio/wav", wav)


def main():
    try:
        from kittentts import KittenTTS
    except ImportError:
        print("Install KittenTTS in this Python environment first. See README.md.", file=sys.stderr)
        return 1
    print("Loading KittenTTS Mini. Uncached model files will be downloaded.", flush=True)
    model = KittenTTS(MODEL)
    try:
        with VoiceServer((HOST, PORT), model) as server:
            print(f"Local voice ready at http://{HOST}:{PORT}", flush=True)
            server.serve_forever()
    except KeyboardInterrupt:
        print("\nLocal voice stopped.")
    except OSError as error:
        print(f"Could not start local voice on port {PORT}: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
