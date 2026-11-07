"""CHE-4385: a PR push or comment must create few workflow runs (GitHub disabled Actions at ~2,500 runs/day).

A skipped run still counts as a created run, so the trigger set is the budget: these tests pin it.
"""

from __future__ import annotations

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

    def test_sweep_skips_the_scan_when_no_open_pr_is_green(self):
        jobs = load("security-gate-sweep.yml")["jobs"]
        self.assertEqual(jobs["list-green"]["outputs"]["prs"], "${{ steps.list.outputs.prs }}")
        self.assertIn("needs.list-green.outputs.prs != '[]'", jobs["sweep"]["if"])


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
