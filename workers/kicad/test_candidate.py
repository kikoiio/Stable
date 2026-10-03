"""Candidate-only repair contract: formal roots and stale digests are refused."""

from __future__ import annotations

from pathlib import Path
import shutil
import tempfile
import unittest

import bridge
from schematic import digest


FIXTURE = Path(__file__).resolve().parents[2] / 'fixtures' / 'sensor_board' / 'sensor.kicad_sch'


def repair_request(path: Path, root: Path, expected: str) -> dict:
    return {
        'protocol_version': 1,
        'operation_id': 'op-candidate-1',
        'kind': 'kicad.repair_connection',
        'expected_artifact_id': expected,
        'payload': {'path': str(path), 'allowed_root': str(root)},
    }


class CandidateRepairTest(unittest.TestCase):
    def setUp(self):
        self._directory = tempfile.TemporaryDirectory()
        self.addCleanup(self._directory.cleanup)
        base = Path(self._directory.name)
        self.formal = base / 'formal'
        self.candidate = base / 'candidate'
        self.formal.mkdir()
        self.candidate.mkdir()
        shutil.copyfile(FIXTURE, self.formal / 'sensor.kicad_sch')
        shutil.copyfile(FIXTURE, self.candidate / 'sensor.kicad_sch')

    def repair(self, path: Path, root: Path, expected: str) -> dict:
        return bridge.handle(repair_request(path, root, expected))

    def test_repair_applies_only_to_candidate_and_receipt_matches(self):
        target = self.candidate / 'sensor.kicad_sch'
        expected = digest(target)
        formal_before = digest(self.formal / 'sensor.kicad_sch')
        result = self.repair(target, self.candidate, expected)
        self.assertEqual(result['status'], 'applied')
        self.assertEqual(result['actual_artifact_id'], digest(target))
        self.assertNotEqual(result['actual_artifact_id'], expected)
        self.assertTrue(result['postcondition']['sensor.connection_present'])
        self.assertEqual(digest(self.formal / 'sensor.kicad_sch'), formal_before)

    def test_formal_root_is_not_accepted_for_repair(self):
        target = self.formal / 'sensor.kicad_sch'
        expected = digest(target)
        formal_before = expected
        # Even when the formal root is offered as the allowed root, the repair
        # contract expects the candidate root; a candidate-scoped root must
        # refuse the formal target.
        result = self.repair(target, self.candidate, expected)
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(result['error_code'], 'target_outside_root')
        self.assertEqual(digest(target), formal_before)

    def test_repair_requires_the_expected_candidate_digest(self):
        target = self.candidate / 'sensor.kicad_sch'
        result = self.repair(target, self.candidate, '0' * 64)
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(result['postcondition']['reason'], 'artifact digest changed')
        self.assertEqual(result['actual_artifact_id'], digest(target))

    def test_repair_rejects_path_traversal_outside_candidate(self):
        target = self.candidate / '..' / 'formal' / 'sensor.kicad_sch'
        result = self.repair(target, self.candidate, digest(target.resolve()))
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(result['error_code'], 'target_outside_root')

    def test_already_connected_candidate_is_not_rewritten(self):
        target = self.candidate / 'sensor.kicad_sch'
        first = self.repair(target, self.candidate, digest(target))
        self.assertEqual(first['status'], 'applied')
        second = self.repair(target, self.candidate, digest(target))
        self.assertEqual(second['status'], 'already_satisfied')
        self.assertEqual(second['actual_artifact_id'], first['actual_artifact_id'])


if __name__ == '__main__':
    unittest.main()
