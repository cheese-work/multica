"""CHE-3339: the mechanical-tier classifier runs on multica PRs, from the base commit, with multica paths mapped to tiers."""

from __future__ import annotations

import importlib.util
import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TIER_DIR = ROOT / "scripts/mechanical-tier"
WORKFLOW = ROOT / ".github/workflows/mechanical-tier.yml"
GIT_ENV = {**os.environ, "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
           "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"}


def load_classifier():
    spec = importlib.util.spec_from_file_location("mechanical_tier", TIER_DIR / "mechanical_tier.py")
    mt = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mt)
    return mt


def git(repo, *args):
    r = subprocess.run(["git", "-C", str(repo), *args], capture_output=True, env=GIT_ENV)
    if r.returncode:
        raise AssertionError(f"git {args}: {r.stderr.decode()}")
    return r.stdout.decode().strip()


def protected(files):
    """Paths the classifier flags as Tier 2 capabilities, for a head commit that adds `files` on a base carrying this
    repo's classifier and registry. A tier needs a pull request binding from GitHub, so the path rules are read instead."""
    mt, d = load_classifier(), tempfile.mkdtemp(prefix="mt-")
    try:
        git(d, "init", "-q", "-b", "main")
        for rel in ("mechanical_tier.py", "registry.json"):
            dst = Path(d, "scripts/mechanical-tier", rel)
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy(TIER_DIR / rel, dst)
        git(d, "add", "-A")
        git(d, "commit", "-q", "-m", "base")
        base = git(d, "rev-parse", "HEAD")
        for rel, text in files.items():
            Path(d, rel).parent.mkdir(parents=True, exist_ok=True)
            Path(d, rel).write_text(text)
        git(d, "add", "-A")
        git(d, "commit", "-q", "-m", "head")
        receipt = mt.classify(d, base, git(d, "rev-parse", "HEAD"))
        assert receipt["error"] is None and receipt["classifier"]["trusted"], receipt
        return {m["path"] for m in receipt["capability_matches"]}
    finally:
        shutil.rmtree(d, ignore_errors=True)


class WorkflowShape(unittest.TestCase):
    def setUp(self):
        self.text = WORKFLOW.read_text()

    def test_runs_on_pull_request_target_so_the_definition_comes_from_the_default_branch(self):
        self.assertRegex(self.text, r"(?m)^  pull_request_target:")
        self.assertNotRegex(self.text, r"(?m)^  pull_request:")

    def test_checks_out_only_the_base_and_never_the_head(self):
        self.assertEqual(self.text.count("actions/checkout@"), 1)
        self.assertIn("ref: ${{ github.event.pull_request.base.sha }}", self.text)
        self.assertIn("persist-credentials: false", self.text)
        self.assertNotIn("pull_request.head.ref", self.text)
        self.assertNotRegex(self.text, r"ref: \$\{\{ github\.event\.pull_request\.head")

    def test_discards_a_reused_workspace_before_checkout(self):
        self.assertLess(self.text.index("-mindepth 1 -delete"), self.text.index("actions/checkout@"))

    def test_does_not_use_the_classifier_from_the_head_or_post_secrets(self):
        self.assertNotIn("secrets.", self.text)
        self.assertIn("scripts/mechanical-tier/mechanical_tier.py classify", self.text)

    def test_stays_a_non_required_receipt_only_check(self):
        self.assertRegex(self.text, r"(?m)^name: mechanical-tier$")
        self.assertRegex(self.text, r"(?m)^permissions:\n  contents: read\n  pull-requests: read\n  checks: read\n  statuses: read\n")
        self.assertNotRegex(self.text, r"(?m)^\s+(statuses|checks|contents|pull-requests): write")


class MulticaPathsMapToTiers(unittest.TestCase):
    def test_privileged_multica_paths_are_tier_2(self):
        paths = ("server/migrations/999_x.up.sql", ".github/CODEOWNERS", ".github/workflows/ci.yml",
                 "server/internal/auth/jwt.go", "server/internal/middleware/client.go",
                 "server/internal/passkey/passkey.go", "scripts/security-gate/security_gate.py",
                 "tests/test_che2671_security_gate.py", "scripts/mechanical-tier/mechanical_tier.py",
                 "deploy/cd/run.sh", "Dockerfile", "server/internal/handler/issue.go",
                 "packages/core/api/schema.ts", "AGENTS.md", "skills/x/SKILL.md")
        self.assertEqual(protected({p: "x\n" for p in paths}), set(paths))

    def test_a_change_of_only_handler_tests_and_docs_is_not_flagged(self):
        paths = ("server/internal/handler/issue_test.go", "docs/note.md", "tests/test_other.py")
        self.assertEqual(protected({p: "x\n" for p in paths}), set())

    def test_registry_is_valid_and_has_no_active_waiver_claim(self):
        reg = json.loads((TIER_DIR / "registry.json").read_text())
        self.assertEqual(reg["schema"], 1)
        self.assertEqual(reg["covered_scopes"], [])


if __name__ == "__main__":
    unittest.main()
