#!/usr/bin/env python3
"""One C00 approval per version; a newer version replaces a waiting one (CHE-2027)."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
GATE = "approve-c00"
STALE_GATE = "deploy-gate"
LOCK = "cd-deploy-c00"
PROTECTED = "c00-production"  # reviewer + wait timer; legacy runs and reruns keep hitting it
RUNTIME = "c00-runtime"  # holds the copied C00 secrets; no reviewers
C00_RUNNER = ["self-hosted", "linux", "x64", "cheese-c00-deploy"]
C00_LOCAL_DEPLOY_STEP = "Deploy to C00 on local runner"


def needs(job):
    value = job.get("needs", [])
    return [value] if isinstance(value, str) else value


class DeployApprovalTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load((ROOT / ".github/workflows/cd-deploy.yml").read_text())
        self.jobs = self.workflow["jobs"]

    def test_baseline_reads_use_one_shared_capture_path(self):
        for name in ["prepare-release-candidate", "deploy"]:
            with self.subTest(job=name):
                steps = self.jobs[name]["steps"]
                capture_at = next(index for index, step in enumerate(steps)
                                  if "capture-remote-baseline.sh" in step.get("run", ""))
                before = json.dumps(steps[:capture_at])
                self.assertIn("actions/setup-node", before)

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

    def test_c00_jobs_run_on_the_c00_runner_and_never_over_ssh(self):
        # Both jobs that touch C00 run on the target-host runner. A fall-back
        # to a hosted runner plus ssh/scp would fail one of these assertions.
        for name in ["prepare-release-candidate", "deploy"]:
            with self.subTest(job=name):
                self.assertEqual(self.jobs[name]["runs-on"], C00_RUNNER)
                text = yaml.safe_dump(self.jobs[name])
                self.assertNotRegex(text, r"(?i)\b(ssh|scp|ssh-keyscan)\b")
                self.assertNotIn("C00_SSH", text)
        self.assertNotIn("C00_SSH", yaml.safe_dump(self.workflow))
        step_names = [s.get("name") for s in self.jobs["deploy"]["steps"]]
        self.assertIn(C00_LOCAL_DEPLOY_STEP, step_names)
        self.assertNotIn("Deploy to C00 over SSH", step_names)

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

    def test_deploy_pulls_with_the_job_token_not_a_stored_pat(self):
        # The registry login uses the job's own short-lived token, so no
        # personal access token has to be stored, rotated or revoked.
        deploy = self.jobs["deploy"]
        self.assertNotIn("secrets.GHCR_PULL_TOKEN", yaml.safe_dump(self.workflow))
        step = next(s for s in deploy["steps"] if s.get("name") == C00_LOCAL_DEPLOY_STEP)
        self.assertEqual(step["env"]["GHCR_PULL_TOKEN"], "${{ github.token }}")
        # A job-level block replaces the workflow-level one: keep what the job
        # already had and add only packages: read.
        self.assertEqual(deploy["permissions"], {"contents": "read", "actions": "read", "packages": "read"})
        for name, job in self.jobs.items():
            if name != "deploy":
                with self.subTest(job=name):
                    self.assertNotIn("packages", job.get("permissions", {}))
        self.assertNotIn("packages", self.workflow["permissions"])

    def test_approved_candidates_are_not_superseded_by_main(self):
        self.assertNotIn("require-current.sh", yaml.safe_dump(self.workflow))
        self.assertNotIn("outputs.current", yaml.safe_dump(self.workflow))
        self.assertEqual(needs(self.jobs["deploy"]), ["admission"])
        self.assertIn("needs.approve-c00.result == 'success'", self.jobs["admission"]["if"])

    def test_gate_holds_no_secrets(self):
        self.assertNotIn("secrets.C00", yaml.safe_dump(self.jobs[GATE]))

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
                self.assertFalse(lock.get("cancel-in-progress", False))
                # Default `single` keeps ONE pending job and evicts it for any
                # newcomer, whatever its version. `max` queues FIFO instead.
                self.assertEqual(lock["queue"], "max")

    def test_live_baseline_is_verified_before_controller_transfer(self):
        steps = self.jobs["deploy"]["steps"]
        checkout = next(step for step in steps if step.get("uses", "").startswith("actions/checkout"))
        self.assertEqual(checkout["with"]["fetch-depth"], 0)
        deploy = next(step for step in steps if step.get("id") == "deploy")
        self.assertNotIn("if", deploy)
        script = deploy["run"]
        self.assertLess(script.index("capture-remote-baseline.sh"), script.index("verify-deploy-baseline.mjs"))
        self.assertLess(script.index("verify-deploy-baseline.mjs"), script.index("build-cutover-bundle.sh"))
        self.assertLess(script.index('if [ "$decision" = "skip" ]'), script.index("build-cutover-bundle.sh"))
        outcome = next(step for step in steps if step.get("name") == "Record outcome")
        self.assertIn("steps.deploy.outputs.decision != 'skip'", outcome["if"])


class LockModel:
    """GitHub's documented concurrency rules for cancel-in-progress: false.
    queue=single: one running job, one pending job; a job that joins evicts the
    pending one. queue=max: up to 100 pending jobs, FIFO; none is evicted."""

    def __init__(self, queue):
        self.queue, self.running, self.pending, self.evicted = queue, None, [], []

    def join(self, job):
        if self.running is None:
            self.running = job
            return
        if self.queue == "single" and self.pending:
            self.evicted.extend(self.pending)
            self.pending = []
        self.pending.append(job)

    def release(self):
        self.running = self.pending.pop(0) if self.pending else None


class StaleStageEvictionTests(unittest.TestCase):
    """Reviewer scenario: the oldest candidate passed its guard while current and
    keeps that stored result. The middle version then holds the lock, the newest
    queues, and only then does the oldest downstream job join the lock."""

    def run_scenario(self, queue):
        lock = LockModel(queue)
        oldest_guard_said_current = True  # stored output; main moved on afterwards
        lock.join("middle:prepare")
        lock.join("newest:prepare")
        if oldest_guard_said_current:
            lock.join("oldest:deploy")
        return lock

    def test_single_pending_slot_loses_the_newest_candidate(self):
        self.assertEqual(self.run_scenario("single").evicted, ["newest:prepare"])

    def test_max_queue_keeps_every_candidate_in_order(self):
        lock = self.run_scenario("max")
        self.assertEqual(lock.evicted, [])
        self.assertEqual(lock.pending, ["newest:prepare", "oldest:deploy"])

    def test_workflow_uses_the_queue_that_never_evicts(self):
        queues = {n: j["concurrency"]["queue"] for n, j in self.jobs.items()
                  if j.get("concurrency", {}).get("group") == LOCK}
        self.assertEqual(queues, {"prepare-release-candidate": "max", "deploy": "max"})
        steps = {step.get("id"): step for step in self.jobs["deploy"]["steps"]}
        self.assertIn("verify-deploy-baseline.mjs", steps["deploy"]["run"])

    @property
    def jobs(self):
        return yaml.safe_load((ROOT / ".github/workflows/cd-deploy.yml").read_text())["jobs"]


class ApprovedDeployBaselineTests(unittest.TestCase):
    STUB = '''#!/usr/bin/env python3
import hashlib, json, os, pathlib, shutil, sys
command = pathlib.Path(sys.argv[0]).name
arguments = sys.argv[1:]
def record(action):
    # The cutover shell runs under `env -i`, so the harness context cannot
    # travel in the environment; it sits next to this stub instead.
    context = json.loads((pathlib.Path(sys.argv[0]).parent / "context.json").read_text())
    with open(context["DEPLOY_LOG"], "a") as log:
        log.write(action + ":" + context["CANDIDATE_SHA"] + "\\n")
if command == "bash":
    script = arguments[0] if arguments else ""
    if script == "deploy/cd/build-cutover-bundle.sh":
        record("bundle")
        bundle = pathlib.Path(arguments[arguments.index("--output") + 1])
        bundle.write_bytes(b"synthetic controller")
        checksum = pathlib.Path(arguments[arguments.index("--checksum-output") + 1])
        checksum.write_text(hashlib.sha256(bundle.read_bytes()).hexdigest() + "  " + str(bundle) + "\\n")
    elif script == "deploy/cd/capture-remote-baseline.sh":
        if os.environ.get("CAPTURE_FAIL"):
            sys.exit("simulated baseline capture failure")
        shutil.copyfile(os.environ["LIVE_TUPLE"], arguments[1])
    elif script == "deploy/cd/capture-release-state.sh" or script.endswith("/capture-release-state.sh"):
        pathlib.Path(arguments[arguments.index("--output") + 1]).write_text("{}")
    elif script.endswith("/ab-health-check.sh"):
        pass
    elif arguments == ["-s"]:
        if "cutover.sh" in sys.stdin.read():
            record("deploy")
    else:
        os.execv("/bin/bash", ["bash", *arguments])
else:
    sys.exit("unexpected command: " + command)
'''

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.repository = self.directory / "repository"
        self.repository.mkdir()
        self.git("init", "-q")
        self.commits = []
        for name in ["baseline", "approved", "newer"]:
            self.git("-c", "user.name=test", "-c", "user.email=test@example.invalid",
                     "commit", "-q", "--allow-empty", "-m", name)
            self.commits.append(self.git("rev-parse", "HEAD").strip())
        for name in ["deploy", ".github"]:
            (self.repository / name).symlink_to(ROOT / name)
        self.bin = self.directory / "bin"
        self.bin.mkdir()
        for command in ["bash"]:
            executable = self.bin / command
            executable.write_text(self.STUB)
            executable.chmod(0o755)
        github = self.bin / "gh"
        github.write_text('#!/usr/bin/env python3\nimport sys\nsys.exit("unexpected GitHub request after approval")\n')
        github.chmod(0o755)
        self.home = self.directory / "home"
        self.home.mkdir()
        self.log = self.directory / "deploy.log"
        self.log.write_text("")
        self.workflow = yaml.safe_load((ROOT / ".github/workflows/cd-deploy.yml").read_text())

    def git(self, *arguments):
        return subprocess.check_output(["git", *arguments], cwd=self.repository, text=True)

    def tuple_for(self, commit):
        snapshot = json.loads((ROOT / "deploy/cd/fixtures/c00-tuple-2026-09-12T235517Z.json").read_text())
        snapshot["application_sha"] = commit
        return snapshot

    def run_deploy(self, candidate, baseline, live, main, approved=True, overrides=None):
        if not approved:
            return "waiting", None
        inputs = self.repository / "cutover-input"
        inputs.mkdir(exist_ok=True)
        (inputs / "manifest.json").write_text(json.dumps({"source_sha": candidate}))
        (inputs / "baseline-tuple.json").write_text(json.dumps(baseline))
        live_path = self.directory / "live.json"
        live_path.write_text(json.dumps(live))
        output = self.directory / "output"
        summary = self.directory / "summary"
        output.write_text("")
        environment = {
            **os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "HOME": str(self.home), "DEPLOY_LOG": str(self.log), "LIVE_TUPLE": str(live_path),
            "CANDIDATE_SHA": candidate, "SOURCE_SHA": candidate,
            "GITHUB_REPOSITORY": "cheese-work/multica", "GITHUB_OUTPUT": str(output),
            "GITHUB_STEP_SUMMARY": str(summary), "STUB_MAIN": main,
            "C00_COMPOSE_DIR": "/synthetic/compose", "RUNNER_TEMP": str(self.directory),
            "C00_STATE_DIR": str(self.directory / "state"), "C00_CUTOVER_STATE_DIR": "/synthetic/cutover",
            "C00_ROUTER_STATE_DIR": "/synthetic/router", "GHCR_PULL_TOKEN": "synthetic token",
        }
        environment.update(overrides or {})
        (self.bin / "context.json").write_text(json.dumps({"DEPLOY_LOG": str(self.log), "CANDIDATE_SHA": candidate}))
        for job in [GATE, STALE_GATE, "deploy"]:
            for step in self.workflow["jobs"].get(job, {}).get("steps", []):
                script = step.get("run", "")
                if "require-current.sh" not in script and step.get("name") != "Deploy to C00 on local runner":
                    continue
                script = script.replace("${{ github.event.workflow_run.head_sha }}", candidate)
                script = script.replace("${{ needs.admission.outputs.source_sha }}", candidate)
                result = subprocess.run(["/bin/bash", "-c", script], cwd=self.repository,
                                        env=environment, text=True, capture_output=True)
                if result.returncode:
                    return "failed", result
                if "current=false" in output.read_text():
                    return "skipped", result
        deployed = "deploy:" + candidate in self.log.read_text()
        return "deployed" if deployed else "skipped", result

    def test_overlapping_approved_runs_deploy_in_order_despite_new_main(self):
        baseline, older, newer = self.commits
        outcome, result = self.run_deploy(older, self.tuple_for(baseline), self.tuple_for(baseline), newer)
        self.assertEqual(outcome, "deployed", result.stderr)
        before = self.log.read_text()
        outcome, _ = self.run_deploy(newer, self.tuple_for(older), self.tuple_for(older), newer, approved=False)
        self.assertEqual(outcome, "waiting")
        self.assertEqual(self.log.read_text(), before)
        outcome, result = self.run_deploy(newer, self.tuple_for(older), self.tuple_for(older), newer)
        self.assertEqual(outcome, "deployed", result.stderr)
        deployments = [line for line in self.log.read_text().splitlines() if line.startswith("deploy:")]
        self.assertEqual(deployments, ["deploy:" + older, "deploy:" + newer])

    def test_candidate_older_than_running_commit_is_skipped_without_transfer(self):
        baseline, older, newer = self.commits
        outcome, result = self.run_deploy(older, self.tuple_for(baseline), self.tuple_for(newer), older)
        self.assertEqual(outcome, "skipped", result.stderr)
        self.assertEqual(self.log.read_text(), "")

    def test_changed_baseline_fails_before_stale_manifest_reaches_c00(self):
        baseline, older, newer = self.commits
        outcome, result = self.run_deploy(newer, self.tuple_for(baseline), self.tuple_for(older), newer)
        self.assertEqual(outcome, "failed", result.stderr)
        self.assertIn("baseline changed", result.stderr.lower())
        self.assertEqual(self.log.read_text(), "")

    def test_equal_running_commit_is_skipped(self):
        baseline, candidate, _ = self.commits
        outcome, result = self.run_deploy(candidate, self.tuple_for(baseline), self.tuple_for(candidate), candidate)
        self.assertEqual(outcome, "skipped", result.stderr)
        self.assertEqual(self.log.read_text(), "")

    def test_capture_timestamp_does_not_change_baseline_identity(self):
        baseline, candidate, _ = self.commits
        live = self.tuple_for(baseline)
        live["captured_at"] = "2026-10-26T00:00:00Z"
        outcome, result = self.run_deploy(candidate, self.tuple_for(baseline), live, candidate)
        self.assertEqual(outcome, "deployed", result.stderr)

    def test_image_config_and_ledger_drift_fail_closed(self):
        baseline, candidate, _ = self.commits
        for field in ["images", "compose", "migration_ledger"]:
            with self.subTest(field=field):
                live = self.tuple_for(baseline)
                if field == "images":
                    live[field]["web"]["digest"] = "sha256:" + "f" * 64
                elif field == "compose":
                    live[field]["sha256"] = "sha256:" + "f" * 64
                else:
                    live[field]["row_count"] += 1
                outcome, result = self.run_deploy(candidate, self.tuple_for(baseline), live, candidate)
                self.assertEqual(outcome, "failed", result.stderr)
                self.assertIn("baseline changed", result.stderr)
                self.assertEqual(self.log.read_text(), "")

    def test_unknown_running_commit_fails_closed(self):
        baseline, candidate, _ = self.commits
        outcome, result = self.run_deploy(candidate, self.tuple_for(baseline), self.tuple_for("f" * 40), candidate)
        self.assertEqual(outcome, "failed", result.stderr)
        self.assertIn("cannot verify deployed commit ancestry", result.stderr)
        self.assertEqual(self.log.read_text(), "")

    def test_diverged_running_commit_fails_closed(self):
        baseline, candidate, _ = self.commits
        self.git("checkout", "-q", "--detach", baseline)
        self.git("-c", "user.name=test", "-c", "user.email=test@example.invalid",
                 "commit", "-q", "--allow-empty", "-m", "diverged")
        live = self.tuple_for(self.git("rev-parse", "HEAD").strip())
        outcome, result = self.run_deploy(candidate, self.tuple_for(baseline), live, candidate)
        self.assertEqual(outcome, "failed", result.stderr)
        self.assertIn("diverges", result.stderr)
        self.assertEqual(self.log.read_text(), "")

    def test_live_capture_failure_fails_closed(self):
        baseline, candidate, _ = self.commits
        outcome, result = self.run_deploy(candidate, self.tuple_for(baseline), self.tuple_for(baseline), candidate,
                                          overrides={"CAPTURE_FAIL": "true"})
        self.assertEqual(outcome, "failed", result.stderr)
        self.assertIn("simulated baseline capture failure", result.stderr)
        self.assertEqual(self.log.read_text(), "")


if __name__ == "__main__":
    unittest.main(verbosity=2)
