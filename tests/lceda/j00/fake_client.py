"""Deterministic fake ClientProbe used by the J00 contract tests.

It models protocol outcomes, rather than pretending to prove any real LCEDA
capability.  Every failure scenario is explicit and no network, process, or
credential access is performed.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from pathlib import Path
import hashlib
import json
from typing import Any

from scripts.lceda.j00_contracts import (
    ArtifactRecord,
    CapabilityObservation,
    CapabilityStatus,
    ErrorClass,
    OperationObservation,
    OperationResult,
    ProjectSnapshot,
    SessionRecord,
    StepResult,
    StepStatus,
)


class FakeBackendError(RuntimeError):
    def __init__(self, message: str, status: StepStatus, error_class: ErrorClass):
        super().__init__(message)
        self.status = status
        self.error_class = error_class


@dataclass(frozen=True)
class FakeCommandResult:
    argv: tuple[str, ...]
    returncode: int | None
    status: StepStatus
    stdout_summary: str = ""
    stderr_summary: str = ""

    @property
    def timed_out(self) -> bool:
        return self.status == StepStatus.TIMED_OUT

    def to_dict(self) -> dict[str, Any]:
        return {
            "argv": list(self.argv),
            "returncode": self.returncode,
            "status": self.status.value,
            "stdout_summary": self.stdout_summary,
            "stderr_summary": self.stderr_summary,
        }


class FakeScenario(str, Enum):
    NORMAL = "normal"
    OLD_VALUE_MISMATCH = "old_value_mismatch"
    TIMEOUT = "timeout"
    CRASH = "crash"
    REOPEN_FAILURE = "reopen_failure"
    EXPORT_FAILURE = "export_failure"
    UNKNOWN_WRITE_RESULT = "unknown_write_result"


SCENARIO_ALIASES = {
    "old-value-mismatch": FakeScenario.OLD_VALUE_MISMATCH.value,
    "reopen-failure": FakeScenario.REOPEN_FAILURE.value,
    "export-failure": FakeScenario.EXPORT_FAILURE.value,
    "unknown-write-result": FakeScenario.UNKNOWN_WRITE_RESULT.value,
    "unknown_write": FakeScenario.UNKNOWN_WRITE_RESULT.value,
}
SCENARIOS = {scenario.value for scenario in FakeScenario}

def _digest(value: Any) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


class FakeClient:
    """A small, stateful fake implementing the plan's ClientProbe surface."""

    ACTUAL_VALUE = "10k"
    NEW_VALUE = "12k"
    NOW = "2026-01-01T00:00:00Z"

    def __init__(self, scenario: str = "normal", *, project_root: str = "candidate") -> None:
        scenario = getattr(scenario, "value", scenario)
        scenario = SCENARIO_ALIASES.get(str(scenario), str(scenario))
        if scenario not in SCENARIOS:
            raise ValueError(f"unknown fake scenario: {scenario}")
        self.scenario = scenario
        self.project_root = project_root
        self.profile_path = "profile"
        self.session: SessionRecord | None = None
        self.generation = 0
        self.parameter_value = self.ACTUAL_VALUE
        self.writes_attempted = 0
        self.write_uncertain = False
        self.saved = False
        self.closed = False
        self._snapshot_counter = 0

    @property
    def write_blocked(self) -> bool:
        return self.write_uncertain or self.scenario in {"old_value_mismatch", "timeout", "crash"}

    def discover(self) -> list[CapabilityObservation]:
        if self.scenario == "crash":
            status, reason = CapabilityStatus.UNVERIFIED, "fake client crashed during discovery"
        elif self.scenario == "timeout":
            status, reason = CapabilityStatus.LIMITED, "fake discovery timed out"
        else:
            status, reason = CapabilityStatus.VERIFIED, "deterministic fake backend"
        return [
            CapabilityObservation("client.discover", status, "discover", reason),
            CapabilityObservation("project.read", status, "discover", reason),
            CapabilityObservation("project.write", CapabilityStatus.UNVERIFIED if self.write_blocked else status, "discover", reason),
        ]

    def start(self, profile: str | None = None, project: str | None = None) -> SessionRecord:
        if self.scenario == "timeout":
            raise FakeBackendError("start timed out", StepStatus.TIMED_OUT, ErrorClass.TIMEOUT)
        if self.scenario == "crash":
            raise FakeBackendError("client crashed while starting", StepStatus.CRASHED, ErrorClass.CRASH)
        self.profile_path = profile or self.profile_path
        self.project_root = project or self.project_root
        self.generation += 1
        self.closed = False
        self.session = SessionRecord(
            session_id=f"fake-session-{self.generation}",
            generation=self.generation,
            client_pid=4100 + self.generation,
            bridge_pid=4200 + self.generation,
            profile_path=self.profile_path,
            project_path=self.project_root,
            started_at=self.NOW,
            cleanup_result=StepStatus.UNKNOWN,
        ).validate()
        return self.session

    def identify(self, session: SessionRecord | None = None) -> ProjectSnapshot:
        return self.observe(session)

    def observe(self, session: SessionRecord | None = None) -> ProjectSnapshot:
        if self.session is None or self.closed:
            raise FakeBackendError("no active session", StepStatus.FAILED, ErrorClass.PROTOCOL)
        return self._snapshot()

    def apply_parameter(
        self,
        expected_old_value: Any,
        normalized_new_value: Any = NEW_VALUE,
        *,
        target: str = "R1.resistance",
        operation_id: str = "op-parameter-1",
    ) -> OperationObservation:
        before = self._snapshot()
        self.writes_attempted += 1
        if self.write_blocked and self.write_uncertain:
            return OperationObservation(operation_id, target, expected_old_value, normalized_new_value, self.generation, _ref(before), None, None, OperationResult.SKIPPED, ErrorClass.PROTOCOL).validate()
        if expected_old_value != self.ACTUAL_VALUE:
            return OperationObservation(operation_id, target, expected_old_value, normalized_new_value, self.generation, _ref(before), None, 2, OperationResult.REJECTED, ErrorClass.PRECONDITION).validate()
        if self.scenario == "timeout":
            return OperationObservation(operation_id, target, expected_old_value, normalized_new_value, self.generation, _ref(before), None, None, OperationResult.UNKNOWN, ErrorClass.TIMEOUT).validate()
        if self.scenario == "crash":
            return OperationObservation(operation_id, target, expected_old_value, normalized_new_value, self.generation, _ref(before), None, -6, OperationResult.UNKNOWN, ErrorClass.CRASH).validate()
        if self.scenario == "unknown_write_result":
            self.parameter_value = normalized_new_value
            self.write_uncertain = True
            return OperationObservation(operation_id, target, expected_old_value, normalized_new_value, self.generation, _ref(before), None, None, OperationResult.UNKNOWN, ErrorClass.UNKNOWN).validate()
        self.parameter_value = normalized_new_value
        after = self._snapshot()
        return OperationObservation(operation_id, target, expected_old_value, normalized_new_value, self.generation, _ref(before), _ref(after), 0, OperationResult.APPLIED, ErrorClass.NONE).validate()

    def save(self, session: SessionRecord | None = None) -> StepResult:
        if self.scenario == "timeout":
            return StepResult("save", StepStatus.TIMED_OUT, "fake save timed out").validate()
        if self.scenario == "crash":
            return StepResult("save", StepStatus.CRASHED, "fake client crashed while saving", exit_code=-6).validate()
        if self.write_uncertain:
            return StepResult("save", StepStatus.UNKNOWN, "write result is unknown; save is not replayed").validate()
        self.saved = True
        return StepResult("save", StepStatus.PASSED, "candidate saved", exit_code=0).validate()

    def reopen(self, project: str | None = None, fresh_session: SessionRecord | None = None) -> ProjectSnapshot:
        if self.scenario == "reopen_failure":
            raise FakeBackendError("fresh session could not reopen project", StepStatus.FAILED, ErrorClass.REOPEN)
        if not self.saved:
            raise FakeBackendError("project was not saved", StepStatus.FAILED, ErrorClass.REOPEN)
        if fresh_session is None:
            self.closed = True
            self.start(self.profile_path, project or self.project_root)
        else:
            self.session = fresh_session
            self.generation = fresh_session.generation
            self.closed = False
        return self._snapshot()

    def check(self, kind: str) -> StepResult:
        if self.scenario in {"timeout", "crash"}:
            status = StepStatus.TIMED_OUT if self.scenario == "timeout" else StepStatus.CRASHED
            return StepResult(kind, status, f"fake {kind} unavailable after {self.scenario}").validate()
        return StepResult(kind, StepStatus.PASSED, f"fake {kind} passed", [f"snapshot-{self._snapshot_counter}"], 0).validate()

    def export(self, kind: str, output_dir: str | Path) -> ArtifactRecord:
        if self.scenario == "export_failure":
            raise FakeBackendError("fake export failed", StepStatus.FAILED, ErrorClass.EXPORT)
        snapshot = self._snapshot()
        output_root = Path(output_dir)
        output_root.mkdir(parents=True, exist_ok=True)
        relative = f"{kind}.json"
        path = output_root / relative
        content = json.dumps({"kind": kind, "snapshot": snapshot.snapshot_sha256}, sort_keys=True).encode()
        path.write_bytes(content)
        return ArtifactRecord(kind, relative, "application/json", len(content), hashlib.sha256(content).hexdigest(), _ref(snapshot), "export", StepStatus.PASSED).validate()

    def close(self, session: SessionRecord | None = None) -> StepResult:
        if self.session is None:
            return StepResult("cleanup", StepStatus.SKIPPED, "no fake session started").validate()
        self.closed = True
        self.session.ended_at = self.NOW
        self.session.exit_code = 0
        self.session.cleanup_result = StepStatus.PASSED
        return StepResult("cleanup", StepStatus.PASSED, "fake session closed", [], 0).validate()

    def command(self, *argv: str) -> FakeCommandResult:
        if self.scenario == "timeout":
            return FakeCommandResult(tuple(argv), None, StepStatus.TIMED_OUT, stderr_summary="timeout")
        if self.scenario == "crash":
            return FakeCommandResult(tuple(argv), -6, StepStatus.CRASHED, stderr_summary="crash")
        return FakeCommandResult(tuple(argv), 0, StepStatus.PASSED, stdout_summary="ok")

    def _snapshot(self) -> ProjectSnapshot:
        self._snapshot_counter += 1
        identity = {"project_id": "fake-project", "parameter": self.parameter_value}
        manifest = _digest({"root": self.project_root, "parameter": self.parameter_value})
        return ProjectSnapshot(
            project_root=self.project_root,
            entrypoint="minimal.eprj3",
            format="eprj3",
            file_manifest_sha256=manifest,
            identity=identity,
            snapshot_sha256=_digest({"manifest": manifest, "counter": self._snapshot_counter}),
            source_session=self.session.session_id if self.session else "none",
            component_count=1,
            parameter_count=1,
            pin_count=2,
            network_count=1,
            document_scope=["minimal.eprj3"],
        ).validate()


FakeBackend = FakeClient


def _ref(snapshot: ProjectSnapshot) -> str:
    return f"snapshot:{snapshot.snapshot_sha256}"


__all__ = ["FakeBackend", "FakeBackendError", "FakeClient", "FakeCommandResult", "FakeScenario", "SCENARIOS"]
