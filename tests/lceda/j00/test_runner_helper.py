"""Lightweight checks for the cross-platform J00 runner helper."""

from __future__ import annotations

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[3]
RUNNER = ROOT / "scripts" / "lceda" / "j00_runner.py"


class RunnerHelperTests(unittest.TestCase):
    def _run(self, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(RUNNER), *args],
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )

    def test_prepare_creates_isolated_environment(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            result = self._run("prepare", "--repo-root", str(repo), "--run-id", "portable-test")
            self.assertEqual(result.returncode, 0, result.stderr)
            payload = json.loads(result.stdout)
            environment = payload["environment"]
            run_dir = Path(environment["run_dir"])
            self.assertTrue((run_dir / "environment.json").is_file())
            self.assertTrue((run_dir / "artifacts" / "environment.json").is_file())
            self.assertEqual(environment["headless_backend"], "native-platform")

            check = self._run("check", "--run-dir", str(run_dir), "--client", sys.executable)
            self.assertEqual(check.returncode, 0, check.stderr)
            self.assertEqual(json.loads(check.stdout)["status"], "ready")

            cleanup = self._run("cleanup", "--run-dir", str(run_dir))
            self.assertEqual(cleanup.returncode, 0, cleanup.stderr)
            self.assertFalse((run_dir / "work").exists())
            self.assertTrue((run_dir / "artifacts").is_dir())

    def test_run_id_rejects_path_traversal(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            result = self._run("prepare", "--repo-root", directory, "--run-id", "../escape")
            self.assertEqual(result.returncode, 2)
            self.assertIn("unsafe", result.stderr)

    def test_missing_public_client_metadata_is_unverified(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            result = self._run(
                "prepare",
                "--repo-root",
                directory,
                "--run-id",
                "unverified-client",
                "--download-client",
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            payload = json.loads(result.stdout)
            self.assertEqual(payload["resources"]["client"]["status"], "unverified")
            self.assertIn("client URL", payload["resources"]["client"]["reason"])


if __name__ == "__main__":
    unittest.main()
