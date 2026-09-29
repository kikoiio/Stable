"""Narrow, repeatable edit for the bundled two-terminal sensor fixture."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import tempfile


TOP_WIRE = '(xy 113.03 100.33) (xy 121.92 100.33)'
BOTTOM_LEAD = '(xy 114.3 105.41) (xy 114.3 102.87)'
MISSING_WIRE = '(xy 114.3 102.87) (xy 121.92 102.87)'
WIRE_FORM = (
    '\t(wire (pts ' + MISSING_WIRE + ') '
    '(stroke (width 0) (type solid)) '
    '(uuid "81110618-6579-58c6-8f1f-78d9234e76d6"))\n'
)
INSERT_BEFORE = '\t(symbol (lib_id "Device:Thermistor_NTC")'


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open('rb') as source:
        for block in iter(lambda: source.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def authorized(path: Path, root: Path) -> bool:
    try:
        path.resolve(strict=True).relative_to(root.resolve(strict=True))
        return True
    except (ValueError, FileNotFoundError):
        return False


def inspect(path: Path) -> dict:
    text = path.read_text(encoding='utf-8')
    supported = (
        text.count(INSERT_BEFORE) == 1
        and text.count('(symbol (lib_id "Connector:Conn_01x02_Socket")') == 1
        and TOP_WIRE in text
        and BOTTOM_LEAD in text
    )
    return {
        'sensor.supported': supported,
        'sensor.connection_present': supported and MISSING_WIRE in text,
        'sensor.endpoint_a': 'RT1.2',
        'sensor.endpoint_b': 'J1.2',
    }


def repair(path: Path, root: Path, expected_digest: str) -> tuple[str, str, dict]:
    if not authorized(path, root):
        return 'blocked', digest(path) if path.exists() else '', {'reason': 'target outside allowed root'}
    facts = inspect(path)
    current = digest(path)
    if not facts['sensor.supported']:
        return 'unsupported', current, facts
    if facts['sensor.connection_present']:
        return 'already_satisfied', current, facts
    if current != expected_digest:
        return 'blocked', current, {'reason': 'artifact digest changed', **facts}

    text = path.read_text(encoding='utf-8')
    if text.count(INSERT_BEFORE) != 1 or MISSING_WIRE in text:
        return 'unsupported', current, facts
    changed = text.replace(INSERT_BEFORE, WIRE_FORM + INSERT_BEFORE, 1)
    fd, temp_name = tempfile.mkstemp(prefix='.sensor-repair-', suffix='.kicad_sch', dir=path.parent)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as output:
            output.write(changed)
            output.flush()
            os.fsync(output.fileno())
        if digest(path) != current:
            return 'blocked', digest(path), {'reason': 'artifact changed during repair'}
        os.replace(temp_name, path)
    finally:
        if os.path.exists(temp_name):
            os.unlink(temp_name)
    return 'applied', digest(path), inspect(path)
