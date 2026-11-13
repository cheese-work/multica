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


# `gh api <path> [--paginate] --jq <filter>`: serve canned JSON for the path and apply the filter with jq, as gh does.
FAKE_GH_SH = r"""#!/bin/bash
path="$2"; filter=""
while [ $# -gt 0 ]; do [ "$1" = --jq ] && filter="$2"; shift; done
case "$path" in
  repos/o/r/commits/*/pulls) sha="${path#repos/o/r/commits/}"; q=".pulls_for[\"${sha%/pulls}\"] // []" ;;
  repos/o/r/commits/*/statuses*) sha="${path#repos/o/r/commits/}"; q=".statuses[\"${sha%%/*}\"] // []" ;;
  repos/o/r/pulls/*) q=".pr[\"${path#repos/o/r/pulls/}\"]" ;;
  "repos/o/r/pulls?state=open&per_page=100") q=".open" ;;
  *) echo "unexpected path $path" >&2; exit 1 ;;
esac
jq -rc "$q" "$FIXTURE_FILE" | jq -rc "$filter"
# A listed path answers with its data and then fails, like a later page or a dropped connection.
if jq -e --arg p "$path" '(.fail // []) | index($p)' "$FIXTURE_FILE" >/dev/null; then exit 42; fi
"""
A, B, C = "a" * 40, "b" * 40, "c" * 40
OLD, FRESH = "2026-11-03T08:00:00Z", "2026-11-03T09:00:05Z"


def sha(n):
    return f"{n:040x}"


def find_fixture(open_prs=(), fresh=()):
    """Open PRs 7 (head A, gate status OLD), 8 (head C, FRESH), plus `open_prs` numbers; `fresh` numbers get FRESH."""
    prs = {7: A, 8: C, **{n: sha(n) for n in open_prs}}
    gate = lambda n: [{"context": "security-gate", "updated_at": FRESH if n in fresh or n == 8 else OLD},
                      {"context": "other", "updated_at": FRESH}]
    return {
        "pulls_for": {A: [{"number": 7, "state": "open", "head": {"sha": A}}],
                      sha(9): [{"number": 9, "state": "open", "head": {"sha": "f" * 40}}]},
        "pr": {str(n): {"number": n, "base": {"sha": B}, "head": {"sha": h}} for n, h in prs.items()},
        "open": [{"number": n, "head": {"sha": h}} for n, h in prs.items()],
        "statuses": {h: gate(n) for n, h in prs.items()},
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

    def test_is_called_by_workflows_that_run_the_default_branch_definition(self):
        # CHE-4385: no event of its own. pr-guards (pull_request_target) and security-gate (dispatch) call it.
        self.assertRegex(self.text, r"(?m)^on:\n  workflow_call:\n\npermissions:")
        self.assertNotRegex(self.text, r"(?m)^  (pull_request|pull_request_target):")

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

    def test_no_workflow_run_cascade_starts_the_classifier(self):
        # CHE-4385: a finished CI run no longer starts a run of its own (the cascade doubled the CI run count). The
        # receipt after CI and a verdict is taken by the security-gate.yml dispatch run, which calls this workflow.
        self.assertNotRegex(self.text, r"(?m)^  workflow_run:")
        self.assertNotRegex(self.text, r"(?m)^  status:")
        self.assertNotRegex(self.text, r"(?m)^  issue_comment:")

    def test_the_classifier_runs_once_per_pull_request_the_find_job_names(self):
        wf = yaml.safe_load(self.text)
        classify = wf["jobs"]["mechanical-tier"]
        self.assertEqual(classify["needs"], "find")
        self.assertEqual(classify["strategy"]["matrix"]["include"], "${{ fromJSON(needs.find.outputs.targets) }}")
        self.assertFalse(classify["strategy"]["fail-fast"])
        # CHE-3990: no concurrency at all. GitHub keeps one running and one pending job per group, so a third event on
        # a head cancels the pending job even with cancel-in-progress: false, and a cancelled run is the latest red check.
        self.assertNotIn("concurrency", classify)
        self.assertNotIn("concurrency", wf)
        self.assertEqual(classify["env"]["HEAD_SHA"], "${{ matrix.head }}")
        self.assertEqual(classify["env"]["PR_NUMBER"], "${{ matrix.pr }}")

    def test_the_find_job_checks_nothing_out_and_the_classify_job_keeps_the_base_only_checkout(self):
        wf = yaml.safe_load(self.text)
        self.assertFalse(any("uses" in step for step in wf["jobs"]["find"]["steps"]))
        self.assertEqual(self.text.count("actions/checkout@"), 1)
        self.assertNotIn("workflow_run.head_branch", self.text)
        self.assertNotRegex(self.text, r"ref: \$\{\{ github\.event\.workflow_run")

    def run_find(self, event, fixture, **env):
        wf = yaml.safe_load(self.text)
        script = wf["jobs"]["find"]["steps"][0]["run"]
        d = tempfile.mkdtemp()
        try:
            fake = Path(d) / "gh"
            fake.write_text(FAKE_GH_SH)
            fake.chmod(0o755)
            (Path(d) / "fixture.json").write_text(json.dumps(fixture))
            out = Path(d) / "out"
            full = {**os.environ, "PATH": d + ":" + os.environ["PATH"], "GITHUB_REPOSITORY": "o/r",
                    "GITHUB_OUTPUT": str(out), "EVENT_NAME": event, "PR_NUMBER": "", "BASE_SHA": "", "HEAD_SHA": "",
                    "INPUT_PR": "", "FIXTURE_FILE": str(Path(d) / "fixture.json"), **env}
            done = subprocess.run(["bash", "-e", "-c", script], env=full, capture_output=True, text=True, timeout=120)
            targets = None
            if out.exists():
                line = [l for l in out.read_text().splitlines() if l.startswith("targets=")]
                targets = json.loads(line[0][len("targets="):]) if line else None
            return done, targets
        finally:
            shutil.rmtree(d, ignore_errors=True)

    def find(self, event, fixture=None, **env):
        done, targets = self.run_find(event, fixture or find_fixture(), **env)
        self.assertEqual(done.returncode, 0, done.stderr)
        return targets

    def test_a_pull_request_event_names_the_pull_request_of_the_event(self):
        got = self.find("pull_request_target", PR_NUMBER="7", BASE_SHA=B, HEAD_SHA=A)
        self.assertEqual(got, [{"pr": 7, "base": B, "head": A}])

    def test_a_dispatch_for_a_pull_request_names_that_pull_request_from_the_api(self):
        self.assertEqual(self.find("workflow_dispatch", INPUT_PR="7"), [{"pr": 7, "base": B, "head": A}])

    def shared_head_fixture(self):
        fx = find_fixture()
        sibling = {"number": 9, "state": "open", "head": {"sha": C}}
        fx["pulls_for"][C] = [{"number": 8, "state": "open", "head": {"sha": C}}, sibling,
                              {"number": 10, "state": "closed", "head": {"sha": C}}]
        fx["pr"]["9"] = {"number": 9, "base": {"sha": B}, "head": {"sha": C}}
        return fx

    def test_a_dispatch_for_a_pull_request_names_it_and_every_open_pull_request_sharing_its_head(self):
        # CHE-4250: a verdict changes the shared head's status, so a sibling's receipt goes stale with it.
        got = self.find("workflow_dispatch", self.shared_head_fixture(), INPUT_PR="8")
        self.assertEqual(sorted(t["pr"] for t in got), [8, 9])

    def test_a_dispatch_reads_one_pull_request_and_scans_no_list(self):
        fx = self.shared_head_fixture()
        fx["fail"] = ["repos/o/r/pulls?state=open&per_page=100", f"repos/o/r/commits/{C}/statuses?per_page=100"]
        done, targets = self.run_find("workflow_dispatch", fx, INPUT_PR="8")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(sorted(t["pr"] for t in targets), [8, 9])

    def test_a_dispatch_input_that_is_not_a_number_is_refused(self):
        for bad in ("", "8; id", "8 9", "$(id)", "x8"):
            done, targets = self.run_find("workflow_dispatch", self.shared_head_fixture(), INPUT_PR=bad)
            self.assertNotEqual(done.returncode, 0, bad)
            self.assertIsNone(targets, bad)

    def test_a_failed_pull_request_read_fails_the_job_instead_of_dropping_it(self):
        fx = find_fixture()
        fx["fail"] = ["repos/o/r/pulls/7"]
        done, targets = self.run_find("workflow_dispatch", fx, INPUT_PR="7")
        self.assertNotEqual(done.returncode, 0, done.stdout)
        self.assertIsNone(targets)

    def test_more_pull_requests_than_a_matrix_can_hold_fails_the_job_instead_of_dropping_some(self):
        fx = find_fixture()
        fx["pulls_for"][A] = [{"number": n, "state": "open", "head": {"sha": A}} for n in range(100, 400)]
        done, _ = self.run_find("workflow_dispatch", fx, INPUT_PR="7")
        self.assertNotEqual(done.returncode, 0)
        self.assertIn("256", done.stdout + done.stderr)

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

    def test_build_install_release_and_daemon_execution_paths_are_tier_2(self):
        paths = ("Makefile", "scripts/install.sh", "scripts/install.ps1", "scripts/install-cli-from-ref.sh",
                 "scripts/build-daemon-binaries.sh", "scripts/daemon-artifact-version.sh",
                 "apps/desktop/electron-builder.yml", "server/internal/daemon/daemon.go")
        self.assertEqual(protected({p: "x\n" for p in paths}), set(paths))

    def test_a_change_of_only_handler_tests_and_docs_is_not_flagged(self):
        paths = ("server/internal/handler/issue_test.go", "docs/note.md", "tests/test_other.py",
                 "scripts/install.test.sh", "scripts/daemon-artifact-version.test.mjs", "server/internal/daemon/daemon_test.go")
        self.assertEqual(protected({p: "x\n" for p in paths}), set())

    def test_a_decision_action_governance_change_is_tier_2_with_no_uncovered_path(self):
        path = "server/internal/decisionaction/config.go"
        mt, d, base, head = head_commit({path: "x\n"})
        try:
            receipt = mt.classify(d, base, head)
            self.assertIsNone(receipt["error"])
            self.assertEqual({m["path"] for m in receipt["capability_matches"]}, {path})
            self.assertEqual(receipt["tier"], 2, receipt["tier_reasons"])
            self.assertEqual(receipt["coverage"]["uncovered"], [])
        finally:
            shutil.rmtree(d, ignore_errors=True)

    def test_a_change_of_only_decision_action_tests_is_not_flagged(self):
        self.assertEqual(protected({"server/internal/decisionaction/config_test.go": "x\n"}), set())

    def test_registry_is_valid_and_covered_scopes_are_exact_paths(self):
        reg = json.loads((TIER_DIR / "registry.json").read_text())
        self.assertEqual(reg["schema"], 1)
        scopes = reg["covered_scopes"]
        self.assertEqual({s["analysis"] for s in scopes}, {"path-registry"} if scopes else set())
        self.assertEqual(len({s["id"] for s in scopes}), len(scopes))
        for s in scopes:
            self.assertFalse(any(c in s["glob"] for c in "*?[{"), s)


if __name__ == "__main__":
    unittest.main()
