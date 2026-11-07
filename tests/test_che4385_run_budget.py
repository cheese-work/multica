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
                         {"opened", "edited", "synchronize", "reopened", "ready_for_review"})
        self.assertTrue(doc["concurrency"]["cancel-in-progress"])
        self.assertIn("github.event.pull_request.number", doc["concurrency"]["group"])

    def test_pr_guards_checks_out_trusted_code_only(self):
        text = (WF / "pr-guards.yml").read_text()
        self.assertNotRegex(text, r"ref: \$\{\{ github\.event\.pull_request\.head")
        self.assertNotIn("secrets.", text)


class NoCommentOrCascadeRuns(unittest.TestCase):
    def test_no_workflow_starts_on_a_comment(self):
        for p in WF.glob("*.yml"):
            self.assertNotIn("issue_comment", triggers(load(p.name)), p.name)

    def test_security_gate_runs_for_the_queue_a_dispatch_and_pr_guards_only(self):
        self.assertEqual(triggers(load("security-gate.yml")), {"workflow_call", "merge_group", "workflow_dispatch"})
        self.assertEqual(list(load("security-gate.yml")["on"]["workflow_dispatch"]["inputs"]), ["pr"])

    def test_mechanical_tier_is_called_never_a_run_of_its_own(self):
        self.assertEqual(triggers(load("mechanical-tier.yml")), {"workflow_call"})

    def test_a_verdict_dispatch_refreshes_the_receipt_inside_the_same_run(self):
        jobs = load("security-gate.yml")["jobs"]
        self.assertEqual(jobs["mechanical-tier"]["uses"], "./.github/workflows/mechanical-tier.yml")
        self.assertIn("workflow_dispatch", jobs["mechanical-tier"]["if"])
        self.assertEqual(jobs["mechanical-tier"]["needs"], "security-gate")

    def test_push_to_main_workflows_do_not_start_each_other(self):
        # Item 5: the only workflow_run chains are CD qualification -> deploy and Main Build -> cache maintenance.
        chains = {}
        for p in WF.glob("*.yml"):
            on = load(p.name)["on"]
            if isinstance(on, dict) and "workflow_run" in on:
                chains[p.name] = on["workflow_run"]["workflows"]
        self.assertEqual(chains, {"cd-deploy.yml": ["CD qualification build"], "cache-maintenance.yml": ["Main Build"]})
        # No push-to-main workflow listens to another one's push through `workflow_run` on CI or the gates.
        self.assertNotIn("CI", [w for ws in chains.values() for w in ws])

if __name__ == "__main__":
    unittest.main()
