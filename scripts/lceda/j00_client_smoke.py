#!/usr/bin/env python3
"""Run a small, isolated CLI smoke against the downloaded LCEDA Pro client."""

from __future__ import annotations

import os
from pathlib import Path
import platform
import subprocess
import sys

try:
    from .j00_isolation import ProcessSupervisor
except ImportError:  # direct script execution
    from j00_isolation import ProcessSupervisor


def _record(status: str, detail: str) -> None:
    artifact_dir = Path(os.environ["J00_ARTIFACT_DIR"])
    artifact_dir.mkdir(parents=True, exist_ok=True)
    with (artifact_dir / "environment.txt").open("a", encoding="utf-8") as handle:
        handle.write(f"client_smoke_status={status}\nclient_smoke_detail={detail}\n")


def _cli(executable: Path, env: dict[str, str], cwd: Path) -> str:
    result = ProcessSupervisor(default_timeout=90, env=env).run(
        [str(executable), "--help"], cwd=cwd, timeout=90
    )
    output = (result.stdout + "\n" + result.stderr).strip()
    if result.timed_out:
        raise RuntimeError("CLI help command timed out")
    if result.returncode != 0:
        detail = next((line.strip() for line in result.stderr.splitlines() if line.strip()), "")
        detail = detail.replace(str(Path(os.environ["J00_RUN_DIR"]).resolve()), "<run-dir>")[:180]
        suffix = f": {detail}" if detail else ""
        raise RuntimeError(f"CLI help command exited with {result.returncode}{suffix}")
    if "usage:" not in output.lower() or "commands:" not in output.lower():
        raise RuntimeError("CLI help output did not contain the documented command list")
    first_line = next((line.strip() for line in output.splitlines() if line.strip()), "help returned")
    return first_line[:160]


def _run() -> str:
    run_dir = Path(os.environ["J00_RUN_DIR"]).resolve()
    client_dir = Path(os.environ["J00_CLIENT_INSTALL_DIR"]).resolve()
    runtime_dir = Path(os.environ["J00_RUNTIME_DIR"]).resolve()
    home = Path(os.environ["J00_HOME"]).resolve()
    for path in (home, runtime_dir, client_dir):
        path.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env.update({
        "HOME": str(home),
        "USERPROFILE": str(home),
        "APPDATA": str(home / "AppData" / "Roaming"),
        "LOCALAPPDATA": str(home / "AppData" / "Local"),
        "TMP": str(runtime_dir),
        "TEMP": str(runtime_dir),
        "XDG_CONFIG_HOME": str(Path(os.environ.get("J00_XDG_CONFIG_HOME", home / "xdg-config"))),
        "XDG_CACHE_HOME": str(Path(os.environ.get("J00_XDG_CACHE_HOME", home / "xdg-cache"))),
        "XDG_DATA_HOME": str(Path(os.environ.get("J00_XDG_DATA_HOME", home / "xdg-data"))),
    })
    for key in ("XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"):
        Path(env[key]).mkdir(parents=True, exist_ok=True)

    if sys.platform.startswith("linux"):
        executable = client_dir / "lceda-pro" / "lceda-pro"
        if not executable.is_file():
            raise RuntimeError("Linux client executable was not found in the extracted package")
        executable.chmod(executable.stat().st_mode | 0o111)
        result = ProcessSupervisor(default_timeout=20, env=env).run(
            [str(executable), "--help", "--no-sandbox", "--disable-gpu", "--ozone-platform=x11"],
            cwd=executable.parent,
            timeout=20,
        )
        output = (result.stdout + "\n" + result.stderr).strip()
        if result.timed_out:
            return "client process remained active for 20s under Xvfb; --help did not exit"
        if result.returncode != 0:
            detail = next((line.strip() for line in result.stderr.splitlines() if line.strip()), "")
            detail = detail.replace(str(run_dir), "<run-dir>")[:180]
            suffix = f": {detail}" if detail else ""
            raise RuntimeError(f"CLI help command exited with {result.returncode}{suffix}")
        if "usage:" not in output.lower() or "commands:" not in output.lower():
            raise RuntimeError("CLI help output did not contain the documented command list")
        first_line = next((line.strip() for line in output.splitlines() if line.strip()), "help returned")
        return first_line[:160]

    if sys.platform == "darwin":
        images = [
            path for path in client_dir.rglob("*")
            if path.is_file() and path.suffix.lower() == ".dmg" and not path.name.startswith("._")
        ]
        if len(images) != 1:
            raise RuntimeError(f"expected one macOS application image, found {len(images)}")
        mount_dir = runtime_dir / "macos-image"
        mount_dir.mkdir(parents=True, exist_ok=True)
        mounted = False
        try:
            subprocess.run(
                ["hdiutil", "attach", "-readonly", "-nobrowse", "-mountpoint", str(mount_dir), str(images[0])],
                check=True, timeout=120, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True,
            )
            mounted = True
            candidates = list(mount_dir.rglob("LCEDA-Pro"))
            executable = next((p for p in candidates if p.is_file() and p.parent.name == "MacOS"), None)
            if executable is None:
                raise RuntimeError("LCEDA-Pro executable was not found in the mounted macOS image")
            return _cli(executable, env, executable.parent)
        finally:
            if mounted:
                subprocess.run(["hdiutil", "detach", str(mount_dir), "-force"], check=False, timeout=60,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    if os.name == "nt":
        installer = Path(os.environ["J00_CLIENT_ARCHIVE"]).resolve()
        if not installer.is_file() or installer.suffix.lower() != ".exe":
            raise RuntimeError("Windows installer archive was not found at the recorded download path")
        install_root = client_dir / "installed"
        install_root.mkdir(parents=True, exist_ok=True)
        result = ProcessSupervisor(default_timeout=240, env=env).run(
            [str(installer), "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-", f"/DIR={install_root}"],
            cwd=runtime_dir, timeout=240,
        )
        if not result.ok:
            raise RuntimeError("Windows installer failed or timed out")
        executables = [p for p in install_root.rglob("*.exe") if p.name.lower() == "lceda-pro.exe"]
        if len(executables) != 1:
            raise RuntimeError(f"expected one installed lceda-pro.exe, found {len(executables)}")
        return _cli(executables[0], env, install_root)

    raise RuntimeError(f"unsupported platform: {sys.platform}")


def main() -> int:
    try:
        detail = _run()
    except Exception as exc:
        reason = str(exc).replace("\n", " ")[:240]
        _record("failed", reason)
        print(f"LCEDA Pro CLI smoke failed on {platform.platform()}: {reason}", file=sys.stderr)
        return 1
    _record("passed", detail)
    print(f"LCEDA Pro CLI help passed on {platform.platform()}: {detail}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
