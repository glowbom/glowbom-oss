"""Contract checks for the optional local voice API."""

import http.client
import importlib.util
import json
import pathlib
import sys
import threading
import unittest
import wave
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("kitten_voice_server", pathlib.Path(__file__).with_name("server.py"))
voice_server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(voice_server)


class FakeModel:
    available_voices = ["Bella", "Jasper", "Luna", "Bruno", "Rosie", "Hugo", "Kiki", "Leo"]

    def __init__(self):
        self.calls = []

    def generate(self, text, voice):
        self.calls.append((text, voice))
        return [0.0] * 100


class FakeSoundFile:
    @staticmethod
    def write(output, audio, sample_rate, format, subtype):
        assert format == "WAV" and subtype == "PCM_16"
        with wave.open(output, "wb") as wav:
            wav.setnchannels(1)
            wav.setsampwidth(2)
            wav.setframerate(sample_rate)
            wav.writeframes(b"\0\0" * len(audio))


class VoiceServerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.model = FakeModel()
        cls.server = voice_server.VoiceServer(("127.0.0.1", 0), cls.model)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def request(self, method, path, body=None, headers=None):
        connection = http.client.HTTPConnection("127.0.0.1", self.server.server_address[1], timeout=5)
        connection.request(method, path, body, headers or {})
        response = connection.getresponse()
        result = response.status, response.getheader("Content-Type"), response.read()
        connection.close()
        return result

    def test_lists_named_and_default_voices(self):
        status, content_type, body = self.request("GET", "/v1/audio/voices")
        self.assertEqual((status, content_type), (200, "application/json"))
        voices = json.loads(body)["voices"]
        self.assertEqual(voices[0], {"id": "default", "name": "Default (Bella)"})
        self.assertIn({"id": "Jasper", "name": "Jasper"}, voices)

    def test_speaks_wav_with_default_voice(self):
        body = json.dumps({"model": "gpt-4o-mini-tts", "input": "Hello Glowbom", "voice": "default", "response_format": "wav"})
        with patch.dict(sys.modules, {"soundfile": FakeSoundFile}):
            status, content_type, wav = self.request("POST", "/v1/audio/speech", body, {"Content-Type": "application/json"})
        self.assertEqual((status, content_type), (200, "audio/wav"))
        self.assertTrue(wav.startswith(b"RIFF"))
        self.assertEqual(self.model.calls[-1], ("Hello Glowbom", "Bella"))

    def test_rejects_unknown_voice_and_long_text(self):
        for payload in ({"input": "Hello", "voice": "unknown"}, {"input": "a" * 2501, "voice": "Bella"}):
            status, _, _ = self.request("POST", "/v1/audio/speech", json.dumps(payload), {"Content-Type": "application/json"})
            self.assertEqual(status, 400)

    def test_rejects_browser_origin(self):
        status, _, _ = self.request("GET", "/v1/audio/voices", headers={"Origin": "https://example.com"})
        self.assertEqual(status, 403)


if __name__ == "__main__":
    unittest.main()
