"""Run KiCad ERC against the exact design digest and preserve its JSON report."""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import subprocess

from schematic import authorized, digest


def _environment(root: Path) -> dict[str, str]:
    env = os.environ.copy()
    env['XDG_CACHE_HOME'] = str(root / '.kicad-cache')
    env['XDG_CONFIG_HOME'] = str(root / '.kicad-config')
    env['XDG_DATA_HOME'] = str(root / '.kicad-data')
    config = Path(env['XDG_CONFIG_HOME']) / 'kicad' / '9.0'
    config.mkdir(parents=True, exist_ok=True)
    for name in ('sym-lib-table', 'fp-lib-table'):
        source = Path('/usr/share/kicad/template') / name
        dest = config / name
        if source.exists() and not dest.exists():
            shutil.copyfile(source, dest)
    return env


def run_erc(path: Path, root: Path, report: Path) -> tuple[str, str, dict, list[str]]:
    if not authorized(path, root):
        return 'blocked', '', {'reason': 'design outside allowed root'}, []
    if not report.resolve().is_relative_to(root.resolve()):
        return 'blocked', digest(path), {'reason': 'report outside allowed root'}, []
    report.parent.mkdir(parents=True, exist_ok=True)
    before = digest(path)
    command = [
        'kicad-cli', 'sch', 'erc', '--format', 'json', '--severity-all',
        '--exit-code-violations', '--output', str(report), str(path),
    ]
    result = subprocess.run(command, capture_output=True, text=True, env=_environment(root), timeout=60)
    after = digest(path)
    if before != after:
        return 'blocked', after, {'reason': 'design changed during ERC'}, []
    if result.returncode not in (0, 5) or not report.is_file():
        return 'blocked', after, {'reason': 'ERC command failed', 'stderr': result.stderr[-2000:]}, []
    try:
        data = json.loads(report.read_text(encoding='utf-8'))
        violations = [item for sheet in data['sheets'] for item in sheet['violations']]
    except (ValueError, KeyError, TypeError) as exc:
        return 'blocked', after, {'reason': 'invalid ERC report', 'detail': str(exc)}, []
    status = 'pass' if not violations else 'fail'
    return status, after, {
        'artifact_id': after,
        'violation_count': len(violations),
        'violation_types': [item['type'] for item in violations],
        'report_format': data.get('$schema', ''),
    }, [str(report)]
