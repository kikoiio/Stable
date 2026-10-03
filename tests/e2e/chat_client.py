#!/usr/bin/env python3
"""Line-delimited JSON client for the Stable conversation socket.

Drives the trusted user entry from e2e scripts: session lifecycle, goal
creation, candidate review/acceptance and approval answers. Every invocation
performs one operation, prints each server message as a JSON line, and exits
non-zero when the service reports an error.
"""

from __future__ import annotations

import argparse
import json
import socket
import sys


def exchange(sock_path: str, message: dict, timeout: float = 120.0) -> list[dict]:
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    conn.settimeout(timeout)
    conn.connect(sock_path)
    try:
        conn.sendall(json.dumps(message).encode() + b'\n')
        out: list[dict] = []
        buffer = b''
        while True:
            chunk = conn.recv(1 << 16)
            if not chunk:
                raise SystemExit(f'server closed the connection without done: {out!r}')
            buffer += chunk
            while b'\n' in buffer:
                line, buffer = buffer.split(b'\n', 1)
                if not line.strip():
                    continue
                msg = json.loads(line)
                out.append(msg)
                print(json.dumps(msg, ensure_ascii=False))
                if msg.get('type') == 'done':
                    return out
    finally:
        conn.close()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--socket', required=True)
    parser.add_argument('--root', default='', help='project/session root for session operations')
    sub = parser.add_subparsers(dest='op', required=True)

    sub.add_parser('session_create')
    load = sub.add_parser('session_load')
    load.add_argument('--session', required=True)

    create = sub.add_parser('create_goal')
    create.add_argument('--session', required=True)
    create.add_argument('--goal', required=True)
    create.add_argument('--text', required=True)

    confirm = sub.add_parser('confirm')
    confirm.add_argument('--session', required=True)
    confirm.add_argument('--goal', required=True)
    confirm.add_argument('--proposal', required=True)

    say = sub.add_parser('say')
    say.add_argument('--session', default='')
    say.add_argument('--goal', required=True)
    say.add_argument('--text', required=True)

    reply = sub.add_parser('reply')
    reply.add_argument('--session', default='')
    reply.add_argument('--goal', required=True)
    reply.add_argument('--text', required=True)

    review = sub.add_parser('review_get')
    review.add_argument('--session', required=True)
    review.add_argument('--candidate', required=True)

    accept = sub.add_parser('review_accept')
    accept.add_argument('--session', required=True)
    accept.add_argument('--candidate', required=True)
    accept.add_argument('--decision', required=True)
    accept.add_argument('--preview-digest', required=True)
    accept.add_argument('--candidate-digest', required=True)
    accept.add_argument('--formal-digest', required=True)
    accept.add_argument('--mode', choices=['normal', 'force'], default='normal')
    accept.add_argument('--confirmed', action='append', default=[])

    approvals = sub.add_parser('approval_list')
    approvals.add_argument('--session', required=True)

    resolve = sub.add_parser('approval_resolve')
    resolve.add_argument('--session', required=True)
    resolve.add_argument('--approval', required=True)
    resolve.add_argument('--choice', required=True, choices=['allow_once', 'save_rule', 'deny'])

    args = parser.parse_args()
    msg: dict = {'op': args.op}
    if args.op == 'session_create':
        msg['project_root'] = args.root
    elif args.op == 'session_load':
        msg.update(project_root=args.root, session_id=args.session)
    elif args.op == 'create_goal':
        msg.update(project_root=args.root, session_id=args.session, goal=args.goal, text=args.text)
    elif args.op == 'confirm':
        msg.update(project_root=args.root, session_id=args.session, goal=args.goal, id=args.proposal)
    elif args.op == 'say':
        msg.update(goal=args.goal, text=args.text, session_id=args.session)
    elif args.op == 'reply':
        msg.update(goal=args.goal, text=args.text, session_id=args.session)
    elif args.op == 'review_get':
        msg.update(candidate_id=args.candidate, session_id=args.session)
    elif args.op == 'review_accept':
        msg.update(candidate_id=args.candidate, session_id=args.session, decision_id=args.decision,
                   preview_digest=args.preview_digest, candidate_digest=args.candidate_digest,
                   formal_digest=args.formal_digest, acceptance_mode=args.mode,
                   confirmed_findings=args.confirmed)
    elif args.op == 'approval_list':
        msg.update(session_id=args.session)
    elif args.op == 'approval_resolve':
        msg.update(session_id=args.session, approval_id=args.approval, approval_choice=args.choice)

    messages = exchange(args.socket, msg)
    for m in messages:
        if m.get('type') == 'error':
            print(f"error: {m.get('error', '')}", file=sys.stderr)
            return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
