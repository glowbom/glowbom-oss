#!/usr/bin/env python3
"""Read-only checks for accidental credentials and local files before publication."""
import argparse
from dataclasses import asdict, dataclass
import json
import os
from pathlib import Path
import re
import sys


# These installed dependencies and generated trees are outside this source audit.
SKIP_DIRECTORIES = frozenset({
    ".git", "node_modules", ".venv", "venv", "__pycache__", ".cache",
    ".pytest_cache", ".mypy_cache", ".ruff_cache", ".next", ".react-router",
    ".source", ".vite", ".gradle", ".dart_tool", ".build", ".swiftpm",
    "Pods", "DerivedData", "build", "dist", "target", "coverage",
})
LOCAL_DIRECTORIES = frozenset({
    "saved_images", "saved_audio", "saved_videos", "logs", "releases",
    "release-resources", ".glowby", "xcuserdata",
})
SECRET_NAMES = frozenset({
    ".npmrc", ".netrc", "credentials", "credentials.json", "auth.json",
    "service-account.json", "serviceAccountKey.json", "key.properties",
    "id_rsa", "id_ecdsa", "id_ed25519",
})
SECRET_SUFFIXES = frozenset({
    ".pem", ".key", ".p12", ".pfx", ".jks", ".keystore",
    ".mobileprovision", ".provisionprofile",
})
LOCAL_SUFFIXES = frozenset({
    ".log", ".pid", ".db", ".sqlite", ".sqlite3", ".dmg", ".ipa",
    ".exe", ".zip", ".tgz", ".gz", ".tar", ".pyc",
})
MEDIA_SUFFIXES = frozenset({
    ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".woff", ".woff2",
    ".ttf", ".otf", ".wasm", ".pdf", ".mp3", ".wav", ".mp4", ".mov",
})
MAX_BYTES = 16 * 1024 * 1024
PRIVATE_ROOT_DIRECTORIES = frozenset({"desktop", "flutter", "swiftui", "site", "cloudflare"})
PRIVATE_KEY = re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH |DSA |ENCRYPTED )?PRIVATE KEY-----")
TOKENS = (
    ("github-token", re.compile(r"\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{50,})\b")),
    ("provider-token", re.compile(r"\b(?:sk-(?:proj-|ant-api\d\d-)?|xai-|gsk_|hf_)[A-Za-z0-9_-]{24,}\b")),
    ("aws-access-id", re.compile(r"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b")),
    ("google-api-key-review", re.compile(r"\bAIza[A-Za-z0-9_-]{30,}\b")),
    ("slack-token", re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{20,}\b")),
    ("jwt-token", re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}\b")),
)


@dataclass(frozen=True, order=True)
class Finding:
    path: str
    line: int
    kind: str


def placeholder(value):
    """Only recognizable dummy strings are exempt, including in test source."""
    if not isinstance(value, str):
        return False
    value = value.strip()
    if not value:
        return True
    body = re.sub(r"^(?:sk-(?:proj-|ant-api\d\d-)?|gh[pousr]_|github_pat_|xai-|gsk_|hf_|AIza|AKIA|ASIA)", "", value)
    if re.fullmatch(r"[xX*._-]+", body):
        return True
    return bool(re.match(r"^(?:fixture|example|dummy|placeholder|test|your)[-_ ]", body, re.I))


def json_credentials(value):
    if isinstance(value, list):
        return {kind for item in value for kind in json_credentials(item)}
    if not isinstance(value, dict):
        return set()
    kinds = set()
    tokens = [value.get("idToken"), value.get("refreshToken")]
    if {"idToken", "refreshToken"}.issubset(value) and any(key in value for key in ("uid", "expiresAt", "expiresIn")):
        if any(isinstance(token, str) and not placeholder(token) for token in tokens):
            kinds.add("credential-store-json")
    if value.get("type") == "service_account" and isinstance(value.get("private_key"), str) and not placeholder(value["private_key"]):
        kinds.add("service-account-json")
    for item in value.values():
        kinds.update(json_credentials(item))
    return kinds


def content_findings(text, name):
    findings = set()
    for match in PRIVATE_KEY.finditer(text):
        findings.add(Finding(name, text.count("\n", 0, match.start()) + 1, "private-key-block"))
    for kind, pattern in TOKENS:
        for match in pattern.finditer(text):
            if not placeholder(match.group()):
                findings.add(Finding(name, text.count("\n", 0, match.start()) + 1, kind))
    if name.lower().endswith(".json"):
        try:
            for kind in json_credentials(json.loads(text)):
                findings.add(Finding(name, 1, kind))
        except (ValueError, RecursionError):
            pass
    return findings


def scan(root):
    root = Path(root).resolve(strict=True)
    if not root.is_dir():
        raise ValueError("Audit root must be a directory")
    findings = set()
    counts = {"scannedFiles": 0, "skippedDirectories": 0, "skippedBinaryFiles": 0}

    def add(path, kind):
        findings.add(Finding(path.relative_to(root).as_posix(), 1, kind))

    def walk_error(error):
        path = Path(error.filename)
        if path.is_relative_to(root):
            add(path, "unreadable-directory")

    for folder, directories, filenames in os.walk(root, followlinks=False, onerror=walk_error):
        base = Path(folder)
        for name in list(directories):
            path = base / name
            if name in SKIP_DIRECTORIES:
                counts["skippedDirectories"] += 1
                directories.remove(name)
            elif path.is_symlink():
                add(path, "directory-symlink-review")
                directories.remove(name)
            elif name in LOCAL_DIRECTORIES or name.endswith(".app"):
                add(path, "local-generated-directory")
                directories.remove(name)
            elif name == ".glowbom" and path.relative_to(root).as_posix() != "project/.glowbom":
                add(path, "local-generated-directory")
                directories.remove(name)
            elif base == root and name in PRIVATE_ROOT_DIRECTORIES:
                add(path, "private-source-directory")
                directories.remove(name)
        for name in filenames:
            path = base / name
            relative = path.relative_to(root)
            if relative.as_posix() == "backend/node/local.ts":
                add(path, "reference-cloud-source")
            if relative.as_posix() == "extras/kitten-tts/:memory:.ses":
                add(path, "local-session-file")
                continue
            if name in SECRET_NAMES or path.suffix.lower() in SECRET_SUFFIXES or "firebase-adminsdk" in name or (name.startswith(".env") and name not in (".env.example", ".env.template")):
                add(path, "secret-file-name")
            if name in (".DS_Store", "local.properties") or path.suffix.lower() in LOCAL_SUFFIXES:
                add(path, "local-file")
            if relative.as_posix() in ("backend/server", "backend/glowbom-backend", "cli/glowbom", "cli/glowby") or (relative.parent.as_posix() == "cli" and re.match(r"(?:glowbom|glowby)-(?:darwin|linux|windows)-", name)):
                add(path, "compiled-cli-or-backend")
            try:
                resolved = path.resolve(strict=True)
                if not resolved.is_relative_to(root):
                    add(path, "escaping-symlink")
                    continue
                if path.is_symlink() and any(part in SKIP_DIRECTORIES for part in resolved.relative_to(root).parts):
                    add(path, "symlink-to-excluded-tree")
                    continue
                if not resolved.is_file():
                    add(path, "special-file")
                    continue
                if path.suffix.lower() in MEDIA_SUFFIXES:
                    counts["skippedBinaryFiles"] += 1
                    continue
                if resolved.stat().st_size > MAX_BYTES:
                    add(path, "unscanned-large-file")
                    continue
                data = resolved.read_bytes()
                if data.startswith((b"\xff\xfe", b"\xfe\xff")):
                    text = data.decode("utf-16")
                elif b"\x00" in data:
                    counts["skippedBinaryFiles"] += 1
                    continue
                else:
                    text = data.decode("utf-8-sig", errors="replace")
                if relative.parts[:2] == ("project", ".glowbom"):
                    # The shipped template contains one empty preview list.
                    if relative.as_posix() != "project/.glowbom/previews.json" or text.strip() != "[]":
                        add(path, "local-project-state")
                counts["scannedFiles"] += 1
                findings.update(content_findings(text, relative.as_posix()))
            except (OSError, UnicodeError, RuntimeError):
                add(path, "unreadable-or-broken-link")
    return sorted(findings), counts


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--json", action="store_true", help="Print only counts and finding paths, lines, and types")
    args = parser.parse_args(argv)
    try:
        findings, counts = scan(args.root)
    except (OSError, ValueError, RuntimeError):
        print("Cannot read the audit root.", file=sys.stderr)
        return 2
    if args.json:
        print(json.dumps({"findings": [asdict(finding) for finding in findings], "counts": counts}, indent=2))
    else:
        for finding in findings:
            print(f"{json.dumps(finding.path)}:{finding.line}: {finding.kind}")
        print(f"Scanned {counts['scannedFiles']} files; found {len(findings)} issue(s) for review.")
        print("This source check skips dependency, cache, and build trees and known binary media. It is not proof that publication is safe.")
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
