#!/usr/bin/env python3
"""Cross-platform J00 runner preparation and launch checks.

This helper is intended for Windows and macOS workflow jobs.  It uses only
Python's standard library and deliberately does not require ``uname``, X11,
Xvfb, or a POSIX shell.  The Linux-specific ``j00_env.sh`` remains the entry
point for the existing Linux workflow.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import sys
import tarfile
from datetime import datetime, timezone
from typing import Any, Iterable
from urllib.parse import urlsplit
from urllib.error import HTTPError
from urllib.request import Request, urlopen
import uuid
import zipfile


SCRIPT_PATH = Path(__file__).resolve()
DEFAULT_REPO_ROOT = SCRIPT_PATH.parents[2]
RUN_ID_PATTERN = r"^[A-Za-z0-9_.-]+$"


class RunnerError(RuntimeError):
    """An expected, user-actionable runner preparation failure."""


class DownloadBlocked(RunnerError):
    """The public artifact endpoint requires access unavailable to a runner."""


def _is_safe_run_id(value: str) -> bool:
    return bool(re.fullmatch(RUN_ID_PATTERN, value))


def _resolved(path: Path) -> Path:
    return path.expanduser().resolve(strict=False)


def _within(child: Path, parent: Path) -> bool:
    child_resolved = _resolved(child)
    parent_resolved = _resolved(parent)
    return child_resolved == parent_resolved or parent_resolved in child_resolved.parents


def _require_within(child: Path, parent: Path, label: str) -> Path:
    resolved = _resolved(child)
    if not _within(resolved, parent):
        raise RunnerError(f"{label} must stay under {parent}: {child}")
    return resolved


def _default_run_id() -> str:
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    return f"{stamp}-{uuid.uuid4().hex[:8]}"


def _load_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except OSError as exc:
        raise RunnerError(f"cannot read JSON file {path}: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise RunnerError(f"invalid JSON file {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise RunnerError(f"JSON file must contain an object: {path}")
    return value


def _manifest_value(manifest: dict[str, Any], key: str) -> Any:
    value: Any = manifest
    for part in key.split("."):
        if not isinstance(value, dict) or part not in value:
            return None
        value = value[part]
    return value


def _client_metadata(
    manifest_path: Path, url: str | None, digest: str | None
) -> tuple[str | None, str | None, str | None]:
    manifest: dict[str, Any] = {}
    if manifest_path.is_file():
        manifest = _load_json(manifest_path)
    if not url:
        for key in (
            "client.download_url",
            "client.source_url",
            "client.url",
            "client.source.url",
            "client.artifact.url",
            "source.url",
            "download_url",
            "source_url",
        ):
            candidate = _manifest_value(manifest, key)
            if isinstance(candidate, str) and candidate:
                url = candidate
                break
    if not digest:
        for key in (
            "client.sha256",
            "client.sha256sum",
            "client.installer_sha256",
            "client.source.sha256",
            "client.artifact.sha256",
            "sha256",
            "sha256sum",
        ):
            candidate = _manifest_value(manifest, key)
            if isinstance(candidate, str) and candidate:
                digest = candidate
                break
    if not url:
        return None, None, "unverified: client URL is not configured in the manifest or arguments"
    if urlsplit(url).scheme.lower() != "https":
        return None, None, "blocked: client URL must use HTTPS"
    if not digest:
        return None, None, "unverified: client SHA-256 is not configured in the manifest or arguments"
    if not re.fullmatch(r"[A-Fa-f0-9]{64}", digest):
        return None, None, "blocked: client SHA-256 is not a 64-character hexadecimal digest"
    return url, digest.lower(), None


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def download_verified(url: str, digest: str, destination: Path) -> Path:
    """Download one HTTPS artifact, verify it, and atomically install it."""

    if urlsplit(url).scheme.lower() != "https":
        raise RunnerError("client URL must use HTTPS")
    if not re.fullmatch(r"[A-Fa-f0-9]{64}", digest):
        raise RunnerError("client SHA-256 must be a 64-character hexadecimal digest")
    destination = _resolved(destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    partial = destination.with_name(destination.name + ".part")
    request = Request(url, headers={"User-Agent": "Stable-J00-runner/1"})
    try:
        with urlopen(request, timeout=60) as response, partial.open("wb") as output:
            while True:
                chunk = response.read(1024 * 1024)
                if not chunk:
                    break
                output.write(chunk)
        if partial.stat().st_size == 0:
            raise RunnerError("downloaded client archive is empty")
        actual = _sha256(partial)
        if actual.lower() != digest.lower():
            raise RunnerError(f"client archive SHA-256 mismatch (expected {digest}, got {actual})")
        os.replace(partial, destination)
    except HTTPError as exc:
        partial.unlink(missing_ok=True)
        if exc.code in (401, 403, 422):
            raise DownloadBlocked(
                f"public client endpoint returned HTTP {exc.code}; authenticated downloads are disabled"
            ) from exc
        raise RunnerError(f"client download failed with HTTP {exc.code}: {exc.reason}") from exc
    except RunnerError:
        partial.unlink(missing_ok=True)
        raise
    except Exception as exc:
        partial.unlink(missing_ok=True)
        raise RunnerError(f"client download failed: {exc}") from exc
    return destination


def _safe_archive_member(root: Path, member_name: str) -> Path:
    target = _resolved(root / member_name)
    if not _within(target, root):
        raise RunnerError(f"archive member escapes extraction directory: {member_name}")
    return target


def extract_archive(archive: Path, destination: Path) -> Path:
    """Extract zip/tar archives while rejecting path traversal entries."""

    archive = _resolved(archive)
    destination = _resolved(destination)
    destination.mkdir(parents=True, exist_ok=True)
    try:
        if zipfile.is_zipfile(archive):
            with zipfile.ZipFile(archive) as bundle:
                for info in bundle.infolist():
                    _safe_archive_member(destination, info.filename)
                bundle.extractall(destination)
            return destination
        if tarfile.is_tarfile(archive):
            with tarfile.open(archive) as bundle:
                for info in bundle.getmembers():
                    _safe_archive_member(destination, info.name)
                    if info.issym() or info.islnk():
                        raise RunnerError(f"archive links are not allowed: {info.name}")
                try:
                    bundle.extractall(destination, filter="data")
                except TypeError:  # Python 3.11 and earlier do not expose ``filter``.
                    bundle.extractall(destination)
            return destination
    except (OSError, tarfile.TarError, zipfile.BadZipFile) as exc:
        raise RunnerError(f"cannot extract client archive {archive}: {exc}") from exc
    raise RunnerError(f"unsupported client archive format: {archive.name}")


def _paths_for_run(run_dir: Path) -> dict[str, Path]:
    return {
        "run_dir": run_dir,
        "artifact_dir": run_dir / "artifacts",
        "work_dir": run_dir / "work",
        "runtime_dir": run_dir / "runtime",
        "home": run_dir / "home",
        "profile_dir": run_dir / "profile",
        "xdg_config_home": run_dir / "xdg-config",
        "xdg_cache_home": run_dir / "xdg-cache",
        "xdg_data_home": run_dir / "xdg-data",
        "client_install_dir": run_dir / "client",
        "client_archive": run_dir / "client.download",
    }


def _resource_record(run_id: str, repo_root: Path, run_dir: Path, manifest: Path) -> dict[str, Any]:
    usage = shutil.disk_usage(repo_root)
    return {
        "schema_version": "j00-runner-v1",
        "run_id": run_id,
        "repository": str(repo_root),
        "run_dir": str(run_dir),
        "manifest": str(manifest),
        "runner": {
            "system": platform.system() or "unknown",
            "release": platform.release() or "unknown",
            "machine": platform.machine() or "unknown",
            "python": platform.python_version(),
        },
        "disk": {"total_bytes": usage.total, "free_bytes": usage.free},
        "headless_backend": "native-platform",
    }


def prepare(args: argparse.Namespace) -> dict[str, Any]:
    repo_root = _resolved(Path(args.repo_root))
    if not repo_root.is_dir():
        raise RunnerError(f"repository root does not exist: {repo_root}")
    run_id = args.run_id or os.environ.get("J00_RUN_ID") or _default_run_id()
    if not _is_safe_run_id(run_id):
        raise RunnerError("run id contains unsafe characters")
    tmp_root = _resolved(
        Path(args.tmp_root) if args.tmp_root else repo_root / ".tmp" / "lceda-j00-cross-platform"
    )
    _require_within(tmp_root, repo_root, "temporary root")
    run_dir = _require_within(tmp_root / run_id, repo_root, "run directory")
    if run_dir.exists():
        raise RunnerError(f"run directory already exists: {run_dir}")
    paths = _paths_for_run(run_dir)
    for key, path in paths.items():
        if key == "client_archive":
            continue
        path.mkdir(parents=True, exist_ok=True)
    manifest = _resolved(
        Path(args.manifest)
        if args.manifest
        else repo_root / "fixtures" / "lceda" / "j00" / "manifest.json"
    )
    record = _resource_record(run_id, repo_root, run_dir, manifest)
    if args.download_client:
        url, digest, metadata_status = _client_metadata(
            manifest,
            args.client_url or os.environ.get("J00_CLIENT_URL"),
            args.client_sha256 or os.environ.get("J00_CLIENT_SHA256"),
        )
        if metadata_status:
            status, _, reason = metadata_status.partition(": ")
            record["client"] = {"status": status, "reason": reason or metadata_status}
        else:
            assert url is not None and digest is not None
            try:
                archive = download_verified(url, digest, paths["client_archive"])
            except DownloadBlocked as exc:
                record["client"] = {"status": "blocked", "url": url, "reason": str(exc)}
            else:
                record["client"] = {
                    "status": "verified",
                    "url": url,
                    "sha256": digest,
                    "archive": str(archive),
                }
                if args.extract_client_to:
                    record["client"]["install_dir"] = str(extract_archive(archive, paths["client_install_dir"]))
    else:
        record["client"] = {
            "status": "unverified",
            "downloaded": False,
            "reason": "client download was not requested",
        }
    environment = {key: str(value) for key, value in paths.items()}
    environment.update({"run_id": run_id, "repo_root": str(repo_root), "headless_backend": "native-platform"})
    (run_dir / "environment.json").write_text(
        json.dumps(environment, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    (paths["artifact_dir"] / "environment.json").write_text(
        json.dumps(record, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return {"environment": environment, "resources": record}


def _check_writable(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True)
    probe = path / ".j00-write-check"
    try:
        probe.write_text("ok", encoding="ascii")
    finally:
        probe.unlink(missing_ok=True)


def _fixture_check(manifest_path: Path, repo_root: Path) -> dict[str, Any]:
    manifest = _load_json(manifest_path)
    project = manifest.get("project")
    if not isinstance(project, dict) or not isinstance(project.get("root"), str):
        raise RunnerError(f"manifest project.root is missing: {manifest_path}")
    project_root = _require_within(repo_root / project["root"], repo_root, "fixture root")
    if not project_root.is_dir():
        raise RunnerError(f"fixture root does not exist: {project_root}")
    checked = 0
    for item in manifest.get("files", []):
        if not isinstance(item, dict) or not isinstance(item.get("path"), str):
            raise RunnerError(f"invalid fixture file entry in {manifest_path}")
        path = _require_within(project_root / item["path"], project_root, "fixture file")
        if not path.is_file():
            raise RunnerError(f"fixture file does not exist: {path}")
        expected = item.get("sha256")
        if isinstance(expected, str) and _sha256(path).lower() != expected.lower():
            raise RunnerError(f"fixture digest mismatch: {path}")
        checked += 1
    return {"root": str(project_root), "files_checked": checked}


def check(args: argparse.Namespace) -> dict[str, Any]:
    run_dir = _resolved(Path(args.run_dir))
    environment_path = run_dir / "environment.json"
    if not environment_path.is_file():
        raise RunnerError(f"prepared environment is missing: {environment_path}")
    environment = _load_json(environment_path)
    for key in ("artifact_dir", "work_dir", "runtime_dir", "home", "profile_dir"):
        _check_writable(Path(str(environment[key])))
    client = str(args.client)
    client_path = Path(client).expanduser()
    if client_path.exists():
        if not client_path.is_file():
            raise RunnerError(f"client path is not a file: {client_path}")
        if os.name != "nt" and not os.access(client_path, os.X_OK):
            raise RunnerError(f"client is not executable: {client_path}")
        resolved_client = str(_resolved(client_path))
    else:
        resolved = shutil.which(client)
        if not resolved:
            raise RunnerError(f"client executable was not found: {client}")
        resolved_client = resolved
    result: dict[str, Any] = {
        "status": "ready",
        "run_dir": str(run_dir),
        "client": resolved_client,
        "headless_backend": environment.get("headless_backend", "native-platform"),
    }
    if args.project:
        project = _resolved(Path(args.project))
        if not project.exists():
            raise RunnerError(f"project path does not exist: {project}")
        result["project"] = str(project)
    if args.manifest:
        result["fixture"] = _fixture_check(_resolved(Path(args.manifest)), _resolved(Path(args.repo_root)))
    return result


def cleanup(args: argparse.Namespace) -> dict[str, Any]:
    run_dir = _resolved(Path(args.run_dir))
    environment_path = run_dir / "environment.json"
    if not environment_path.is_file():
        raise RunnerError(f"refusing cleanup without environment marker: {environment_path}")
    removed: list[str] = []
    for name in ("work", "profile", "home", "xdg-config", "xdg-cache", "xdg-data", "runtime", "client"):
        path = run_dir / name
        if path.is_dir() and not path.is_symlink():
            shutil.rmtree(path)
            removed.append(str(path))
    archive_part = run_dir / "client.download.part"
    if archive_part.exists():
        archive_part.unlink()
        removed.append(str(archive_part))
    return {"status": "cleaned", "run_dir": str(run_dir), "removed": removed}


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    prepare_parser = subparsers.add_parser("prepare", help="create an isolated project-local run directory")
    prepare_parser.add_argument("--repo-root", default=str(DEFAULT_REPO_ROOT), type=Path)
    prepare_parser.add_argument("--tmp-root", type=Path)
    prepare_parser.add_argument("--run-id")
    prepare_parser.add_argument("--manifest", type=Path)
    prepare_parser.add_argument("--download-client", action="store_true")
    prepare_parser.add_argument("--client-url")
    prepare_parser.add_argument("--client-sha256")
    prepare_parser.add_argument("--extract-client-to", action="store_true")
    download_parser = subparsers.add_parser("download", help="download and verify one HTTPS archive")
    download_parser.add_argument("--url", required=True)
    download_parser.add_argument("--sha256", required=True)
    download_parser.add_argument("--destination", required=True, type=Path)
    download_parser.add_argument("--extract-to", type=Path)
    check_parser = subparsers.add_parser("check", help="validate a prepared run before launching a client")
    check_parser.add_argument("--run-dir", required=True, type=Path)
    check_parser.add_argument("--client", required=True)
    check_parser.add_argument("--project", type=Path)
    check_parser.add_argument("--manifest", type=Path)
    check_parser.add_argument("--repo-root", default=str(DEFAULT_REPO_ROOT), type=Path)
    cleanup_parser = subparsers.add_parser("cleanup", help="remove transient state while retaining evidence")
    cleanup_parser.add_argument("--run-dir", required=True, type=Path)
    return parser


def main(argv: Iterable[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "prepare":
            result = prepare(args)
        elif args.command == "download":
            try:
                archive = download_verified(args.url, args.sha256, args.destination)
            except DownloadBlocked as exc:
                result = {"status": "blocked", "reason": str(exc)}
            else:
                result = {"status": "downloaded", "archive": str(archive), "sha256": _sha256(archive)}
            if args.extract_to and result["status"] == "downloaded":
                result["extract_dir"] = str(extract_archive(archive, args.extract_to))
        elif args.command == "check":
            result = check(args)
        else:
            result = cleanup(args)
    except RunnerError as exc:
        print(f"j00_runner: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
