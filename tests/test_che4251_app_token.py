"""CHE-4251: the `ci-app-token` environment (c00-multicado App key) is reachable only from main / merge_group jobs."""

import json
import os
import re
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[1]
WORKFLOWS = ROOT / ".github/workflows"
ENV_NAME = "ci-app-token"
SECRET_RE = re.compile(r"C00_MULTICADO_")
UNTRUSTED = {"merge_group", "pull_request", "pull_request_target", "issue_comment"}
MINT = "actions/create-github-app-token@"
DEFAULT_BRANCH_ONLY = {"workflow_run", "schedule"}  # GitHub reads these definitions from the default branch


def workflows():
    for path in sorted(WORKFLOWS.glob("*.yml")):
        yield path.name, yaml.safe_load(path.read_text())


def triggers(wf):
    on = wf.get("on", wf.get(True))  # PyYAML reads the bare `on` key as True
    return set(on if isinstance(on, (list, dict)) else [on])


def env_of(job):
    env = job.get("environment")
    return env.get("name", "") if isinstance(env, dict) else str(env or "")


def uses_app_token(job):
    return ENV_NAME in env_of(job) or any(SECRET_RE.search(str(s)) for s in job.get("steps", []))


def run_filter(wf):
    on = wf.get("on", wf.get(True))
    return on.get("workflow_run", {}) if isinstance(on, dict) else {}


def violations():
    """Return every rule break; empty means the guard holds.

    A workflow whose definition a queued or PR change can edit (merge_group, pull_request, pull_request_target,
    issue_comment) must never reach the App key: the environment branch policy admits the queue refs. Only a
    `workflow_run` definition, which GitHub reads from the default branch, may hold it.
    """
    found = []
    for name, wf in workflows():
        for job_id, job in wf.get("jobs", {}).items():
            if not uses_app_token(job):
                continue
            where = f"{name}:{job_id}"
            untrusted = triggers(wf) & UNTRUSTED
            if untrusted:
                found.append(f"{where}: reaches the App key from a definition a PR or queue change controls ({sorted(untrusted)})")
            if not triggers(wf) & DEFAULT_BRANCH_ONLY:
                found.append(f"{where}: key holder is not a workflow_run or schedule (default-branch) definition")
            if ENV_NAME not in env_of(job):
                found.append(f"{where}: reads the App secrets without the {ENV_NAME} environment")
            gate = str(job.get("if", ""))
            if "workflow_run" in triggers(wf) and run_filter(wf).get("branches") != ["main"] and not (
                "github.event.workflow_run.event == 'merge_group'" in gate
                and "github.event.workflow_run.head_repository.full_name == github.repository" in gate
            ):
                found.append(f"{where}: workflow_run job is not limited to main or to a same-repo merge_group run")
            for step in job.get("steps", []):
                text = str(step)
                if SECRET_RE.search(text) and not str(step.get("uses", "")).startswith(MINT):
                    found.append(f"{where}: secret used outside {MINT}")
                if str(step.get("uses", "")).startswith("actions/checkout@"):
                    ref = str(step.get("with", {}).get("ref", ""))
                    if re.search(r"head|pull|workflow_run", ref):
                        found.append(f"{where}: checks out the triggering run's code beside the App token")
    return found


class Guard(unittest.TestCase):
    def test_no_violations(self):
        self.assertEqual(violations(), [])

    def swapped(self, files):
        global WORKFLOWS
        d = tempfile.TemporaryDirectory()
        for name, wf in files.items():
            (Path(d.name) / name).write_text(yaml.safe_dump(wf))
        old, WORKFLOWS = WORKFLOWS, Path(d.name)
        try:
            return violations()
        finally:
            WORKFLOWS = old
            d.cleanup()

    def test_guard_catches_a_merge_group_job_with_the_environment(self):
        # The exact CHE-4251 review finding: a queue-controlled definition naming the environment.
        wf = {"on": {"merge_group": {}}, "jobs": {"j": {"environment": ENV_NAME, "steps": [{"run": "echo"}]}}}
        self.assertTrue(self.swapped({"x.yml": wf}))

    def test_guard_catches_a_pr_job_with_the_secret(self):
        wf = {"on": {"pull_request": {}}, "jobs": {"j": {"steps": [{"run": "echo ${{ secrets.C00_MULTICADO_APP_ID }}"}]}}}
        self.assertTrue(self.swapped({"x.yml": wf}))

    def test_guard_catches_an_ungated_workflow_run_job(self):
        wf = {"on": {"workflow_run": {"workflows": ["a"]}}, "jobs": {"j": {"environment": ENV_NAME, "steps": [{"run": "echo"}]}}}
        self.assertTrue(self.swapped({"x.yml": wf}))

    def test_guard_catches_a_workflow_run_job_that_checks_out_the_run_head(self):
        wf = {"on": {"workflow_run": {"workflows": ["a"], "branches": ["main"]}},
              "jobs": {"j": {"environment": ENV_NAME,
                             "steps": [{"uses": "actions/checkout@x", "with": {"ref": "${{ github.event.workflow_run.head_sha }}"}}]}}}
        self.assertTrue(self.swapped({"x.yml": wf}))

    def test_security_gate_definition_never_names_the_key(self):
        text = (WORKFLOWS / "security-gate.yml").read_text()
        self.assertNotIn(ENV_NAME, text)
        self.assertNotIn("C00_MULTICADO_", text)
        self.assertNotIn("create-github-app-token", text)

    def test_deploy_path_uses_the_app_token(self):
        # CHE-5148: the queue gate is off and holds no App token any more.
        jobs = dict(workflows())
        pre = jobs["cd-deploy.yml"]["jobs"]["candidate-preflight"]
        for job in (pre,):
            self.assertIn(ENV_NAME, env_of(job))
            mint = [s for s in job["steps"] if str(s.get("uses", "")).startswith(MINT)]
            self.assertEqual(len(mint), 1)
            w = mint[0]["with"]
            self.assertEqual(w["repositories"], "multica")
            self.assertTrue(any(k.startswith("permission-") for k in w))
            self.assertNotIn("write", {v for k, v in w.items() if k != "permission-statuses"})

    def test_queue_handoff_holds_no_secret_and_the_gate_skips_the_queue_once_trusted(self):
        sg = dict(workflows())["security-gate.yml"]["jobs"]
        self.assertEqual(sg["queue-handoff"]["permissions"], {})
        self.assertIn("vars.SECURITY_GATE_QUEUE_TRUSTED != 'true'", sg["security-gate"]["if"])
        q = dict(workflows())["security-gate-queue.yml"]["jobs"]["security-gate-queue"]
        self.assertIn("vars.SECURITY_GATE_QUEUE_TRUSTED == 'true'", q["if"])


class Wiring(unittest.TestCase):
    def test_polling_and_selection_steps_get_both_tokens_for_the_reserve_check(self):
        jobs = dict(workflows())
        pre = jobs["cd-deploy.yml"]["jobs"]["candidate-preflight"]
        select = next(s for s in pre["steps"] if s.get("id") == "candidate")
        self.assertIn("steps.app.outputs.token", select["env"]["APP_TOKEN"])
        self.assertIn("secrets.GITHUB_TOKEN", select["env"]["FALLBACK_TOKEN"])

    def test_jobs_that_hold_c00_secrets_never_get_the_app_key(self):
        # prepare-release-candidate and deploy hold C00 secrets in their own environments; the key stays out of them.
        cd = dict(workflows())["cd-deploy.yml"]["jobs"]
        for name in ("prepare-release-candidate", "authorize-c00", "admission", "deploy"):
            text = json.dumps(cd[name])
            self.assertNotIn(ENV_NAME, text, name)
            self.assertNotIn("C00_MULTICADO_", text, name)


class PickToken(unittest.TestCase):
    def run_pick(self, remaining, app="APP", fallback="GHT"):
        with tempfile.TemporaryDirectory() as d:
            gh = Path(d) / "gh"
            gh.write_text('#!/usr/bin/env bash\n[ "$GH_TOKEN" = APP ] || exit 9\n'
                          '[ "$FAKE_REMAINING" = fail ] && exit 1\necho "$FAKE_REMAINING"\n')
            gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
            out = Path(d) / "out"
            out.touch()
            r = subprocess.run(["bash", str(ROOT / "deploy/cd/pick-api-token.sh")], capture_output=True, text=True,
                               env={**os.environ, "PATH": f"{d}:{os.environ['PATH']}", "APP_TOKEN": app,
                                    "FALLBACK_TOKEN": fallback, "GITHUB_OUTPUT": str(out), "FAKE_REMAINING": remaining})
            return r, out.read_text()

    def test_uses_app_token_above_the_floor(self):
        r, out = self.run_pick("4000")
        self.assertEqual(out, "token=APP\n")
        self.assertIn("4000", r.stdout)
        self.assertNotIn("APP", r.stdout.replace("App token", "").replace("::add-mask::APP", ""))

    def test_server_reserve_plus_headroom_is_kept(self):
        # Floor 1,500 for the server plus a 100 headroom for what one poll or a sibling job spends before the next read.
        self.assertEqual(self.run_pick("1600")[1], "token=APP\n")
        self.assertEqual(self.run_pick("1599")[1], "token=GHT\n")
        r, out = self.run_pick("1500")
        self.assertEqual(out, "token=GHT\n")
        self.assertIn("falling back", r.stdout + r.stderr)

    def test_unreadable_budget_or_missing_app_token_falls_back(self):
        self.assertEqual(self.run_pick("fail")[1], "token=GHT\n")
        self.assertEqual(self.run_pick("4000", app="")[1], "token=GHT\n")


class GateBackoffDeferred(unittest.TestCase):
    """Split decision (7 Nov 2026): the gate's rate-limit backoff and freshness patches moved to CHE-4415.
    With the App token the gate behaves as on main: a rate limit is an ordinary error, never a wait."""

    def test_script_does_not_wait_out_a_rate_limit(self):
        import importlib.util
        from unittest import mock

        spec = importlib.util.spec_from_file_location("security_gate", ROOT / "scripts/security-gate/security_gate.py")
        sg = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(sg)
        err = subprocess.CalledProcessError(1, ["gh"], output="", stderr="HTTP 403: API rate limit exceeded")
        with mock.patch.object(sg.subprocess, "run", side_effect=err) as run, \
                mock.patch.object(sg.time, "sleep") as sleep:
            with self.assertRaises(subprocess.CalledProcessError):
                sg._gh("repos/x/y")
        self.assertEqual(run.call_count, 1)
        sleep.assert_not_called()

    def test_script_and_workflows_carry_no_gate_backoff(self):
        gate = (ROOT / "scripts/security-gate/security_gate.py").read_text()
        for needle in ("RATE_LIMIT", "RateLimited", "StaleDecision", "_reserve_write_budget"):
            self.assertNotIn(needle, gate)
        for name in ("security-gate.yml", "security-gate-queue.yml"):
            text = (WORKFLOWS / name).read_text()
            for needle in ("gh_retry", "sleep ", "rate limit", "return 75"):
                self.assertNotIn(needle, text, f"{name}: {needle}")


if __name__ == "__main__":
    unittest.main()
