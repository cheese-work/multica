"""CHE-4639: security-gate-queue starts only for a merge-queue run of security-gate.

A skipped run still counts as a created run (GitHub run budget), so the trigger filter is the fix: the job-level `if`
alone left ~1,249 skipped runs a day, one per non-queue security-gate run.
"""

from __future__ import annotations

import unittest
from pathlib import Path

import yaml

WF = Path(__file__).resolve().parents[1] / ".github/workflows"


def load(name: str) -> dict:
    doc = yaml.safe_load((WF / name).read_text())
    doc["on"] = doc.pop(True, doc.get("on"))  # PyYAML reads the bare key `on` as True
    return doc


class SecurityGateQueueTrigger(unittest.TestCase):
    def setUp(self):
        self.doc = load("security-gate-queue.yml")

    def test_only_a_run_on_a_merge_queue_branch_starts_the_workflow(self):
        # `branches` on workflow_run matches the triggering run's head branch; a merge_group run's head branch is the queue ref.
        self.assertEqual(self.doc["on"]["workflow_run"]["branches"], ["gh-readonly-queue/**"])

    def test_still_waits_for_security_gate_completion(self):
        wr = self.doc["on"]["workflow_run"]
        self.assertEqual(wr["workflows"], ["security-gate"])
        self.assertEqual(wr["types"], ["completed"])

    def test_the_job_guard_stays_as_the_trust_check(self):
        cond = self.doc["jobs"]["security-gate-queue"]["if"]
        self.assertIn("github.event.workflow_run.event == 'merge_group'", cond)
        self.assertIn("head_repository.full_name == github.repository", cond)


if __name__ == "__main__":
    unittest.main()
