"""Contract tests for J00's typed records and fake probe backend."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from scripts.lceda.j00_contracts import (
    CapabilityObservation,
    CapabilityStatus,
    ContractError,
    ErrorClass,
    OperationResult,
    ProjectSnapshot,
    RunContext,
    StepStatus,
    from_json,
    redact,
    to_json,
)
from tests.lceda.j00.fake_client import FakeBackendError, FakeClient


SHA = "a" * 64


def _run_context() -> RunContext:
    return RunContext(
        run_id="run-fake-1",
        commit_sha=SHA,
        runner_image="ubuntu-24.04",
        runner_arch="x86_64",
        client_version="fake-1",
        client_sha256=SHA,
        probe_version="j00-test",
        fixture_sha256=SHA,
        started_at="2026-01-01T00:00:00Z",
        env_summary={"HOME": "[isolated]"},
    )


class ContractRecordTests(unittest.TestCase):
    def test_round_trip_and_enum_normalization(self) -> None:
        record = CapabilityObservation("project.read", "verified", "observe", "read succeeded")
        payload = json.loads(to_json(record))
        self.assertEqual(payload["status"], "verified")
        decoded = from_json(to_json(record), CapabilityObservation)
        self.assertEqual(decoded.status, CapabilityStatus.VERIFIED)

    def test_missing_digest_is_rejected(self) -> None:
        data = _run_context().to_dict()
        data["fixture_sha256"] = ""
        with self.assertRaises(ContractError):
            RunContext.from_dict(data)

    def test_unknown_status_is_rejected(self) -> None:
        data = {"capability_id": "project.read", "status": "passed", "stage": "observe", "reason": "x"}
        with self.assertRaises(ContractError):
            CapabilityObservation.from_dict(data)

    def test_path_escape_is_rejected(self) -> None:
        data = {
            "project_root": "candidate",
            "entrypoint": "../formal/main.eprj3",
            "format": "eprj3",
            "file_manifest_sha256": SHA,
            "identity": {"id": "fake"},
            "snapshot_sha256": SHA,
            "source_session": "session-1",
        }
        with self.assertRaises(ContractError):
            ProjectSnapshot.from_dict(data)

    def test_sensitive_fields_are_redacted(self) -> None:
        redacted = redact({"token": "secret", "nested": {"password": "pw", "ok": 1}})
        self.assertEqual(redacted["token"], "[REDACTED]")
        self.assertEqual(redacted["nested"]["password"], "[REDACTED]")
        self.assertEqual(redacted["nested"]["ok"], 1)


class FakeProbeTests(unittest.TestCase):
    def test_normal_read_write_save_reopen_check_export_close(self) -> None:
        fake = FakeClient()
        session = fake.start("profile", "candidate")
        before = fake.observe(session)
        operation = fake.apply_parameter("10k", "12k")
        self.assertEqual(operation.result, OperationResult.APPLIED)
        self.assertNotEqual(operation.before_snapshot_ref, operation.after_snapshot_ref)
        self.assertEqual(fake.save(session).status, StepStatus.PASSED)
        reopened = fake.reopen("candidate")
        self.assertEqual(reopened.identity["parameter"], "12k")
        self.assertEqual(fake.check("erc").status, StepStatus.PASSED)
        with tempfile.TemporaryDirectory() as directory:
            artifact = fake.export("bom", directory)
            self.assertEqual(artifact.validation, StepStatus.PASSED)
            self.assertTrue((Path(directory) / "bom.json").is_file())
        self.assertEqual(fake.close().status, StepStatus.PASSED)
        self.assertEqual(before.identity["parameter"], "10k")

    def test_old_value_mismatch_has_no_write_side_effect(self) -> None:
        fake = FakeClient("old_value_mismatch")
        fake.start()
        operation = fake.apply_parameter("1k", "12k")
        self.assertEqual(operation.result, OperationResult.REJECTED)
        self.assertEqual(operation.error_class, ErrorClass.PRECONDITION)
        self.assertEqual(fake.parameter_value, fake.ACTUAL_VALUE)
        self.assertEqual(fake.save().status, StepStatus.PASSED)

    def test_unknown_write_result_blocks_replay(self) -> None:
        fake = FakeClient("unknown_write_result")
        fake.start()
        operation = fake.apply_parameter("10k", "12k")
        self.assertEqual(operation.result, OperationResult.UNKNOWN)
        self.assertEqual(operation.error_class, ErrorClass.UNKNOWN)
        self.assertEqual(fake.save().status, StepStatus.UNKNOWN)
        skipped = fake.apply_parameter("12k", "15k")
        self.assertEqual(skipped.result, OperationResult.SKIPPED)

    def test_timeout_and_crash_are_classified(self) -> None:
        timeout = FakeClient("timeout")
        with self.assertRaises(FakeBackendError) as timeout_error:
            timeout.start()
        self.assertEqual(timeout_error.exception.status, StepStatus.TIMED_OUT)
        crash = FakeClient("crash")
        with self.assertRaises(FakeBackendError) as crash_error:
            crash.start()
        self.assertEqual(crash_error.exception.status, StepStatus.CRASHED)

    def test_reopen_and_export_failures_are_explicit(self) -> None:
        reopen = FakeClient("reopen_failure")
        reopen.start()
        reopen.apply_parameter("10k", "12k")
        reopen.save()
        with self.assertRaises(FakeBackendError) as reopen_error:
            reopen.reopen()
        self.assertEqual(reopen_error.exception.error_class, ErrorClass.REOPEN)

        export = FakeClient("export_failure")
        export.start()
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(FakeBackendError) as export_error:
                export.export("bom", directory)
        self.assertEqual(export_error.exception.error_class, ErrorClass.EXPORT)


if __name__ == "__main__":
    unittest.main()
