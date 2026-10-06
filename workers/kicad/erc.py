"""Run KiCad ERC against the exact design digest and preserve its JSON report."""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import subprocess

from schematic import authorized, digest


CHECKER_ID = 'kicad-cli-erc'
DEFAULT_MAX_VIOLATIONS = 0


def _private_dir(env: dict[str, str], name: str, root: Path, suffix: str) -> Path:
    """Use a profile-provided private directory only when it stays in root."""
    value = env.get(name)
    candidate = Path(value) if value else root / suffix
    try:
        candidate.resolve().relative_to(root.resolve())
    except ValueError as exc:
        raise ValueError(f'{name} must stay inside the private run root') from exc
    candidate.mkdir(parents=True, exist_ok=True)
    return candidate


def _template_root(env: dict[str, str]) -> Path:
    value = env.get('STABLE_KICAD_TEMPLATE_ROOT', '/usr/share/kicad/template')
    path = Path(value)
    if not path.is_absolute():
        raise ValueError('STABLE_KICAD_TEMPLATE_ROOT must be absolute')
    return path


def kicad_environment(root: Path) -> dict[str, str]:
    env = os.environ.copy()
    env['XDG_CACHE_HOME'] = str(_private_dir(env, 'XDG_CACHE_HOME', root, '.kicad-cache'))
    env['XDG_CONFIG_HOME'] = str(_private_dir(env, 'XDG_CONFIG_HOME', root, '.kicad-config'))
    env['XDG_DATA_HOME'] = str(_private_dir(env, 'XDG_DATA_HOME', root, '.kicad-data'))
    template_root = _template_root(env)
    config = Path(env['XDG_CONFIG_HOME']) / 'kicad' / '9.0'
    config.mkdir(parents=True, exist_ok=True)
    for name in ('sym-lib-table', 'fp-lib-table'):
        source = template_root / name
        dest = config / name
        if source.exists() and not dest.exists():
            shutil.copyfile(source, dest)
    return env


# Backward-compatible private alias for any local consumers predating the
# shared dependency collector.
_environment = kicad_environment


def kicad_cli_version(env: dict[str, str]) -> str | None:
    """Return the real kicad-cli version, or None when it cannot be determined.

    A guessed version is never returned: without the real version the check
    result cannot serve as verifiable evidence.
    """
    try:
        result = subprocess.run(
            ['kicad-cli', 'version'], capture_output=True, text=True, env=env, timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    version = result.stdout.strip()
    return version or None


def run_erc(path: Path, root: Path, report: Path, max_violations: int = DEFAULT_MAX_VIOLATIONS, state_root: Path | None = None) -> tuple[str, str, dict, list[str]]:
    if not authorized(path, root):
        return 'blocked', '', {'reason': 'design outside allowed root'}, []
    # KiCad user state and the report belong to the private run directory so
    # the project root can stay read-only; legacy callers without a separate
    # state root keep both under the allowed root.
    state = Path(state_root) if state_root else root
    if not report.resolve().is_relative_to(state.resolve()):
        return 'blocked', digest(path), {'reason': 'report outside the private run root'}, []
    env = kicad_environment(state)
    version = kicad_cli_version(env)
    if version is None:
        return 'blocked', digest(path), {'reason': 'kicad-cli version unavailable; result not verifiable'}, []
    report.parent.mkdir(parents=True, exist_ok=True)
    before = digest(path)
    command = [
        'kicad-cli', 'sch', 'erc', '--format', 'json', '--severity-all',
        '--exit-code-violations', '--output', str(report), str(path),
    ]
    result = subprocess.run(command, capture_output=True, text=True, env=env, timeout=60)
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
    count = len(violations)
    status = 'pass' if count <= max_violations else 'fail'
    return status, after, {
        'artifact_id': after,
        'checker_id': CHECKER_ID,
        'checker_version': version,
        'violation_count': count,
        'max_violations': max_violations,
        'violation_types': [item['type'] for item in violations],
        'report_format': data.get('$schema', ''),
    }, [str(report)]
