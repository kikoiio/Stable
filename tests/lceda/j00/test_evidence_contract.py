from __future__ import annotations

import json
from pathlib import Path
import tempfile
import unittest

from scripts.lceda.j00_evidence import artifact_record, redact_text, validate_artifact, write_report
from scripts.lceda.j00_redact import sanitize_tree


class EvidenceContractTests(unittest.TestCase):
    def test_artifact_digest_and_stale_source_are_detected(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            artifact = base / "bom.json"
            artifact.write_text('{"ok":true}\n', encoding="utf-8")
            record = artifact_record(artifact, kind="bom", source_digest="current")
            self.assertEqual(validate_artifact(record, current_source_digest="current"), (True, "ok"))
            self.assertEqual(validate_artifact(record, current_source_digest="stale")[0], False)
            artifact.write_text('{"ok":false}\n', encoding="utf-8")
            self.assertEqual(validate_artifact(record)[0], False)

    def test_report_and_logs_are_redacted(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            output = Path(root)
            paths = write_report(
                output,
                {
                    "run_context": {"run_id": "test", "client_version": "unverified", "fixture_digest": "x"},
                    "capabilities": [{"id": "client.discover", "state": "unverified", "reason": "token=secret"}],
                    "exit_conclusion": "unverified",
                },
                secrets=["secret"],
                logs={"client.log": "Authorization: Bearer secret\n"},
            )
            self.assertTrue(paths["report"].is_file())
            self.assertIn("credential redacted", paths["report"].read_text(encoding="utf-8"))
            self.assertNotIn("secret", paths["log:client.log"].read_text(encoding="utf-8"))
            self.assertTrue((output / "capability-matrix.json").is_file())

    def test_cookie_headers_and_authorization_are_redacted(self) -> None:
        text = (
            "Cookie: session=cookie-secret; path=/\n"
            "Authorization: Bearer authorization-secret\n"
            '{"cookie":"json-cookie-secret"}\n'
        )
        redacted = redact_text(text)
        for secret in ("cookie-secret", "authorization-secret", "json-cookie-secret"):
            self.assertNotIn(secret, redacted)
        self.assertEqual(json.loads(redacted.splitlines()[2]), {"cookie": "[credential redacted]"})

    def test_artifact_sanitizer_only_copies_approved_reports(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            source = base / "artifacts"
            destination = base / "redacted-artifacts"
            source.mkdir()
            (source / "cli-probe.json").write_text(
                '{"headers":{"Cookie":"json-cookie-secret"}}\n', encoding="utf-8"
            )
            (source / "J00-BOM.csv").write_text("designator,value\nR1,10k\n", encoding="utf-8")
            trace = source / "client-logs-workflow" / "0" / "trace" / "client.log"
            trace.parent.mkdir(parents=True)
            trace.write_text("Cookie: trace-cookie-secret\n", encoding="utf-8")
            (source / "xvfb.log").write_text("non-report log\n", encoding="utf-8")

            written = sanitize_tree(source, destination)

            self.assertEqual(set(written), {"cli-probe.json", "J00-BOM.csv"})
            self.assertFalse((destination / "client-logs-workflow").exists())
            self.assertFalse((destination / "xvfb.log").exists())
            report = json.loads((destination / "cli-probe.json").read_text(encoding="utf-8"))
            self.assertEqual(report["headers"]["Cookie"], "[credential redacted]")


if __name__ == "__main__":
    unittest.main()
