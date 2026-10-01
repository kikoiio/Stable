#!/usr/bin/env python3
"""Loopback Chat Completions fixture for installed-package tests."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TRANSLATE_MARKER = "Translate the user's acceptance requirements"
ASK_MARKER = "请停下来问我"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        if self.path != '/v1/chat/completions':
            self.send_error(404)
            return
        raw = self.rfile.read(int(self.headers['Content-Length']))
        request = json.loads(raw)
        prompt = request['messages'][-1]['content']
        if prompt.startswith(TRANSLATE_MARKER):
            content = self.transpile(prompt)
        else:
            content = json.dumps(self.decide(prompt))
        response = dict(id='mock-001', choices=[dict(finish_reason='stop', message=dict(role='assistant', content=content))])
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def transpile(self, prompt):
        nl = prompt.rsplit('User requirements: ', 1)[-1]
        if '美观' in nl:
            proposal = dict(status='reject', criteria=[], reason='美观无法机器验证，请改用可验证的验收条件')
        elif '放宽' in nl:
            proposal = dict(status='ok', reason='映射到放宽后的 ERC 与传感器连接检查', criteria=[
                dict(id='erc-clean', kind='kicad.erc_clean', payload=dict(max_violations=2)),
                dict(id='sensor-connection', kind='sensor.connection_present',
                     payload=dict(endpoint_a='RT1.2', endpoint_b='J1.2')),
            ])
        else:
            proposal = dict(status='ok', reason='映射到 ERC 与传感器连接检查', criteria=[
                dict(id='erc-clean', kind='kicad.erc_clean', payload=dict(max_violations=0)),
                dict(id='sensor-connection', kind='sensor.connection_present',
                     payload=dict(endpoint_a='RT1.2', endpoint_b='J1.2')),
            ])
        return json.dumps(proposal)

    def decide(self, prompt):
        context = json.loads(prompt.split('Context: ', 1)[1])
        observation = context['observation']
        conversation = context.get('conversation') or []
        texts = [m.get('text', '') for m in conversation]
        facts = json.loads(observation['facts']) if isinstance(observation['facts'], str) else observation['facts']
        if any(ASK_MARKER in t for t in texts):
            return dict(kind='ask_human', capability='', target='', parameters={},
                        expected_artifact_id='', reason='这条故障超出我的能力，请说明应如何处理')
        design, computer = facts['design'], facts['computer']
        if any(t for t in texts) and not any('按 J1.2' in t for t in texts):
            # Acknowledge steering without acting so the scenario stays steppable.
            return dict(kind='observe', capability='', target='', parameters={},
                        expected_artifact_id='', reason='收到用户消息，保持观察')
        kind, capability = 'observe', ''
        if not design['sensor.supported']:
            kind = 'ask_human'
        elif computer['status'] in ('absent', 'stale'):
            kind, capability = 'open_computer', 'computer.ensure_open'
        elif not design['sensor.connection_present']:
            kind, capability = 'execute_capability', 'kicad.repair_connection'
        return dict(kind=kind, capability=capability,
                    target=facts['artifact_path'] if capability else '',
                    parameters={},
                    expected_artifact_id=observation['artifact_id'] if capability else '',
                    reason='current observed KiCad state')


if __name__ == '__main__':
    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    print(server.server_port, flush=True)
    server.serve_forever()
