#!/usr/bin/env python3
"""Persistent isolated Xvfb/KiCad session controlled through one-shot JSON calls."""

from __future__ import annotations

import hashlib
import ctypes
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def inside(path: Path, root: Path) -> bool:
    try:
        path.resolve(strict=True).relative_to(root.resolve(strict=True))
        return True
    except (ValueError, FileNotFoundError):
        return False


def pid_alive(pid: int, name: str) -> bool:
    if pid <= 0:
        return False
    try:
        status = Path(f'/proc/{pid}/stat').read_text().split()
        cmdline = Path(f'/proc/{pid}/cmdline').read_bytes()
        return status[2] != 'Z' and name.encode() in cmdline
    except (OSError, IndexError):
        return False


def x_env(root: Path, display: str) -> dict[str, str]:
    env = os.environ.copy()
    env['DISPLAY'] = display
    env['XDG_CACHE_HOME'] = str(root / '.computer-cache')
    env['XDG_CONFIG_HOME'] = str(root / '.computer-config')
    env['XDG_DATA_HOME'] = str(root / '.computer-data')
    config = Path(env['XDG_CONFIG_HOME']) / 'kicad' / '9.0'
    config.mkdir(parents=True, exist_ok=True)
    for name in ('sym-lib-table', 'fp-lib-table'):
        source = Path('/usr/share/kicad/template') / name
        dest = config / name
        if source.exists() and not dest.exists():
            shutil.copyfile(source, dest)
    return env


def window(root: Path, display: str, stem: str) -> str:
    env = x_env(root, display)
    result = subprocess.run(['xwininfo', '-root', '-tree'], env=env, text=True, capture_output=True, timeout=5)
    if result.returncode != 0:
        return ''
    for line in result.stdout.splitlines():
        if stem in line and 'eeschema' in line:
            match = re.search(r'0x[0-9a-fA-F]+', line)
            if match:
                return match.group(0) + ' ' + line.strip()
    return ''


def accept_first_run_dialog(display: str, window_id: int) -> None:
    """Accept KiCad's default local settings in the isolated display."""
    x11 = ctypes.CDLL('libX11.so.6')
    xtst = ctypes.CDLL('libXtst.so.6')
    x11.XOpenDisplay.argtypes = [ctypes.c_char_p]
    x11.XOpenDisplay.restype = ctypes.c_void_p
    x11.XKeysymToKeycode.argtypes = [ctypes.c_void_p, ctypes.c_ulong]
    x11.XKeysymToKeycode.restype = ctypes.c_uint
    x11.XSetInputFocus.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_int, ctypes.c_ulong]
    x11.XFlush.argtypes = [ctypes.c_void_p]
    x11.XCloseDisplay.argtypes = [ctypes.c_void_p]
    xtst.XTestFakeKeyEvent.argtypes = [ctypes.c_void_p, ctypes.c_uint, ctypes.c_int, ctypes.c_ulong]
    connection = x11.XOpenDisplay(display.encode())
    if not connection:
        raise RuntimeError('cannot open isolated display for KiCad setup')
    try:
        x11.XSetInputFocus(connection, window_id, 1, 0)
        key = x11.XKeysymToKeycode(connection, 0xff0d)
        xtst.XTestFakeKeyEvent(connection, key, 1, 0)
        xtst.XTestFakeKeyEvent(connection, key, 0, 0)
        x11.XFlush(connection)
    finally:
        x11.XCloseDisplay(connection)


def observation(root: Path, path: Path, handle: dict, capture: bool = True) -> dict:
    session_id = handle.get('session_id', '')
    generation = int(handle.get('generation', 0))
    display = handle.get('display', '')
    current = digest(path) if path.is_file() else ''
    alive = (
        pid_alive(int(handle.get('xvfb_pid', 0)), 'Xvfb')
        and pid_alive(int(handle.get('eeschema_pid', 0)), 'eeschema')
        and current == handle.get('artifact_id')
    )
    identity = window(root, display, path.stem) if alive else ''
    screenshot = ''
    if alive and identity and capture:
        screen_dir = root / 'screenshots'
        screen_dir.mkdir(parents=True, exist_ok=True)
        safe_id = re.sub(r'[^A-Za-z0-9_.-]', '_', session_id)
        target = screen_dir / f'{safe_id}-{generation}-{time.time_ns()}.png'
        result = subprocess.run(['import', '-window', 'root', str(target)], env=x_env(root, display), capture_output=True, timeout=10)
        if result.returncode == 0 and target.is_file() and target.stat().st_size > 0:
            screenshot = str(target)
    state = 'open' if alive and identity and (not capture or screenshot) else 'stale'
    return {
        'session_id': session_id,
        'generation': generation,
        'status': state,
        'opened_artifact_id': handle.get('artifact_id', ''),
        'window_identity': identity,
        'screenshot_path': screenshot,
        'runtime_handle': json.dumps(handle, separators=(',', ':')),
        'observed_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
    }


def stop(handle: dict) -> None:
    for key, name in (('eeschema_pid', 'eeschema'), ('xvfb_pid', 'Xvfb')):
        pid = int(handle.get(key, 0))
        if pid_alive(pid, name):
            os.kill(pid, signal.SIGTERM)


def clear_owned_lock(handle: dict, path: Path) -> None:
    if handle.get('path') != str(path.resolve()):
        return
    for _ in range(20):
        if not pid_alive(int(handle.get('eeschema_pid', 0)), 'eeschema'):
            break
        time.sleep(0.1)
    if not pid_alive(int(handle.get('eeschema_pid', 0)), 'eeschema'):
        path.with_name('~' + path.name + '.lck').unlink(missing_ok=True)


def start(root: Path, path: Path, session_id: str, generation: int) -> dict:
    logs = root / 'computer-logs'
    logs.mkdir(parents=True, exist_ok=True)
    display = ''
    xvfb = None
    for number in range(100, 300):
        if Path(f'/tmp/.X{number}-lock').exists():
            continue
        candidate = f':{number}'
        with (logs / f'xvfb-{generation}.log').open('ab') as output:
            process = subprocess.Popen(['Xvfb', candidate, '-screen', '0', '1280x800x24', '-nolisten', 'tcp'],
                                       stdout=output, stderr=output, start_new_session=True)
        for _ in range(30):
            if Path(f'/tmp/.X11-unix/X{number}').exists():
                display, xvfb = candidate, process
                break
            if process.poll() is not None:
                break
            time.sleep(0.1)
        if xvfb:
            break
    if not xvfb:
        raise RuntimeError('unable to start Xvfb')
    env = x_env(root, display)
    with (logs / f'eeschema-{generation}.log').open('ab') as output:
        gui = subprocess.Popen(['eeschema', str(path.resolve())], env=env, stdout=output, stderr=output,
                               start_new_session=True)
    handle = {
        'session_id': session_id,
        'generation': generation,
        'display': display,
        'xvfb_pid': xvfb.pid,
        'eeschema_pid': gui.pid,
        'artifact_id': digest(path),
        'path': str(path.resolve()),
    }
    accepted = set()
    for _ in range(80):
        if window(root, display, path.stem):
            return handle
        seen = subprocess.run(['xwininfo', '-root', '-tree'], env=env, text=True, capture_output=True, timeout=5)
        for line in seen.stdout.splitlines():
            if ('配置 KiCad 设置路径' in line or '配置全局符号库表' in line) and 'eeschema' in line:
                match = re.search(r'0x[0-9a-fA-F]+', line)
                if match and match.group(0) not in accepted:
                    accept_first_run_dialog(display, int(match.group(0), 16))
                    accepted.add(match.group(0))
        if gui.poll() is not None:
            break
        time.sleep(0.15)
    snapshot = subprocess.run(['xwininfo', '-root', '-tree'], env=env, text=True, capture_output=True, timeout=5)
    (logs / f'windows-{generation}.log').write_text(snapshot.stdout + '\n' + snapshot.stderr)
    subprocess.run(['import', '-window', 'root', str(logs / f'failed-{generation}.png')],
                   env=env, capture_output=True, timeout=10)
    stop(handle)
    raise RuntimeError('KiCad schematic window did not open')


def handle(request: dict) -> dict:
    operation_id = request.get('operation_id', '')
    result = {
        'protocol_version': 1, 'operation_id': operation_id, 'status': 'blocked',
        'actual_artifact_id': '', 'evidence_paths': [], 'postcondition': {}, 'error_code': '',
    }
    if request.get('protocol_version') != 1 or not operation_id:
        result['error_code'] = 'protocol_error'
        return result
    payload = request.get('payload') or {}
    root = Path(payload.get('allowed_root', ''))
    path = Path(payload.get('path', ''))
    if not payload.get('allowed_root') or not payload.get('path') or not inside(path, root):
        result['error_code'] = 'target_outside_root'
        return result
    kind = request.get('kind')
    prior = payload.get('session') or {}
    if isinstance(prior.get('runtime_handle'), str):
        try:
            prior_handle = json.loads(prior['runtime_handle'])
        except ValueError:
            prior_handle = {}
    else:
        prior_handle = {}
    session_id = prior.get('id') or prior_handle.get('session_id') or 'computer-' + request.get('goal_id', '')
    if kind == 'computer.observe':
        observed = observation(root, path, prior_handle) if prior_handle else {
            'session_id': session_id, 'generation': int(prior.get('generation', 0)),
            'status': 'stale', 'opened_artifact_id': '', 'window_identity': '',
            'screenshot_path': '', 'runtime_handle': '', 'observed_at': '',
        }
        result['status'] = 'observed' if observed['status'] == 'open' else 'stale'
    elif kind in ('computer.ensure_open', 'computer.recover'):
        if kind == 'computer.ensure_open' and prior_handle:
            observed = observation(root, path, prior_handle)
            if observed['status'] != 'open':
                stop(prior_handle)
                clear_owned_lock(prior_handle, path)
                new_handle = start(root, path, session_id, int(prior.get('generation', 0)) + 1)
                observed = observation(root, path, new_handle)
        else:
            if prior_handle:
                stop(prior_handle)
                clear_owned_lock(prior_handle, path)
            generation = int(prior.get('generation', 0)) + 1
            new_handle = start(root, path, session_id, generation)
            observed = observation(root, path, new_handle)
        result['status'] = 'applied' if observed['status'] == 'open' else 'blocked'
    else:
        result['status'] = 'unsupported'
        return result
    result['actual_artifact_id'] = digest(path)
    result['postcondition'] = observed
    if observed['screenshot_path']:
        result['evidence_paths'] = [observed['screenshot_path']]
    return result


if __name__ == '__main__':
    try:
        request = json.load(sys.stdin)
        print(json.dumps(handle(request), ensure_ascii=False), flush=True)
    except Exception as exc:
        print(json.dumps({
            'protocol_version': 1,
            'operation_id': request.get('operation_id', '') if 'request' in locals() else '',
            'status': 'blocked', 'actual_artifact_id': '', 'evidence_paths': [],
            'postcondition': {'reason': str(exc)}, 'error_code': 'computer_exception',
        }), flush=True)
