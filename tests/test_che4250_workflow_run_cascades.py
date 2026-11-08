"""CHE-4250: workflow_run cascades must not burn the repo's shared GITHUB_TOKEN budget and stall CD."""

from __future__ import annotations

import os
import subprocess
import tempfile
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

    def test_a_finished_ci_run_refreshes_a_receipt(self):
        self.assertEqual(self.doc["on"]["workflow_run"]["workflows"], ["CI"])

    def test_merge_queue_and_default_branch_ci_runs_do_not_refresh_a_receipt(self):
        # Neither has an open pull request for its head SHA; every one was an empty run.
        self.assertEqual(self.doc["on"]["workflow_run"]["branches-ignore"], ["main", "gh-readonly-queue/**"])

    def test_a_push_does_not_classify_while_ci_is_still_pending(self):
        # A receipt taken before CI finishes cannot verify afterwards; the CI completion takes the useful one.
        self.assertEqual(self.doc["on"]["pull_request_target"]["types"], ["reopened", "ready_for_review"])

    def test_a_verdict_refresh_arrives_as_a_dispatch_naming_one_pull_request(self):
        self.assertEqual(list(self.doc["on"]["workflow_dispatch"]["inputs"]), ["pr"])
        self.assertTrue(self.doc["on"]["workflow_dispatch"]["inputs"]["pr"]["required"])

    def test_an_upstream_run_that_produced_no_result_makes_no_api_call(self):
        cond = self.doc["jobs"]["find"]["if"]
        self.assertIn("github.event.workflow_run.conclusion", cond)
        for noisy in ("cancelled", "skipped"):
            self.assertIn(noisy, cond)

    def test_classify_has_no_concurrency_group(self):
        # CHE-3990: a concurrency group (even cancel-in-progress: false) cancels the pending job on a third event,
        # and a cancelled run becomes the latest red check for the head. Every run is an idempotent receipt.
        self.assertNotIn("concurrency", self.doc["jobs"]["mechanical-tier"])
        self.assertNotIn("concurrency", self.doc)


class CacheMaintenanceTriggers(unittest.TestCase):
    def test_runs_on_schedule_by_hand_and_after_main_build_only(self):
        # A run per completed PR CI run only burned API calls; the schedule and Main Build bound the cache budget.
        on = load("cache-maintenance.yml")["on"]
        self.assertEqual(set(on), {"schedule", "workflow_dispatch", "workflow_run"})
        self.assertEqual(on["workflow_run"]["workflows"], ["Main Build"])


FAKE_GH = r"""#!/bin/bash
echo "$*" >> "$GH_LOG"
case "$*" in
  "api repos/o/r/pulls/"*) echo "$HEAD" ;;
  "api repos/o/r/actions/workflows/ci.yml/runs?head_sha="*) echo "$PENDING" ;;
  "workflow run "*) ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
"""


class SecurityGateRefresh(unittest.TestCase):
    def setUp(self):
        self.doc = load("security-gate.yml")
        self.job = self.doc["jobs"]["refresh-mechanical-tier"]

    def refresh(self, pr="8", pending="0", event="issue_comment"):
        script = self.job["steps"][0]["run"]
        with tempfile.TemporaryDirectory() as d:
            fake = Path(d) / "gh"
            fake.write_text(FAKE_GH)
            fake.chmod(0o755)
            log = Path(d) / "log"
            env = {**os.environ, "PATH": d + ":" + os.environ["PATH"], "GH_LOG": str(log), "HEAD": "c" * 40,
                   "PENDING": pending, "PR_NUMBER": pr, "GITHUB_REPOSITORY": "o/r", "DEFAULT_BRANCH": "main"}
            done = subprocess.run(["bash", "-e", "-c", script], env=env, capture_output=True, text=True, timeout=60)
            return done, (log.read_text().splitlines() if log.exists() else [])

    def test_a_finished_ci_run_gets_a_dispatch_for_the_pull_request(self):
        done, calls = self.refresh()
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(calls[-1], "workflow run mechanical-tier.yml -R o/r --ref main -f pr=8")

    def test_a_running_ci_run_gets_no_dispatch_because_its_completion_refreshes_the_receipt(self):
        done, calls = self.refresh(pending="1")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertFalse([c for c in calls if c.startswith("workflow run")])

    def test_a_pull_request_number_that_is_not_digits_dispatches_nothing(self):
        for bad in ("", "8; id", "$(id)"):
            done, calls = self.refresh(pr=bad)
            self.assertEqual(done.returncode, 0, done.stderr)
            self.assertEqual(calls, [], bad)

    def test_only_a_verdict_comment_run_dispatches(self):
        cond = self.job["if"]
        self.assertIn("github.event_name == 'issue_comment'", cond)
        self.assertIn("needs.security-gate.result", cond)

    def test_the_job_holds_the_dispatch_permission_and_touches_no_pull_request_content(self):
        self.assertEqual(self.job["needs"], "security-gate")
        self.assertEqual(self.job["permissions"], {"actions": "write", "contents": "read", "pull-requests": "read"})
        self.assertFalse(any("uses" in step for step in self.job["steps"]))

    def test_the_evaluation_job_gains_no_permission(self):
        self.assertEqual(self.doc["permissions"], {"contents": "read", "pull-requests": "read", "statuses": "write"})
        self.assertNotIn("permissions", self.doc["jobs"]["security-gate"])
        self.assertNotIn("run-name", self.doc)


class SecurityGateComments(unittest.TestCase):
    def test_a_plain_comment_schedules_no_job(self):
        cond = load("security-gate.yml")["jobs"]["security-gate"]["if"]
        self.assertIn("contains(github.event.comment.body, 'Security-Gate-Verdict:')", cond)


if __name__ == "__main__":
    unittest.main()
