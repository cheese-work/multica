#!/usr/bin/env python3
"""authorize-c00.py decides who may deploy to C00 (CHE-4582, D5 = A, D6 = A).

Cheese's merge into `main` is the deploy authorization. A dispatch needs the
owner. Any other actor builds and stops (push) or fails at the gate (dispatch).
`gh` is a stub that serves canned API responses; nothing here reaches GitHub.
"""
import importlib.util
import json
import os
from pathlib import Path
import random
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

MARKER = "C00 deploy deferred by quiescence preflight"
DEPLOY_JOBS = ("deploy", "readback", "readback-delay", "readback-confirm")


def history_fixtures(runs):
    """API fixtures for a newest-first history. `runs` is [(id, jobs)] or [(id, jobs, run_fields)].

    A job is (name, conclusion) or (name, conclusion, [(step, conclusion)]). `run_fields` is merged
    into the run entry of the runs list (run_attempt, event).
    """
    fixtures = {}
    # 100 runs a page, as the script asks; a short page ends the history.
    pages = [runs[i:i + 100] for i in range(0, len(runs), 100)] or [[]]
    for number, page in enumerate(pages, start=1):
        fixtures[f"repos/{REPO}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page=100&page={number}"] = {
            "workflow_runs": [{"id": entry[0], **(entry[2] if len(entry) > 2 else {})} for entry in page]
        }
    if len(pages[-1]) == 100:
        fixtures[f"repos/{REPO}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page=100&page={len(pages) + 1}"] = {
            "workflow_runs": []
        }
    for entry in runs:
        jobs = []
        for job in entry[1]:
            record = {"name": job[0], "conclusion": job[1]}
            if len(job) > 2:
                record["steps"] = [{"name": name, "conclusion": conclusion} for name, conclusion in job[2]]
            jobs.append(record)
        fixtures[f"repos/{REPO}/actions/runs/{entry[0]}/jobs?per_page=100"] = {"jobs": jobs}
    return fixtures


def deferral(rid, attempt=1, event="schedule", marker="success"):
    """A first-attempt run the S1 producer marked: deploy failed, marker step ran, readback skipped."""
    steps = [("Deploy", "failure")] + ([(MARKER, marker)] if marker else [])
    jobs = [("deploy", "failure", steps), ("readback", "skipped"), ("readback-delay", "skipped"), ("readback-confirm", "skipped")]
    return (rid, jobs, {"run_attempt": attempt, "event": event})


def legacy_deferral(rid):
    """Today's unmarked deferral: deploy exited 1, nothing else ran."""
    return (rid, [("deploy", "failure", [("Deploy", "failure")]), ("readback", "skipped")], {"run_attempt": 1, "event": "schedule"})


def success_run(rid, attempt=1):
    jobs = [("deploy", "success"), ("readback", "success"), ("readback-delay", "success"), ("readback-confirm", "success")]
    return (rid, jobs, {"run_attempt": attempt, "event": "schedule"})


def failure_run(rid, attempt=1):
    jobs = [("deploy", "success"), ("readback", "failure"), ("readback-delay", "skipped"), ("readback-confirm", "skipped")]
    return (rid, jobs, {"run_attempt": attempt, "event": "schedule"})


def silent_run(rid):
    return (rid, [("authorize-c00", "success"), ("deploy", "skipped"), ("readback", "skipped")], {"run_attempt": 1, "event": "schedule"})


def load_module():
    spec = importlib.util.spec_from_file_location("authorize_c00", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def reference_authorized(runs, current_run):
    """A copy of the rule on `main` before CHE-9223 S2: the oracle for the equivalence tests.

    Walk finished runs newest first, 100 a page, at most 5 pages; the first run with a
    failure in the 4 deploy jobs suspends, the first with a success (and no failure) lifts.
    Also returns the API calls the walk made.
    """
    calls = []
    for page in range(1, 6):
        chunk = runs[(page - 1) * 100:page * 100]
        calls.append(f"repos/{REPO}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page=100&page={page}")
        for entry in chunk:
            if str(entry[0]) == str(current_run):
                continue
            calls.append(f"repos/{REPO}/actions/runs/{entry[0]}/jobs?per_page=100")
            results = [job[1] for job in entry[1] if job[0] in DEPLOY_JOBS]
            if "failure" in results:
                return False, calls
            if "success" in results:
                return True, calls
        if len(chunk) < 100:
            return True, calls
    return False, calls


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
        """Return (returncode, outputs, stderr). `runs` is newest-first [(id, [(job, conclusion)])], see history_fixtures; `pulls` is [pull()]."""
        pulls = pulls or []
        fixtures = {f"repos/{REPO}/commits/{head_sha}/pulls": [listed for listed, _ in pulls]}
        for listed, detail in pulls:
            fixtures[f"repos/{REPO}/pulls/{listed['number']}"] = None if detail_failure else detail
        fixtures.update(history_fixtures(runs or []))
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

    # -- CHE-9223 S2: a marked deferral is named; the decision does not change ----
    def explain(self, runs, current_run="99"):
        """Run `--explain`; return (returncode, stdout lines, stderr). GITHUB_OUTPUT must stay untouched."""
        env = {
            **os.environ,
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "GH_FIXTURES": json.dumps(history_fixtures(runs)),
            "GITHUB_OUTPUT": str(self.output),
            "GITHUB_REPOSITORY": REPO,
            "GITHUB_RUN_ID": current_run,
            "GH_TOKEN": "x",
        }
        result = subprocess.run(["python3", str(SCRIPT), "--explain"], env=env, text=True, capture_output=True)
        self.assertEqual(self.output.read_text(), "", "--explain must not write GITHUB_OUTPUT")
        return result.returncode, result.stdout.splitlines(), result.stderr

    def test_a_first_attempt_marked_deferral_at_the_head_is_named_and_still_suspends(self):
        code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=[deferral(45047985980), success_run(3)])
        self.assertEqual((code, out["authorized"]), (0, "false"), text)
        self.assertIn("run 45047985980 was deferred by the quiescence preflight; nothing was cut over", text)
        self.assertIn("Automatic retry is not active yet (CHE-9330)", text)
        self.assertNotIn("failed after deploying", text)
        # Title and last sentence stay.
        self.assertIn("title=C00 auto-deploy suspended", text)
        self.assertIn("A successful dispatch by the owner lifts the suspension.", text)

    def test_runs_that_are_not_a_first_attempt_marked_deferral_are_failures_with_todays_text(self):
        cases = {
            "rerun with a marked deferral": deferral(45, attempt=2),
            "legacy unmarked deferral": legacy_deferral(45),
            "observation_failed, no marker": deferral(45, marker=None),
            "marker success but readback failed": (45, [("deploy", "failure", [(MARKER, "success")]), ("readback", "failure")], {"run_attempt": 1}),
            "deploy step and marker skipped": (45, [("deploy", "failure", [("Deploy", "skipped"), (MARKER, "skipped")]), ("readback", "skipped")], {"run_attempt": 1}),
            "marker step failed": deferral(45, marker="failure"),
            "readback-delay ran": (45, [("deploy", "failure", [(MARKER, "success")]), ("readback-delay", "failure")], {"run_attempt": 1}),
            "no run_attempt in the run": (45, [("deploy", "failure", [(MARKER, "success")]), ("readback", "skipped")]),
        }
        for name, run in cases.items():
            with self.subTest(case=name):
                self.output.write_text("")
                code, out, text = self.authorize("workflow_run", pulls=[pull()], runs=[run])
                self.assertEqual((code, out["authorized"]), (0, "false"), text)
                self.assertIn("run 45 failed after deploying", text)
                self.assertNotIn("deferred by the quiescence preflight", text)

    def test_explain_counts_deferrals_in_a_row_and_stops_at_three(self):
        for count in (1, 2, 3):
            with self.subTest(count=count):
                runs = [deferral(200 - i) for i in range(count)] + [success_run(50), deferral(40)]
                code, lines, err = self.explain(runs)
                self.assertEqual(code, 0, err)
                self.assertEqual(lines.count(f"deferral streak {count}/3"), 1, lines)
                self.assertIn("authorized=false", lines)
                self.assertEqual([l for l in lines if l.endswith(": deferral")], [f"run {200 - i} attempt 1 event schedule: deferral" for i in range(count)])
        # Four in a row: stops at the third, never lists the fourth.
        code, lines, err = self.explain([deferral(200 - i) for i in range(5)])
        self.assertEqual(code, 0, err)
        self.assertIn("deferral streak 3/3", lines)
        self.assertEqual(len([l for l in lines if l.endswith(": deferral")]), 3, lines)

    def test_explain_reports_a_failure_after_one_deferral(self):
        code, lines, err = self.explain([deferral(9), failure_run(8), deferral(7)])
        self.assertEqual(code, 0, err)
        self.assertEqual(lines[0], "run 9 attempt 1 event schedule: deferral")
        self.assertEqual(lines[1], "run 8 attempt 1 event schedule: failure")
        self.assertIn("deferral streak 1/3", lines)
        self.assertIn("authorized=false", lines)
        self.assertNotIn("run 7 attempt 1 event schedule: deferral", lines)

    def test_explain_lists_silent_runs_and_skips_the_current_run(self):
        code, lines, err = self.explain([(99, [("deploy", "failure")], {"run_attempt": 1, "event": "schedule"}), silent_run(5), deferral(4)])
        self.assertEqual(code, 0, err)
        self.assertEqual(lines[:2], ["run 5 attempt 1 event schedule: silent", "run 4 attempt 1 event schedule: deferral"])
        self.assertTrue(all(not l.startswith("run 99") for l in lines), lines)

    def test_explain_authorized_follows_gate_mode_for_a_success_or_empty_history(self):
        for name, runs, expected in [("success head", [success_run(5)], "true"), ("no history", [], "true"), ("failure head", [failure_run(5)], "false")]:
            with self.subTest(case=name):
                code, lines, err = self.explain(runs)
                self.assertEqual(code, 0, err)
                self.assertIn(f"authorized={expected}", lines)
                self.assertIn("deferral streak 0/3", lines)

    def test_every_existing_history_keeps_its_decision(self):
        """AC2: the new `authorized` equals the rule on main for every fixture history in this file."""
        module = load_module()
        for name, runs in FIXTURE_HISTORIES.items():
            with self.subTest(history=name):
                self.assertEqual(decide(module, runs), reference_authorized(runs, "99")[0])

    def test_500_generated_histories_keep_their_decision_and_their_api_calls(self):
        module = load_module()
        rng = random.Random(9370)
        for index in range(500):
            runs = generated_history(rng, index)
            with self.subTest(index=index):
                expected, expected_calls = reference_authorized(runs, "99")
                actual, calls = decide(module, runs, log=True)
                self.assertEqual(actual, expected)
                self.assertEqual(calls, expected_calls)

    def test_gate_mode_makes_the_same_api_calls_as_before(self):
        for name, runs in [("deferral then success", [deferral(9), success_run(8)]), ("silent, failure, success", [silent_run(9), failure_run(8), success_run(7)]),
                           ("clean history", [success_run(9), deferral(8)]), ("the current run first", [deferral(99), silent_run(5), success_run(4)])]:
            with self.subTest(history=name):
                module = load_module()
                _, calls = decide(module, runs, log=True)
                self.assertEqual(calls, reference_authorized(runs, "99")[1])

    def test_classify_is_pure_and_total(self):
        module = load_module()
        run = {"id": 1, "run_attempt": 1}
        marked = history_fixtures([deferral(1)])[f"repos/{REPO}/actions/runs/1/jobs?per_page=100"]["jobs"]
        self.assertEqual(module.classify(run, marked), "deferral")
        self.assertEqual(module.classify({"id": 1, "run_attempt": 2}, marked), "failure")
        self.assertEqual(module.classify(run, []), "silent")
        self.assertEqual(module.classify(run, [{"name": "deploy", "conclusion": "success"}]), "success")
        self.assertEqual(module.classify(run, [{"name": "deploy", "conclusion": "failure"}]), "failure")
        self.assertEqual(module.classify(run, [{"name": "deploy", "conclusion": "failure", "steps": None}]), "failure")
        self.assertEqual(module.classify(run, [{"name": "deploy", "conclusion": "skipped"}, {"name": "other", "conclusion": "failure"}]), "silent")


def decide(module, runs, current_run="99", log=False):
    """`authorized` as the gate walk decides it, over a fake gh_api. With `log`, also the API calls made."""
    fixtures = history_fixtures(runs)
    calls = []

    def fake_gh_api(path):
        calls.append(path)
        return fixtures[path]

    module.gh_api = fake_gh_api
    authorized = module.suspended_by(REPO, current_run) is None
    return (authorized, calls) if log else authorized


def generated_history(rng, index):
    """A seeded newest-first history. Mixes deferrals, failures, successes, silent runs, reruns, legacy runs."""
    length = rng.choice([0, 1, 2, 3, 5, 8, 20]) if index % 25 else rng.choice([99, 100, 101, 250, 499, 500, 520])
    runs = []
    for position in range(length):
        rid = 10000 - position
        kind = rng.choices(["deferral", "rerun_deferral", "legacy", "observation_failed", "failure", "success", "silent", "cancelled", "current"],
                           weights=[3, 1, 1, 1, 2, 3, 12 if length > 30 else 4, 1, 1])[0]
        if kind == "deferral":
            runs.append(deferral(rid, event=rng.choice(["schedule", "workflow_dispatch"])))
        elif kind == "rerun_deferral":
            runs.append(deferral(rid, attempt=rng.choice([2, 3])))
        elif kind == "legacy":
            runs.append(legacy_deferral(rid))
        elif kind == "observation_failed":
            runs.append(deferral(rid, marker=None))
        elif kind == "failure":
            runs.append(rng.choice([failure_run, lambda r: (r, [("deploy", "failure")], {"run_attempt": 1})])(rid))
        elif kind == "success":
            runs.append(success_run(rid, attempt=rng.choice([1, 2])))
        elif kind == "cancelled":
            runs.append((rid, [("deploy", "cancelled"), ("readback", "skipped")], {"run_attempt": 1}))
        elif kind == "current":
            runs.append((99, [("deploy", "failure")], {"run_attempt": 1}))
        else:
            runs.append(silent_run(rid))
    return runs


FIXTURE_HISTORIES = {
    "failed readback-delay": [(4, [("deploy", "success"), ("readback", "success"), ("readback-delay", "failure"), ("readback-confirm", "skipped")]),
                              (3, [("deploy", "success"), ("readback", "success"), ("readback-delay", "success"), ("readback-confirm", "success")])],
    "failed readback-confirm": [(4, [("deploy", "success"), ("readback", "success"), ("readback-confirm", "failure")]),
                                (3, [("deploy", "success"), ("readback", "success"), ("readback-confirm", "success")])],
    "deploy failure after a stopped run": [(5, [("authorize-c00", "success"), ("deploy", "skipped")]), (4, [("deploy", "failure"), ("readback", "skipped")]), (3, [("deploy", "success"), ("readback", "success")])],
    "readback failure after a stopped run": [(5, [("authorize-c00", "success"), ("deploy", "skipped")]), (4, [("deploy", "success"), ("readback", "failure")]), (3, [("deploy", "success"), ("readback", "success")])],
    "later success lifts": [(6, [("deploy", "success"), ("readback", "success")]), (4, [("deploy", "failure")])],
    "current run and non-deploys": [(99, [("deploy", "failure")]), (5, [("deploy", "skipped")]), (4, [("deploy", "cancelled")])],
    "failure behind 130 stopped": [(1000 - i, [("deploy", "skipped"), ("readback", "skipped")]) for i in range(130)] + [(500, [("deploy", "failure")]), (400, [("deploy", "success")])],
    "success behind 130 stopped": [(1000 - i, [("deploy", "skipped")]) for i in range(130)] + [(500, [("deploy", "success")])],
    "never deployed": [(1000 - i, [("deploy", "skipped")]) for i in range(130)],
    "five full pages of stopped runs": [(10000 - i, [("deploy", "skipped")]) for i in range(500)],
    "dispatch history failure": [(4, [("deploy", "failure")])],
    "empty": [],
    "marked deferral head": [deferral(9), success_run(8)],
    "marked deferral behind success": [success_run(9), deferral(8)],
}


if __name__ == "__main__":
    unittest.main(verbosity=2)
