"""Isolation and process lifecycle helpers for the J00 probe.

The module deliberately has no Stable runtime dependency. It is safe to use
from local contract tests and from the GitHub Actions probe.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import signal
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterable, Mapping, Sequence


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def sha256_tree(root: Path, *, exclude: Iterable[str] = ()) -> str:
    """Return a stable digest of regular files below *root*."""

    root = root.resolve()
    excluded = {Path(item).as_posix().strip("/") for item in exclude}
    digest = hashlib.sha256()
    if not root.exists():
        return digest.hexdigest()
    for path in sorted(root.rglob("*")):
        if not path.is_file() or path.is_symlink():
            continue
        relative = path.relative_to(root).as_posix()
        if any(relative == item or relative.startswith(item + "/") for item in excluded):
            continue
        digest.update(relative.encode("utf-8"))
        digest.update(b"\0")
        digest.update(bytes.fromhex(sha256_file(path)))
        digest.update(b"\n")
    return digest.hexdigest()


def contained_path(root: Path, relative: str) -> Path:
    """Resolve a relative path and reject traversal or absolute paths."""

    candidate = Path(relative)
    if candidate.is_absolute():
        raise ValueError(f"absolute path is not allowed: {relative}")
    resolved_root = root.resolve()
    resolved = (resolved_root / candidate).resolve()
    try:
        resolved.relative_to(resolved_root)
    except ValueError as exc:
        raise ValueError(f"path escapes root: {relative}") from exc
    return resolved


def copy_candidate(formal_root: Path, candidate_root: Path) -> None:
    formal_root = formal_root.resolve()
    candidate_root = candidate_root.resolve()
    if not formal_root.is_dir():
        raise FileNotFoundError(f"formal project is not a directory: {formal_root}")
    links = [path for path in formal_root.rglob("*") if path.is_symlink()]
    if links:
        raise ValueError(f"formal project contains unsupported symlinks: {links[0].relative_to(formal_root)}")
    if candidate_root.exists():
        raise FileExistsError(f"candidate already exists: {candidate_root}")
    candidate_root.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(formal_root, candidate_root, symlinks=True)


def _mode_snapshot(root: Path) -> dict[str, int]:
    modes: dict[str, int] = {}
    for path in [root, *root.rglob("*")]:
        try:
            modes[path.relative_to(root).as_posix() if path != root else "."] = path.stat().st_mode & 0o777
        except FileNotFoundError:
            continue
    return modes


def set_read_only(root: Path) -> dict[str, int]:
    """Make a project tree read-only and return modes for restoration."""

    modes = _mode_snapshot(root)
    for path in [root, *root.rglob("*")]:
        if path.is_symlink():
            continue
        try:
            current = path.stat().st_mode & 0o777
        except FileNotFoundError:
            continue
        if path.is_dir():
            path.chmod(current & ~0o222 | 0o555)
        elif not path.is_symlink():
            path.chmod(current & ~0o222 | 0o444)
    return modes


def restore_modes(root: Path, modes: Mapping[str, int]) -> None:
    for relative, mode in modes.items():
        path = root if relative == "." else root / relative
        try:
            path.chmod(mode)
        except FileNotFoundError:
            continue


@dataclass
class CommandResult:
    argv: list[str]
    returncode: int | None
    stdout: str
    stderr: str
    timed_out: bool = False
    duration_ms: int = 0
    pid: int | None = None

    @property
    def ok(self) -> bool:
        return self.returncode == 0 and not self.timed_out


class ProcessSupervisor:
    """Run one process at a time and terminate its process group on failure."""

    def __init__(self, *, default_timeout: float = 60.0, env: Mapping[str, str] | None = None):
        self.default_timeout = default_timeout
        self.env = dict(env or {})
        self.children: set[int] = set()

    def run(
        self,
        argv: Sequence[str],
        *,
        cwd: Path | None = None,
        env: Mapping[str, str] | None = None,
        timeout: float | None = None,
        input_text: str | None = None,
    ) -> CommandResult:
        merged = os.environ.copy()
        merged.update(self.env)
        if env:
            merged.update(env)
        started = time.monotonic()
        process = subprocess.Popen(
            list(argv),
            cwd=str(cwd) if cwd else None,
            env=merged,
            stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            start_new_session=True,
        )
        self.children.add(process.pid)
        timed_out = False
        try:
            stdout, stderr = process.communicate(
                input=input_text,
                timeout=self.default_timeout if timeout is None else timeout,
            )
        except subprocess.TimeoutExpired as exc:
            timed_out = True
            self.terminate_pid(process.pid)
            stdout, stderr = process.communicate()
            stdout = stdout or exc.stdout or ""
            stderr = stderr or exc.stderr or ""
        finally:
            self.children.discard(process.pid)
        return CommandResult(
            argv=list(argv),
            returncode=process.returncode,
            stdout=stdout,
            stderr=stderr,
            timed_out=timed_out,
            duration_ms=int((time.monotonic() - started) * 1000),
            pid=process.pid,
        )

    @staticmethod
    def terminate_pid(pid: int, *, grace: float = 2.0) -> bool:
        def gone() -> bool:
            try:
                waited, _ = os.waitpid(pid, os.WNOHANG)
                if waited == pid:
                    return True
            except ChildProcessError:
                pass
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                return True
            return False

        try:
            os.killpg(pid, signal.SIGTERM)
        except ProcessLookupError:
            return True
        deadline = time.monotonic() + grace
        while time.monotonic() < deadline:
            if gone():
                return True
            time.sleep(0.05)
        try:
            os.killpg(pid, signal.SIGKILL)
        except ProcessLookupError:
            return True
        deadline = time.monotonic() + 1.0
        while time.monotonic() < deadline:
            if gone():
                return True
            time.sleep(0.05)
        return gone()

    def terminate_tree(self) -> list[int]:
        remaining: list[int] = []
        for pid in list(self.children):
            if not self.terminate_pid(pid):
                remaining.append(pid)
            self.children.discard(pid)
        return remaining


@dataclass
class IsolationResult:
    formal_before: str
    formal_after: str | None = None
    candidate_digest: str | None = None
    candidate_root: str | None = None
    formal_unchanged: bool = False
    candidate_distinct: bool = False
    cleanup_ok: bool = False
    offline_state: str = "unverified"
    errors: list[str] = field(default_factory=list)


class IsolationVerifier:
    def __init__(
        self,
        formal_root: Path,
        candidate_root: Path,
        *,
        profile_root: Path | None = None,
        protect_formal: bool = False,
    ):
        self.formal_root = formal_root.resolve()
        self.candidate_root = candidate_root.resolve()
        self.profile_root = profile_root.resolve() if profile_root else None
        self.protect_formal = protect_formal
        self._formal_modes: dict[str, int] | None = None
        self.result: IsolationResult | None = None

    def prepare(self) -> IsolationResult:
        if self.formal_root == self.candidate_root:
            raise ValueError("formal and candidate roots must differ")
        formal_before = sha256_tree(self.formal_root)
        copy_candidate(self.formal_root, self.candidate_root)
        if self.profile_root:
            self.profile_root.mkdir(parents=True, exist_ok=True)
        if self.protect_formal:
            self._formal_modes = set_read_only(self.formal_root)
        self.result = IsolationResult(
            formal_before=formal_before,
            candidate_digest=sha256_tree(self.candidate_root),
            candidate_root=str(self.candidate_root),
            # A freshly copied candidate is expected to have the same content
            # digest as the formal root. Identity is established by the
            # separate path/profile/session checks below.
            candidate_distinct=self.candidate_root != self.formal_root,
        )
        return self.result

    def verify_formal_unchanged(self) -> bool:
        if self.result is None:
            raise RuntimeError("prepare must be called first")
        self.result.formal_after = sha256_tree(self.formal_root)
        self.result.formal_unchanged = self.result.formal_after == self.result.formal_before
        if not self.result.formal_unchanged:
            self.result.errors.append("formal project digest changed")
        return self.result.formal_unchanged

    def verify_candidate_identity(self, expected_identity: str | None = None) -> bool:
        if self.result is None:
            raise RuntimeError("prepare must be called first")
        if not self.candidate_root.exists() or self.candidate_root == self.formal_root:
            self.result.errors.append("candidate root is missing or aliases formal root")
            self.result.candidate_distinct = False
            return False
        if expected_identity is not None:
            identity_file = self.candidate_root / ".j00-project-identity"
            if not identity_file.exists() or identity_file.read_text(encoding="utf-8").strip() != expected_identity:
                self.result.errors.append("candidate identity does not match expected identity")
                return False
        return self.result.candidate_distinct

    def set_offline_state(self, state: str) -> None:
        if self.result is None:
            raise RuntimeError("prepare must be called first")
        if state not in {"verified", "limited", "unavailable", "unverified"}:
            raise ValueError(f"unknown offline state: {state}")
        self.result.offline_state = state

    def finalize(self, supervisor: ProcessSupervisor | None = None) -> IsolationResult:
        if self.result is None:
            raise RuntimeError("prepare must be called first")
        remaining = supervisor.terminate_tree() if supervisor else []
        if remaining:
            self.result.errors.append(f"processes still alive: {remaining}")
        if self._formal_modes is not None:
            restore_modes(self.formal_root, self._formal_modes)
        self.result.cleanup_ok = not remaining
        return self.result


def write_isolation_result(path: Path, result: IsolationResult) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(result.__dict__, indent=2, sort_keys=True) + "\n", encoding="utf-8")
