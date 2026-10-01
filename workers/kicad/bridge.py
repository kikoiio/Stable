#!/usr/bin/env python3
"""One JSON request on stdin, one JSON result on stdout."""

from __future__ import annotations

import json
from pathlib import Path
import sys

from schematic import authorized, digest, inspect, repair
from erc import DEFAULT_MAX_VIOLATIONS, run_erc


def handle(request: dict) -> dict:
    operation_id = request.get('operation_id', '')
    result = {
        'protocol_version': 1,
        'operation_id': operation_id,
        'status': 'unsupported',
        'actual_artifact_id': '',
        'evidence_paths': [],
        'postcondition': {},
        'error_code': '',
    }
    if request.get('protocol_version') != 1 or not operation_id:
        result['status'] = 'blocked'
        result['error_code'] = 'protocol_error'
        return result
    payload = request.get('payload') or {}
    path = Path(payload.get('path', ''))
    root = Path(payload.get('allowed_root', ''))
    if not payload.get('path') or not payload.get('allowed_root') or not authorized(path, root):
        result['status'] = 'blocked'
        result['error_code'] = 'target_outside_root'
        return result
    kind = request.get('kind')
    if kind == 'inspect_design':
        result['status'] = 'observed'
        result['actual_artifact_id'] = digest(path)
        result['postcondition'] = inspect(path)
    elif kind == 'kicad.repair_connection':
        status, actual, facts = repair(path, root, request.get('expected_artifact_id', ''))
        result.update(status=status, actual_artifact_id=actual, postcondition=facts)
    elif kind == 'kicad.run_erc':
        report = Path(payload.get('report_path', ''))
        threshold = payload.get('max_violations', DEFAULT_MAX_VIOLATIONS)
        if not payload.get('report_path'):
            result['status'] = 'blocked'
            result['error_code'] = 'report_path_missing'
        elif not isinstance(threshold, int) or isinstance(threshold, bool) or threshold < 0:
            result['status'] = 'blocked'
            result['error_code'] = 'invalid_max_violations'
        else:
            status, actual, facts, paths = run_erc(path, root, report, threshold)
            result.update(status=status, actual_artifact_id=actual, postcondition=facts, evidence_paths=paths)
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
            'postcondition': {'reason': str(exc)}, 'error_code': 'capability_exception',
        }), flush=True)
