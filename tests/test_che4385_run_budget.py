"""CHE-4385: a PR push or comment must create few workflow runs (GitHub disabled Actions at ~2,500 runs/day).

A skipped run still counts as a created run, so the trigger set is the budget: these tests pin it.
"""

from __future__ import annotations

import os
import re
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
            types = on.get("pull_request_target", {}).get("types")
            # pr-guard-rules-edit (CHE-5208) starts only on a title/body/base edit or a removed reviewer, never on a push.
            self.assertTrue("paths" in on.get("pull_request", {}) or types in (["dequeued"], ["edited", "review_request_removed"]), name)
        self.assertIn("pr-guards.yml", starters)
        self.assertEqual(starters & set(MERGED_AWAY), set())

    def test_secret_scan_runs_on_pr_inside_ci_not_as_its_own_run(self):
        self.assertNotIn("pull_request", triggers(load("secret-scan.yml")))
        self.assertIn("gitleaks", load("ci.yml")["jobs"])

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
        # Only the read-only workflow token (the changed-file list, CHE-4016); no other secret.
        self.assertEqual(re.findall(r"secrets\.\w+", text), ["secrets.GITHUB_TOKEN"])


class NoCommentOrCascadeRuns(unittest.TestCase):
    def test_only_security_gate_starts_on_a_comment_and_it_includes_created(self):
        # `created` is restored (Cheese, CHE-4385): a new FAIL verdict must flip the status without waiting for a sweep.
        # The job-level `if` skips every comment that is not a trusted verdict, but that run still counts (about 284/day).
        for p in WF.glob("*.yml"):
            on = load(p.name)["on"]
            if "issue_comment" in triggers(load(p.name)):
                self.assertEqual(p.name, "security-gate.yml")
                self.assertEqual(sorted(on["issue_comment"]["types"]), ["created", "deleted", "edited"])

    def test_the_queue_workflow_starts_only_for_a_merge_queue_branch(self):
        # `workflow_run` runs only from main; the branch filter stops a non-queue security-gate run starting a skipped queue run.
        run = load("security-gate-queue.yml")["on"]["workflow_run"]
        self.assertEqual(run["workflows"], ["security-gate"])
        self.assertEqual(run["branches"], ["gh-readonly-queue/**"])

    def test_mechanical_tier_is_called_never_a_run_of_its_own(self):
        self.assertEqual(triggers(load("mechanical-tier.yml")), {"workflow_call"})

    def test_a_verdict_dispatch_refreshes_the_receipt_inside_the_same_run(self):
        jobs = load("security-gate.yml")["jobs"]
        self.assertEqual(jobs["mechanical-tier"]["uses"], "./.github/workflows/mechanical-tier.yml")
        self.assertIn("workflow_dispatch", jobs["mechanical-tier"]["if"])
        self.assertEqual(jobs["mechanical-tier"]["needs"], "security-gate")

    def test_push_to_main_workflows_do_not_start_each_other(self):
        # The only workflow_run chain is security-gate -> security-gate-queue (CHE-4251); cd-deploy runs on a schedule (CHE-5182).
        # It fires once per security-gate run, now only a queue entry, a verdict dispatch or a verdict edit or
        # delete: no PR event and no created comment starts one. main-build calls the other two in its own run.
        chains = {}
        for p in WF.glob("*.yml"):
            on = load(p.name)["on"]
            if isinstance(on, dict) and "workflow_run" in on:
                chains[p.name] = on["workflow_run"]["workflows"]
        self.assertEqual(
            chains,
            {
                "security-gate-queue.yml": ["security-gate"],
                "pr-main-sync.yml": ["Main Build"],  # CHE-4963: one run per main push, as its old Main Build job was
            },
        )
        # No push-to-main workflow listens to another one's push through `workflow_run` on CI or the gates.
        self.assertNotIn("CI", [w for ws in chains.values() for w in ws])

    def test_main_build_runs_cache_maintenance_after_the_warm_jobs(self):
        self.assertEqual(triggers(load("cache-maintenance.yml")), {"schedule", "workflow_dispatch", "workflow_call"})
        job = load("main-build.yml")["jobs"]["cache-maintenance"]
        self.assertEqual(job["uses"], "./.github/workflows/cache-maintenance.yml")
        self.assertEqual(set(job["needs"]), {"push-go-cache-warm", "push-turbo-cache-warm", "daemon-artifact"})
        self.assertEqual(job["if"].strip(), "${{ always() }}")  # it ran after every completed Main Build before
        self.assertEqual(job["permissions"], {"contents": "read", "actions": "write"})

    def test_pr_main_sync_follows_a_main_build_push_run_and_main_build_does_not_call_it(self):
        # CHE-4963: it holds the App key, so the repo guard (CHE-4251) requires a schedule or workflow_run definition.
        doc = load("pr-main-sync.yml")
        self.assertEqual(triggers(doc), {"workflow_run", "workflow_dispatch"})
        run = doc["on"]["workflow_run"]
        self.assertEqual((run["workflows"], run["types"], run["branches"]), (["Main Build"], ["completed"], ["main"]))
        gate = doc["jobs"]["sync"]["if"]
        self.assertIn("github.event.workflow_run.event == 'push'", gate)
        # A failed or cancelled Main Build still has a main push to sync after, as the old job had no needs (CHE-4963).
        self.assertNotIn("conclusion", gate)
        self.assertNotIn("pr-main-sync", load("main-build.yml")["jobs"])

    def test_called_workflows_use_a_concurrency_group_other_than_the_callers(self):
        caller = load("main-build.yml")["concurrency"]["group"]
        for name in ("cache-maintenance.yml",):
            self.assertNotEqual(load(name)["concurrency"]["group"], caller)

    def test_pr_main_sync_requests_only_permissions_the_app_installation_grants(self):
        # Run 43137581396 failed with 422 minting contents:write + pull-requests:write: the installation grants
        # pull-requests read only (cd-deploy mints it). The script reads PRs and merges through update-branch (CHE-4963).
        steps = load("pr-main-sync.yml")["jobs"]["sync"]["steps"]
        mint = next(s["with"] for s in steps if "create-github-app-token" in s.get("uses", ""))
        perms = {k: v for k, v in mint.items() if k.startswith("permission-")}
        self.assertEqual(perms, {"permission-contents": "write", "permission-pull-requests": "read"})

    def test_pr_main_sync_keeps_its_gate_runner_and_dry_run_switch(self):
        text = (WF / "pr-main-sync.yml").read_text()
        job = load("pr-main-sync.yml")["jobs"]["sync"]
        self.assertIn("PR_MAIN_SYNC_ENABLED == 'true'", job["if"])
        self.assertEqual(job["runs-on"], ["self-hosted", "linux", "x64", "cheese-x99"])
        self.assertIn("PR_MAIN_SYNC_ENABLED != 'true'", text)
        self.assertNotRegex(text, r"ref: \$\{\{ github\.event\.pull_request\.head")


if __name__ == "__main__":
    unittest.main()
