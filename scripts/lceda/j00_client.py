"""Client probe adapter used by the J00 orchestration script.

The real client command names and response schemas are intentionally supplied
by the fixture manifest. This keeps the probe usable while Linux capability
discovery is still in progress and prevents arbitrary commands from becoming
the default model surface.
"""

from __future__ import annotations

import json
import os
import shlex
import uuid
from abc import ABC, abstractmethod
from pathlib import Path
from typing import Any, Mapping, Sequence

try:  # Supports both ``python scripts/lceda/j00_probe.py`` and package imports.
    from .j00_isolation import CommandResult, ProcessSupervisor, sha256_tree
except ImportError:  # pragma: no cover - direct script execution path
    from j00_isolation import CommandResult, ProcessSupervisor, sha256_tree


class ProbeUnavailable(RuntimeError):
    pass


class ClientProbe(ABC):
    @abstractmethod
    def discover(self) -> list[dict[str, Any]]: ...

    @abstractmethod
    def start(self, profile: Path, project: Path) -> dict[str, Any]: ...

    @abstractmethod
    def identify(self, session: Mapping[str, Any]) -> dict[str, Any]: ...

    @abstractmethod
    def observe(self, session: Mapping[str, Any]) -> dict[str, Any]: ...

    @abstractmethod
    def apply_parameter(self, session: Mapping[str, Any], operation: Mapping[str, Any]) -> dict[str, Any]: ...

    @abstractmethod
    def save(self, session: Mapping[str, Any]) -> dict[str, Any]: ...

    @abstractmethod
    def reopen(self, profile: Path, project: Path) -> dict[str, Any]: ...

    @abstractmethod
    def check(self, session: Mapping[str, Any], kind: str) -> dict[str, Any]: ...

    @abstractmethod
    def export(self, session: Mapping[str, Any], kind: str, output_dir: Path) -> dict[str, Any]: ...

    @abstractmethod
    def close(self, session: Mapping[str, Any]) -> dict[str, Any]: ...


def _format_argv(template: Sequence[str], values: Mapping[str, Any]) -> list[str]:
    result: list[str] = []
    for item in template:
        result.append(item.format(**{key: str(value) for key, value in values.items()}))
    return result


def _command_result(result: CommandResult) -> dict[str, Any]:
    return {
        "argv": result.argv,
        "returncode": result.returncode,
        "stdout": result.stdout,
        "stderr": result.stderr,
        "timed_out": result.timed_out,
        "duration_ms": result.duration_ms,
        "pid": result.pid,
    }


def _parse_stdout(result: CommandResult) -> dict[str, Any]:
    try:
        payload = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise ProbeUnavailable(f"client returned non-JSON output: {exc}") from exc
    if not isinstance(payload, dict):
        raise ProbeUnavailable("client response must be a JSON object")
    return payload


class CommandClientProbe(ClientProbe):
    def __init__(
        self,
        manifest: Mapping[str, Any],
        *,
        workspace_root: Path,
        supervisor: ProcessSupervisor | None = None,
        timeout: float = 60.0,
    ):
        self.manifest = manifest
        self.workspace_root = workspace_root.resolve()
        self.supervisor = supervisor or ProcessSupervisor(default_timeout=timeout)
        self.timeout = timeout
        self.commands = manifest.get("commands", {})
        if not isinstance(self.commands, Mapping):
            self.commands = {}
        self.current: dict[str, Any] | None = None
        self.generation = 0

    def _command(self, name: str, values: Mapping[str, Any]) -> CommandResult:
        template = self.commands.get(name)
        if not isinstance(template, list) or not template or any(not isinstance(item, str) for item in template):
            raise ProbeUnavailable(f"command is not configured: {name}")
        return self.supervisor.run(_format_argv(template, values), cwd=self.workspace_root, timeout=self.timeout)

    def discover(self) -> list[dict[str, Any]]:
        client = self.manifest.get("client", {})
        source = self.manifest.get("source", {})
        source_url = client.get("source_url") or client.get("url") or source.get("url")
        version = client.get("version") or client.get("version_candidate")
        configured = bool(source_url and version and client.get("installer_sha256"))
        capabilities: list[dict[str, Any]] = []
        if not configured:
            reason = "client source/version/installer digest is not pinned"
            for capability in ("client.discover", "client.cli", "client.mcp", "client.hidden_window"):
                capabilities.append({"capability_id": capability, "status": "unverified", "stage": "discover", "reason": reason})
            return capabilities
        probe = self.commands.get("discover")
        if not probe:
            return [{"capability_id": "client.discover", "status": "unavailable", "stage": "discover", "reason": "discover command is not configured"}]
        result = self._command("discover", {})
        if not result.ok:
            return [{"capability_id": "client.discover", "status": "unavailable", "stage": "discover", "reason": result.stderr or "discover command failed", "exit_code": result.returncode}]
        try:
            payload = _parse_stdout(result)
        except ProbeUnavailable as exc:
            return [{"capability_id": "client.discover", "status": "unavailable", "stage": "discover", "reason": str(exc), "exit_code": result.returncode}]
        return list(payload.get("capabilities", [{"capability_id": "client.discover", "status": "verified", "stage": "discover", "reason": "command returned valid JSON"}]))

    def _session(self, profile: Path, project: Path, result: CommandResult) -> dict[str, Any]:
        self.generation += 1
        session_id = f"lceda-{uuid.uuid4().hex[:12]}"
        session = {
            "session_id": session_id,
            "generation": self.generation,
            "client_pid": result.pid,
            "bridge_pid": None,
            "profile": str(profile),
            "project": str(project),
            "returncode": result.returncode,
            "command": _command_result(result),
        }
        self.current = session
        return session

    def start(self, profile: Path, project: Path) -> dict[str, Any]:
        result = self._command("start", {"profile": profile, "project": project, "session": "new"})
        if not result.ok:
            raise ProbeUnavailable(f"client start failed: {result.stderr or result.returncode}")
        return self._session(profile, project, result)

    def _read_project_json(self, project: Path, session: Mapping[str, Any]) -> dict[str, Any]:
        entry = str(self.manifest.get("project", {}).get("entry", "project.json"))
        path = project / entry
        if not path.is_file():
            raise ProbeUnavailable(f"project entry is missing: {entry}")
        try:
            document = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise ProbeUnavailable(f"project entry is not readable JSON: {exc}") from exc
        return snapshot_from_document(project, entry, document, str(session.get("session_id", "unknown")), self.workspace_root)

    def _response_or_snapshot(self, name: str, session: Mapping[str, Any]) -> dict[str, Any]:
        values = {"profile": session.get("profile", ""), "project": session.get("project", ""), "session": session.get("session_id", "")}
        try:
            result = self._command(name, values)
        except ProbeUnavailable:
            return self._read_project_json(Path(str(session["project"])), session)
        if not result.ok:
            raise ProbeUnavailable(f"client {name} failed: {result.stderr or result.returncode}")
        payload = _parse_stdout(result)
        return payload.get("snapshot", payload)

    def identify(self, session: Mapping[str, Any]) -> dict[str, Any]:
        return self._response_or_snapshot("identify", session)

    def observe(self, session: Mapping[str, Any]) -> dict[str, Any]:
        return self._response_or_snapshot("observe", session)

    def apply_parameter(self, session: Mapping[str, Any], operation: Mapping[str, Any]) -> dict[str, Any]:
        values = {"profile": session.get("profile", ""), "project": session.get("project", ""), "session": session.get("session_id", "")}
        values.update({key: operation.get(key, "") for key in ("object_id", "path", "old_value", "new_value")})
        try:
            result = self._command("apply_parameter", values)
        except ProbeUnavailable:
            return {"result": "unavailable", "reason": "parameter command is not configured"}
        payload = _command_result(result)
        if result.timed_out:
            payload.update({"result": "unknown", "reason": "parameter command timed out"})
        elif not result.ok:
            payload.update({"result": "failed", "reason": result.stderr or "parameter command failed"})
        else:
            try:
                payload.update(_parse_stdout(result))
            except ProbeUnavailable as exc:
                payload.update({"result": "unknown", "reason": str(exc)})
        return payload

    def save(self, session: Mapping[str, Any]) -> dict[str, Any]:
        try:
            result = self._command("save", {"profile": session.get("profile", ""), "project": session.get("project", ""), "session": session.get("session_id", "")})
        except ProbeUnavailable as exc:
            return {"status": "unavailable", "reason": str(exc)}
        return {"status": "passed" if result.ok else "failed", "command": _command_result(result)}

    def reopen(self, profile: Path, project: Path) -> dict[str, Any]:
        return self.start(profile, project)

    def check(self, session: Mapping[str, Any], kind: str) -> dict[str, Any]:
        try:
            result = self._command(f"check_{kind}", {"profile": session.get("profile", ""), "project": session.get("project", ""), "session": session.get("session_id", "")})
        except ProbeUnavailable as exc:
            return {"kind": kind, "status": "unavailable", "reason": str(exc)}
        if not result.ok:
            return {"kind": kind, "status": "failed", "reason": result.stderr or "check command failed", "exit_code": result.returncode}
        try:
            payload = _parse_stdout(result)
        except ProbeUnavailable as exc:
            return {"kind": kind, "status": "unavailable", "reason": str(exc)}
        payload.setdefault("kind", kind)
        payload.setdefault("status", "passed")
        return payload

    def export(self, session: Mapping[str, Any], kind: str, output_dir: Path) -> dict[str, Any]:
        try:
            result = self._command(f"export_{kind}", {"profile": session.get("profile", ""), "project": session.get("project", ""), "session": session.get("session_id", ""), "output": output_dir})
        except ProbeUnavailable as exc:
            return {"kind": kind, "status": "unavailable", "reason": str(exc)}
        if not result.ok:
            return {"kind": kind, "status": "failed", "reason": result.stderr or "export command failed", "exit_code": result.returncode}
        try:
            payload = _parse_stdout(result)
        except ProbeUnavailable as exc:
            return {"kind": kind, "status": "unavailable", "reason": str(exc)}
        payload.setdefault("kind", kind)
        payload.setdefault("status", "passed")
        return payload

    def close(self, session: Mapping[str, Any]) -> dict[str, Any]:
        try:
            result = self._command("close", {"profile": session.get("profile", ""), "project": session.get("project", ""), "session": session.get("session_id", "")})
        except ProbeUnavailable as exc:
            return {"status": "unavailable", "reason": str(exc)}
        return {"status": "passed" if result.ok else "failed", "command": _command_result(result)}


def snapshot_from_document(project: Path, entry: str, document: Mapping[str, Any], session_id: str, workspace_root: Path) -> dict[str, Any]:
    project = project.resolve()
    logical_root = project.name
    identity = document.get("project", document.get("identity", {}))
    components = document.get("components", [])
    parameters = document.get("parameters", [])
    nets = document.get("nets", [])
    pins = sum(len(item.get("pins", [])) for item in components if isinstance(item, Mapping))
    digest = sha256_tree(project)
    entry_path = f"{logical_root}/{entry}"
    return {
        "project_root": logical_root,
        "entrypoint": entry_path,
        "format": str(document.get("format", "eprj3")),
        "file_manifest_sha256": digest,
        "identity": identity if isinstance(identity, Mapping) else {"value": identity},
        "snapshot_sha256": digest,
        "source_session": session_id,
        "component_count": len(components) if isinstance(components, list) else 0,
        "parameter_count": len(parameters) if isinstance(parameters, list) else 0,
        "pin_count": pins,
        "network_count": len(nets) if isinstance(nets, list) else 0,
        "document_scope": [entry_path],
    }


def load_manifest(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))
