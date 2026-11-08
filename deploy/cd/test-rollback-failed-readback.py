#!/usr/bin/env python3
"""rollback-failed-readback.sh rolls C00 back by digest after a failed readback (CHE-4582, CHE-2365).

The controller and health check are stubs. The real render-cutover-remote-script.sh
builds the command and the real live-application-sha.sh identifies the live commit
through a stub `docker`. The full baseline capture is a stub that fails until the
rollback has run, as it does on C00 while the failed colour is stopped.
Nothing here reaches C00.
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
DEPLOYED, NEWER, PREDECESSOR = "a" * 40, "b" * 40, "c" * 40


class RollbackTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        (self.dir / "deploy/cd").mkdir(parents=True)
        for name in ["rollback-failed-readback.sh", "render-cutover-remote-script.sh", "live-application-sha.sh"]:
            if (CD / name).exists():
                shutil.copy(CD / name, self.dir / "deploy/cd" / name)
        self.compose = self.dir / "compose"
        self.compose.mkdir()
        (self.compose / ".env").write_text("POSTGRES_USER=u\nPOSTGRES_PASSWORD=p\nPOSTGRES_DB=d\n")
        self.cutover_state = self.dir / "cutover-state"
        self.cutover_state.mkdir()
        (self.cutover_state / "cutover-state.json").write_text('{"active_colour": "blue"}')
        self.calls = self.dir / "calls"
        self.calls.write_text("")
        self.bin = self.dir / "bin"
        self.bin.mkdir()

    def stub(self, name, body):
        (self.dir / "deploy/cd" / name).write_text(body)

    def docker(self, label="", tag="", services_running=False):
        # The active colour's backend is a stopped container: `ps -q` lists
        # running containers only, `ps -aq` lists it too.
        images = json.dumps([{"Repository": "r", "Tag": tag}]) if tag else "[]"
        script = f'''#!/usr/bin/env bash
case "$*" in
  *"ps -aq backend-blue"*) echo stopped-backend ;;
  *"ps -q"*) {'echo running-backend' if services_running else ':'} ;;
  *"images backend-blue"*) echo '{images}' ;;
  inspect*revision*stopped-backend) echo '{label}' ;;
  *) exit 1 ;;
esac
'''
        (self.bin / "docker").write_text(script)
        (self.bin / "docker").chmod(0o755)

    def rollback(self, running=DEPLOYED, rollback_ok=True, healthy_after=True, sha=DEPLOYED, label=None, tag=""):
        self.docker(label=running if label is None else label, tag=tag)
        live = json.dumps({"application_sha": PREDECESSOR})
        # The full baseline needs running services: it fails until the rollback ran.
        self.stub("capture-remote-baseline.sh",
                  f'grep -q cutover "{self.calls}" || {{ echo "could not read compose config-hash" >&2; exit 1; }}\n'
                  f"echo '{live}' > \"$1\"\n")
        self.stub("cutover.sh",
                  f'echo "cutover $* url=$CUTOVER_DATABASE_URL router=$ROUTER_STATE_DIR token=$GHCR_PULL_TOKEN" >> "{self.calls}"\n'
                  + ("" if rollback_ok else "exit 1\n"))
        self.stub("ab-health-check.sh", f'echo "health $*" >> "{self.calls}"\n' + ("" if healthy_after else "exit 1\n"))
        env = {**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}", "C00_COMPOSE_DIR": str(self.compose),
               "C00_CUTOVER_STATE_DIR": str(self.cutover_state), "C00_ROUTER_STATE_DIR": "/router-state", "GHCR_PULL_TOKEN": "tok"}
        return subprocess.run(["bash", "deploy/cd/rollback-failed-readback.sh", sha], cwd=self.dir, env=env, text=True, capture_output=True)

    def recorded(self):
        return self.calls.read_text()

    def test_rolls_back_by_digest_when_c00_still_runs_the_deployed_commit(self):
        result = self.rollback()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        text = self.recorded()
        self.assertIn(f"cutover rollback --compose-dir {self.compose} --state-dir {self.cutover_state}", text)
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

    def test_rolls_back_when_the_active_colour_is_stopped(self):
        # CHE-4582 review finding 1: the failed colour went down, so the full
        # baseline capture cannot run. Identity comes from the stopped container.
        result = self.rollback()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("cutover rollback", self.recorded())

    def test_a_stopped_newer_commit_is_still_protected(self):
        result = self.rollback(running=NEWER)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("cutover", self.recorded())

    def test_an_unidentifiable_live_commit_fails_closed(self):
        result = self.rollback(label="")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("cutover", self.recorded())

    def test_identity_falls_back_to_the_image_tag_resolved_in_git(self):
        subprocess.run("git init -q . && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m x", shell=True, cwd=self.dir, check=True)
        head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=self.dir, text=True, capture_output=True, check=True).stdout.strip()
        result = self.rollback(sha=head, label="", tag=head[:8])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("cutover rollback", self.recorded())

    def test_short_sha_is_refused(self):
        self.assertEqual(self.rollback(sha="abc").returncode, 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
