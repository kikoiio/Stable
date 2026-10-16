#!/usr/bin/env python3
"""Copy only approved J00 summaries into a redacted artifact directory."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import tempfile

try:
    from .j00_evidence import redact, redact_text
except ImportError:  # pragma: no cover - direct script execution
    from j00_evidence import redact, redact_text


SAFE_ARTIFACTS = frozenset(
    {
        "environment.txt",
        "cli-probe.json",
        "J00-BOM.csv",
        "probe/evidence/j00-report.json",
        "probe/evidence/compatibility-report.md",
        "probe/evidence/capability-matrix.json",
    }
)


def sanitize_tree(
    source_root: Path,
    destination_root: Path,
    *,
    workspace: str = "",
    home: str = "",
) -> list[str]:
    source_root = Path(os.path.abspath(source_root))
    destination_root = Path(os.path.abspath(destination_root))
    if (
        source_root == destination_root
        or source_root in destination_root.parents
        or destination_root in source_root.parents
    ):
        raise ValueError("source and destination roots must be separate directories")
    if not source_root.is_dir():
        source_root.mkdir(parents=True, exist_ok=True)

    destination_root.parent.mkdir(parents=True, exist_ok=True)
    temporary_root = Path(
        tempfile.mkdtemp(prefix=f".{destination_root.name}-", dir=destination_root.parent)
    )
    written: list[str] = []
    try:
        for current_root, directories, filenames in os.walk(source_root, followlinks=False):
            directories[:] = [
                name
                for name in directories
                if not (Path(current_root) / name).is_symlink()
            ]
            for filename in filenames:
                source = Path(current_root) / filename
                if source.is_symlink():
                    continue
                relative = source.relative_to(source_root).as_posix()
                if relative not in SAFE_ARTIFACTS:
                    continue
                text = source.read_text(encoding="utf-8")
                for marker, replacement in (
                    (workspace, "<redacted-path>"),
                    (home, "<redacted-home>"),
                ):
                    if marker:
                        text = text.replace(marker, replacement)
                if source.suffix.lower() == ".json":
                    text = json.dumps(
                        redact(json.loads(text)),
                        ensure_ascii=False,
                        indent=2,
                        sort_keys=True,
                    ) + "\n"
                else:
                    text = redact_text(text)
                destination = temporary_root / relative
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.write_text(text, encoding="utf-8")
                written.append(relative)

        if destination_root.is_symlink():
            destination_root.unlink()
        elif destination_root.exists():
            shutil.rmtree(destination_root)
        temporary_root.replace(destination_root)
        return written
    except Exception:
        shutil.rmtree(temporary_root, ignore_errors=True)
        raise


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source_root", type=Path)
    parser.add_argument("destination_root", type=Path)
    parser.add_argument("--workspace", default="")
    parser.add_argument("--home", default="")
    args = parser.parse_args()
    written = sanitize_tree(
        args.source_root,
        args.destination_root,
        workspace=args.workspace,
        home=args.home,
    )
    print(f"J00: sanitized {len(written)} approved evidence file(s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
