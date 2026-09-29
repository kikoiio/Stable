#!/usr/bin/env python3
"""Loopback Chat Completions fixture for installed-package tests."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

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
        context = json.loads(prompt.split('Context: ', 1)[1])
        observation = context['observation']
        facts = json.loads(observation['facts']) if isinstance(observation['facts'], str) else observation['facts']
        design, computer = facts['design'], facts['computer']
        kind, capability = 'observe', ''
        if not design['sensor.supported']:
            kind = 'ask_human'
        elif computer['status'] in ('absent', 'stale'):
            kind, capability = 'open_computer', 'computer.ensure_open'
        elif not design['sensor.connection_present']:
            kind, capability = 'execute_capability', 'kicad.repair_connection'
        proposal = dict(kind=kind, capability=capability,
                        target=facts['artifact_path'] if capability else '',
                        parameters={},
                        expected_artifact_id=observation['artifact_id'] if capability else '',
                        reason='current observed KiCad state')
        response = dict(id='mock-001', choices=[dict(finish_reason='stop', message=dict(role='assistant', content=json.dumps(proposal)))])
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

if __name__ == '__main__':
    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    print(server.server_port, flush=True)
    server.serve_forever()
