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

import yaml

ROOT = Path(__file__).resolve().parents[1]
TIER_DIR = ROOT / "scripts/mechanical-tier"
WORKFLOW = ROOT / ".github/workflows/mechanical-tier.yml"
GIT_ENV = {**os.environ, "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
           "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"}


# `gh api <path> --jq <filter>`: serve canned JSON for the path and apply the filter with jq, as gh does.
FAKE_GH_SH = r"""#!/bin/bash
path="$2"; filter="$4"
case "$path" in
  repos/o/r/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/pulls) key=pulls_a ;;
  repos/o/r/commits/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee/pulls) key=pulls_e ;;
  repos/o/r/commits/*/pulls) key=none ;;
  repos/o/r/pulls/7) key=pr7 ;;
  repos/o/r/pulls/8) key=pr8 ;;
  repos/o/r/pulls/9) key=pr9 ;;
  "repos/o/r/pulls?state=open&sort=updated&direction=desc&per_page=30") key=open ;;
  repos/o/r/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/statuses*) key=st_a ;;
  repos/o/r/commits/cccccccccccccccccccccccccccccccccccccccc/statuses*) key=st_c ;;
  repos/o/r/commits/*/statuses*) key=none ;;
  *) echo "unexpected path $path" >&2; exit 1 ;;
esac
printf '%s' "$FIXTURE" | jq -rc ".$key" | jq -rc "$filter"
"""
A, B, C, E = "a" * 40, "b" * 40, "c" * 40, "e" * 40
FIND_FIXTURE = {
    "none": [],
    "pulls_a": [{"number": 7, "state": "open", "head": {"sha": A}}],
    "pulls_e": [{"number": 9, "state": "open", "head": {"sha": "f" * 40}}],
    "pr7": {"number": 7, "base": {"sha": B}, "head": {"sha": A}},
    "pr8": {"number": 8, "base": {"sha": B}, "head": {"sha": C}},
    "pr9": {"number": 9, "base": {"sha": B}, "head": {"sha": "f" * 40}},
    "open": [{"number": 8, "head": {"sha": C}}, {"number": 7, "head": {"sha": A}}],
    "st_a": [{"context": "security-gate", "updated_at": "2026-11-03T08:00:00Z"}],
    "st_c": [{"context": "security-gate", "updated_at": "2026-11-03T09:00:05Z"},
             {"context": "other", "updated_at": "2026-11-03T09:00:05Z"}],
}


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


def head_commit(files):
    """(mt, repo, base, head): a base commit carrying this repo's classifier and registry, and a head that adds `files`."""
    mt, d = load_classifier(), tempfile.mkdtemp(prefix="mt-")
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
    return mt, d, base, git(d, "rev-parse", "HEAD")


def protected(files):
    """Paths the classifier flags as Tier 2 capabilities, for a head commit that adds `files`. A tier needs a pull
    request binding from GitHub, so the path rules are read instead."""
    mt, d, base, head = head_commit(files)
    try:
        receipt = mt.classify(d, base, head)
        assert receipt["error"] is None and receipt["classifier"]["trusted"], receipt
        return {m["path"] for m in receipt["capability_matches"]}
    finally:
        shutil.rmtree(d, ignore_errors=True)


def fake_gh(base, head, ci_status, conclusion):
    """The GitHub API paths `fetch_ci` reads, for pull request 7 with one required check `build` on `head`."""
    def doc(obj):
        return 0, json.dumps(obj), ""

    def gh(path):
        if "/rules/branches/" in path:
            return doc([{"type": "required_status_checks", "parameters": {"required_status_checks": [{"context": "build"}]}}])
        if path.endswith("/branches/main"):
            return doc({"protected": True, "protection": {"required_status_checks": {"checks": [], "contexts": []}}})
        if "/check-runs" in path:
            run = {"id": 1, "name": "build", "status": ci_status, "conclusion": conclusion, "head_sha": head}
            return doc({"check_runs": [run]})
        if "/status" in path:
            return doc({"state": "success", "sha": head, "statuses": []})
        if "/pulls/" in path:
            return doc({"head": {"sha": head}, "base": {"ref": "main", "sha": base}})
        return 1, "", "unexpected path " + path
    return gh


class WorkflowShape(unittest.TestCase):
    def setUp(self):
        self.text = WORKFLOW.read_text()

    def test_runs_on_pull_request_target_so_the_definition_comes_from_the_default_branch(self):
        self.assertRegex(self.text, r"(?m)^  pull_request_target:")
        self.assertNotRegex(self.text, r"(?m)^  pull_request:")

    def test_checks_out_only_the_base_and_never_the_head(self):
        self.assertEqual(self.text.count("actions/checkout@"), 1)
        self.assertIn("ref: ${{ matrix.base }}", self.text)
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


class ReceiptRefreshAfterCi(unittest.TestCase):
    def setUp(self):
        self.text = WORKFLOW.read_text()

    def test_a_finished_ci_or_security_gate_run_re_runs_the_classifier(self):
        # `status` events posted with GITHUB_TOKEN start no workflow run; workflow_run does fire for them.
        self.assertRegex(self.text, r"(?m)^  workflow_run:\n    workflows: \[CI, security-gate\]\n    types: \[completed\]")
        self.assertNotRegex(self.text, r"(?m)^  status:")
        self.assertNotRegex(self.text, r"(?m)^  issue_comment:")

    def test_the_classifier_runs_once_per_pull_request_the_find_job_names(self):
        wf = yaml.safe_load(self.text)
        classify = wf["jobs"]["mechanical-tier"]
        self.assertEqual(classify["needs"], "find")
        self.assertEqual(classify["strategy"]["matrix"]["include"], "${{ fromJSON(needs.find.outputs.targets) }}")
        self.assertFalse(classify["strategy"]["fail-fast"])
        self.assertEqual(classify["concurrency"], {"group": "mechanical-tier-${{ matrix.head }}", "cancel-in-progress": False})
        self.assertEqual(classify["env"]["HEAD_SHA"], "${{ matrix.head }}")
        self.assertEqual(classify["env"]["PR_NUMBER"], "${{ matrix.pr }}")

    def test_the_find_job_checks_nothing_out_and_the_classify_job_keeps_the_base_only_checkout(self):
        wf = yaml.safe_load(self.text)
        self.assertFalse(any("uses" in step for step in wf["jobs"]["find"]["steps"]))
        self.assertEqual(self.text.count("actions/checkout@"), 1)
        self.assertNotIn("workflow_run.head_branch", self.text)
        self.assertNotRegex(self.text, r"ref: \$\{\{ github\.event\.workflow_run")

    def find(self, event, **env):
        wf = yaml.safe_load(self.text)
        script = wf["jobs"]["find"]["steps"][0]["run"]
        d = tempfile.mkdtemp()
        try:
            fake = Path(d) / "gh"
            fake.write_text(FAKE_GH_SH)
            fake.chmod(0o755)
            out = Path(d) / "out"
            full = {**os.environ, "PATH": d + ":" + os.environ["PATH"], "GITHUB_REPOSITORY": "o/r",
                    "GITHUB_OUTPUT": str(out), "EVENT_NAME": event, "PR_NUMBER": "", "BASE_SHA": "", "HEAD_SHA": "",
                    "RUN_NAME": "", "RUN_CREATED": "", "FIXTURE": json.dumps(FIND_FIXTURE), **env}
            done = subprocess.run(["bash", "-e", "-c", script], env=full, capture_output=True, text=True, timeout=60)
            self.assertEqual(done.returncode, 0, done.stderr)
            line = [l for l in out.read_text().splitlines() if l.startswith("targets=")][0]
            return json.loads(line[len("targets="):])
        finally:
            shutil.rmtree(d, ignore_errors=True)

    def test_a_pull_request_event_names_the_pull_request_of_the_event(self):
        got = self.find("pull_request_target", PR_NUMBER="7", BASE_SHA="b" * 40, HEAD_SHA="a" * 40)
        self.assertEqual(got, [{"pr": 7, "base": "b" * 40, "head": "a" * 40}])

    def test_a_finished_run_for_a_pull_request_head_names_that_pull_request_from_the_api(self):
        got = self.find("workflow_run", HEAD_SHA="a" * 40, RUN_NAME="CI", RUN_CREATED="2026-11-03T09:00:00Z")
        self.assertEqual(got, [{"pr": 7, "base": "b" * 40, "head": "a" * 40}])

    def test_a_security_gate_run_on_the_default_branch_sha_names_the_pull_requests_whose_gate_status_it_wrote(self):
        got = self.find("workflow_run", HEAD_SHA="d" * 40, RUN_NAME="security-gate", RUN_CREATED="2026-11-03T09:00:00Z")
        self.assertEqual(got, [{"pr": 8, "base": "b" * 40, "head": "c" * 40}])

    def test_a_ci_run_with_no_open_pull_request_for_its_head_names_nothing_and_does_not_scan(self):
        self.assertEqual(self.find("workflow_run", HEAD_SHA="d" * 40, RUN_NAME="CI", RUN_CREATED="2026-11-03T09:00:00Z"), [])

    def test_a_pull_request_whose_head_moved_since_the_run_is_not_named(self):
        self.assertEqual(self.find("workflow_run", HEAD_SHA="e" * 40, RUN_NAME="CI", RUN_CREATED="2026-11-03T09:00:00Z"), [])

    def test_a_receipt_taken_while_ci_is_pending_fails_verify_once_ci_succeeds_and_a_fresh_one_passes(self):
        mt, d, base, head = head_commit({"docs/note.md": "x\n"})
        try:
            pending = fake_gh(base, head, "in_progress", None)
            done = fake_gh(base, head, "completed", "success")
            early = mt.classify(d, base, head, ci=mt.fetch_ci(pending, "o/r", head, None, pr=7))
            self.assertEqual(early["tier"], 1)
            ok, why = mt.verify(early, d, ci=mt.fetch_ci(done, "o/r", head, None, pr=7))
            self.assertFalse(ok, "a pending-CI receipt must not verify against finished CI")
            self.assertIn("recomputed ci differs from the receipt", why)
            fresh = mt.classify(d, base, head, ci=mt.fetch_ci(done, "o/r", head, None, pr=7))
            self.assertEqual(fresh["tier"], 0, fresh["tier_reasons"])
            self.assertEqual(mt.verify(fresh, d, ci=mt.fetch_ci(done, "o/r", head, None, pr=7)), (True, []))
        finally:
            shutil.rmtree(d, ignore_errors=True)


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
