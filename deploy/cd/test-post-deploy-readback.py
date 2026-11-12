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
        self.manifest = self.dir / "manifest.json"
        self.manifest.write_text(json.dumps({"image_digests": {"backend": DIGEST, "web": DIGEST}}))

    def stub(self, name, body):
        (self.dir / "deploy/cd" / name).write_text(body)

    def readback(self, health_ok=True, running="deployed", digest=DIGEST, manifest=True):
        sha = self.commits["deployed"]
        self.stub("ab-health-check.sh", "exit 0\n" if health_ok else "echo '::error::unhealthy'; exit 1\n")
        live = {"application_sha": self.commits[running],
                "images": {"backend": {"digest": digest}, "web": {"digest": digest}}}
        self.stub("capture-remote-baseline.sh", f"echo '{json.dumps(live)}' > \"$1\"\n")
        env = {**os.environ, "C00_ROUTER_STATE_DIR": "/r", "C00_CUTOVER_STATE_DIR": "/c"}
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

    def test_short_sha_is_refused(self):
        env = {**os.environ, "C00_ROUTER_STATE_DIR": "/r", "C00_CUTOVER_STATE_DIR": "/c"}
        result = subprocess.run(["bash", "deploy/cd/post-deploy-readback.sh", "abc", "1"], cwd=self.dir, env=env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
