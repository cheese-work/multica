#!/usr/bin/env python3
"""cd-deploy.yml deploys on a 4-hour schedule, not after every qualified build (CHE-5182)."""
from pathlib import Path
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
TEXT = (ROOT / ".github/workflows/cd-deploy.yml").read_text()
WORKFLOW = yaml.safe_load(TEXT)
JOBS = WORKFLOW["jobs"]
# yaml reads the bare key `on` as True.
TRIGGERS = WORKFLOW.get("on", WORKFLOW.get(True))
FIRST = "schedule-resolve"
# 00:17, 04:17, 08:17, 12:17, 16:17, 20:17 ICT. Off the hour: GitHub drops top-of-hour schedules under load.
CRON = "17 1,5,9,13,17,21 * * *"


def needs_of(job):
    needs = job.get("needs", [])
    return [needs] if isinstance(needs, str) else list(needs)


class DeploySchedule(unittest.TestCase):
    def test_triggers_are_schedule_and_dispatch_only(self):
        self.assertEqual(TRIGGERS["schedule"], [{"cron": CRON}])
        self.assertIn("workflow_dispatch", TRIGGERS)
        self.assertNotIn("workflow_run", TRIGGERS)

    def test_no_job_reads_a_workflow_run_event(self):
        self.assertNotIn("github.event.workflow_run", TEXT.split("jobs:", 1)[1])
        self.assertNotIn("github.event.workflow_run", WORKFLOW["run-name"])

    def test_first_job_is_cheap_and_takes_no_c00_resource(self):
        job = JOBS[FIRST]
        self.assertNotIn("needs", job)
        self.assertIn("schedule", job["if"])
        self.assertEqual(job["runs-on"], ["self-hosted", "linux", "x64", "cheese-x99"])
        for key in ("environment", "concurrency"):
            self.assertNotIn(key, job)
        dumped = yaml.safe_dump(job)
        self.assertNotIn("cheese-c00-deploy", dumped)
        self.assertNotIn("cd-deploy-c00", dumped)
        self.assertNotIn("secrets.C00_COMPOSE_DIR", dumped)
        self.assertIn("sha", job["outputs"])

    def test_scheduled_chain_runs_only_when_there_is_something_new(self):
        for name in ("authorize-c00", "candidate-preflight"):
            with self.subTest(job=name):
                self.assertIn(FIRST, needs_of(JOBS[name]))
                self.assertIn(f"needs.{FIRST}.outputs.sha != ''", JOBS[name]["if"])
        self.assertIn("workflow_dispatch", JOBS["authorize-c00"]["if"])
        self.assertNotIn("workflow_dispatch", JOBS["candidate-preflight"]["if"])

    def test_scheduled_candidate_reaches_the_gate_and_the_preflight(self):
        gate = next(s for s in JOBS["authorize-c00"]["steps"] if "authorize-c00.py" in s.get("run", ""))
        pre = next(s for s in JOBS["candidate-preflight"]["steps"] if s.get("id") == "candidate")
        notify = next(s for s in JOBS["notify-approval"]["steps"] if "notify-approval.sh" in s.get("run", ""))
        for env, key in ((gate["env"], "HEAD_SHA"), (pre["env"], "SOURCE_SHA"), (notify["env"], "HEAD_SHA")):
            self.assertEqual(env[key], f"${{{{ needs.{FIRST}.outputs.sha }}}}")

    def test_deploy_path_after_resolve_is_unchanged(self):
        self.assertIn("needs.admission.result == 'success'", JOBS["deploy"]["if"])
        self.assertIn("needs.deploy.result == 'success'", JOBS["readback"]["if"])
        self.assertIn("needs.readback.result == 'failure'", JOBS["rollback"]["if"])
        for name in ("prepare-release-candidate", "deploy", "rollback"):
            self.assertEqual(JOBS[name]["concurrency"]["group"], "cd-deploy-c00")

    def test_backup_dispatch_marker_runs_the_scheduled_chain(self):
        marker = TRIGGERS["workflow_dispatch"]["inputs"]["schedule_backup"]
        self.assertFalse(marker.get("required"))
        for name in ("schedule-resolve", "candidate-preflight", "notify-approval"):
            with self.subTest(job=name):
                self.assertIn("inputs.schedule_backup", JOBS[name]["if"])
        self.assertIn("inputs.schedule_backup", JOBS["prepare-release-candidate"]["if"])
        gate = next(s for s in JOBS["authorize-c00"]["steps"] if "authorize-c00.py" in s.get("run", ""))
        self.assertIn("inputs.schedule_backup", gate["env"]["INPUT_SCHEDULE_BACKUP"])

    def test_a_dispatch_without_the_marker_keeps_the_manual_path(self):
        self.assertNotIn("inputs.schedule_backup != ''", JOBS["prepare-release-candidate"]["if"])
        self.assertIn("workflow_dispatch", JOBS["authorize-c00"]["if"])


if __name__ == "__main__":
    unittest.main()
