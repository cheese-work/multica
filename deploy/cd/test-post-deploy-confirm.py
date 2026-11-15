#!/usr/bin/env python3
"""post-deploy-confirm.sh judges the whole 15-minute window from history after the delay (CHE-5175)."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]
DEPLOYED, NEWER = "deployed", "newer"
DIGEST = "sha256:" + "1" * 64
T0 = "1700000000"
OBS = "run-7-1"


def event(action, service="backend-blue"):
    return {"Type": "container", "Action": action, "Actor": {"Attributes": {"com.docker.compose.service": service}}}


class ConfirmTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        (self.dir / "deploy/cd").mkdir(parents=True)
        (self.dir / "bin").mkdir()
        (self.dir / "state").mkdir()
        (self.dir / "state/cutover-state.json").write_text(json.dumps({"active_colour": "blue"}))
        shutil.copy(ROOT / "deploy/cd/post-deploy-confirm.sh", self.dir / "deploy/cd/")
        self.stub("deploy/cd/health-window.mjs", "")
        subprocess.run(["git", "init", "-q"], cwd=self.dir, check=True)
        self.commits = {}
        for name in [DEPLOYED, NEWER]:
            subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@e.i",
                            "commit", "-q", "--allow-empty", "-m", name], cwd=self.dir, check=True)
            self.commits[name] = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.dir, text=True).strip()
        (self.dir / "manifest.json").write_text(json.dumps({"image_digests": {"backend": DIGEST, "web": DIGEST}}))

    def stub(self, path, body):
        target = self.dir / path
        target.write_text(body)
        target.chmod(0o755)

    def journal(self, ok=True, reason="journal is not ok"):
        """Stand-in for `health-window.mjs verify`; test-health-window.py owns what it accepts."""
        self.stub("deploy/cd/health-window.mjs",
                  f'import fs from "node:fs";\nfs.writeFileSync("{self.dir}/verify-args", process.argv.slice(2).join(" "));\n'
                  + ('' if ok else f'console.error("::error::{reason}");\nprocess.exit(1);\n'))

    def confirm(self, events=(), running=DEPLOYED, digest=DIGEST, restarts=0, baseline=0, healthy=True, t0=T0,
                journal_ok=True, journal_reason="journal is not ok", obs=OBS):
        self.journal(journal_ok, journal_reason)
        live = {"application_sha": self.commits[running], "images": {"backend": {"digest": digest}, "web": {"digest": digest}}}
        self.stub("deploy/cd/capture-remote-baseline.sh", f"echo '{json.dumps(live)}' > \"$1\"\n")
        self.stub("deploy/cd/restart-count.sh", f"echo {restarts}\n")
        self.stub("deploy/cd/ab-health-check.sh",
                  f'echo "$@" > "{self.dir}/health-args"\n' + ("" if healthy else 'echo "::error::route unhealthy" >&2\nexit 1\n'))
        lines = "\n".join(json.dumps(e) for e in events)
        (self.dir / "events.ndjson").write_text(lines + ("\n" if lines else ""))
        # `docker events` is the only docker call the script makes; record its arguments.
        self.stub("bin/docker", f'case "$1" in\n'
                  f'  events) echo "$@" > "{self.dir}/docker-args"; cat "{self.dir}/events.ndjson" ;;\n'
                  f'  *) exit 64 ;;\nesac\n')
        env = {**os.environ, "PATH": f"{self.dir}/bin:{os.environ['PATH']}", "C00_CUTOVER_STATE_DIR": str(self.dir / "state"),
               "C00_ROUTER_STATE_DIR": "/r",
               "C00_STATE_DIR": "/state"}
        args = ["bash", "deploy/cd/post-deploy-confirm.sh", "check", self.commits[DEPLOYED], t0, str(baseline), obs, "manifest.json"]
        return subprocess.run(args, cwd=self.dir, env=env, text=True, capture_output=True)

    def test_a_quiet_window_passes_and_asks_docker_for_history_since_t0(self):
        result = self.confirm(events=[event("start"), event("exec_start")])
        self.assertEqual(result.returncode, 0, result.stderr)
        args = (self.dir / "docker-args").read_text()
        self.assertIn(f"--since {T0}", args)
        self.assertIn("--until", args)

    def test_unhealthy_event_in_the_window_fails(self):
        result = self.confirm(events=[event("health_status: unhealthy", "frontend-blue")])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("health_status: unhealthy", result.stderr)

    def test_die_oom_and_restart_events_fail(self):
        for action in ["die", "oom", "restart"]:
            with self.subTest(action=action):
                self.assertNotEqual(self.confirm(events=[event(action)]).returncode, 0)

    def test_events_of_other_services_or_the_other_colour_are_ignored(self):
        events = [event("die", "postgres"), event("die", "backend-green"), event("die", "router")]
        self.assertEqual(self.confirm(events=events).returncode, 0)

    def test_a_grown_restart_count_fails(self):
        result = self.confirm(restarts=3, baseline=2)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("restart", result.stderr)

    def test_an_unchanged_restart_count_passes(self):
        self.assertEqual(self.confirm(restarts=2, baseline=2).returncode, 0)

    def test_digest_mismatch_fails(self):
        result = self.confirm(digest="sha256:" + "2" * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("digest", result.stderr)

    def test_a_descendant_deploy_passes_even_with_old_colour_events(self):
        result = self.confirm(running=NEWER, events=[event("die"), event("health_status: unhealthy")])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("descendant", result.stdout)

    def test_a_failing_route_health_check_at_confirmation_fails_even_with_a_quiet_event_log(self):
        result = self.confirm(events=[event("start")], healthy=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("route unhealthy", result.stderr)

    def test_a_healthy_route_is_sampled_with_the_router_and_cutover_state(self):
        self.assertEqual(self.confirm().returncode, 0)
        args = (self.dir / "health-args").read_text()
        self.assertIn("--router-state-dir /r", args)
        self.assertIn(f"--cutover-state-dir {self.dir / 'state'}", args)

    def test_the_journal_is_verified_for_this_deploy(self):
        result = self.confirm()
        self.assertEqual(result.returncode, 0, result.stderr)
        args = (self.dir / "verify-args").read_text()
        self.assertEqual(args, f"verify --dir /state/health-observer --id {OBS} --sha {self.commits[DEPLOYED]}")

    def test_a_journal_that_does_not_verify_fails_even_with_a_healthy_route_and_quiet_events(self):
        # Closes the passive-log gap: a recovered no-traffic outage or a /health 500 leaves
        # failed samples in the journal, and nothing else in the window.
        result = self.confirm(journal_ok=False, journal_reason="sample 7 failed: GET /health 500")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("sample 7 failed", result.stderr)
        self.assertIn("journal", result.stderr)

    def test_the_router_log_is_not_read(self):
        self.assertEqual(self.confirm().returncode, 0)
        self.assertFalse((self.dir / "docker-logs-args").exists())

    def test_a_descendant_deploy_does_not_verify_the_older_journal(self):
        # The newer deploy has its own journal and confirm; the older window never rolls it back.
        result = self.confirm(running=NEWER, journal_ok=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.dir / "verify-args").exists())

    def test_the_observation_id_is_required_and_checked(self):
        env = {**os.environ, "C00_CUTOVER_STATE_DIR": str(self.dir / "state"), "C00_ROUTER_STATE_DIR": "/r", "C00_STATE_DIR": "/state"}
        result = subprocess.run(["bash", "deploy/cd/post-deploy-confirm.sh", "check", self.commits[DEPLOYED], T0, "0", "../x", "manifest.json"],
                                cwd=self.dir, env=env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 2)
        result = subprocess.run(["bash", "deploy/cd/post-deploy-confirm.sh", "check", self.commits[DEPLOYED], T0, "0"],
                                cwd=self.dir, env=env, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)

    def test_a_window_that_has_not_elapsed_fails(self):
        for t0 in [str(int(time.time())), str(int(time.time()) - 600)]:
            with self.subTest(t0=t0):
                result = self.confirm(t0=t0)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("window", result.stderr)

    def test_a_window_that_has_elapsed_passes(self):
        self.assertEqual(self.confirm(t0=str(int(time.time()) - 901)).returncode, 0)

    def test_bad_arguments_are_refused(self):
        env = {**os.environ, "C00_CUTOVER_STATE_DIR": str(self.dir / "state")}
        for args in [["check", "abc", T0, "0", OBS], ["check", self.commits[DEPLOYED], "now", "0", OBS], ["check", self.commits[DEPLOYED], T0, "x", OBS], ["nope"]]:
            with self.subTest(args=args):
                result = subprocess.run(["bash", "deploy/cd/post-deploy-confirm.sh", *args], cwd=self.dir, env=env, text=True, capture_output=True)
                self.assertEqual(result.returncode, 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
