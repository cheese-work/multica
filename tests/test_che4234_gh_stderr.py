"""CHE-4234: a failing `gh api` call must show its stderr, so a red gate run names its cause."""

from __future__ import annotations

import importlib.util
import os
import stat
import subprocess
import tempfile
import unittest
import unittest.mock
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("security_gate", ROOT / "scripts/security-gate/security_gate.py")
sg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sg)


class GhStderrTest(unittest.TestCase):
    def test_failure_surfaces_stderr_and_still_raises(self):
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp, "gh")
            fake.write_text("#!/bin/sh\necho 'gh: HTTP 502 Bad Gateway' >&2\nexit 1\n")
            fake.chmod(fake.stat().st_mode | stat.S_IEXEC)
            old = os.environ["PATH"]
            os.environ["PATH"] = f"{tmp}:{old}"
            try:
                with self.assertRaises(subprocess.CalledProcessError) as ctx, \
                        unittest.mock.patch("sys.stderr", new_callable=__import__("io").StringIO) as err:
                    sg._gh("repos/o/r/pulls/1")
            finally:
                os.environ["PATH"] = old
        self.assertIn("HTTP 502 Bad Gateway", err.getvalue())
        self.assertIn("HTTP 502 Bad Gateway", ctx.exception.stderr)


if __name__ == "__main__":
    unittest.main()
