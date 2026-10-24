#!/usr/bin/env python3
"""One C00 approval per version; a newer version replaces a waiting one (CHE-2027)."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
SOURCE_SHA = "c689a72ae" + "1" * 31
NEW_HEAD = "93ac2140b" + "2" * 31
GATE = "approve-c00"
STALE_GATE = "deploy-gate"
LOCK = "cd-deploy-c00"
PROTECTED = "c00-production"  # reviewer + wait timer; legacy runs and reruns keep hitting it
RUNTIME = "c00-runtime"  # holds the copied C00 secrets; no reviewers


def needs(job):
    value = job.get("needs", [])
    return [value] if isinstance(value, str) else value


class DeployApprovalTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load((ROOT / ".github/workflows/cd-deploy.yml").read_text())
        self.jobs = self.workflow["jobs"]

    def ancestors(self, name):
        seen, todo = set(), list(needs(self.jobs[name]))
        while todo:
            current = todo.pop()
            if current not in seen:
                seen.add(current)
                todo.extend(needs(self.jobs[current]))
        return seen

    def test_exactly_one_job_asks_for_approval(self):
        gated = [n for n, j in self.jobs.items() if j.get("environment") == PROTECTED]
        self.assertEqual(gated, [GATE])

    def test_protected_environment_is_never_given_up(self):
        # Removing its reviewer would leave old runs and downstream-job reruns,
        # which still use the old workflow, with no approval at all.
        self.assertEqual({j.get("environment") for j in self.jobs.values()} - {None}, {PROTECTED, RUNTIME})

    def test_every_c00_job_runs_only_after_the_approval(self):
        for name in ["prepare-release-candidate", "admission", "deploy"]:
            with self.subTest(job=name):
                self.assertIn(GATE, self.ancestors(name))
        self.assertIn(GATE, needs(self.jobs["prepare-release-candidate"]))
        self.assertIn(GATE, needs(self.jobs["admission"]))

    def test_secret_jobs_do_not_ask_again(self):
        for name in ["prepare-release-candidate", "deploy"]:
            with self.subTest(job=name):
                self.assertEqual(self.jobs[name]["environment"], RUNTIME)

    def test_stale_stage_never_enters_the_lock(self):
        # A job entering a concurrency group evicts the group's pending job, so
        # a stale stage entering late would evict the newest candidate. Every
        # job that joins the lock is therefore skipped, before it can queue, by
        # a freshness check made in an earlier job.
        self.assertIn("admission", needs(self.jobs[STALE_GATE]))
        self.assertNotIn("concurrency", self.jobs[STALE_GATE])
        self.assertEqual(self.jobs[STALE_GATE]["outputs"]["current"], "${{ steps.guard.outputs.current || 'true' }}")
        self.assertIn(STALE_GATE, needs(self.jobs["deploy"]))
        self.assertIn("needs.deploy-gate.outputs.current == 'true'", self.jobs["deploy"]["if"])
        self.assertIn("needs.approve-c00.outputs.current == 'true'", self.jobs["prepare-release-candidate"]["if"])
        for name, job in self.jobs.items():
            if job.get("concurrency", {}).get("group") == LOCK:
                with self.subTest(job=name):
                    self.assertIn("outputs.current == 'true'", job["if"])

    def test_gate_holds_no_secrets(self):
        for name in [GATE, STALE_GATE]:
            self.assertNotIn("secrets.C00", yaml.safe_dump(self.jobs[name]))

    def test_waiting_approval_is_replaced_by_a_newer_version(self):
        gate = self.jobs[GATE]["concurrency"]
        self.assertTrue(gate["cancel-in-progress"])
        self.assertNotEqual(gate["group"], LOCK)

    def test_no_workflow_level_lock_behind_a_waiting_approval(self):
        self.assertNotIn("concurrency", self.workflow)

    def test_mutation_stays_serialized_and_is_never_cancelled(self):
        for name in ["prepare-release-candidate", "deploy"]:
            with self.subTest(job=name):
                lock = self.jobs[name]["concurrency"]
                self.assertEqual(lock["group"], LOCK)
                self.assertIs(lock["cancel-in-progress"], False)

    def test_superseded_run_is_skipped_before_it_mutates_c00(self):
        guard = self.jobs[GATE]["outputs"]["current"]
        self.assertIn("steps.guard.outputs.current", guard)
        self.assertIn("needs.approve-c00.outputs.current == 'true'", self.jobs["admission"]["if"])
        steps = self.jobs["deploy"]["steps"]
        names = [s["name"] for s in steps]
        guard_at = next(i for i, s in enumerate(steps) if s.get("id") == "guard")
        mutate_at = names.index("Deploy to C00 over SSH")
        self.assertLess(guard_at, mutate_at)
        self.assertIn("steps.guard.outputs.current != 'false'", steps[mutate_at]["if"])


class LockModel:
    """GitHub's documented concurrency rule for cancel-in-progress: false:
    one running job, one pending job; a job that joins evicts the pending one."""

    def __init__(self):
        self.running, self.pending, self.evicted = None, None, []

    def join(self, job):
        if self.running is None:
            self.running = job
        else:
            if self.pending is not None:
                self.evicted.append(self.pending)
            self.pending = job

    def release(self):
        self.running, self.pending = self.pending, None


class StaleStageEvictionTests(unittest.TestCase):
    """Reviewer scenario: three approved versions; the oldest finishes
    admission late, after the middle one holds the lock and the newest queued."""

    def run_scenario(self, stale_stage_may_join):
        lock = LockModel()
        main_head = "newest"
        lock.join("middle:prepare")
        lock.join("newest:prepare")
        if stale_stage_may_join("oldest", main_head):
            lock.join("oldest:deploy")
        return lock

    def test_without_the_freshness_gate_the_newest_candidate_is_evicted(self):
        lock = self.run_scenario(lambda version, head: True)
        self.assertEqual(lock.evicted, ["newest:prepare"])

    def test_the_freshness_gate_keeps_the_newest_candidate_queued(self):
        # deploy-gate's rule: join the lock only if the candidate is the main head.
        lock = self.run_scenario(lambda version, head: version == head)
        self.assertEqual(lock.evicted, [])
        self.assertEqual(lock.pending, "newest:prepare")


class RequireCurrentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        bin_dir = self.directory / "bin"
        bin_dir.mkdir()
        stub = bin_dir / "gh"
        stub.write_text('#!/bin/sh\n[ -z "$STUB_API_ERROR" ] || { echo simulated GitHub API failure >&2; exit 1; }\necho "$STUB_MAIN"\n')
        stub.chmod(0o755)
        self.path = str(bin_dir) + os.pathsep + os.environ["PATH"]

    def run_guard(self, main_sha, api_error=""):
        output = self.directory / "output"
        summary = self.directory / "summary"
        output.write_text("")
        result = subprocess.run(
            ["bash", str(ROOT / "deploy/cd/require-current.sh"), SOURCE_SHA],
            env={**os.environ, "PATH": self.path, "STUB_MAIN": main_sha, "STUB_API_ERROR": api_error,
                 "GITHUB_REPOSITORY": "cheese-work/multica", "GITHUB_OUTPUT": str(output),
                 "GITHUB_STEP_SUMMARY": str(summary)},
            text=True, capture_output=True,
        )
        return result, output.read_text(), summary.read_text() if summary.exists() else ""

    def test_current_candidate_proceeds(self):
        result, output, _ = self.run_guard(SOURCE_SHA)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.strip(), "current=true")

    def test_superseded_candidate_is_skipped_not_failed(self):
        result, output, summary = self.run_guard(NEW_HEAD)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.strip(), "current=false")
        self.assertIn(SOURCE_SHA, summary)
        self.assertIn(NEW_HEAD, summary)

    def test_api_failure_fails_closed(self):
        result, output, _ = self.run_guard(SOURCE_SHA, api_error="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output.strip(), "")


if __name__ == "__main__":
    unittest.main(verbosity=2)
