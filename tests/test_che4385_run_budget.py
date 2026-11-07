"""CHE-4385: a PR push or comment must create few workflow runs (GitHub disabled Actions at ~2,500 runs/day).

A skipped run still counts as a created run, so the trigger set is the budget: these tests pin it.
"""

from __future__ import annotations

import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml

WF = Path(__file__).resolve().parents[1] / ".github/workflows"
PR_EVENTS = {"pull_request", "pull_request_target"}
MERGED_AWAY = ["pr-close-target-check.yml", "pr-changelog-check.yml", "fork-sync-guard.yml"]


def load(name: str) -> dict:
    doc = yaml.safe_load((WF / name).read_text())
    doc["on"] = doc.pop(True, doc.get("on"))  # PyYAML reads the bare key `on` as True
    return doc


def triggers(doc: dict) -> set[str]:
    on = doc["on"]
    return {on} if isinstance(on, str) else set(on)


class OneWorkflowPerPrPush(unittest.TestCase):
    def test_the_three_guard_workflows_are_one_pr_guards_workflow(self):
        for name in MERGED_AWAY:
            self.assertFalse((WF / name).exists(), name)
        jobs = load("pr-guards.yml")["jobs"]
        for job in ("close-target", "changelog-line", "fork-sync-guard", "security-gate", "mechanical-tier"):
            self.assertIn(job, jobs)

    def test_pr_guards_is_the_only_workflow_besides_ci_that_a_pr_push_starts(self):
        starters = {p.name for p in WF.glob("*.yml") if PR_EVENTS & triggers(load(p.name))}
        # CI (needs the PR merge commit) stays. Others start only on a path-filtered `pull_request` or a rare `dequeued`.
        for name in starters - {"ci.yml", "pr-guards.yml"}:
            on = load(name)["on"]
            self.assertTrue("paths" in on.get("pull_request", {}) or on.get("pull_request_target", {}).get("types") == ["dequeued"], name)
        self.assertIn("pr-guards.yml", starters)
        self.assertEqual(starters & set(MERGED_AWAY), set())

    def test_pr_guards_runs_the_pr_event_once_and_cancels_a_superseded_run(self):
        doc = load("pr-guards.yml")
        self.assertEqual(set(doc["on"]), {"pull_request_target"})
        self.assertEqual(set(doc["on"]["pull_request_target"]["types"]),
                         {"opened", "synchronize", "reopened", "ready_for_review"})
        # A body or title edit does not re-run the guards (about 37 sample runs); the next push does.
        self.assertNotIn("edited", doc["on"]["pull_request_target"]["types"])
        self.assertTrue(doc["concurrency"]["cancel-in-progress"])
        self.assertIn("github.event.pull_request.number", doc["concurrency"]["group"])

    def test_pr_guards_checks_out_trusted_code_only(self):
        text = (WF / "pr-guards.yml").read_text()
        self.assertNotRegex(text, r"ref: \$\{\{ github\.event\.pull_request\.head")
        self.assertNotIn("secrets.", text)


class NewFailVerdictRevokesAGreenCommit(unittest.TestCase):
    """A comment no longer starts security-gate, so a sweep re-reads the verdicts of open green PRs (Cheese, CHE-4385)."""

    def test_sweep_runs_on_a_schedule_and_by_hand_only(self):
        self.assertEqual(triggers(load("security-gate-sweep.yml")), {"schedule", "workflow_dispatch"})
        crons = [c["cron"] for c in load("security-gate-sweep.yml")["on"]["schedule"]]
        self.assertEqual(crons, ["*/30 * * * *"])  # 48 runs a day

    def test_sweep_overlap_queues_instead_of_cancelling_a_revoke(self):
        self.assertFalse(load("security-gate-sweep.yml")["concurrency"]["cancel-in-progress"])

    def test_sweep_posts_through_the_gate_script_from_the_default_branch(self):
        text = (WF / "security-gate-sweep.yml").read_text()
        self.assertIn("scripts/security-gate/security_gate.py", text)
        self.assertIn("--post", text)
        self.assertIn("ref: ${{ github.event.repository.default_branch }}", text)
        self.assertNotRegex(text, r"ref: \$\{\{ github\.event\.pull_request\.head")
        self.assertNotIn("secrets.", text)

    def test_sweep_skips_the_scan_when_no_open_pr_needs_one(self):
        jobs = load("security-gate-sweep.yml")["jobs"]
        self.assertEqual(jobs["list"]["outputs"]["green"], "${{ steps.list.outputs.green }}")
        self.assertEqual(jobs["list"]["outputs"]["queued"], "${{ steps.list.outputs.queued }}")
        self.assertIn("needs.list.outputs.green != '[]'", jobs["sweep"]["if"])
        self.assertIn("needs.list.outputs.queued != '[]'", jobs["revoke"]["if"])

    def test_a_failed_pr_listing_fails_the_job_instead_of_listing_nothing(self):
        # Terra P1: process substitution hid the exit status of `gh api`, so a failed list looked like "no open PR".
        step = load("security-gate-sweep.yml")["jobs"]["list"]["steps"][0]["run"]
        self.assertIn("set -euo pipefail", step)
        self.assertNotIn("< <(", step)
        self.assertRegex(step, r'pulls="\$\(gh api --paginate')
        self.assertNotRegex(step, r"\[ .* \] && ")  # a false test as the loop's last command would trip errexit

    def test_a_pr_whose_revocation_failed_is_retried_while_a_queue_branch_exists(self):
        # Terra P1: the status is written non-success before the queue heads are flipped; a failed flip was never retried.
        jobs = load("security-gate-sweep.yml")["jobs"]
        run = "\n".join(str(st.get("run", "")) for st in jobs["revoke"]["steps"])
        self.assertIn("--revoke-only", run)
        self.assertNotIn("--semgrep", run)
        self.assertEqual(jobs["revoke"]["strategy"]["matrix"]["pr"], "${{ fromJSON(needs.list.outputs.queued) }}")
        self.assertIn("gh-readonly-queue", (WF / "security-gate-sweep.yml").read_text())

    def test_a_sweep_job_never_runs_alongside_the_gate_for_the_same_pr(self):
        # Terra P1: a sweep could read a PASS and post success after the gate had posted a revocation.
        gate = load("security-gate.yml")["jobs"]["security-gate"]["concurrency"]["group"]
        self.assertTrue(gate.startswith("security-gate-${{ github.event.pull_request.number"))
        for name in ("sweep", "revoke"):
            job = load("security-gate-sweep.yml")["jobs"][name]
            self.assertEqual(job["concurrency"]["group"], "security-gate-${{ matrix.pr }}")
            # false: a sweep queues behind a running gate; the gate (cancel-in-progress true) cancels a queued or running sweep
            self.assertFalse(job["concurrency"]["cancel-in-progress"])
            self.assertFalse(job["strategy"]["fail-fast"])


class SweepStepsBehavior(unittest.TestCase):
    """Runs the sweep's real `run:` scripts under bash with a stubbed `gh` (and `python3`), and asserts what they do."""

    FAKE_GH = """#!/bin/bash
case "$*" in
  *"/pulls?state=open"*) [ "$FAKE_FAIL" = pulls ] && exit 1; printf '%s' "$FAKE_PULLS" ;;
  *"git/matching-refs"*) [ "$FAKE_FAIL" = refs ] && exit 1; printf '%s' "$FAKE_REFS" ;;
  *"/status"*) [ "$FAKE_FAIL" = status ] && exit 1; printf '%s' "$FAKE_STATE" ;;
  *) exit 1 ;;
esac
"""
    RECORD_PY = '#!/bin/bash\nprintf "%s\\n" "$*" >> "$FAKE_ARGV"\n'

    def step(self, job: str, needle: str) -> str:
        steps = load("security-gate-sweep.yml")["jobs"].get(job, {}).get("steps", [])
        runs = [str(st["run"]) for st in steps if needle in str(st.get("run", ""))]
        self.assertTrue(runs, f"no `{needle}` step in job {job}")
        return runs[0]

    def run_step(self, run: str, **env) -> tuple[subprocess.CompletedProcess, str, str]:
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            for name, body in (("gh", self.FAKE_GH), ("python3", self.RECORD_PY)):
                (tmp / name).write_text(body)
                (tmp / name).chmod(tmp.joinpath(name).stat().st_mode | stat.S_IXUSR)
            out, argv = tmp / "out", tmp / "argv"
            out.touch()
            argv.touch()
            e = {"PATH": f"{tmp}:{os.environ['PATH']}", "GITHUB_OUTPUT": str(out), "FAKE_ARGV": str(argv),
                 "GITHUB_REPOSITORY": "o/r", "DEFAULT_BRANCH": "main", "GITHUB_SERVER_URL": "https://x",
                 "GITHUB_RUN_ID": "1", "PR_NUMBER": "7", "FAKE_PULLS": "", "FAKE_REFS": "", "FAKE_STATE": "",
                 "FAKE_FAIL": "", **env}
            proc = subprocess.run(["bash", "-e", "-c", run], env=e, cwd=tmp, capture_output=True, text=True)
            return proc, out.read_text(), argv.read_text()

    def listing(self, **env):
        return self.run_step(self.step("list", "gh api"), **env)

    def test_a_failed_pr_listing_fails_the_job_and_publishes_nothing(self):
        for failing in ("pulls", "refs", "status"):
            proc, out, _ = self.listing(FAKE_FAIL=failing, FAKE_PULLS="7 " + "a" * 40, FAKE_REFS="refs/heads/gh-readonly-queue/main/pr-1-x",
                                        FAKE_STATE="success")
            self.assertNotEqual(proc.returncode, 0, failing)
            self.assertNotIn("green=", out, failing)

    def test_a_green_open_pr_is_listed_for_the_sweep(self):
        proc, out, _ = self.listing(FAKE_PULLS="7 " + "a" * 40, FAKE_STATE="success")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("green=[7]", out)
        self.assertIn("queued=[]", out)

    def test_a_non_success_pr_is_listed_for_a_revoke_retry_only_while_a_queue_branch_exists(self):
        pulls = "7 " + "a" * 40
        proc, out, _ = self.listing(FAKE_PULLS=pulls, FAKE_STATE="failure", FAKE_REFS="refs/heads/gh-readonly-queue/main/pr-1-x")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("queued=[7]", out)
        self.assertIn("green=[]", out)
        proc, out, _ = self.listing(FAKE_PULLS=pulls, FAKE_STATE="failure")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("queued=[]", out)

    def test_the_revoke_job_calls_the_script_for_its_pr_with_revoke_only_and_no_scan(self):
        proc, _, argv = self.run_step(self.step("revoke", "--revoke-only"))
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("--revoke-only", argv)
        self.assertRegex(argv, r"--pr 7\b")
        self.assertNotIn("--semgrep", argv)


class NoCommentOrCascadeRuns(unittest.TestCase):
    def test_no_workflow_starts_on_a_new_comment(self):
        # security-gate keeps `edited`/`deleted` only, so a PASS is revoked without a dispatch; `created` is the cost.
        for p in WF.glob("*.yml"):
            on = load(p.name)["on"]
            if "issue_comment" in triggers(load(p.name)):
                self.assertEqual(p.name, "security-gate.yml")
                self.assertEqual(sorted(on["issue_comment"]["types"]), ["deleted", "edited"])

    def test_security_gate_runs_for_the_queue_a_dispatch_a_comment_edit_and_pr_guards_only(self):
        self.assertEqual(triggers(load("security-gate.yml")), {"workflow_call", "merge_group", "workflow_dispatch", "issue_comment"})
        self.assertEqual(list(load("security-gate.yml")["on"]["workflow_dispatch"]["inputs"]), ["pr"])

    def test_mechanical_tier_is_called_never_a_run_of_its_own(self):
        self.assertEqual(triggers(load("mechanical-tier.yml")), {"workflow_call"})

    def test_a_verdict_dispatch_refreshes_the_receipt_inside_the_same_run(self):
        jobs = load("security-gate.yml")["jobs"]
        self.assertEqual(jobs["mechanical-tier"]["uses"], "./.github/workflows/mechanical-tier.yml")
        self.assertIn("workflow_dispatch", jobs["mechanical-tier"]["if"])
        self.assertEqual(jobs["mechanical-tier"]["needs"], "security-gate")

    def test_push_to_main_workflows_do_not_start_each_other(self):
        # The only workflow_run chain is CD qualification -> deploy; main-build calls the other two in its own run.
        chains = {}
        for p in WF.glob("*.yml"):
            on = load(p.name)["on"]
            if isinstance(on, dict) and "workflow_run" in on:
                chains[p.name] = on["workflow_run"]["workflows"]
        self.assertEqual(chains, {"cd-deploy.yml": ["CD qualification build"]})
        # No push-to-main workflow listens to another one's push through `workflow_run` on CI or the gates.
        self.assertNotIn("CI", [w for ws in chains.values() for w in ws])

    def test_main_build_runs_cache_maintenance_after_the_warm_jobs(self):
        self.assertEqual(triggers(load("cache-maintenance.yml")), {"schedule", "workflow_dispatch", "workflow_call"})
        job = load("main-build.yml")["jobs"]["cache-maintenance"]
        self.assertEqual(job["uses"], "./.github/workflows/cache-maintenance.yml")
        self.assertEqual(set(job["needs"]), {"push-go-cache-warm", "push-turbo-cache-warm", "daemon-artifact"})
        self.assertEqual(job["if"].strip(), "${{ always() }}")  # it ran after every completed Main Build before
        self.assertEqual(job["permissions"], {"contents": "read", "actions": "write"})

    def test_main_build_runs_pr_main_sync_and_it_has_no_push_trigger(self):
        self.assertEqual(triggers(load("pr-main-sync.yml")), {"workflow_dispatch", "workflow_call"})
        job = load("main-build.yml")["jobs"]["pr-main-sync"]
        self.assertEqual(job["uses"], "./.github/workflows/pr-main-sync.yml")
        self.assertEqual(job["permissions"], {"contents": "read"})
        self.assertNotIn("needs", job)  # it did not wait for the build before either

    def test_called_workflows_use_a_concurrency_group_other_than_the_callers(self):
        caller = load("main-build.yml")["concurrency"]["group"]
        for name in ("cache-maintenance.yml", "pr-main-sync.yml"):
            self.assertNotEqual(load(name)["concurrency"]["group"], caller)

    def test_pr_main_sync_keeps_its_gate_runner_and_dry_run_switch(self):
        text = (WF / "pr-main-sync.yml").read_text()
        job = load("pr-main-sync.yml")["jobs"]["sync"]
        self.assertIn("PR_MAIN_SYNC_ENABLED == 'true'", job["if"])
        self.assertEqual(job["runs-on"], ["self-hosted", "linux", "x64", "cheese-x99"])
        self.assertIn("PR_MAIN_SYNC_ENABLED != 'true'", text)
        self.assertNotRegex(text, r"ref: \$\{\{ github\.event\.pull_request\.head")


if __name__ == "__main__":
    unittest.main()
