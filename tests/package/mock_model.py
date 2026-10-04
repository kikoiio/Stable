#!/usr/bin/env python3
"""Loopback Chat Completions fixture for installed-package tests."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TRANSLATE_MARKER = "Translate the user's acceptance requirements"
ASK_MARKER = "请停下来问我"
# When the user text carries this marker, the fixture inserts one controlled
# command round between the read and the edit so e2e can observe command
# output flowing back into the conversation.
COMMAND_MARKER = "运行命令验证"
COMMAND_PROBE = "printf 'm04-probe-'; grep -c '(wire' /workspace/project/sensor.kicad_sch"
MISSING_WIRE = '(wire (pts (xy 114.3 102.87) (xy 121.92 102.87))\n        (stroke (width 0) (type solid)) (uuid "81110618-6579-58c6-8f1f-78d9234e76d6"))'


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def log_request_failure(self, error):
        # Keep E2E diagnostics out of the response body while retaining the
        # request-level failure in the run root for bounded local debugging.
        self.log_line(repr(error))

    def log_line(self, text):
        try:
            with open(self.server.log_path, 'a', encoding='utf-8') as output:
                output.write(text + '\n')
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
        for message in request.get('messages') or []:
            if message.get('role') == 'tool':
                # Record tool results reaching the next model turn so e2e can
                # prove tool output (e.g. command stdout) flowed back.
                self.log_line('TOOL_RESULT ' + str(message.get('content'))[:500])
        tool_call = self.goal_tool_call(request)
        if prompt.startswith(TRANSLATE_MARKER):
            content = self.transpile(prompt)
        elif 'Context: ' in prompt:
            content = json.dumps(self.decide(prompt))
        else:
            # Goal runs use the shared tool executor. The fixture deliberately
            # performs one formal read followed by one candidate edit, then
            # stops so the surrounding workflow owns review and acceptance.
            content = '收到，我会基于当前事实继续推进目标。'
        if request.get('stream'):
            self.respond_sse(request, content, tool_call)
            return
        response = dict(id='mock-001', choices=[dict(finish_reason='stop', message=dict(role='assistant', content=content))])
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def goal_tool_call(self, request):
        if not request.get('tools'):
            return None
        messages = request.get('messages') or []
        if not messages:
            return None
        edit_call = dict(id='mock-edit-1', name='edit_file', arguments=json.dumps({
            'file_path': 'sensor.kicad_sch',
            'old_string': '\\t(wire (pts (xy 114.3 105.41) (xy 114.3 102.87))\\n        (stroke (width 0) (type solid)) (uuid "ddd57af5-2125-566a-bcf5-c11ca6ff8a52"))',
            'new_string': '\\t(wire (pts (xy 114.3 105.41) (xy 114.3 102.87))\\n        (stroke (width 0) (type solid)) (uuid "ddd57af5-2125-566a-bcf5-c11ca6ff8a52"))\\n\\t' + MISSING_WIRE,
        }))
        # The marker path edits the real file bytes (tab-indented segment).
        wire_segment = '\t(wire (pts (xy 114.3 105.41) (xy 114.3 102.87))\n        (stroke (width 0) (type solid)) (uuid "ddd57af5-2125-566a-bcf5-c11ca6ff8a52"))'
        edit_call_real = dict(id='mock-edit-1', name='edit_file', arguments=json.dumps({
            'file_path': 'sensor.kicad_sch',
            'old_string': wire_segment,
            'new_string': wire_segment + '\n\t' + MISSING_WIRE,
        }))
        wants_command = any(
            COMMAND_MARKER in (m.get('content') or '')
            for m in messages
            if isinstance(m.get('content'), str)
        )
        if messages[-1].get('role') == 'tool':
            tool_results = [m for m in messages if m.get('role') == 'tool']
            if wants_command:
                if len(tool_results) == 1:
                    return dict(id='mock-command-1', name='command', arguments=json.dumps({'command': COMMAND_PROBE}))
                if len(tool_results) == 2:
                    return edit_call_real
                return None
            if len(tool_results) == 1:
                return edit_call
            return None
        return dict(id='mock-read-1', name='read_file', arguments=json.dumps({'file_path': 'sensor.kicad_sch', 'limit': 2000}))

    def respond_sse(self, request, content, tool_call=None):
        if tool_call:
            frames = [
                dict(id='mock-001', choices=[dict(index=0, delta=dict(role='assistant', tool_calls=[dict(index=0, id=tool_call['id'], type='function', function=dict(name=tool_call['name'], arguments=tool_call['arguments']))]), finish_reason=None)]),
                dict(id='mock-001', choices=[dict(index=0, delta=dict(), finish_reason='tool_calls')], usage=dict(prompt_tokens=1, completion_tokens=1)),
            ]
        else:
            frames = [
                dict(id='mock-001', choices=[dict(index=0, delta=dict(role='assistant', content=content), finish_reason=None)]),
                dict(id='mock-001', choices=[dict(index=0, delta=dict(), finish_reason='stop')], usage=dict(prompt_tokens=1, completion_tokens=1)),
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
