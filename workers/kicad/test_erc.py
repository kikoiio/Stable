"""ERC checker-version and threshold tests with a controlled kicad-cli."""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

import bridge
from erc import CHECKER_ID, kicad_environment, run_erc
from schematic import CONNECTION_CHECKER_ID, CONNECTION_CHECKER_VERSION


FIXTURE = Path(__file__).resolve().parents[2] / 'fixtures' / 'sensor_board' / 'sensor.kicad_sch'
VERSION = '9.0.4'


def report_with(violations: int) -> str:
    items = [{'type': 'pin_not_connected', 'severity': 'error'} for _ in range(violations)]
    return json.dumps({'$schema': 'https://schemas.kicad.org/erc.v1', 'sheets': [{'violations': items}]})


def fake_kicad_cli(violations: int = 0, version_rc: int = 0, erc_rc: int = 0, report_body: str | None = None):
    """A subprocess.run stand-in answering kicad-cli version and ERC calls."""
    def run(command, capture_output=False, text=False, env=None, timeout=None):
        if command[:2] == ['kicad-cli', 'version']:
            return subprocess.CompletedProcess(command, version_rc, stdout=VERSION if version_rc == 0 else '')
        if command[1:3] == ['sch', 'erc']:
            output = Path(command[command.index('--output') + 1])
            if report_body is not None:
                output.write_text(report_body, encoding='utf-8')
            else:
                output.write_text(report_with(violations), encoding='utf-8')
            return subprocess.CompletedProcess(command, erc_rc, stdout='', stderr='')
        raise AssertionError(f'unexpected command: {command}')
    return run


class ErcTest(unittest.TestCase):
    def setUp(self):
        self._directory = tempfile.TemporaryDirectory()
        self.addCleanup(self._directory.cleanup)
        self.root = Path(self._directory.name)
        self.design = self.root / 'sensor.kicad_sch'
        shutil.copyfile(FIXTURE, self.design)
        self.report = self.root / 'reports' / 'erc.json'

    def call_erc(self, violations=0, max_violations=0, **kwargs):
        with mock.patch('erc.subprocess.run', fake_kicad_cli(violations=violations, **kwargs)):
            return run_erc(self.design, self.root, self.report, max_violations)

    def test_profile_xdg_directories_stay_private(self):
        private = self.root / 'profile-config'
        with mock.patch.dict(os.environ, {
            'XDG_CONFIG_HOME': str(private),
            'XDG_CACHE_HOME': str(self.root / 'profile-cache'),
            'XDG_DATA_HOME': str(self.root / 'profile-data'),
            'STABLE_KICAD_TEMPLATE_ROOT': str(self.root / 'templates'),
        }, clear=False):
            env = kicad_environment(self.root)
        self.assertEqual(Path(env['XDG_CONFIG_HOME']), private)
        self.assertTrue(Path(env['XDG_CONFIG_HOME']).is_relative_to(self.root))
        self.assertEqual(Path(env['STABLE_KICAD_TEMPLATE_ROOT']), self.root / 'templates')

    def test_profile_xdg_directory_outside_run_root_is_rejected(self):
        with mock.patch.dict(os.environ, {'XDG_CONFIG_HOME': str(self.root.parent / 'outside')}, clear=False):
            with self.assertRaises(ValueError):
                kicad_environment(self.root)

    def test_checker_identity_and_version_from_real_cli(self):
        status, artifact, facts, paths = self.call_erc(violations=0)
        self.assertEqual(status, 'pass')
        self.assertEqual(facts['checker_id'], CHECKER_ID)
        self.assertEqual(facts['checker_version'], VERSION)
        self.assertEqual(facts['violation_count'], 0)
        self.assertEqual(facts['max_violations'], 0)
        self.assertEqual(paths, [str(self.report)])

    def test_version_command_failure_is_not_verifiable(self):
        status, _, facts, paths = self.call_erc(version_rc=1)
        self.assertNotEqual(status, 'pass')
        self.assertEqual(status, 'blocked')
        self.assertNotIn('checker_version', facts)
        self.assertEqual(paths, [])
        self.assertFalse(self.report.exists())

    def test_threshold_zero_equal_and_exceeded(self):
        self.assertEqual(self.call_erc(violations=0, max_violations=0)[0], 'pass')
        self.assertEqual(self.call_erc(violations=2, max_violations=2)[0], 'pass')
        status, _, facts, _ = self.call_erc(violations=3, max_violations=2)
        self.assertEqual(status, 'fail')
        self.assertEqual(facts['violation_count'], 3)

    def test_invalid_report_cannot_pass(self):
        status, _, facts, _ = self.call_erc(report_body='not json')
        self.assertEqual(status, 'blocked')
        self.assertIn('invalid ERC report', facts['reason'])

    def test_erc_command_failure_cannot_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            design = root / 'sensor.kicad_sch'
            shutil.copyfile(FIXTURE, design)
            missing = root / 'reports' / 'erc.json'

            def no_report(command, capture_output=False, text=False, env=None, timeout=None):
                if command[:2] == ['kicad-cli', 'version']:
                    return subprocess.CompletedProcess(command, 0, stdout=VERSION)
                return subprocess.CompletedProcess(command, 0, stdout='', stderr='')

            with mock.patch('erc.subprocess.run', no_report):
                status, _, facts, _ = run_erc(design, root, missing)
            self.assertEqual(status, 'blocked')


class BridgeTest(unittest.TestCase):
    def setUp(self):
        self._directory = tempfile.TemporaryDirectory()
        self.addCleanup(self._directory.cleanup)
        self.root = Path(self._directory.name)
        self.design = self.root / 'sensor.kicad_sch'
        shutil.copyfile(FIXTURE, self.design)

    def request(self, kind, **payload):
        return {
            'protocol_version': 1,
            'operation_id': 'op-1',
            'kind': kind,
            'goal_id': 'g',
            'expected_artifact_id': '',
            'payload': {'path': str(self.design), 'allowed_root': str(self.root), **payload},
        }

    def call_bridge_erc(self, violations, **payload):
        with mock.patch('erc.subprocess.run', fake_kicad_cli(violations=violations)):
            return bridge.handle(self.request('kicad.run_erc', **payload))

    def test_missing_threshold_defaults_to_zero(self):
        report = str(self.root / 'reports' / 'erc.json')
        self.assertEqual(self.call_bridge_erc(0, report_path=report)['status'], 'pass')
        self.assertEqual(self.call_bridge_erc(1, report_path=report)['status'], 'fail')

    def test_request_threshold_is_respected(self):
        report = str(self.root / 'reports' / 'erc.json')
        result = self.call_bridge_erc(2, report_path=report, max_violations=2)
        self.assertEqual(result['status'], 'pass')
        self.assertEqual(result['postcondition']['violation_count'], 2)
        self.assertEqual(result['postcondition']['max_violations'], 2)

    def test_invalid_threshold_blocked(self):
        report = str(self.root / 'reports' / 'erc.json')
        result = self.call_bridge_erc(0, report_path=report, max_violations=-1)
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(result['error_code'], 'invalid_max_violations')

    def test_inspect_design_reports_connection_checker(self):
        result = bridge.handle(self.request('inspect_design'))
        self.assertEqual(result['status'], 'observed')
        facts = result['postcondition']
        self.assertEqual(facts['connection_checker_id'], CONNECTION_CHECKER_ID)
        self.assertEqual(facts['connection_checker_version'], CONNECTION_CHECKER_VERSION)
        self.assertNotIn('screenshot', json.dumps(facts))


if __name__ == '__main__':
    unittest.main()
