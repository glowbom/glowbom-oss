import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("audit_publication", Path(__file__).with_name("audit-publication.py"))
audit = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = audit
spec.loader.exec_module(audit)


class PublicationAuditTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="glowbom-audit-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def put(self, name, value):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value)
        return path

    def kinds(self):
        return {(finding.path, finding.kind) for finding in audit.scan(self.root)[0]}

    def test_finds_key_and_token_in_test_source_without_printing_values(self):
        token = "sk-" + "abcDEF0123456789" * 3
        marker = "-" * 5 + "BEGIN PRIVATE KEY" + "-" * 5
        self.put("tests/leak.test.ts", "const key = '" + token + "';\n" + marker)
        findings, _ = audit.scan(self.root)
        self.assertEqual([(f.line, f.kind) for f in findings], [(1, "provider-token"), (2, "private-key-block")])
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(audit.main(["--root", str(self.root), "--json"]), 1)
        self.assertNotIn(token, output.getvalue())
        self.assertNotIn(marker, output.getvalue())

    def test_allows_explicit_fixtures_and_public_oauth_identifiers(self):
        self.put("example.ts", "\n".join(["sk-" + "x" * 40, "sk-fixture-" + "abc123" * 8, "123456789012-clientname.apps.googleusercontent.com"]))
        self.put("tests/fixture.json", json.dumps({"idToken": "fixture-id-token", "refreshToken": "fixture-refresh-token", "uid": "fixture-user", "expiresAt": 1}))
        self.assertEqual(self.kinds(), set())

    def test_finds_account_storage_json_under_arbitrary_filename(self):
        self.put("notes/data.json", json.dumps({"idToken": "opaque-live-value", "refreshToken": "opaque-refresh-value", "uid": "user", "email": "person@example.test", "expiresIn": 3600, "expiresAt": 1}))
        self.put("cli/example.go", 'type credentials struct { RefreshToken string `json:"refreshToken"` }')
        self.assertEqual(self.kinds(), {("notes/data.json", "credential-store-json")})

    def test_finds_nested_service_account_config(self):
        self.put("config.json", json.dumps({"account": {"type": "service_account", "private_key": "non-placeholder-key-material", "client_email": "service@example.test"}}))
        self.assertEqual(self.kinds(), {("config.json", "service-account-json")})

    def test_reports_local_files_and_skips_installed_dependencies(self):
        for name in (".env.local", "signing.p12", "saved_images/private.png", "logs/session.log", "cli/glowbom-darwin-arm64", "project/android/local.properties", "extras/kitten-tts/:memory:.ses"):
            self.put(name, "local data")
        token = "ghp_" + "abcDEF0123456789" * 3
        self.put("node_modules/fixture/key.txt", token)
        self.put(".git/config", token)
        self.put(".venv/example.py", token)
        kinds = self.kinds()
        self.assertIn((".env.local", "secret-file-name"), kinds)
        self.assertIn(("signing.p12", "secret-file-name"), kinds)
        self.assertIn(("saved_images", "local-generated-directory"), kinds)
        self.assertIn(("logs", "local-generated-directory"), kinds)
        self.assertIn(("cli/glowbom-darwin-arm64", "compiled-cli-or-backend"), kinds)
        self.assertIn(("project/android/local.properties", "local-file"), kinds)
        self.assertIn(("extras/kitten-tts/:memory:.ses", "local-session-file"), kinds)
        self.assertFalse(any(kind == "github-token" for _, kind in kinds))

    def test_does_not_follow_escaping_or_directory_symlinks(self):
        outside = Path(self.temp.name).parent / (Path(self.temp.name).name + "-outside")
        outside.write_text("must not read")
        self.addCleanup(outside.unlink)
        (self.root / "linked.txt").symlink_to(outside)
        (self.root / "broken.txt").symlink_to("missing.txt")
        (self.root / "linked-dir").symlink_to(self.root, target_is_directory=True)
        self.assertEqual(self.kinds(), {("linked.txt", "escaping-symlink"), ("broken.txt", "unreadable-or-broken-link"), ("linked-dir", "directory-symlink-review")})

    def test_checks_safe_internal_file_link_but_not_excluded_targets(self):
        token = "xai-" + "abcDEF0123456789" * 3
        self.put("config/source.txt", token)
        (self.root / "linked.txt").symlink_to("config/source.txt")
        self.put("node_modules/fixture.txt", "installed")
        (self.root / "dependency.txt").symlink_to("node_modules/fixture.txt")
        self.assertEqual(self.kinds(), {("config/source.txt", "provider-token"), ("linked.txt", "provider-token"), ("dependency.txt", "symlink-to-excluded-tree")})

    def test_reports_unreadable_root_and_unscanned_large_source(self):
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(audit.main(["--root", str(self.root / "missing")]), 2)
        self.put("huge.txt", "x" * (audit.MAX_BYTES + 1))
        self.assertEqual(self.kinds(), {("huge.txt", "unscanned-large-file")})

    def test_allows_only_the_empty_shipped_project_state(self):
        path = self.put("project/.glowbom/previews.json", "[]\n")
        self.assertEqual(self.kinds(), set())
        path.write_text('[{"project":"personal"}]')
        self.assertEqual(self.kinds(), {("project/.glowbom/previews.json", "local-project-state")})

    def test_rejects_private_source_roots_without_blocking_project_templates(self):
        for name in audit.PRIVATE_ROOT_DIRECTORIES:
            self.put(name + "/source.txt", "private source")
            self.put("project/" + name + "/source.txt", "portable template")
        self.put("backend/node/local.ts", "reference cloud code")
        expected = {(name, "private-source-directory") for name in audit.PRIVATE_ROOT_DIRECTORIES}
        expected.add(("backend/node/local.ts", "reference-cloud-source"))
        self.assertEqual(self.kinds(), expected)


if __name__ == "__main__":
    unittest.main()
