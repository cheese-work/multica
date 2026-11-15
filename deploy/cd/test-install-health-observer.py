#!/usr/bin/env python3
"""install-health-observer.sh installs the native C00 health observer timer (CHE-5175)."""
from pathlib import Path
import os
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        (self.dir / "bin").mkdir()
        (self.dir / "unit").mkdir()
        (self.dir / "bin/systemctl").write_text(f'#!/bin/sh\necho "$@" >> "{self.dir}/systemctl.log"\n')
        (self.dir / "bin/systemctl").chmod(0o755)

    def install(self, *args, **extra):
        env = {**os.environ, "PATH": f"{self.dir}/bin:{os.environ['PATH']}", "SYSTEMD_UNIT_DIR": str(self.dir / "unit"), **extra}
        return subprocess.run(["bash", str(ROOT / "deploy/cd/install-health-observer.sh"), *args], env=env, text=True, capture_output=True)

    def test_installs_the_service_and_timer_for_the_state_dir_and_enables_the_timer(self):
        result = self.install("/srv/multica/state", "deploy")
        self.assertEqual(result.returncode, 0, result.stderr)
        service = (self.dir / "unit/multica-health-observer.service").read_text()
        timer = (self.dir / "unit/multica-health-observer.timer").read_text()
        self.assertIn("health-window.mjs tick --dir /srv/multica/state/health-observer --source timer", service)
        self.assertIn("User=deploy", service)
        self.assertIn("ConditionPathExists=/srv/multica/state/health-observer/active", service)
        self.assertIn("OnUnitActiveSec=5s", timer)
        self.assertNotIn("@", service + timer)
        log = (self.dir / "systemctl.log").read_text()
        self.assertIn("daemon-reload", log)
        self.assertIn("enable --now multica-health-observer.timer", log)

    def test_the_service_is_a_oneshot_that_holds_no_ci_resource(self):
        self.install("/srv/multica/state", "deploy")
        service = (self.dir / "unit/multica-health-observer.service").read_text()
        self.assertIn("Type=oneshot", service)
        self.assertIn("TimeoutStartSec=", service)

    def test_refuses_unsafe_arguments_and_installs_nothing(self):
        for args in [[], ["relative/dir", "deploy"], ["/srv/x y", "deploy"], ["/srv/x", "bad user"], ["/srv/x"]]:
            with self.subTest(args=args):
                result = self.install(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(list((self.dir / "unit").iterdir()), [])

    def test_the_unit_files_in_the_repo_have_no_unfilled_placeholder_other_than_the_three(self):
        text = "".join(p.read_text() for p in (ROOT / "deploy/cd/observer").iterdir())
        import re
        self.assertEqual(sorted(set(re.findall(r"@[A-Z_]+@", text))), ["@NODE@", "@STATE_DIR@", "@USER@"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
