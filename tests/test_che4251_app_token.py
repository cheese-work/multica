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
PR_TRIGGERS = {"pull_request", "pull_request_target", "issue_comment"}
MINT = "actions/create-github-app-token@"


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


def violations():
    """Return every rule break; empty means the guard holds."""
    found = []
    for name, wf in workflows():
        for job_id, job in wf.get("jobs", {}).items():
            if not uses_app_token(job):
                continue
            where = f"{name}:{job_id}"
            mixed = bool(triggers(wf) & PR_TRIGGERS)
            if mixed and "github.event_name == 'merge_group'" not in env_of(job):
                found.append(f"{where}: PR-triggered workflow, environment is not gated on merge_group")
            if ENV_NAME not in env_of(job):
                found.append(f"{where}: reads the App secrets without the {ENV_NAME} environment")
            for step in job.get("steps", []):
                text = str(step)
                if SECRET_RE.search(text) and not str(step.get("uses", "")).startswith(MINT):
                    found.append(f"{where}: secret used outside {MINT}")
                if SECRET_RE.search(text) and mixed and "github.event_name == 'merge_group'" not in str(step.get("if", "")):
                    found.append(f"{where}: mint step not gated on merge_group")
                if str(step.get("uses", "")).startswith("actions/checkout@"):
                    ref = str(step.get("with", {}).get("ref", ""))
                    if re.search(r"head|pull|github\.sha", ref) and mixed:
                        found.append(f"{where}: checks out PR code beside the App token")
    return found


class Guard(unittest.TestCase):
    def test_no_violations(self):
        self.assertEqual(violations(), [])

    def test_guard_catches_an_ungated_pr_job(self):
        wf = {"on": {"pull_request": {}}, "jobs": {"j": {"environment": ENV_NAME, "steps": [{"run": "echo"}]}}}
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / "x.yml").write_text(yaml.safe_dump(wf))
            global WORKFLOWS
            old, WORKFLOWS = WORKFLOWS, Path(d)
            try:
                self.assertTrue(violations())
            finally:
                WORKFLOWS = old

    def test_deploy_path_and_queue_gate_use_the_app_token(self):
        jobs = dict(workflows())
        pre = jobs["cd-deploy.yml"]["jobs"]["candidate-preflight"]
        gate = jobs["security-gate.yml"]["jobs"]["security-gate"]
        for job in (pre, gate):
            self.assertIn(ENV_NAME, env_of(job))
            mint = [s for s in job["steps"] if str(s.get("uses", "")).startswith(MINT)]
            self.assertEqual(len(mint), 1)
            w = mint[0]["with"]
            self.assertEqual(w["repositories"], "multica")
            self.assertTrue(any(k.startswith("permission-") for k in w))
            self.assertNotIn("permission-contents", {k for k, v in w.items() if v == "write"})
        self.assertEqual(gate["steps"][[s.get("uses", "").startswith(MINT) for s in gate["steps"]].index(True)]["with"]["permission-statuses"], "write")


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
        for name in ("prepare-release-candidate", "approve-c00", "admission", "deploy"):
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


if __name__ == "__main__":
    unittest.main()
