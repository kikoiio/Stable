import shutil
import tempfile
import unittest
from pathlib import Path

from schematic import BOTTOM_LEAD, digest, inspect, repair


FIXTURE = Path(__file__).resolve().parents[2] / 'fixtures' / 'sensor_board' / 'sensor.kicad_sch'


class RepairTest(unittest.TestCase):
    def test_repair_once_and_preserve_fixture(self):
        original = digest(FIXTURE)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            run = root / 'sensor.kicad_sch'
            shutil.copyfile(FIXTURE, run)
            self.assertFalse(inspect(run)['sensor.connection_present'])
            status, changed, facts = repair(run, root, original)
            self.assertEqual(status, 'applied')
            self.assertTrue(facts['sensor.connection_present'])
            status, second, _ = repair(run, root, original)
            self.assertEqual(status, 'already_satisfied')
            self.assertEqual(changed, second)
            self.assertEqual(digest(FIXTURE), original)

    def test_old_digest_and_outside_path_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            run = root / 'sensor.kicad_sch'
            shutil.copyfile(FIXTURE, run)
            status, _, _ = repair(run, root, 'stale')
            self.assertEqual(status, 'blocked')
            status, _, _ = repair(FIXTURE, root, digest(FIXTURE))
            self.assertEqual(status, 'blocked')

    def test_unknown_layout_is_not_guessed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            run = root / 'sensor.kicad_sch'
            run.write_text(FIXTURE.read_text().replace(BOTTOM_LEAD, '(xy 10 10) (xy 20 20)'))
            before = digest(run)
            status, after, facts = repair(run, root, before)
            self.assertEqual(status, 'unsupported')
            self.assertFalse(facts['sensor.supported'])
            self.assertEqual(after, before)
            self.assertEqual(digest(run), before)


if __name__ == '__main__':
    unittest.main()
