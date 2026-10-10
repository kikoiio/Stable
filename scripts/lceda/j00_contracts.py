"""Data contracts used by the J00 compatibility probe.

The module deliberately has no third party dependencies.  Records are plain
dataclasses and are validated at the boundary where JSON is read or written;
this keeps the fake backend and a future real client adapter interchangeable.
"""

from __future__ import annotations

from dataclasses import dataclass, field, fields, is_dataclass
from datetime import datetime, timezone
from enum import Enum
import hashlib
import json
import os
import re
from typing import Any, ClassVar, Mapping, TypeVar


class ContractError(ValueError):
    """Raised when a J00 record does not satisfy its wire contract."""


class CapabilityStatus(str, Enum):
    VERIFIED = "verified"
    LIMITED = "limited"
    UNAVAILABLE = "unavailable"
    UNVERIFIED = "unverified"


class StepStatus(str, Enum):
    PASSED = "passed"
    FAILED = "failed"
    SKIPPED = "skipped"
    TIMED_OUT = "timed_out"
    CRASHED = "crashed"
    UNKNOWN = "unknown"
    UNAVAILABLE = "unavailable"


class OperationResult(str, Enum):
    APPLIED = "applied"
    REJECTED = "rejected"
    FAILED = "failed"
    UNKNOWN = "unknown"
    SKIPPED = "skipped"


class ErrorClass(str, Enum):
    NONE = "none"
    PRECONDITION = "precondition"
    TIMEOUT = "timeout"
    CRASH = "crash"
    REOPEN = "reopen"
    EXPORT = "export"
    PROTOCOL = "protocol"
    CLEANUP = "cleanup"
    UNKNOWN = "unknown"


_SHA256 = re.compile(r"^[0-9a-fA-F]{64}$")
_GIT_SHA = re.compile(r"^[0-9a-fA-F]{7,64}$")
_RUN_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")
_ISO_UTC = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,6})?(?:Z|\+00:00)$"
)
_REDACTED = "[REDACTED]"
_SENSITIVE_KEYS = {
    "token",
    "password",
    "passwd",
    "secret",
    "api_key",
    "apikey",
    "authorization",
    "cookie",
    "credential",
    "credentials",
}


def _required(value: Any, name: str) -> Any:
    if value is None or (isinstance(value, str) and not value.strip()):
        raise ContractError(f"{name} is required")
    return value


def _string(value: Any, name: str) -> str:
    _required(value, name)
    if not isinstance(value, str):
        raise ContractError(f"{name} must be a string")
    return value


def _sha256(value: Any, name: str) -> str:
    value = _string(value, name)
    if not _SHA256.fullmatch(value):
        raise ContractError(f"{name} must be a SHA-256 hex digest")
    return value.lower()


def _git_sha(value: Any, name: str) -> str:
    value = _string(value, name)
    if not _GIT_SHA.fullmatch(value):
        raise ContractError(f"{name} must be a hexadecimal Git commit SHA")
    return value.lower()


def _timestamp(value: Any, name: str) -> str:
    value = _string(value, name)
    if not _ISO_UTC.fullmatch(value):
        raise ContractError(f"{name} must be an ISO-8601 UTC timestamp")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ContractError(f"{name} must be a valid timestamp") from exc
    if parsed.tzinfo is None or parsed.utcoffset() != timezone.utc.utcoffset(parsed):
        raise ContractError(f"{name} must be in UTC")
    return value


def _relative_path(value: Any, name: str) -> str:
    value = _string(value, name)
    normalized = value.replace("\\", "/")
    if os.path.isabs(value) or normalized.startswith("/"):
        raise ContractError(f"{name} must be relative")
    parts = [part for part in normalized.split("/") if part not in ("", ".")]
    if ".." in parts:
        raise ContractError(f"{name} must not escape its root")
    if not parts:
        raise ContractError(f"{name} must not be empty")
    return "/".join(parts)


def _enum(value: Any, enum_type: type[Enum], name: str) -> Any:
    try:
        return enum_type(value)
    except (ValueError, TypeError) as exc:
        allowed = ", ".join(item.value for item in enum_type)
        raise ContractError(f"{name} has unknown value {value!r}; expected {allowed}") from exc


def _mapping(value: Any, name: str) -> dict[str, Any]:
    if value is None:
        return {}
    if not isinstance(value, Mapping):
        raise ContractError(f"{name} must be an object")
    return dict(value)


def _string_list(value: Any, name: str) -> list[str]:
    if value is None:
        return []
    if not isinstance(value, (list, tuple)) or any(not isinstance(item, str) for item in value):
        raise ContractError(f"{name} must be an array of strings")
    return list(value)


def _nonnegative_int(value: Any, name: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise ContractError(f"{name} must be a non-negative integer")
    return value


def _optional_int(value: Any, name: str) -> int | None:
    if value is None:
        return None
    return _nonnegative_int(value, name)


def _exit_code(value: Any, name: str) -> int | None:
    if value is None:
        return None
    if not isinstance(value, int) or isinstance(value, bool):
        raise ContractError(f"{name} must be an integer")
    return value


def _as_dict(value: Any) -> dict[str, Any]:
    if is_dataclass(value):
        result: dict[str, Any] = {}
        for item in fields(value):
            raw = getattr(value, item.name)
            if isinstance(raw, Enum):
                raw = raw.value
            elif is_dataclass(raw):
                raw = _as_dict(raw)
            elif isinstance(raw, list):
                raw = [_as_dict(entry) if is_dataclass(entry) else (entry.value if isinstance(entry, Enum) else entry) for entry in raw]
            elif isinstance(raw, dict):
                raw = {key: (_as_dict(entry) if is_dataclass(entry) else (entry.value if isinstance(entry, Enum) else entry)) for key, entry in raw.items()}
            result[item.name] = raw
        return result
    if isinstance(value, Mapping):
        return {str(key): value_item for key, value_item in value.items()}
    raise TypeError(f"cannot serialize {type(value)!r}")


def _ensure_no_unknown(data: Mapping[str, Any], allowed: set[str], kind: str) -> None:
    unknown = set(data) - allowed
    if unknown:
        raise ContractError(f"{kind} has unknown fields: {', '.join(sorted(unknown))}")


@dataclass
class RunContext:
    run_id: str
    commit_sha: str
    runner_image: str
    runner_arch: str
    client_version: str
    client_sha256: str
    probe_version: str
    fixture_sha256: str
    started_at: str
    ended_at: str | None = None
    env_summary: dict[str, Any] = field(default_factory=dict)

    def validate(self) -> "RunContext":
        if not isinstance(self.run_id, str) or not _RUN_ID.fullmatch(self.run_id):
            raise ContractError("run_id must be an identifier")
        _git_sha(self.commit_sha, "commit_sha")
        _string(self.runner_image, "runner_image")
        _string(self.runner_arch, "runner_arch")
        _string(self.client_version, "client_version")
        _sha256(self.client_sha256, "client_sha256")
        _string(self.probe_version, "probe_version")
        _sha256(self.fixture_sha256, "fixture_sha256")
        _timestamp(self.started_at, "started_at")
        if self.ended_at is not None:
            _timestamp(self.ended_at, "ended_at")
        _mapping(self.env_summary, "env_summary")
        return self

    def to_dict(self) -> dict[str, Any]:
        return _as_dict(self.validate())

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "RunContext":
        _ensure_no_unknown(data, {"run_id", "commit_sha", "runner_image", "runner_arch", "client_version", "client_sha256", "probe_version", "fixture_sha256", "started_at", "ended_at", "env_summary"}, "RunContext")
        record = cls(**dict(data))
        return record.validate()


@dataclass
class CapabilityObservation:
    capability_id: str
    status: CapabilityStatus | str
    stage: str
    reason: str
    evidence_refs: list[str] = field(default_factory=list)
    depends_on: list[str] = field(default_factory=list)

    def validate(self) -> "CapabilityObservation":
        _string(self.capability_id, "capability_id")
        self.status = _enum(self.status, CapabilityStatus, "status")
        _string(self.stage, "stage")
        _string(self.reason, "reason")
        self.evidence_refs = _string_list(self.evidence_refs, "evidence_refs")
        self.depends_on = _string_list(self.depends_on, "depends_on")
        return self

    def to_dict(self) -> dict[str, Any]:
        return _as_dict(self.validate())

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "CapabilityObservation":
        _ensure_no_unknown(data, {"capability_id", "status", "stage", "reason", "evidence_refs", "depends_on"}, "CapabilityObservation")
        return cls(**dict(data)).validate()


@dataclass
class ProjectSnapshot:
    project_root: str
    entrypoint: str
    format: str
    file_manifest_sha256: str
    identity: dict[str, Any]
    snapshot_sha256: str
    source_session: str
    component_count: int | None = None
    parameter_count: int | None = None
    pin_count: int | None = None
    network_count: int | None = None
    document_scope: list[str] = field(default_factory=list)

    def validate(self) -> "ProjectSnapshot":
        self.project_root = _relative_path(self.project_root, "project_root")
        self.entrypoint = _relative_path(self.entrypoint, "entrypoint")
        _string(self.format, "format")
        _sha256(self.file_manifest_sha256, "file_manifest_sha256")
        _mapping(self.identity, "identity")
        _sha256(self.snapshot_sha256, "snapshot_sha256")
        _string(self.source_session, "source_session")
        for name in ("component_count", "parameter_count", "pin_count", "network_count"):
            setattr(self, name, _optional_int(getattr(self, name), name))
        self.document_scope = _string_list(self.document_scope, "document_scope")
        for path in self.document_scope:
            _relative_path(path, "document_scope")
        return self

    def to_dict(self) -> dict[str, Any]:
        return _as_dict(self.validate())

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "ProjectSnapshot":
        _ensure_no_unknown(data, {"project_root", "entrypoint", "format", "file_manifest_sha256", "identity", "snapshot_sha256", "source_session", "component_count", "parameter_count", "pin_count", "network_count", "document_scope"}, "ProjectSnapshot")
        return cls(**dict(data)).validate()


@dataclass
class OperationObservation:
    operation_id: str
    target: str
    expected_old_value: Any
    normalized_new_value: Any
    generation: int
    before_snapshot_ref: str
    after_snapshot_ref: str | None
    exit_code: int | None
    result: OperationResult | str
    error_class: ErrorClass | str = ErrorClass.NONE
    evidence_refs: list[str] = field(default_factory=list)

    def validate(self) -> "OperationObservation":
        _string(self.operation_id, "operation_id")
        _string(self.target, "target")
        _nonnegative_int(self.generation, "generation")
        _string(self.before_snapshot_ref, "before_snapshot_ref")
        if self.after_snapshot_ref is not None:
            _string(self.after_snapshot_ref, "after_snapshot_ref")
        self.exit_code = _exit_code(self.exit_code, "exit_code")
        self.result = _enum(self.result, OperationResult, "result")
        self.error_class = _enum(self.error_class, ErrorClass, "error_class")
        self.evidence_refs = _string_list(self.evidence_refs, "evidence_refs")
        if self.result == OperationResult.APPLIED and not self.after_snapshot_ref:
            raise ContractError("applied operation requires after_snapshot_ref")
        if self.result == OperationResult.UNKNOWN and self.error_class == ErrorClass.NONE:
            raise ContractError("unknown operation result requires an error class")
        return self

    def to_dict(self) -> dict[str, Any]:
        return _as_dict(self.validate())

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "OperationObservation":
        _ensure_no_unknown(data, {"operation_id", "target", "expected_old_value", "normalized_new_value", "generation", "before_snapshot_ref", "after_snapshot_ref", "exit_code", "result", "error_class", "evidence_refs"}, "OperationObservation")
        return cls(**dict(data)).validate()


@dataclass
class ArtifactRecord:
    artifact_type: str
    relative_path: str
    mime: str
    size: int
    sha256: str
    source_snapshot_ref: str
    generated_by: str
    validation: StepStatus | str

    def validate(self) -> "ArtifactRecord":
        _string(self.artifact_type, "artifact_type")
        self.relative_path = _relative_path(self.relative_path, "relative_path")
        _string(self.mime, "mime")
        self.size = _nonnegative_int(self.size, "size")
        _sha256(self.sha256, "sha256")
        _string(self.source_snapshot_ref, "source_snapshot_ref")
        _string(self.generated_by, "generated_by")
        self.validation = _enum(self.validation, StepStatus, "validation")
        if self.validation == StepStatus.PASSED and self.size == 0:
            raise ContractError("passed artifact must be non-empty")
        return self

    def to_dict(self) -> dict[str, Any]:
        return _as_dict(self.validate())

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "ArtifactRecord":
        _ensure_no_unknown(data, {"artifact_type", "relative_path", "mime", "size", "sha256", "source_snapshot_ref", "generated_by", "validation"}, "ArtifactRecord")
        return cls(**dict(data)).validate()


@dataclass
class SessionRecord:
    session_id: str
    generation: int
    client_pid: int | None
    bridge_pid: int | None
    profile_path: str
    project_path: str
    started_at: str
    ended_at: str | None = None
    exit_code: int | None = None
    cleanup_result: StepStatus | str = StepStatus.UNKNOWN

    def validate(self) -> "SessionRecord":
        _string(self.session_id, "session_id")
        _nonnegative_int(self.generation, "generation")
        self.client_pid = _optional_int(self.client_pid, "client_pid")
        self.bridge_pid = _optional_int(self.bridge_pid, "bridge_pid")
        self.profile_path = _relative_path(self.profile_path, "profile_path")
        self.project_path = _relative_path(self.project_path, "project_path")
        _timestamp(self.started_at, "started_at")
        if self.ended_at is not None:
            _timestamp(self.ended_at, "ended_at")
        self.exit_code = _exit_code(self.exit_code, "exit_code")
        self.cleanup_result = _enum(self.cleanup_result, StepStatus, "cleanup_result")
        return self

    def to_dict(self) -> dict[str, Any]:
        return _as_dict(self.validate())

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "SessionRecord":
        _ensure_no_unknown(data, {"session_id", "generation", "client_pid", "bridge_pid", "profile_path", "project_path", "started_at", "ended_at", "exit_code", "cleanup_result"}, "SessionRecord")
        return cls(**dict(data)).validate()


@dataclass
class StepResult:
    stage: str
    status: StepStatus | str
    reason: str
    evidence_refs: list[str] = field(default_factory=list)
    exit_code: int | None = None

    def validate(self) -> "StepResult":
        _string(self.stage, "stage")
        self.status = _enum(self.status, StepStatus, "status")
        _string(self.reason, "reason")
        self.evidence_refs = _string_list(self.evidence_refs, "evidence_refs")
        self.exit_code = _exit_code(self.exit_code, "exit_code")
        return self

    def to_dict(self) -> dict[str, Any]:
        return _as_dict(self.validate())

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "StepResult":
        _ensure_no_unknown(data, {"stage", "status", "reason", "evidence_refs", "exit_code"}, "StepResult")
        return cls(**dict(data)).validate()


@dataclass
class J00Report:
    run_context: RunContext | Mapping[str, Any]
    capabilities: list[CapabilityObservation | Mapping[str, Any]]
    snapshots: list[ProjectSnapshot | Mapping[str, Any]] = field(default_factory=list)
    operations: list[OperationObservation | Mapping[str, Any]] = field(default_factory=list)
    artifacts: list[ArtifactRecord | Mapping[str, Any]] = field(default_factory=list)
    sessions: list[SessionRecord | Mapping[str, Any]] = field(default_factory=list)
    stages: list[StepResult | Mapping[str, Any]] = field(default_factory=list)
    isolation: dict[str, Any] = field(default_factory=dict)
    resource_observations: list[dict[str, Any]] = field(default_factory=list)
    redacted_log_refs: list[str] = field(default_factory=list)
    exit_conclusion: str = ""

    def validate(self) -> "J00Report":
        self.run_context = self.run_context if isinstance(self.run_context, RunContext) else RunContext.from_dict(self.run_context)
        self.capabilities = [item if isinstance(item, CapabilityObservation) else CapabilityObservation.from_dict(item) for item in self.capabilities]
        self.snapshots = [item if isinstance(item, ProjectSnapshot) else ProjectSnapshot.from_dict(item) for item in self.snapshots]
        self.operations = [item if isinstance(item, OperationObservation) else OperationObservation.from_dict(item) for item in self.operations]
        self.artifacts = [item if isinstance(item, ArtifactRecord) else ArtifactRecord.from_dict(item) for item in self.artifacts]
        self.sessions = [item if isinstance(item, SessionRecord) else SessionRecord.from_dict(item) for item in self.sessions]
        self.stages = [item if isinstance(item, StepResult) else StepResult.from_dict(item) for item in self.stages]
        self.isolation = _mapping(self.isolation, "isolation")
        if not isinstance(self.resource_observations, list) or any(not isinstance(item, Mapping) for item in self.resource_observations):
            raise ContractError("resource_observations must be an array of objects")
        self.redacted_log_refs = _string_list(self.redacted_log_refs, "redacted_log_refs")
        _string(self.exit_conclusion, "exit_conclusion")
        return self

    def to_dict(self) -> dict[str, Any]:
        return _as_dict(self.validate())

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "J00Report":
        _ensure_no_unknown(data, {"run_context", "capabilities", "snapshots", "operations", "artifacts", "sessions", "stages", "isolation", "resource_observations", "redacted_log_refs", "exit_conclusion"}, "J00Report")
        if not isinstance(data.get("capabilities"), list):
            raise ContractError("capabilities is required and must be an array")
        return cls(**dict(data)).validate()


def redact(value: Any) -> Any:
    """Return a JSON-safe copy with common credential values removed."""
    if isinstance(value, Mapping):
        return {str(key): (_REDACTED if str(key).lower() in _SENSITIVE_KEYS else redact(item)) for key, item in value.items()}
    if isinstance(value, list):
        return [redact(item) for item in value]
    if isinstance(value, tuple):
        return [redact(item) for item in value]
    if isinstance(value, Enum):
        return value.value
    if is_dataclass(value):
        return redact(_as_dict(value))
    return value


T = TypeVar("T")


def to_json(value: Any, *, indent: int | None = None) -> str:
    """Serialize and validate a record or report as redacted JSON."""
    if hasattr(value, "to_dict"):
        payload = value.to_dict()
    elif isinstance(value, Mapping):
        payload = dict(value)
    else:
        raise ContractError(f"cannot serialize {type(value)!r}")
    return json.dumps(redact(payload), sort_keys=True, indent=indent, ensure_ascii=True)


def from_json(payload: str, record_type: type[T]) -> T:
    try:
        data = json.loads(payload)
    except json.JSONDecodeError as exc:
        raise ContractError("invalid JSON") from exc
    if not isinstance(data, Mapping) or not hasattr(record_type, "from_dict"):
        raise ContractError("record JSON must be an object and record_type must support from_dict")
    return record_type.from_dict(data)  # type: ignore[attr-defined, no-any-return]


def validate_json(payload: str, record_type: type[T] | None = None) -> dict[str, Any]:
    """Validate a JSON object, optionally against one of the typed records."""
    try:
        data = json.loads(payload)
    except json.JSONDecodeError as exc:
        raise ContractError("invalid JSON") from exc
    if not isinstance(data, Mapping):
        raise ContractError("contract JSON must be an object")
    if record_type is not None:
        from_json(payload, record_type)
    return dict(data)


def sha256_bytes(data: bytes) -> str:
    """Small helper shared by fixture/fake implementations."""
    return hashlib.sha256(data).hexdigest()


__all__ = [
    "ArtifactRecord",
    "CapabilityObservation",
    "CapabilityStatus",
    "ContractError",
    "ErrorClass",
    "J00Report",
    "OperationObservation",
    "OperationResult",
    "ProjectSnapshot",
    "RunContext",
    "SessionRecord",
    "StepResult",
    "StepStatus",
    "from_json",
    "redact",
    "sha256_bytes",
    "to_json",
    "validate_json",
]
