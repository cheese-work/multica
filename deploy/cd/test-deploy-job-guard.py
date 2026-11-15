#!/usr/bin/env python3
"""cd-deploy.yml: a manual dispatch must reach deploy, readback and rollback (CHE-5147).

candidate-preflight only runs on `schedule`, so on a dispatch it is skipped.
GitHub's implicit `if: success()` is false when ANY job up the needs chain was
skipped, so a job downstream of it without an explicit `always()` guard is
skipped too, even though its direct need succeeded. The automatic path hides
this because candidate-preflight runs there.
"""
from pathlib import Path
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/cd-deploy.yml"
SKIPPED_ON_DISPATCH = "candidate-preflight"


def load_jobs():
    return yaml.safe_load(WORKFLOW.read_text())["jobs"]


def needs_of(job):
    needs = job.get("needs", [])
    return [needs] if isinstance(needs, str) else list(needs)


def downstream_of(jobs, root):
    found, grew = set(), True
    while grew:
        grew = False
        for name, job in jobs.items():
            if name not in found and any(n == root or n in found for n in needs_of(job)):
                found.add(name)
                grew = True
    return found


class DeployJobGuard(unittest.TestCase):
    def test_skipped_on_dispatch_job_is_a_candidate_preflight_gate(self):
        jobs = load_jobs()
        self.assertIn("schedule", jobs[SKIPPED_ON_DISPATCH]["if"])
        self.assertNotIn("workflow_dispatch", jobs[SKIPPED_ON_DISPATCH]["if"])

    def test_every_job_downstream_of_a_dispatch_skipped_job_has_explicit_guard(self):
        jobs = load_jobs()
        downstream = downstream_of(jobs, SKIPPED_ON_DISPATCH)
        self.assertTrue({"deploy", "readback", "rollback"} <= downstream, downstream)
        for name in sorted(downstream):
            with self.subTest(job=name):
                self.assertIn("always()", str(jobs[name].get("if", "")))

    def test_deploy_runs_only_after_admission_succeeded(self):
        condition = str(load_jobs()["deploy"]["if"])
        self.assertIn("needs.admission.result == 'success'", condition)

    def test_authorize_c00_actor_gate_still_feeds_admission(self):
        jobs = load_jobs()
        self.assertIn("authorize-c00", needs_of(jobs["admission"]))
        self.assertIn("needs.authorize-c00.outputs.authorized == 'true'", jobs["admission"]["if"])


if __name__ == "__main__":
    unittest.main()
