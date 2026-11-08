#!/usr/bin/env python3
"""rollback-failed-readback.sh rolls C00 back by digest after a failed readback (CHE-4582, CHE-2365).

The controller, health check and baseline capture are stubs; the real
render-cutover-remote-script.sh builds the command. Nothing here reaches C00.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
CD = ROOT / "deploy/cd"
DEPLOYED, NEWER = "a" * 40, "b" * 40


class RollbackTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        (self.dir / "deploy/cd").mkdir(parents=True)
        for name in ["rollback-failed-readback.sh", "render-cutover-remote-script.sh"]:
            if (CD / name).exists():
                shutil.copy(CD / name, self.dir / "deploy/cd" / name)
        self.compose = self.dir / "compose"
        self.compose.mkdir()
        (self.compose / ".env").write_text("POSTGRES_USER=u\nPOSTGRES_PASSWORD=p\nPOSTGRES_DB=d\n")
        self.calls = self.dir / "calls"
        self.calls.write_text("")

    def stub(self, name, body):
        (self.dir / "deploy/cd" / name).write_text(body)

    def rollback(self, running=DEPLOYED, rollback_ok=True, healthy_after=True, sha=DEPLOYED):
        live = json.dumps({"application_sha": running})
        self.stub("capture-remote-baseline.sh", f"echo '{live}' > \"$1\"\n")
        self.stub("cutover.sh",
                  f'echo "cutover $* url=$CUTOVER_DATABASE_URL router=$ROUTER_STATE_DIR token=$GHCR_PULL_TOKEN" >> "{self.calls}"\n'
                  + ("" if rollback_ok else "exit 1\n"))
        self.stub("ab-health-check.sh", f'echo "health $*" >> "{self.calls}"\n' + ("" if healthy_after else "exit 1\n"))
        env = {**os.environ, "C00_COMPOSE_DIR": str(self.compose), "C00_CUTOVER_STATE_DIR": "/cutover-state",
               "C00_ROUTER_STATE_DIR": "/router-state", "GHCR_PULL_TOKEN": "tok"}
        return subprocess.run(["bash", "deploy/cd/rollback-failed-readback.sh", sha], cwd=self.dir, env=env, text=True, capture_output=True)

    def recorded(self):
        return self.calls.read_text()

    def test_rolls_back_by_digest_when_c00_still_runs_the_deployed_commit(self):
        result = self.rollback()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        text = self.recorded()
        self.assertIn(f"cutover rollback --compose-dir {self.compose} --state-dir /cutover-state", text)
        self.assertIn("url=postgres://u:p@postgres:5432/d?sslmode=disable", text)
        self.assertIn("router=/router-state", text)
        self.assertIn("token=tok", text)
        self.assertIn("health", text)

    def test_does_not_roll_back_a_commit_that_is_no_longer_live(self):
        # A newer deploy landed meanwhile: its colour is not ours to retire.
        result = self.rollback(running=NEWER)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("cutover", self.recorded())
        self.assertIn("no longer", result.stdout)

    def test_a_failed_rollback_fails_the_job_loudly(self):
        result = self.rollback(rollback_ok=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("health", self.recorded())

    def test_an_unhealthy_route_after_rollback_fails_the_job(self):
        self.assertNotEqual(self.rollback(healthy_after=False).returncode, 0)

    def test_short_sha_is_refused(self):
        self.assertEqual(self.rollback(sha="abc").returncode, 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
