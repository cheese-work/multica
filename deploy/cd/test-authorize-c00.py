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


PR_NUMBER = 1234


def pull(sha=SHA, merged_by="congvc-dev", base="main", merged=True, number=PR_NUMBER, detail_sha=None, detail_base=None):
    """(list entry, detail). GitHub's commits/SHA/pulls list has no `merged_by`; only pulls/N does."""
    listed = {
        "number": number,
        "merged_at": "2026-11-08T03:00:00Z" if merged else None,
        "merge_commit_sha": sha,
        "base": {"ref": base},
    }
    detail = {
        **listed,
        "merge_commit_sha": detail_sha or sha,
        "base": {"ref": detail_base or base},
        "merged_by": {"login": merged_by} if merged_by else None,
    }
    return listed, detail


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

    def authorize(self, event, actor="", head_sha=SHA, candidate="", extra="", pulls=None, runs=None, current_run="99", detail_failure=False, backup=""):
        """Return (returncode, outputs, stderr). `runs` is newest-first [(id, [(job, conclusion)])]; `pulls` is [pull()]."""
        pulls = pulls or []
        fixtures = {f"repos/{REPO}/commits/{head_sha}/pulls": [listed for listed, _ in pulls]}
        for listed, detail in pulls:
            fixtures[f"repos/{REPO}/pulls/{listed['number']}"] = None if detail_failure else detail
        runs = runs or []
        # 100 runs a page, as the script asks; a short page ends the history.
        pages = [runs[i:i + 100] for i in range(0, len(runs), 100)] or [[]]
        for number, page in enumerate(pages, start=1):
            fixtures[f"repos/{REPO}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page=100&page={number}"] = {
                "workflow_runs": [{"id": rid} for rid, _ in page]
            }
        if len(pages[-1]) == 100:
            fixtures[f"repos/{REPO}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page=100&page={len(pages) + 1}"] = {
                "workflow_runs": []
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
            "INPUT_SCHEDULE_BACKUP": backup,
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
            "detail says another merge commit": [pull(detail_sha=OTHER_SHA)],
            "detail says another base": [pull(detail_base="release")],
        }
        for name, pulls in cases.items():
            with self.subTest(case=name):
                self.output.write_text("")
                code, out, text = self.authorize("workflow_run", pulls=pulls)
                self.assertEqual(code, 0, text)
                self.assertEqual(out["authorized"], "false")

    def test_the_merger_comes_from_the_pull_detail_not_the_commit_list(self):
        # The commit list carries no merged_by (P1, review of e0d5b41ab4). Two PRs
        # share the commit; only the one whose merge commit is HEAD_SHA counts.
        other = pull(sha=OTHER_SHA, merged_by="congvc-dev", number=7)
        mine = pull(merged_by="someone-else")
        self.assertNotIn("merged_by", mine[0])
        code, out, text = self.authorize("workflow_run", pulls=[other, mine])
        self.assertEqual((code, out["authorized"]), (0, "false"), text)

    def test_a_pull_detail_failure_fails_closed_and_loud(self):
        code, out, text = self.authorize("workflow_run", pulls=[pull()], detail_failure=True)
        self.assertNotEqual(code, 0)
        self.assertNotEqual(out.get("authorized"), "true")
        self.assertIn("GitHub API read failed", text)

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
    def test_a_failed_readback_delay_suspends_auto_deploy(self):
        runs = [(4, [("deploy", "success"), ("readback", "success"), ("readback-delay", "failure"), ("readback-confirm", "skipped")]),
                (3, [("deploy", "success"), ("readback", "success"), ("readback-delay", "success"), ("readback-confirm", "success")])]
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=runs)
        self.assertEqual(code, 0, text)
        self.assertEqual(out["authorized"], "false")
        self.assertIn("suspended", text)

    def test_a_failed_readback_confirm_suspends_auto_deploy(self):
        runs = [(4, [("deploy", "success"), ("readback", "success"), ("readback-confirm", "failure")]),
                (3, [("deploy", "success"), ("readback", "success"), ("readback-confirm", "success")])]
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=runs)
        self.assertEqual(code, 0, text)
        self.assertEqual(out["authorized"], "false")
        self.assertIn("suspended", text)

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

    def test_suspension_does_not_expire_after_many_stopped_runs(self):
        # P2, review of e0d5b41ab4: 20 built-and-stopped runs used to push a failed deploy out of view.
        stopped = [(1000 - i, [("deploy", "skipped"), ("readback", "skipped")]) for i in range(130)]
        runs = stopped + [(500, [("deploy", "failure")]), (400, [("deploy", "success")])]
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=runs)
        self.assertEqual((code, out["authorized"]), (0, "false"), text)
        self.assertIn("suspended", text)

    def test_a_deploy_found_after_many_stopped_runs_decides(self):
        stopped = [(1000 - i, [("deploy", "skipped")]) for i in range(130)]
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=stopped + [(500, [("deploy", "success")])])
        self.assertEqual((code, out["authorized"]), (0, "true"), text)

    def test_history_that_never_deployed_is_not_suspended(self):
        stopped = [(1000 - i, [("deploy", "skipped")]) for i in range(130)]
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=stopped)
        self.assertEqual((code, out["authorized"]), (0, "true"), text)

    def test_an_unreadable_history_fails_closed(self):
        # Five full pages of stopped runs and no deploy in sight: unknown, so do not deploy.
        stopped = [(10000 - i, [("deploy", "skipped")]) for i in range(500)]
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=stopped)
        self.assertEqual((code, out["authorized"]), (0, "false"), text)
        self.assertIn("suspended", text)

    def test_a_dispatch_is_never_suspended(self):
        # The owner's dispatch is the way out of a suspension.
        code, out, text = self.authorize("workflow_dispatch", actor="congvc-dev", runs=[(4, [("deploy", "failure")])])
        self.assertEqual((code, out["authorized"]), (0, "true"), text)

    # -- CHE-5353: the freshness alarm's backup dispatch follows the schedule rules --
    def test_backup_dispatch_by_the_workflow_robot_follows_the_merger_rule(self):
        code, out, text = self.authorize("workflow_dispatch", actor="github-actions[bot]", backup="true", pulls=[pull()])
        self.assertEqual((code, out["authorized"], out["candidate_sha"]), (0, "true", SHA), text)

    def test_backup_dispatch_of_a_commit_merged_by_anyone_else_builds_and_stops(self):
        code, out, text = self.authorize("workflow_dispatch", actor="github-actions[bot]", backup="true", pulls=[pull(merged_by="someone-else")])
        self.assertEqual((code, out["authorized"]), (0, "false"), text)

    def test_backup_dispatch_is_suspended_like_a_scheduled_run(self):
        code, out, text = self.authorize("workflow_dispatch", actor="github-actions[bot]", backup="true", pulls=[pull()], runs=[(4, [("deploy", "failure")])])
        self.assertEqual((code, out["authorized"]), (0, "false"), text)
        self.assertIn("suspended", text)

    def test_backup_marker_from_any_other_actor_is_refused(self):
        for actor in ["congvc-dev", "someone-else", "", "github-actions"]:
            with self.subTest(actor=actor):
                self.output.write_text("")
                code, out, text = self.authorize("workflow_dispatch", actor=actor, backup="true", pulls=[pull()])
                self.assertNotEqual(code, 0)
                self.assertNotEqual(out.get("authorized"), "true")
                self.assertIn("refused", text)

    def test_the_robot_without_the_marker_is_still_refused(self):
        code, out, text = self.authorize("workflow_dispatch", actor="github-actions[bot]", pulls=[pull()])
        self.assertNotEqual(code, 0)
        self.assertNotEqual(out.get("authorized"), "true")


if __name__ == "__main__":
    unittest.main(verbosity=2)
