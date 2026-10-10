"""Evidence, artifact validation and redacted reporting for J00."""

from __future__ import annotations

import hashlib
import json
import mimetypes
import os
import re
from pathlib import Path
from typing import Any, Iterable, Mapping


REDACT_PATTERNS = (
    re.compile(r"(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+"),
    re.compile(r"(?i)(token|secret|password|api[_-]?key)(\s*[:=]\s*)[^\s,;]+"),
)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def redact_text(value: str, secrets: Iterable[str] = ()) -> str:
    result = value
    for secret in secrets:
        if secret and len(secret) >= 4:
            result = result.replace(secret, "[credential redacted]")
    for pattern in REDACT_PATTERNS:
        result = pattern.sub(lambda match: match.group(1) + "[credential redacted]", result)
    return result


def redact(value: Any, secrets: Iterable[str] = ()) -> Any:
    if isinstance(value, str):
        return redact_text(value, secrets)
    if isinstance(value, Mapping):
        return {str(key): redact(item, secrets) for key, item in value.items()}
    if isinstance(value, list):
        return [redact(item, secrets) for item in value]
    if isinstance(value, tuple):
        return [redact(item, secrets) for item in value]
    return value


def artifact_record(path: Path, *, kind: str, source_digest: str | None = None) -> dict[str, Any]:
    if not path.is_file():
        raise FileNotFoundError(path)
    size = path.stat().st_size
    if size == 0:
        raise ValueError(f"artifact is empty: {path}")
    mime = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
    return {
        "kind": kind,
        "path": path.as_posix(),
        "mime": mime,
        "size": size,
        "sha256": sha256_file(path),
        "source_digest": source_digest,
        "valid": True,
    }


def validate_artifact(record: Mapping[str, Any], *, current_source_digest: str | None = None) -> tuple[bool, str]:
    path = Path(str(record.get("path", "")))
    if not path.is_file():
        return False, "artifact file is missing"
    if path.stat().st_size == 0:
        return False, "artifact file is empty"
    actual = sha256_file(path)
    if record.get("sha256") != actual:
        return False, "artifact digest changed"
    source = record.get("source_digest")
    if current_source_digest is not None and source != current_source_digest:
        return False, "artifact source digest is stale"
    return True, "ok"


def write_json(path: Path, value: Any, *, secrets: Iterable[str] = ()) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(redact(value, secrets), indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )


def markdown_summary(report: Mapping[str, Any]) -> str:
    context = report.get("context", report.get("run_context", {}))
    capabilities = report.get("capabilities", [])
    lines = [
        "# J00 Compatibility Report",
        "",
        f"- Run: `{context.get('run_id', 'unknown')}`",
        f"- Commit: `{context.get('commit_sha', 'unknown')}`",
        f"- Client: `{context.get('client_version', 'unknown')}`",
        f"- Fixture: `{context.get('fixture_digest', 'unknown')}`",
        "",
        "## Capabilities",
        "",
        "| Capability | State | Reason |",
        "|---|---|---|",
    ]
    for item in capabilities:
        lines.append(
            f"| `{item.get('id', 'unknown')}` | `{item.get('state', 'unverified')}` | {item.get('reason', '')} |"
        )
    exit_result = report.get("exit", {})
    exit_state = exit_result.get("state") if isinstance(exit_result, Mapping) else None
    if not exit_state:
        exit_state = report.get("exit_conclusion", "unverified")
    exit_reason = exit_result.get("reason", "") if isinstance(exit_result, Mapping) else ""
    lines.extend(
        [
            "",
            "## Exit",
            "",
            f"- Result: `{exit_state}`",
            f"- Reason: {exit_reason}",
        ]
    )
    return "\n".join(lines) + "\n"


def write_report(
    output_dir: Path,
    report: Mapping[str, Any],
    *,
    secrets: Iterable[str] = (),
    logs: Mapping[str, str] | None = None,
) -> dict[str, Path]:
    output_dir.mkdir(parents=True, exist_ok=True)
    clean_report = redact(report, secrets)
    report_path = output_dir / "j00-report.json"
    report_path.write_text(json.dumps(clean_report, indent=2, sort_keys=True, ensure_ascii=False) + "\n", encoding="utf-8")
    summary_path = output_dir / "compatibility-report.md"
    summary_path.write_text(markdown_summary(clean_report), encoding="utf-8")
    matrix_path = output_dir / "capability-matrix.json"
    matrix_path.write_text(json.dumps(capability_matrix(clean_report), indent=2, sort_keys=True, ensure_ascii=False) + "\n", encoding="utf-8")
    paths = {"report": report_path, "summary": summary_path, "matrix": matrix_path}
    if logs:
        for name, content in logs.items():
            safe_name = Path(name).name
            log_path = output_dir / safe_name
            log_path.write_text(redact_text(content, secrets), encoding="utf-8")
            paths[f"log:{safe_name}"] = log_path
    return paths


def capability_matrix(report: Mapping[str, Any]) -> list[dict[str, Any]]:
    return [
        {
            "id": item.get("id"),
            "state": item.get("state", "unverified"),
            "reason": item.get("reason", ""),
            "evidence": item.get("evidence", []),
        }
        for item in report.get("capabilities", [])
    ]
