from __future__ import annotations

import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

from scripts.lceda.j00_isolation import (
    IsolationVerifier,
    ProcessSupervisor,
    contained_path,
    sha256_tree,
)


class IsolationContractTests(unittest.TestCase):
    def test_candidate_copy_is_distinct_and_formal_digest_is_stable(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            formal = base / "formal"
            formal.mkdir()
            (formal / "project.json").write_text('{"value":"10k"}\n', encoding="utf-8")
            candidate = base / "candidate"
            verifier = IsolationVerifier(formal, candidate, profile_root=base / "profile", protect_formal=True)
            result = verifier.prepare()
            self.assertTrue(result.candidate_distinct)
            self.assertTrue(verifier.verify_candidate_identity())
            (candidate / "project.json").write_text('{"value":"12k"}\n', encoding="utf-8")
            self.assertTrue(verifier.verify_formal_unchanged())
            self.assertEqual(sha256_tree(formal), result.formal_before)
            verifier.finalize()
            self.assertTrue(os.access(formal / "project.json", os.W_OK))

    def test_path_containment_rejects_absolute_and_traversal(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            with self.assertRaises(ValueError):
                contained_path(base, "../outside")
            with self.assertRaises(ValueError):
                contained_path(base, "/etc/passwd")
            inside = contained_path(base, "child/file.txt")
            self.assertEqual(inside, base / "child/file.txt")

    def test_path_containment_rejects_symlink_component(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            outside = base / "outside"
            outside.mkdir()
            (base / "link").symlink_to(outside, target_is_directory=True)
            with self.assertRaises(ValueError):
                contained_path(base, "link/file.txt")

    def test_symlinked_formal_entries_fail_closed(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            formal = base / "formal"
            formal.mkdir()
            outside = base / "outside.txt"
            outside.write_text("outside", encoding="utf-8")
            (formal / "link").symlink_to(outside)
            with self.assertRaises(ValueError):
                IsolationVerifier(formal, base / "candidate").prepare()

    def test_supervisor_timeout_terminates_process_group(self) -> None:
        supervisor = ProcessSupervisor(default_timeout=0.05)
        result = supervisor.run([sys.executable, "-c", "import time; time.sleep(5)"], timeout=0.05)
        self.assertTrue(result.timed_out)
        self.assertFalse(supervisor.children)
        self.assertIsNotNone(result.returncode)

    def test_cleanup_reports_live_processes(self) -> None:
        supervisor = ProcessSupervisor(default_timeout=1)
        process = __import__("subprocess").Popen([sys.executable, "-c", "import time; time.sleep(5)"], start_new_session=True)
        supervisor.children.add(process.pid)
        remaining = supervisor.terminate_tree()
        self.assertFalse(remaining)
        deadline = time.time() + 2
        while process.poll() is None and time.time() < deadline:
            time.sleep(0.02)
        self.assertIsNotNone(process.poll())

    def test_windows_termination_falls_back_to_process_methods(self) -> None:
        process = Mock()

        def fake_kill(_pid: int, sig: int) -> None:
            if sig == 0:
                raise ProcessLookupError
            raise AttributeError("os.kill unavailable")

        with patch.object(os, "name", "nt"), patch.object(os, "kill", side_effect=fake_kill):
            self.assertTrue(ProcessSupervisor.terminate_pid(12345, grace=0, process=process))
        process.terminate.assert_called_once_with()


if __name__ == "__main__":
    unittest.main()
