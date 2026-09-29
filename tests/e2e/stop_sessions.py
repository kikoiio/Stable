"""Stop only the isolated GUI processes recorded for one completed test run."""

import json
import os
from pathlib import Path
import signal
import sqlite3
import sys


root = Path(sys.argv[1]).resolve()
database = root / "state.db"
if not database.is_file():
    raise SystemExit(0)

with sqlite3.connect(database) as connection:
    handles = [json.loads(row[0]) for row in connection.execute(
        "SELECT runtime_handle FROM computer_sessions WHERE runtime_handle <> ''"
    )]

for handle in handles:
    design = Path(handle.get("path", ""))
    if not design.is_absolute() or not design.resolve(strict=False).is_relative_to(root):
        continue
    for key, expected in (
        ("eeschema_pid", f"eeschema {design}"),
        ("xvfb_pid", f"Xvfb {handle.get('display', '')} "),
    ):
        pid = handle.get(key)
        if not isinstance(pid, int) or pid <= 1:
            continue
        try:
            command = Path(f"/proc/{pid}/cmdline").read_bytes().replace(b"\0", b" ").decode()
        except (FileNotFoundError, PermissionError):
            continue
        if command.startswith(expected):
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
