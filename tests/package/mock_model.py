#!/usr/bin/env python3
"""Loopback Chat Completions fixture for installed-package tests."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TRANSLATE_MARKER = "Translate the user's acceptance requirements"
ASK_MARKER = "请停下来问我"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def log_request_failure(self, error):
        # Keep E2E diagnostics out of the response body while retaining the
        # request-level failure in the run root for bounded local debugging.
        try:
            with open(self.server.log_path, 'a', encoding='utf-8') as output:
                output.write(repr(error) + '\\n')
        except OSError:
            pass

    def do_POST(self):
        if self.path != '/v1/chat/completions':
            self.send_error(404)
            return
        try:
            raw = self.rfile.read(int(self.headers['Content-Length']))
            request = json.loads(raw)
            prompt = request['messages'][-1]['content']
        except Exception as error:
            self.log_request_failure(error)
            raise
        if prompt.startswith(TRANSLATE_MARKER):
            content = self.transpile(prompt)
        elif 'Context: ' in prompt:
            content = json.dumps(self.decide(prompt))
        else:
            # Goal agent runs stream a short narrative with the intent as the
            # prompt (no decision Context); the text itself is unused.
            content = '收到，我会基于当前事实继续推进目标。'
        if request.get('stream'):
            self.respond_sse(request, content)
            return
        response = dict(id='mock-001', choices=[dict(finish_reason='stop', message=dict(role='assistant', content=content))])
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def respond_sse(self, request, content):
        frames = [
            dict(id='mock-001', choices=[dict(index=0, delta=dict(role='assistant', content=content), finish_reason=None)]),
            dict(id='mock-001', choices=[dict(index=0, delta=dict(), finish_reason='stop')],
                 usage=dict(prompt_tokens=1, completion_tokens=1)),
        ]
        body = ''.join('data: ' + json.dumps(frame) + '\n\n' for frame in frames) + 'data: [DONE]\n\n'
        raw = body.encode()
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

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
        marker = 'Context: ' if 'Context: ' in prompt else '\n{'
        if marker == '\n{':
            context = json.loads('{' + prompt.rsplit('\n{', 1)[1])
        else:
            context = json.loads(prompt.split(marker, 1)[1])
        observation = context['observation']
        conversation = context.get('conversation') or []
        if 'artifact_id' not in observation:
            facts = observation.get('facts') or {}
            observation['artifact_id'] = facts.get('artifact_id', '')
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
    server.log_path = __import__('os').environ.get('STABLE_MOCK_LOG', '/tmp/stable-mock.log')
    print(server.server_port, flush=True)
    server.serve_forever()
