from __future__ import annotations

from pathlib import Path
import tempfile
import unittest

from scripts.lceda.j00_evidence import artifact_record, validate_artifact, write_report


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


if __name__ == "__main__":
    unittest.main()
