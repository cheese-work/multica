#!/usr/bin/env python3
"""authorize-c00.py decides who may deploy to C00 (CHE-4582, D5 = A, D6 = A).

Cheese's merge into `main` is the deploy authorization. A dispatch needs the
owner. Any other actor builds and stops (push) or fails at the gate (dispatch).
`gh` is a stub that serves canned API responses; nothing here reaches GitHub.
"""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/cd/authorize-c00.py"
REPO = "cheese-work/multica"
SHA = "a" * 40
OTHER_SHA = "b" * 40

STUB = """#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
assert args[0] == "api", args
path = args[1]
fixtures = json.loads(os.environ["GH_FIXTURES"])
if path not in fixtures:
    sys.exit("unexpected API request: " + path)
value = fixtures[path]
if value is None:
    sys.exit("simulated API failure: " + path)
print(json.dumps(value))
"""


def pull(sha=SHA, merged_by="congvc-dev", base="main", merged=True):
    return {
        "merged_at": "2026-11-08T03:00:00Z" if merged else None,
        "merge_commit_sha": sha,
        "base": {"ref": base},
        "merged_by": {"login": merged_by} if merged_by else None,
    }


def run(run_id, jobs=None):
    return {"id": run_id, "jobs": jobs or []}


class AuthorizeC00Tests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        directory = Path(self.temp.name)
        bin_dir = directory / "bin"
        bin_dir.mkdir()
        gh = bin_dir / "gh"
        gh.write_text(STUB)
        gh.chmod(0o755)
        self.bin = bin_dir
        self.output = directory / "output"
        self.output.write_text("")

    def authorize(self, event, actor="", head_sha=SHA, candidate="", extra="", pulls=None, runs=None, current_run="99"):
        """Return (returncode, outputs, stderr). `runs` is newest-first [(id, [(job, conclusion)])]."""
        fixtures = {f"repos/{REPO}/commits/{head_sha}/pulls": [] if pulls is None else pulls}
        runs = runs or []
        fixtures[f"repos/{REPO}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page=20"] = {
            "workflow_runs": [{"id": rid} for rid, _ in runs]
        }
        for rid, jobs in runs:
            fixtures[f"repos/{REPO}/actions/runs/{rid}/jobs?per_page=100"] = {
                "jobs": [{"name": name, "conclusion": conclusion} for name, conclusion in jobs]
            }
        env = {
            **os.environ,
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "GH_FIXTURES": json.dumps(fixtures),
            "GITHUB_OUTPUT": str(self.output),
            "GITHUB_REPOSITORY": REPO,
            "GITHUB_RUN_ID": current_run,
            "GH_TOKEN": "x",
            "EVENT_NAME": event,
            "TRIGGERING_ACTOR": actor,
            "HEAD_SHA": head_sha,
            "INPUT_CANDIDATE_SHA": candidate,
            "EXTRA_AUTHORIZERS": extra,
        }
        result = subprocess.run(["python3", str(SCRIPT)], env=env, text=True, capture_output=True)
        outputs = dict(line.split("=", 1) for line in self.output.read_text().splitlines() if "=" in line)
        return result.returncode, outputs, result.stdout + result.stderr

    # -- (a) the owner's merge deploys with no further click ------------------
    def test_merge_by_the_owner_is_authorized_and_names_the_merged_commit(self):
        code, out, text = self.authorize("workflow_run", pulls=[pull()])
        self.assertEqual(code, 0, text)
        self.assertEqual(out["authorized"], "true")
        self.assertEqual(out["candidate_sha"], SHA)

    def test_merge_by_an_allowlisted_agent_identity_is_authorized(self):
        for extra in ["agent-merger", "someone, agent-merger ,x"]:
            with self.subTest(extra=extra):
                self.output.write_text("")
                code, out, text = self.authorize("workflow_run", pulls=[pull(merged_by="agent-merger")], extra=extra)
                self.assertEqual(code, 0, text)
                self.assertEqual(out["authorized"], "true")

    # -- (b) any other merger builds and stops --------------------------------
    def test_merge_by_anyone_else_builds_and_stops_without_failing(self):
        for merger in ["someone-else", "congvc-dev-bot", "xcongvc-dev", "agent-merger"]:
            with self.subTest(merger=merger):
                self.output.write_text("")
                code, out, text = self.authorize("workflow_run", pulls=[pull(merged_by=merger)])
                self.assertEqual(code, 0, text)
                self.assertEqual(out["authorized"], "false")
                self.assertEqual(out.get("candidate_sha", ""), "")

    def test_empty_allowlist_entries_authorize_nobody(self):
        code, out, _ = self.authorize("workflow_run", pulls=[pull(merged_by="someone-else")], extra=" , ,")
        self.assertEqual((code, out["authorized"]), (0, "false"))

    def test_a_commit_without_a_merged_main_pull_is_not_authorized(self):
        cases = {
            "no pull": [],
            "open pull": [pull(merged=False)],
            "other base": [pull(base="release")],
            "different merge commit": [pull(sha=OTHER_SHA)],
            "no merger recorded": [pull(merged_by=None)],
        }
        for name, pulls in cases.items():
            with self.subTest(case=name):
                self.output.write_text("")
                code, out, text = self.authorize("workflow_run", pulls=pulls)
                self.assertEqual(code, 0, text)
                self.assertEqual(out["authorized"], "false")

    def test_api_failure_fails_closed_and_loud(self):
        fixtures = {f"repos/{REPO}/commits/{SHA}/pulls": None}
        env = {**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
               "GH_FIXTURES": json.dumps(fixtures), "GITHUB_OUTPUT": str(self.output),
               "GITHUB_REPOSITORY": REPO, "GITHUB_RUN_ID": "99", "GH_TOKEN": "x",
               "EVENT_NAME": "workflow_run", "HEAD_SHA": SHA, "EXTRA_AUTHORIZERS": ""}
        result = subprocess.run(["python3", str(SCRIPT)], env=env, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("authorized=true", self.output.read_text())

    # -- (c) a dispatch by another actor still fails before any C00 step ------
    def test_dispatch_by_the_owner_is_authorized_and_passes_the_input_through(self):
        for candidate in [SHA, ""]:
            with self.subTest(candidate=candidate):
                self.output.write_text("")
                code, out, text = self.authorize("workflow_dispatch", actor="congvc-dev", candidate=candidate)
                self.assertEqual(code, 0, text)
                self.assertEqual(out["authorized"], "true")
                self.assertEqual(out["candidate_sha"], candidate)

    def test_dispatch_by_another_actor_fails(self):
        for actor in ["someone-else", "congvc-dev-bot", "", "congvc-devx"]:
            with self.subTest(actor=actor):
                self.output.write_text("")
                code, out, text = self.authorize("workflow_dispatch", actor=actor)
                self.assertNotEqual(code, 0)
                self.assertNotEqual(out.get("authorized"), "true")
                self.assertIn("refused", text)

    # -- suspension: a failed automatic deploy stops further auto-deploys ----
    def test_latest_deploy_failure_suspends_auto_deploy(self):
        for failed in ["deploy", "readback"]:
            with self.subTest(failed=failed):
                self.output.write_text("")
                runs = [
                    (5, [("authorize-c00", "success"), ("deploy", "skipped")]),  # built and stopped: ignored
                    (4, [("deploy", "success" if failed == "readback" else "failure"),
                         ("readback", "failure" if failed == "readback" else "skipped")]),
                    (3, [("deploy", "success"), ("readback", "success")]),
                ]
                code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=runs)
                self.assertEqual(code, 0, text)
                self.assertEqual(out["authorized"], "false")
                self.assertIn("suspended", text)

    def test_a_later_successful_deploy_lifts_the_suspension(self):
        runs = [(6, [("deploy", "success"), ("readback", "success")]), (4, [("deploy", "failure")])]
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=runs)
        self.assertEqual((code, out["authorized"]), (0, "true"), text)

    def test_the_current_run_and_runs_that_never_deployed_do_not_count(self):
        runs = [(99, [("deploy", "failure")]), (5, [("deploy", "skipped")]), (4, [("deploy", "cancelled")])]
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=runs)
        self.assertEqual((code, out["authorized"]), (0, "true"), text)

    def test_a_dispatch_is_never_suspended(self):
        # The owner's dispatch is the way out of a suspension.
        code, out, text = self.authorize("workflow_dispatch", actor="congvc-dev", runs=[(4, [("deploy", "failure")])])
        self.assertEqual((code, out["authorized"]), (0, "true"), text)


if __name__ == "__main__":
    unittest.main(verbosity=2)
