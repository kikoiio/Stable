import hashlib
import importlib.util
import io
import json
import pathlib
import tempfile
import unittest
from unittest import mock


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

    def test_observe_reports_missing_screenshot_capability(self):
        handle = {
            'session_id': 'computer-goal-1', 'generation': 1,
            'display': ':101', 'xvfb_pid': 101, 'eeschema_pid': 102,
            'artifact_id': hashlib.sha256(b'candidate design').hexdigest(),
            'path': str(self.design.resolve()),
        }
        with mock.patch.object(bridge, 'pid_alive', return_value=True), \
             mock.patch.object(bridge, 'window', return_value='0x1 eeschema sensor'), \
             mock.patch.object(bridge, 'import_tool', return_value=None):
            response = bridge.observation(self.runtime, self.design, handle)
        self.assertEqual(response['status'], 'stale')
        self.assertEqual(response['reason'], 'screenshot_tool_unavailable')

    def test_stop_classifies_invalid_owned_pid(self):
        self.assertEqual(bridge.stop({'eeschema_pid': 'bad', 'xvfb_pid': 0}), ['eeschema:invalid_pid'])

    def test_stop_never_signals_pid_with_unrelated_process_identity(self):
        with mock.patch.object(bridge, 'pid_alive', side_effect=lambda pid, name: (pid, name) in {(101, 'Xvfb')}), \
             mock.patch.object(bridge, '_wait_dead', return_value=True), \
             mock.patch.object(bridge.os, 'kill') as kill:
            failures = bridge.stop({'eeschema_pid': 202, 'xvfb_pid': 101})

        self.assertEqual(failures, [])
        kill.assert_called_once_with(101, bridge.signal.SIGTERM)

    def test_clear_owned_lock_refuses_path_outside_handle(self):
        unrelated_lock = self.project / '~sensor.kicad_sch.lck'
        unrelated_lock.write_text('unrelated lock', encoding='utf-8')
        handle = {'path': str(self.design.resolve()), 'eeschema_pid': 0}

        self.assertFalse(bridge.clear_owned_lock(handle, unrelated_lock))
        self.assertEqual(unrelated_lock.read_text(encoding='utf-8'), 'unrelated lock')


if __name__ == '__main__':
    unittest.main()
