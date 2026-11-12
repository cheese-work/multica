#!/usr/bin/env python3
"""post-deploy-readback.sh passes only when C00 is healthy and runs the deployed commit (CHE-4582)."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/cd/post-deploy-readback.sh"
DEPLOYED, NEWER, OTHER = "a" * 40, "b" * 40, "c" * 40
DIGEST = "sha256:" + "1" * 64


class ReadbackTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        (self.dir / "deploy/cd").mkdir(parents=True)
        shutil.copy(SCRIPT, self.dir / "deploy/cd/post-deploy-readback.sh")
        subprocess.run(["git", "init", "-q"], cwd=self.dir, check=True)
        self.commits = {}
        for name in ["deployed", "newer", "other"]:
            if name == "other":
                subprocess.run(["git", "checkout", "-q", "--orphan", "side"], cwd=self.dir, check=True)
            subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@e.i", "commit", "-q", "--allow-empty", "-m", name],
                           cwd=self.dir, check=True)
            self.commits[name] = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.dir, text=True).strip()
        self.arm_args = self.dir / "arm-args"
        self.arm_fails = False
        self.arm_exit = None
        self.manifest = self.dir / "manifest.json"
        self.manifest.write_text(json.dumps({"image_digests": {"backend": DIGEST, "web": DIGEST}}))

    def stub(self, name, body):
        (self.dir / "deploy/cd" / name).write_text(body)

    def env(self, **extra):
        # health-window.mjs is stubbed: its own suite covers the journal.
        (self.dir / "deploy/cd/health-window.mjs").write_text(
            f'import fs from "node:fs";\nfs.writeFileSync("{self.arm_args}", process.argv.slice(2).join(" "));\n'
            + ('console.error("::error::no sample from the native timer");process.exit(1);\n' if self.arm_fails else "")
            + (f'console.error("::error::C00 no longer runs the armed commit");process.exit({self.arm_exit});\n' if self.arm_exit else ""))
        return {**os.environ, "C00_ROUTER_STATE_DIR": "/r", "C00_CUTOVER_STATE_DIR": "/c", "C00_COMPOSE_DIR": "/compose",
                "C00_STATE_DIR": "/state", "OBSERVATION_ID": "run-7-1", **extra}

    def output(self):
        out = self.dir / "gh-output"
        return out.read_text() if out.exists() else ""

    def readback(self, health_ok=True, running="deployed", digest=DIGEST, manifest=True, drop_env=None, then=None):
        """`then` is the commit C00 runs on the second baseline read (after a newer deploy landed mid-arm)."""
        sha = self.commits["deployed"]
        self.stub("ab-health-check.sh", "exit 0\n" if health_ok else "echo '::error::unhealthy'; exit 1\n")

        def baseline(name):
            live = {"application_sha": self.commits[name], "images": {"backend": {"digest": digest}, "web": {"digest": digest}}}
            return json.dumps(live)

        reads = self.dir / "reads"
        reads.write_text("0")
        second = baseline(then or running)
        self.stub("capture-remote-baseline.sh",
                  f'n=$(( $(cat {reads}) + 1 )); echo $n > {reads}\n'
                  f'if [ "$n" -ge 2 ]; then echo \'{second}\' > "$1"; else echo \'{baseline(running)}\' > "$1"; fi\n')
        env = self.env(GITHUB_OUTPUT=str(self.dir / "gh-output"))
        env.pop(drop_env, None)
        args = ["bash", "deploy/cd/post-deploy-readback.sh", sha, "1"] + (["manifest.json"] if manifest else [])
        return subprocess.run(args, cwd=self.dir, env=env, text=True, capture_output=True)

    def test_healthy_deployed_commit_with_matching_digests_passes(self):
        self.assertEqual(self.readback().returncode, 0)

    def test_unhealthy_window_fails(self):
        self.assertNotEqual(self.readback(health_ok=False).returncode, 0)

    def test_digest_mismatch_fails(self):
        result = self.readback(digest="sha256:" + "2" * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("digest", result.stderr)

    def test_unrelated_running_commit_fails(self):
        result = self.readback(running="other")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not", result.stderr)

    def test_a_descendant_running_commit_passes(self):
        self.assertEqual(self.readback(running="newer").returncode, 0)

    def test_a_descendant_reports_superseded_so_the_workflow_skips_the_old_window(self):
        # Without t0 and observation the confirm job cannot judge this deploy; the workflow
        # must skip the delay and confirm instead of failing the older run (CHE-5175).
        for kwargs in ({"running": "newer"}, {"running": "deployed", "then": "newer"}):
            with self.subTest(**kwargs):
                (self.dir / "gh-output").unlink(missing_ok=True)
                self.arm_exit = 3 if "then" in kwargs else None
                result = self.readback(**kwargs)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("superseded=true", self.output())
                self.assertNotIn("t0=", self.output())

    def test_the_deployed_commit_is_not_reported_superseded(self):
        self.assertEqual(self.readback().returncode, 0)
        self.assertNotIn("superseded", self.output())

    def test_unhealthy_route_is_retried_until_healthy_within_the_wait(self):
        # CHE-5175: the readback waits for health, bounded, instead of watching a window.
        sha = self.commits["deployed"]
        counter = self.dir / "tries"
        counter.write_text("0")
        self.stub("ab-health-check.sh", f'n=$(( $(cat {counter}) + 1 )); echo $n > {counter}; [ "$n" -ge 2 ]\n')
        live = {"application_sha": sha, "images": {"backend": {"digest": DIGEST}, "web": {"digest": DIGEST}}}
        self.stub("capture-remote-baseline.sh", f"echo '{json.dumps(live)}' > \"$1\"\n")
        env = self.env(READBACK_RETRY_SECONDS="0")
        result = subprocess.run(["bash", "deploy/cd/post-deploy-readback.sh", sha, "30", "manifest.json"], cwd=self.dir, env=env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(counter.read_text().strip(), "2")

    def test_the_health_check_samples_once_per_try_not_a_window(self):
        sha = self.commits["deployed"]
        self.stub("ab-health-check.sh", 'echo "$@" >> args; exit 0\n')
        live = {"application_sha": sha, "images": {"backend": {"digest": DIGEST}, "web": {"digest": DIGEST}}}
        self.stub("capture-remote-baseline.sh", f"echo '{json.dumps(live)}' > \"$1\"\n")
        env = self.env()
        subprocess.run(["bash", "deploy/cd/post-deploy-readback.sh", sha, "120"], cwd=self.dir, env=env, check=True, capture_output=True)
        self.assertIn("--window 0", (self.dir / "args").read_text())

    def test_a_healthy_route_arms_the_native_observer_for_the_whole_window(self):
        self.assertEqual(self.readback().returncode, 0)
        args = self.arm_args.read_text()
        sha = self.commits["deployed"]
        for part in ["arm", "--dir /state/health-observer", "--id run-7-1", f"--sha {sha}", "--window 900", "--interval 10",
                     "--verify-seconds 30", "--compose-dir /compose", "--cutover-state-dir /c", "--router-state-dir /r",
                     "--health-script deploy/cd/ab-health-check.sh", "--live-sha-script deploy/cd/live-application-sha.sh"]:
            self.assertIn(part, args)

    def test_a_missing_sample_from_the_native_timer_fails_the_readback(self):
        self.arm_fails = True
        result = self.readback()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("native timer", result.stderr)

    def test_an_unhealthy_route_never_arms(self):
        self.assertNotEqual(self.readback(health_ok=False).returncode, 0)
        self.assertFalse(self.arm_args.exists())

    def test_a_newer_deploy_that_already_landed_is_never_armed_over(self):
        # CHE-5175: a delayed older readback must not supersede the newer deploy's journal.
        result = self.readback(running="newer")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.arm_args.exists())

    def test_a_newer_deploy_that_lands_while_arming_passes_without_a_journal(self):
        self.arm_exit = 3
        result = self.readback(running="deployed", then="newer")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("descendant", result.stdout)

    def test_arming_refused_for_an_unrelated_commit_fails(self):
        self.arm_exit = 3
        result = self.readback(running="deployed", then="other")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not", result.stderr)

    def test_arming_refused_while_c00_still_runs_the_deployed_commit_fails(self):
        self.arm_exit = 3
        self.assertNotEqual(self.readback(running="deployed", then="deployed").returncode, 0)

    def test_an_unrelated_running_commit_never_arms(self):
        self.assertNotEqual(self.readback(running="other").returncode, 0)
        self.assertFalse(self.arm_args.exists())

    def test_the_observation_id_is_required(self):
        result = self.readback(drop_env="OBSERVATION_ID")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("OBSERVATION_ID", result.stderr)

    def test_short_sha_is_refused(self):
        env = self.env()
        result = subprocess.run(["bash", "deploy/cd/post-deploy-readback.sh", "abc", "1"], cwd=self.dir, env=env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
