import hashlib
import importlib.util
import io
import json
import pathlib
import tempfile
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name('bridge.py')
SPEC = importlib.util.spec_from_file_location('stable_computer_bridge', MODULE_PATH)
bridge = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bridge)


class CandidateBridgeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temp.name)
        self.project = self.root / 'project'
        self.candidate = self.root / 'candidate'
        self.runtime = self.root / 'runtime'
        for path in (self.project, self.candidate, self.runtime):
            path.mkdir()
        self.design = self.candidate / 'sensor.kicad_sch'
        self.design.write_text('candidate design', encoding='utf-8')

    def tearDown(self):
        self.temp.cleanup()

    def request(self, kind='computer.observe', path=None, allowed_root=None):
        return {
            'protocol_version': 1,
            'operation_id': 'op-1',
            'goal_id': 'goal-1',
            'kind': kind,
            'payload': {
                'path': str(path or self.design),
                'allowed_root': str(allowed_root or self.candidate),
                'run_root': str(self.runtime),
            },
        }

    def test_observe_binds_to_candidate_and_keeps_runtime_separate(self):
        response = bridge.handle(self.request())
        self.assertEqual(response['status'], 'stale')
        self.assertEqual(response['actual_artifact_id'], hashlib.sha256(b'candidate design').hexdigest())
        self.assertFalse((self.candidate / '.computer-cache').exists())

    def test_formal_project_path_is_rejected(self):
        formal = self.project / 'sensor.kicad_sch'
        formal.write_text('formal sentinel', encoding='utf-8')
        response = bridge.handle(self.request(path=formal))
        self.assertEqual(response['error_code'], 'target_outside_root')
        self.assertEqual(formal.read_text(encoding='utf-8'), 'formal sentinel')

    def test_persistent_session_protocol_returns_one_response_per_request(self):
        request = json.dumps(self.request()).encode('utf-8') + b'\n'
        reader, writer = io.BytesIO(request), io.BytesIO()
        bridge.serve_session_stream(reader, writer)
        lines = writer.getvalue().splitlines()
        self.assertEqual(json.loads(lines[0]), {'ready': True})
        response = json.loads(lines[1])
        self.assertEqual(response['operation_id'], 'op-1')
        self.assertEqual(response['status'], 'stale')


if __name__ == '__main__':
    unittest.main()
