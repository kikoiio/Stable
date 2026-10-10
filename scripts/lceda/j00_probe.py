#!/usr/bin/env python3
"""Run the J00 compatibility probe.

The default local mode uses the repository substitute fixture and reports the
official-client capabilities as unverified. A real run is enabled only when a
manifest contains pinned client metadata and command templates.
"""

from __future__ import annotations

import argparse
from dataclasses import asdict, is_dataclass
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile
from typing import Any, Mapping

try:
    from .j00_client import CommandClientProbe, ClientProbe, snapshot_from_document
    from .j00_contracts import (
        ArtifactRecord,
        CapabilityObservation,
        CapabilityStatus,
        ErrorClass,
        J00Report,
        OperationObservation,
        OperationResult,
        ProjectSnapshot,
        RunContext,
        SessionRecord,
        StepResult,
        StepStatus,
    )
    from .j00_evidence import artifact_record, write_report
    from .j00_isolation import IsolationVerifier, ProcessSupervisor, sha256_file, sha256_tree
except ImportError:  # pragma: no cover - direct script execution path
    from j00_client import CommandClientProbe, ClientProbe, snapshot_from_document
    from j00_contracts import (
        ArtifactRecord,
        CapabilityObservation,
        CapabilityStatus,
        ErrorClass,
        J00Report,
        OperationObservation,
        OperationResult,
        ProjectSnapshot,
        RunContext,
        SessionRecord,
        StepResult,
        StepStatus,
    )
    from j00_evidence import artifact_record, write_report
    from j00_isolation import IsolationVerifier, ProcessSupervisor, sha256_file, sha256_tree


PROBE_VERSION = "j00-probe-v1"
ZERO_SHA = "0" * 64


def now_utc() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def record_dict(value: Any) -> dict[str, Any]:
    if hasattr(value, "to_dict"):
        return value.to_dict()
    if is_dataclass(value):
        return asdict(value)
    if isinstance(value, Mapping):
        return dict(value)
    raise TypeError(f"cannot convert record {type(value)!r}")


def load_json(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"expected object: {path}")
    return value


def verify_fixture(manifest: Mapping[str, Any], fixture_root: Path) -> tuple[str, dict[str, Any]]:
    project = manifest.get("project", {})
    allowed = project.get("allowed_files", [])
    if not isinstance(allowed, list):
        raise ValueError("manifest project.allowed_files must be an array")
    actual = sorted(path.relative_to(fixture_root).as_posix() for path in fixture_root.rglob("*") if path.is_file())
    if sorted(allowed) != actual:
        raise ValueError(f"fixture file set differs from manifest: expected {sorted(allowed)}, actual {actual}")
    for item in manifest.get("files", []):
        relative = item["path"]
        path = fixture_root / relative
        if not path.is_file():
            raise FileNotFoundError(path)
        digest = sha256_file(path)
        if digest != item["sha256"]:
            raise ValueError(f"fixture digest mismatch for {relative}")
        if path.stat().st_size != item["size_bytes"]:
            raise ValueError(f"fixture size mismatch for {relative}")
    return sha256_tree(fixture_root), load_json(fixture_root / str(project.get("entry", "project.json")))


def _step(stage: str, status: str, reason: str, *, exit_code: int | None = None, evidence: list[str] | None = None) -> dict[str, Any]:
    return StepResult(stage, status, reason, evidence or [], exit_code).to_dict()


def _capability(value: Any) -> dict[str, Any]:
    if hasattr(value, "to_dict"):
        value = value.to_dict()
    if not isinstance(value, Mapping):
        raise TypeError(f"invalid capability record: {type(value)!r}")
    data = dict(value)
    if "capability_id" not in data and "id" in data:
        data["capability_id"] = data.pop("id")
    data.setdefault("status", "unverified")
    data.setdefault("stage", "discover")
    data.setdefault("reason", "no reason recorded")
    return CapabilityObservation.from_dict(data).to_dict()


def _snapshot(value: Any) -> dict[str, Any]:
    if hasattr(value, "to_dict"):
        return value.to_dict()
    return ProjectSnapshot.from_dict(value).to_dict()


def _session(value: Any) -> dict[str, Any]:
    if hasattr(value, "to_dict"):
        return value.to_dict()
    data = dict(value)
    # Command adapters keep absolute paths for process execution, while the
    # persisted contract only accepts project-relative paths.
    if "profile_path" not in data:
        data["profile_path"] = Path(str(data.pop("profile", "profile"))).name or "profile"
    if "project_path" not in data:
        data["project_path"] = Path(str(data.pop("project", "candidate"))).name or "candidate"
    data.setdefault("started_at", now_utc())
    data.setdefault("ended_at", None)
    data.setdefault("exit_code", data.pop("returncode", None))
    data.setdefault("cleanup_result", "unknown")
    return SessionRecord.from_dict(data).to_dict()


def _call_start(backend: Any, profile: Path, project: Path) -> Any:
    try:
        return backend.start(profile=profile, project=project)
    except TypeError:
        return backend.start(str(profile), str(project))


def _call_observe(backend: Any, session: Any) -> Any:
    try:
        return backend.observe(session)
    except TypeError:
        return backend.observe()


def _call_identify(backend: Any, session: Any) -> Any:
    try:
        return backend.identify(session)
    except TypeError:
        return backend.identify()


def _call_apply(backend: Any, session: Any, operation: Mapping[str, Any]) -> Any:
    try:
        return backend.apply_parameter(session, operation)
    except TypeError:
        return backend.apply_parameter(
            operation.get("old_value"),
            operation.get("new_value"),
            target=f"{operation.get('object_id')}.{operation.get('path')}",
            operation_id="op-parameter-1",
        )


def _call_save(backend: Any, session: Any) -> Any:
    try:
        return backend.save(session)
    except TypeError:
        return backend.save()


def _call_reopen(backend: Any, profile: Path, project: Path) -> Any:
    try:
        return backend.reopen(profile=profile, project=project)
    except TypeError:
        return backend.reopen(str(project))


def _call_check(backend: Any, session: Any, kind: str) -> Any:
    try:
        return backend.check(session, kind)
    except TypeError:
        return backend.check(kind)


def _call_export(backend: Any, session: Any, kind: str, output: Path) -> Any:
    try:
        return backend.export(session, kind, output)
    except TypeError:
        return backend.export(kind, output)


def _call_close(backend: Any, session: Any) -> Any:
    try:
        return backend.close(session)
    except TypeError:
        return backend.close()


class SubstituteBackend(ClientProbe):
    """Local deterministic backend for contract smoke tests only."""

    def __init__(self, manifest: Mapping[str, Any], workspace_root: Path):
        self.manifest = manifest
        self.workspace_root = workspace_root
        self.session: dict[str, Any] | None = None
        self.generation = 0

    def discover(self) -> list[dict[str, Any]]:
        reason = "repository-local substitute; official client capability is unverified"
        return [
            {"capability_id": "client.discover", "status": "unverified", "stage": "discover", "reason": reason},
            {"capability_id": "project.read", "status": "limited", "stage": "discover", "reason": reason},
            {"capability_id": "project.write", "status": "unverified", "stage": "discover", "reason": reason},
        ]

    def start(self, profile: Path, project: Path) -> dict[str, Any]:
        self.generation += 1
        self.session = {
            "session_id": f"substitute-{self.generation}",
            "generation": self.generation,
            "client_pid": None,
            "bridge_pid": None,
            "profile": str(profile),
            "project": str(project),
            "started_at": now_utc(),
        }
        return self.session

    def _snapshot(self, session: Mapping[str, Any]) -> dict[str, Any]:
        project = Path(str(session["project"]))
        entry = str(self.manifest["project"]["entry"])
        document = load_json(project / entry)
        return snapshot_from_document(project, entry, document, str(session["session_id"]), self.workspace_root)

    def identify(self, session: Mapping[str, Any]) -> dict[str, Any]:
        return self._snapshot(session)

    def observe(self, session: Mapping[str, Any]) -> dict[str, Any]:
        return self._snapshot(session)

    def apply_parameter(self, session: Mapping[str, Any], operation: Mapping[str, Any]) -> dict[str, Any]:
        project = Path(str(session["project"]))
        entry = str(self.manifest["project"]["entry"])
        path = project / entry
        document = load_json(path)
        before = self._snapshot(session)
        target = str(operation["object_id"])
        key = str(operation["path"])
        expected = operation.get("old_value")
        updated = operation.get("new_value")
        actual = next((item.get(key) for item in document.get("parameters", []) if item.get("object_id") == target), None)
        if actual != expected:
            return {"result": "rejected", "error_class": "precondition", "exit_code": 2, "before_snapshot_ref": f"snapshot:{before['snapshot_sha256']}"}
        for item in document.get("parameters", []):
            if item.get("object_id") == target:
                item[key] = updated
        for component in document.get("components", []):
            if component.get("id") == target:
                component[key] = updated
        path.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
        after = self._snapshot(session)
        return {
            "result": "applied",
            "error_class": "none",
            "exit_code": 0,
            "before_snapshot_ref": f"snapshot:{before['snapshot_sha256']}",
            "after_snapshot_ref": f"snapshot:{after['snapshot_sha256']}",
            "generation": int(session["generation"]),
        }

    def save(self, session: Mapping[str, Any]) -> dict[str, Any]:
        return {"status": "passed", "reason": "substitute file write completed", "exit_code": 0}

    def reopen(self, profile: Path, project: Path) -> dict[str, Any]:
        return self.start(profile, project)

    def check(self, session: Mapping[str, Any], kind: str) -> dict[str, Any]:
        return {"kind": kind, "status": "unavailable", "reason": "official client is not present; substitute cannot prove this check"}

    def export(self, session: Mapping[str, Any], kind: str, output_dir: Path) -> dict[str, Any]:
        return {"kind": kind, "status": "unavailable", "reason": "official client is not present; substitute cannot produce an official export"}

    def close(self, session: Mapping[str, Any]) -> dict[str, Any]:
        return {"status": "passed", "reason": "substitute session closed", "exit_code": 0}


def run_probe(
    manifest_path: Path,
    *,
    output_dir: Path,
    backend_name: str = "auto",
    client_backend: Any | None = None,
) -> dict[str, Any]:
    manifest = load_json(manifest_path)
    fixture_root = manifest_path.parent / str(manifest["project"]["root"])
    fixture_digest, _ = verify_fixture(manifest, fixture_root)
    output_dir.mkdir(parents=True, exist_ok=True)
    work_root = output_dir / "work"
    candidate_root = work_root / fixture_root.name
    profile_root = output_dir / "profile"
    formal_root = fixture_root
    supervisor = ProcessSupervisor(default_timeout=float(manifest.get("limits", {}).get("operation_timeout_seconds", 60)))
    isolation = IsolationVerifier(formal_root, candidate_root, profile_root=profile_root, protect_formal=True)
    isolation.prepare()
    capabilities: list[dict[str, Any]] = []
    snapshots: list[dict[str, Any]] = []
    operations: list[dict[str, Any]] = []
    artifacts: list[dict[str, Any]] = []
    sessions: list[dict[str, Any]] = []
    stages: list[dict[str, Any]] = []
    backend: Any
    if client_backend is not None:
        backend = client_backend
    elif backend_name == "fake" or (backend_name == "auto" and manifest.get("status") == "substitute"):
        backend = SubstituteBackend(manifest, output_dir)
    else:
        backend = CommandClientProbe(manifest, workspace_root=output_dir, supervisor=supervisor)
    session: Any | None = None
    try:
        try:
            capabilities = [_capability(item) for item in backend.discover()]
            stages.append(_step("discover", "passed", "capability discovery completed"))
        except Exception as exc:
            stages.append(_step("discover", "failed", str(exc)))

        try:
            session = _call_start(backend, profile_root, candidate_root)
            sessions.append(_session(session))
            stages.append(_step("start", "passed", "candidate session started"))
            identified = _snapshot(_call_identify(backend, session))
            snapshots.append(identified)
            observed = _snapshot(_call_observe(backend, session))
            snapshots.append(observed)
            stages.append(_step("observe", "passed", "candidate identified and observed"))
        except Exception as exc:
            stages.append(_step("start", "failed", str(exc)))
            session = None

        parameter = manifest.get("probe", {}).get("parameter")
        if session is not None and isinstance(parameter, Mapping):
            try:
                operation_raw = _call_apply(backend, session, parameter)
                if isinstance(operation_raw, OperationObservation):
                    operation = operation_raw.to_dict()
                else:
                    operation = dict(operation_raw)
                    operation.setdefault("operation_id", "op-parameter-1")
                    operation.setdefault("target", f"{parameter.get('object_id')}.{parameter.get('path')}")
                    operation.setdefault("expected_old_value", parameter.get("old_value"))
                    operation.setdefault("normalized_new_value", parameter.get("new_value"))
                    operation.setdefault("generation", int(_session(session).get("generation", 0)))
                    operation.setdefault("before_snapshot_ref", snapshots[-1].get("snapshot_sha256", "before"))
                    operation.setdefault("after_snapshot_ref", None)
                    operation.setdefault("exit_code", None)
                    operation.setdefault("result", "unknown")
                    operation.setdefault("error_class", "unknown" if operation["result"] == "unknown" else "none")
                    operation = OperationObservation.from_dict(operation).to_dict()
                operations.append(operation)
                status = "passed" if operation.get("result") == "applied" else "failed"
                stages.append(_step("apply", status, f"parameter operation {operation.get('result')}"))
            except Exception as exc:
                stages.append(_step("apply", "failed", str(exc)))

            try:
                save_raw = _call_save(backend, session)
                save_dict = record_dict(save_raw)
                save_status = save_dict.get("status", save_dict.get("status", "unknown"))
                if save_status == "passed":
                    stages.append(_step("save", "passed", save_dict.get("reason", "candidate saved"), exit_code=save_dict.get("exit_code")))
                elif save_status in {"unavailable", "unknown"}:
                    stages.append(_step("save", "unavailable", save_dict.get("reason", "save unavailable")))
                else:
                    stages.append(_step("save", "failed", save_dict.get("reason", "save failed"), exit_code=save_dict.get("exit_code")))
            except Exception as exc:
                stages.append(_step("save", "failed", str(exc)))

            try:
                _call_close(backend, session)
                fresh = _call_reopen(backend, profile_root, candidate_root)
                sessions.append(_session(fresh))
                session = fresh
                reopened = _snapshot(_call_observe(backend, session))
                snapshots.append(reopened)
                stages.append(_step("reopen", "passed", "fresh session reopened candidate"))
            except Exception as exc:
                stages.append(_step("reopen", "failed", str(exc)))

        if session is not None:
            for kind in ("schematic_rules", "network", "parameters"):
                try:
                    check = record_dict(_call_check(backend, session, kind))
                    status = check.get("status", "unavailable")
                    stages.append(_step(f"check:{kind}", "passed" if status == "passed" else ("unavailable" if status == "unavailable" else "failed"), check.get("reason", status), exit_code=check.get("exit_code")))
                except Exception as exc:
                    stages.append(_step(f"check:{kind}", "failed", str(exc)))
            for kind in ("bom", "netlist", "screenshot", "report"):
                try:
                    exported = record_dict(_call_export(backend, session, kind, output_dir / "exports"))
                    status = exported.get("status", "unavailable")
                    stages.append(_step(f"export:{kind}", "passed" if status == "passed" else ("unavailable" if status == "unavailable" else "failed"), exported.get("reason", status), exit_code=exported.get("exit_code")))
                    record = exported.get("artifact")
                    if record:
                        artifacts.append(record)
                except Exception as exc:
                    stages.append(_step(f"export:{kind}", "failed", str(exc)))
            try:
                close_raw = record_dict(_call_close(backend, session))
                stages.append(_step("cleanup:client", "passed" if close_raw.get("status") == "passed" else "failed", close_raw.get("reason", "client close"), exit_code=close_raw.get("exit_code")))
            except Exception as exc:
                stages.append(_step("cleanup:client", "failed", str(exc)))
    finally:
        isolation.verify_formal_unchanged()
        isolation.verify_candidate_identity()
        isolation.finalize(supervisor)
        stages.append(_step("cleanup", "passed" if isolation.result and isolation.result.cleanup_ok else "failed", "isolation cleanup completed"))

    client = manifest.get("client", {})
    context = RunContext(
        run_id=os.environ.get("J00_RUN_ID", f"local-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S')}")[:128],
        commit_sha=os.environ.get("GITHUB_SHA", ZERO_SHA),
        runner_image=os.environ.get("ImageOS", "local"),
        runner_arch=os.uname().machine,
        client_version=str(client.get("version") or client.get("version_candidate") or "unverified"),
        client_sha256=str(client.get("installer_sha256") or ZERO_SHA),
        probe_version=PROBE_VERSION,
        fixture_sha256=fixture_digest,
        started_at=now_utc(),
        ended_at=now_utc(),
        env_summary={"backend": backend_name, "manifest_status": manifest.get("status", "unknown")},
    ).validate()
    if isolation.result:
        isolation_dict = isolation.result.__dict__.copy()
    else:
        isolation_dict = {}
    official = manifest.get("status") != "substitute" and client.get("confirmed") is True
    successful = official and any(item.get("stage") == "reopen" and item.get("status") == "passed" for item in stages) and isolation_dict.get("formal_unchanged") and isolation_dict.get("cleanup_ok")
    exit_conclusion = "verified" if successful else "unverified: official client, checks, exports, or isolation evidence is incomplete"
    report = J00Report(
        run_context=context,
        capabilities=capabilities,
        snapshots=snapshots,
        operations=operations,
        artifacts=artifacts,
        sessions=sessions,
        stages=stages,
        isolation=isolation_dict,
        resource_observations=[],
        redacted_log_refs=[],
        exit_conclusion=exit_conclusion,
    ).validate()
    report_dict = report.to_dict()
    write_report(output_dir / "evidence", report_dict)
    return report_dict


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, default=Path(__file__).resolve().parents[2] / "fixtures/lceda/j00/manifest.json")
    parser.add_argument("--output", type=Path, default=None)
    parser.add_argument("--backend", choices=("auto", "fake", "real"), default="auto")
    args = parser.parse_args()
    output = args.output or (Path(os.environ.get("J00_ARTIFACT_DIR", ".tmp/lceda-j00/local/artifacts")) / "probe")
    report = run_probe(args.manifest, output_dir=output, backend_name=args.backend)
    print(json.dumps({"exit_conclusion": report["exit_conclusion"], "evidence_dir": str(output / "evidence")}, sort_keys=True))
    return 0 if report["exit_conclusion"] == "verified" else 2


if __name__ == "__main__":
    raise SystemExit(main())
