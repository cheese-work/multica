"""CHE-4250: workflow_run cascades must not burn the repo's shared GITHUB_TOKEN budget and stall CD."""

from __future__ import annotations

import unittest
from pathlib import Path

import yaml

WF = Path(__file__).resolve().parents[1] / ".github/workflows"


def load(name: str) -> dict:
    doc = yaml.safe_load((WF / name).read_text())
    doc["on"] = doc.pop(True, doc.get("on"))  # PyYAML reads the bare key `on` as True
    return doc


class MechanicalTierTriggers(unittest.TestCase):
    def setUp(self):
        self.doc = load("mechanical-tier.yml")

    def test_only_a_finished_ci_run_refreshes_a_receipt(self):
        # security-gate runs on every PR comment; its completions were ~40% of mechanical-tier runs (CHE-4250).
        self.assertEqual(self.doc["on"]["workflow_run"]["workflows"], ["CI"])

    def test_merge_queue_ci_runs_do_not_refresh_a_receipt(self):
        # A merge_group CI run has no open pull request for its head SHA; every one was an empty run.
        self.assertEqual(self.doc["on"]["workflow_run"]["branches-ignore"], ["gh-readonly-queue/**"])

    def test_an_upstream_run_that_produced_no_result_makes_no_api_call(self):
        cond = self.doc["jobs"]["find"]["if"]
        self.assertIn("github.event.workflow_run.conclusion", cond)
        for noisy in ("cancelled", "skipped"):
            self.assertIn(noisy, cond)

    def test_classify_concurrency_is_keyed_by_head_and_never_cancels(self):
        # CHE-3990: cancel-in-progress turned a green check red.
        conc = self.doc["jobs"]["mechanical-tier"]["concurrency"]
        self.assertFalse(conc["cancel-in-progress"])
        self.assertIn("matrix.head", conc["group"])


class CacheMaintenanceTriggers(unittest.TestCase):
    def test_runs_on_schedule_and_by_hand_only(self):
        # The 6-hourly schedule bounds the cache budget; a run per completed CI run only burned API calls.
        self.assertEqual(set(load("cache-maintenance.yml")["on"]), {"schedule", "workflow_dispatch"})


class SecurityGateComments(unittest.TestCase):
    def test_a_plain_comment_schedules_no_job(self):
        cond = load("security-gate.yml")["jobs"]["security-gate"]["if"]
        self.assertIn("contains(github.event.comment.body, 'Security-Gate-Verdict:')", cond)


if __name__ == "__main__":
    unittest.main()
