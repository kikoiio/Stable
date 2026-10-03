"""Content-based dependency collection tests for the two supported checks."""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest import mock

import bridge
from dependencies import collect_dependencies
from schematic import CONNECTION_CHECKER_ID, CONNECTION_CHECKER_VERSION


FIXTURE = Path(__file__).resolve().parents[2] / 'fixtures' / 'sensor_board' / 'sensor.kicad_sch'


def table(entries: dict[str, str]) -> str:
    rows = '\n'.join(f'  (lib (name "{name}")(type "KiCad")(uri "{uri}")(options "")(descr "test"))' for name, uri in entries.items())
    return f'(sym_lib_table\n{rows}\n)\n'


class DependencyTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.design = self.root / 'sensor.kicad_sch'
        shutil.copyfile(FIXTURE, self.design)
        (self.root / 'sensor.kicad_pro').write_text('{}', encoding='utf-8')
        self.symbols = self.root / 'symbols'
        self.symbols.mkdir()
        (self.symbols / 'Device.kicad_sym').write_text('(kicad_symbol_lib (version 1))', encoding='utf-8')
        (self.symbols / 'Connector.kicad_sym').write_text('(kicad_symbol_lib (version 1))', encoding='utf-8')
        self.config = self.root / 'config'
        self.config_dir = self.config / 'kicad' / '9.0'
        self.config_dir.mkdir(parents=True)
        (self.config_dir / 'sym-lib-table').write_text(table({
            'Device': '${KICAD9_SYMBOL_DIR}/Device.kicad_sym',
            'Connector': '${KICAD9_SYMBOL_DIR}/Connector.kicad_sym',
        }), encoding='utf-8')
        self.env = {
            'XDG_CONFIG_HOME': str(self.config),
            'XDG_CACHE_HOME': str(self.root / 'cache'),
            'XDG_DATA_HOME': str(self.root / 'data'),
            'KICAD9_SYMBOL_DIR': str(self.symbols),
        }

    def collect(self, checker_version='9.0.8'):
        return collect_dependencies(self.design, self.root, env=self.env, checker_version=checker_version)

    def erc(self):
        return self.collect()[0]

    def connection(self):
        return self.collect()[1]

    def test_baseline_has_both_families_and_optional_project_table(self):
        erc, connection = self.collect()
        self.assertTrue(erc['available'], erc['reason'])
        self.assertEqual(erc['family'], 'kicad.erc')
        self.assertEqual(connection['family'], 'sensor.connection')
        self.assertEqual(connection['checker_id'], CONNECTION_CHECKER_ID)
        self.assertEqual(connection['checker_version'], CONNECTION_CHECKER_VERSION)
        project_table = next(x for x in erc['sources'] if x['kind'] == 'project_symbol_table')
        self.assertEqual(project_table['state'], 'absent_optional')
        self.assertTrue(any(x['kind'] == 'global_symbol_table' for x in erc['sources']))
        self.assertTrue(all(x['state'] == 'present' for x in erc['sources'] if x['kind'] == 'symbol_library'))

    def test_only_erc_settings_affect_project_settings_fingerprint(self):
        before = self.erc()['fingerprint']
        (self.root / 'sensor.kicad_pro').write_text(json.dumps({'board': {'page_layout_descr_file': 'x'}}), encoding='utf-8')
        self.assertEqual(self.erc()['fingerprint'], before)
        (self.root / 'sensor.kicad_pro').write_text(json.dumps({'schematic': {'erc_exclusions': ['rule-a']}}), encoding='utf-8')
        after = self.erc()
        self.assertNotEqual(after['fingerprint'], before)
        project_erc = next(x for x in after['sources'] if x['kind'] == 'project_erc')
        self.assertEqual(project_erc['state'], 'present')

    def test_mtime_does_not_affect_hash_but_content_does(self):
        before = self.erc()['fingerprint']
        project = self.root / 'sensor.kicad_pro'
        old = project.stat()
        os.utime(project, ns=(old.st_atime_ns, old.st_mtime_ns + 5_000_000_000))
        self.assertEqual(self.erc()['fingerprint'], before)
        project.write_text('{"erc":{"enabled":false}}', encoding='utf-8')
        os.utime(project, ns=(old.st_atime_ns, old.st_mtime_ns))
        self.assertNotEqual(self.erc()['fingerprint'], before)

    def test_symbol_library_content_changes_fingerprint(self):
        before = self.erc()['fingerprint']
        symbol = self.symbols / 'Device.kicad_sym'
        symbol.write_text('(kicad_symbol_lib (version 2))', encoding='utf-8')
        self.assertNotEqual(self.erc()['fingerprint'], before)

    def test_project_and_global_table_changes_are_tracked(self):
        before = self.erc()['fingerprint']
        (self.root / 'sym-lib-table').write_text(table({
            'Device': '${KICAD9_SYMBOL_DIR}/Device.kicad_sym',
            'Connector': '${KICAD9_SYMBOL_DIR}/Connector.kicad_sym',
            'Unused': '${KICAD9_SYMBOL_DIR}/Device.kicad_sym',
        }), encoding='utf-8')
        project = self.erc()
        self.assertNotEqual(project['fingerprint'], before)
        project_table = next(x for x in project['sources'] if x['kind'] == 'project_symbol_table')
        self.assertEqual(project_table['state'], 'present')
        # Project mappings now take precedence; changing the selected global
        # fallback no longer changes this project's dependency snapshot.
        self.assertFalse(any(x['kind'] == 'global_symbol_table' for x in project['sources']))

        (self.root / 'sym-lib-table').unlink()
        global_before = self.erc()['fingerprint']
        table_path = self.config_dir / 'sym-lib-table'
        table_path.write_text(table({
            'Device': '${KICAD9_SYMBOL_DIR}/Device.kicad_sym',
            'Connector': '${KICAD9_SYMBOL_DIR}/Connector.kicad_sym',
            'Unused': '${KICAD9_SYMBOL_DIR}/Device.kicad_sym',
        }), encoding='utf-8')
        self.assertNotEqual(self.erc()['fingerprint'], global_before)

    def test_path_variable_value_is_hashed_but_never_exposed(self):
        before = self.erc()['fingerprint']
        second = self.root / 'symbols-copy'
        shutil.copytree(self.symbols, second)
        self.env['KICAD9_SYMBOL_DIR'] = str(second)
        after = self.erc()
        self.assertNotEqual(after['fingerprint'], before)
        variable = next(x for x in after['sources'] if x['kind'] == 'path_variable')
        self.assertEqual(variable['identity'], 'KICAD9_SYMBOL_DIR')
        self.assertNotIn(str(second), json.dumps(after))

    def test_missing_malformed_inputs_and_unknown_checker_are_unavailable(self):
        project = self.root / 'sensor.kicad_pro'
        project.unlink()
        result = self.erc()
        self.assertFalse(result['available'])
        self.assertTrue(any(x['state'] == 'missing_required' for x in result['sources']))

        project.write_text('{bad json', encoding='utf-8')
        result = self.erc()
        self.assertFalse(result['available'])
        self.assertTrue(any(x['state'] == 'malformed' and x['kind'] == 'project_erc' for x in result['sources']))

        project.write_text('{}', encoding='utf-8')
        (self.symbols / 'Device.kicad_sym').unlink()
        result = self.erc()
        self.assertFalse(result['available'])
        self.assertTrue(any(x['state'] == 'missing_required' and x['identity'] == 'Device' for x in result['sources']))

        with mock.patch('dependencies.kicad_cli_version', return_value=None):
            self.assertFalse(collect_dependencies(self.design, self.root, env=self.env)[0]['available'])
        with mock.patch('dependencies.CONNECTION_CHECKER_VERSION', ''):
            connection = collect_dependencies(self.design, self.root, env=self.env)[1]
            self.assertFalse(connection['available'])
            self.assertIn('version unavailable', connection['reason'])

    def test_malformed_table_is_unavailable(self):
        (self.config_dir / 'sym-lib-table').write_text('(sym_lib_table (lib (name "Device"))', encoding='utf-8')
        result = self.erc()
        self.assertFalse(result['available'])
        self.assertTrue(any(x['state'] == 'malformed' and x['kind'] == 'global_symbol_table' for x in result['sources']))

    def test_bridge_read_only_operation_returns_dependencies(self):
        response = bridge.handle({
            'protocol_version': 1,
            'operation_id': 'dependencies-1',
            'kind': 'kicad.describe_dependencies',
            'goal_id': 'g',
            'payload': {'path': str(self.design), 'allowed_root': str(self.root)},
        })
        self.assertEqual(response['status'], 'observed')
        self.assertEqual(len(response['postcondition']['dependencies']), 2)

    def test_bridge_rejects_design_change_during_dependency_collection(self):
        collect = bridge.collect_dependencies

        def collect_then_change(path, root, env=None):
            result = collect(path, root)
            Path(path).write_text(Path(path).read_text(encoding='utf-8') + '\n', encoding='utf-8')
            return result

        with mock.patch('bridge.collect_dependencies', side_effect=collect_then_change):
            response = bridge.handle({
                'protocol_version': 1,
                'operation_id': 'dependencies-race',
                'kind': 'kicad.describe_dependencies',
                'goal_id': 'g',
                'payload': {'path': str(self.design), 'allowed_root': str(self.root)},
            })
        self.assertEqual(response['status'], 'stale')
        self.assertEqual(response['error_code'], 'design_changed_during_dependency_collection')


if __name__ == '__main__':
    unittest.main()
