#!/usr/bin/env python3
"""Fixture tests for the C00 deploy freshness alarm (CHE-3634, CHE-9243): rerun-once, slot deadline, trusted release receipt and rate-limit rules."""
import contextlib
import datetime as dt
import importlib.util
import io
import json
import re
import subprocess
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

spec = importlib.util.spec_from_file_location("fresh", Path(__file__).with_name("check-deploy-freshness.py"))
assert spec is not None and spec.loader is not None
fresh = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fresh)

SHA = "d2593932fc23ade672b8d1962041ef9086762473"
OLD = "c" * 40
NOW = dt.datetime(2026, 11, 5, 12, tzinfo=dt.timezone.utc)
REPO = "cheese-work/multica"
M = dt.timedelta(minutes=1)


def at(h, m=0, day=5):
    return dt.datetime(2026, 11, day, h, m, tzinfo=dt.timezone.utc)


S = at(5, 17)  # a cd-deploy.yml slot
S2 = S + dt.timedelta(hours=4)  # the next slot


def head_at(minutes_ago):
    return NOW - dt.timedelta(minutes=minutes_ago)


def qual(conclusion, attempt=1, run_id=10, created="2026-11-05T10:00:00Z"):
    return {"id": run_id, "head_sha": SHA, "conclusion": conclusion, "status": "completed" if conclusion else "in_progress",
            "run_attempt": attempt, "created_at": created, "html_url": f"https://example.test/run/{run_id}"}


def receipt_name(sha):
    return f"release-receipt old={'1' * 40} new={sha} digest=sha256:{'a' * 64}"


def deploy(sha=SHA, done="2026-11-05T05:40:00Z", run_id=20, branch="main", path=".github/workflows/cd-deploy.yml", repo=REPO,
           deploy_job="success", receipt_job="success", receipt_branch="main", step="success", name=None, error=None):
    """A successful `CD deploy to C00` run as the API lists it, plus its `jobs` payload under `_jobs` (CHE-9243).
    The run title is `· schedule` or `· main`, never the SHA; only the receipt step names the deployed SHA."""
    steps = [{"name": "Set up job", "conclusion": "success"}, {"name": name or receipt_name(sha), "conclusion": step}]
    jobs = [{"name": "deploy", "conclusion": deploy_job},
            {"name": "release-receipt", "conclusion": receipt_job, "head_branch": receipt_branch, "completed_at": done, "steps": steps}]
    return {"id": run_id, "head_branch": branch, "path": path, "repository": {"full_name": repo}, "display_title": "CD deploy to C00 · schedule",
            "conclusion": "success", "status": "completed", "_jobs": jobs, "_error": error}


def commit_json(sha, minutes_ago):
    return {"sha": sha, "commit": {"committer": {"date": head_at(minutes_ago).isoformat()}}}


def commit_at(sha, when):
    return {"sha": sha, "commit": {"committer": {"date": when.isoformat()}}}


def sched(created):
    return {"id": 30, "event": "schedule", "created_at": created, "html_url": "https://example.test/run/30"}


class AlarmCase(unittest.TestCase):
    """Runs main() against a fake GitHub API."""

    DISPATCH = ("workflow", "run", "cd-deploy.yml", "--ref", "main", "-f", "schedule_backup=true", "-R", REPO)
    DEFAULT_LOG = "2026-11-05T10:01:02Z ok\n2026-11-05T10:01:03Z ##[error]buildx failed: boom\n"

    def run_main(self, qual_runs=(), deploy_runs=(), minutes_ago=None, older=(), commits=None, now=NOW, deploy_list_error=None,
                 schedule_runs=(sched("2026-11-05T09:20:00Z"),), any_runs=(), dispatch_fails=False, log=DEFAULT_LOG, argv=(), live="unset"):
        if commits is None:
            commits = [commit_json(SHA, minutes_ago), *older]
        qual_jobs = [{"id": 7, "name": "build-images", "conclusion": "failure"}, {"id": 8, "name": "cd-qualification", "conclusion": "skipped"}]
        by_id = {r["id"]: r for r in deploy_runs}
        seen = []

        def fake(*args):
            seen.append(args)
            path = args[-1]
            if args[:2] == ("workflow", "run") and dispatch_fails:
                raise subprocess.CalledProcessError(1, "gh", stderr="HTTP 403")
            if args[:2] in (("run", "rerun"), ("workflow", "run")):
                return ""
            if "/commits?" in path:
                return json.dumps(commits)
            if "workflows/cd-qualification.yml/runs" in path:
                return json.dumps({"workflow_runs": list(qual_runs)})
            if "workflows/cd-deploy.yml/runs" in path:
                if "event=schedule" in path:
                    return json.dumps({"workflow_runs": list(schedule_runs)})
                if "status=success" in path:
                    if deploy_list_error:
                        raise deploy_list_error
                    return json.dumps({"workflow_runs": [{k: v for k, v in r.items() if k != "_error"} for r in deploy_runs]})
                if "branch=main" in path:
                    return json.dumps({"workflow_runs": list(any_runs)})
            m = re.search(r"/actions/runs/(\d+)/jobs\?", path)
            if m:
                run = by_id[int(m.group(1))]
                if run["_error"]:
                    raise run["_error"]
                return json.dumps({"jobs": run["_jobs"]})
            if path.endswith("/jobs?per_page=100"):
                return json.dumps({"jobs": qual_jobs})
            if path.endswith("/jobs/7/logs"):
                return log
            raise AssertionError(path)

        out = io.StringIO()
        env = {"GITHUB_REPOSITORY": REPO, "C00_API_URL": "https://c00.test"}
        with patch.dict(fresh.os.environ, env), patch.object(fresh, "gh_api", side_effect=fake), patch.object(fresh, "utcnow", return_value=now), \
                contextlib.ExitStack() as stack, contextlib.redirect_stdout(out):
            if live != "unset":
                stack.enter_context(patch.object(fresh, "live_commit", return_value=live))
            rc = fresh.main(argv)
        return rc, out.getvalue(), seen

    @staticmethod
    def dispatches(seen):
        return [a for a in seen if a[:2] == ("workflow", "run")]

    @staticmethod
    def reruns(seen):
        return [a for a in seen if a[:2] == ("run", "rerun")]

    @staticmethod
    def jobs_calls(seen):
        return [a for a in seen if re.search(r"/actions/runs/\d+/jobs\?", a[-1])]


class RerunTest(unittest.TestCase):
    def test_failed_first_attempt_is_rerun_once(self):
        self.assertEqual(fresh.rerun_id([qual("failure")]), 10)

    def test_second_attempt_failure_is_not_rerun_again(self):
        self.assertIsNone(fresh.rerun_id([qual("failure", attempt=2)]))

    def test_only_the_newest_qualification_run_counts(self):
        old = qual("failure", run_id=1, created="2026-11-05T09:00:00Z")
        new = qual("success", run_id=2, created="2026-11-05T10:00:00Z")
        self.assertIsNone(fresh.rerun_id([old, new]))
        self.assertIsNone(fresh.rerun_id([new, old]))

    def test_success_running_and_cancelled_are_not_rerun(self):
        for conclusion in ("success", None, "cancelled", "skipped"):
            self.assertIsNone(fresh.rerun_id([qual(conclusion)]), conclusion)

    def test_no_qualification_run_is_not_rerun(self):
        self.assertIsNone(fresh.rerun_id([]))


class DeadlineTest(unittest.TestCase):
    """CHE-9243 A1: the deadline is the first slot strictly after the merge, plus 90 min."""

    def test_deadline_is_the_next_slot_plus_90_minutes(self):
        self.assertEqual(fresh.deadline(S + 5 * M), S2 + 90 * M)
        self.assertEqual(fresh.deadline(S - 60 * M), S + 90 * M)

    def test_a_merge_exactly_at_a_slot_waits_for_the_following_slot(self):
        self.assertEqual(fresh.deadline(S), S2 + 90 * M)

    def test_slots_cross_midnight(self):
        self.assertEqual(fresh.deadline(at(21, 30)), at(1, 17, day=6) + 90 * M)
        self.assertEqual(fresh.deadline(at(23, 59, day=30)), dt.datetime(2026, 12, 1, 1, 17, tzinfo=dt.timezone.utc) + 90 * M)


class UndeployedCommitTest(unittest.TestCase):
    """CHE-7522: the window belongs to the oldest commit newer than what runs, not to the newest commit."""

    A, B = "a" * 40, "b" * 40

    def test_oldest_commit_newer_than_the_deployed_one(self):
        commits = [(self.B, head_at(40)), (self.A, head_at(100)), (OLD, head_at(300))]
        self.assertEqual(fresh.undeployed_commit(commits, lambda sha: sha == OLD), (self.A, head_at(100)))

    def test_head_deployed_leaves_nothing_undeployed(self):
        commits = [(self.B, head_at(40)), (self.A, head_at(100))]
        self.assertIsNone(fresh.undeployed_commit(commits, lambda sha: sha == self.B))

    def test_nothing_deployed_in_the_list_gives_the_oldest_listed_commit(self):
        commits = [(self.B, head_at(40)), (self.A, head_at(100))]
        self.assertEqual(fresh.undeployed_commit(commits, lambda sha: False), (self.A, head_at(100)))


class CreditedShaTest(AlarmCase):
    def credited(self, runs):
        by_id = {r["id"]: r for r in runs}
        with patch.object(fresh, "gh_api", side_effect=lambda *a: json.dumps({"jobs": by_id[int(re.search(r"runs/(\d+)/jobs", a[-1]).group(1))]["_jobs"]})):
            return fresh.credited_sha(REPO, runs)

    def test_r1a_a_trusted_receipt_credits_the_sha_it_names(self):
        self.assertEqual(self.credited([deploy(SHA)]), SHA)

    def test_fp02_the_receipt_that_completed_last_wins_not_the_newest_run(self):
        newest_created = deploy(OLD, run_id=21, done="2026-11-05T06:10:00Z")
        older_created = deploy(SHA, run_id=20, done="2026-11-05T06:30:00Z")
        self.assertEqual(self.credited([newest_created, older_created]), SHA)
        self.assertEqual(self.credited([older_created, newest_created]), SHA)

    def test_no_trusted_run_credits_nothing(self):
        self.assertIsNone(self.credited([deploy(SHA, deploy_job="skipped")]))
        self.assertIsNone(self.credited([]))


class WindowTest(AlarmCase):
    """R1 to R4, R6: red or green at the slot deadline. S = 05:17 UTC, S2 = S + 4 h."""

    def alarm(self, merged, now, deploy_runs=(), qual_runs=(), older=()):
        return self.run_main(qual_runs=qual_runs, deploy_runs=deploy_runs, commits=[commit_at(SHA, merged), *older], now=now)

    def test_r1a_a_schedule_run_with_a_trusted_receipt_for_head_is_deployed(self):
        rc, out, _ = self.alarm(S - 60 * M, S + 120 * M, deploy_runs=[deploy(SHA)])
        self.assertEqual((rc, out), (0, "main %s is deployed to C00.\n" % SHA[:9]))

    def test_r1b_the_same_history_without_a_receipt_alerts(self):
        run = deploy(SHA, deploy_job="skipped")
        rc, out, _ = self.alarm(S - 60 * M, S + 120 * M, deploy_runs=[run])
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)

    def test_r2_a_merge_just_after_a_slot_is_not_late_before_the_next_slot_passes(self):
        rc, out, _ = self.alarm(S + 5 * M, S + 125 * M)
        self.assertEqual(rc, 0, out)
        self.assertNotIn("::error::", out)

    def test_r3a_one_minute_before_the_next_slot_deadline_is_green(self):
        rc, out, _ = self.alarm(S + 5 * M, S2 + 89 * M)
        self.assertEqual(rc, 0, out)

    def test_r3b_at_the_next_slot_deadline_is_red(self):
        rc, out, _ = self.alarm(S + 5 * M, S2 + 90 * M)
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)

    def test_r4_the_qualification_run_is_diagnostic_text_only(self):
        # merged 05:07 UTC (12:07 ICT), qualified at 05:37: the 05:17 slot still sets the deadline at 06:47
        late = dict(qual("success", created="2026-11-05T05:37:00Z"), html_url="https://example.test/run/qualified")
        rc, out, _ = self.alarm(S - 10 * M, S + 90 * M, qual_runs=[late])
        self.assertEqual(rc, 1)
        self.assertIn("https://example.test/run/qualified", out)

    def test_r4_just_before_the_deadline_is_green(self):
        late = qual("success", created="2026-11-05T05:37:00Z")
        self.assertEqual(self.alarm(S - 10 * M, S + 89 * M, qual_runs=[late])[0], 0)

    def test_error_names_commit_merge_slot_and_deadline_in_utc_and_ict(self):
        rc, out, _ = self.alarm(at(1, 58), at(7, 0))
        self.assertEqual(rc, 1)
        for text in (SHA[:9], "01:58 UTC", "08:58 ICT", "05:17 UTC", "12:17 ICT", "06:47 UTC", "13:47 ICT"):
            self.assertIn(text, out)

    def test_r6_a_second_merge_does_not_move_the_deadline(self):
        # A merged 04:17 UTC and never deployed (deadline 06:47); B merged 06:30 (its own deadline is 10:47).
        a, b = "a" * 40, "b" * 40
        commits = [commit_at(b, at(6, 30)), commit_at(a, S - 60 * M), commit_at(OLD, S - 600 * M)]
        rc, out, _ = self.run_main(deploy_runs=[deploy(OLD)], commits=commits, now=at(6, 50))
        self.assertEqual(rc, 1)
        self.assertIn(a[:9], out)
        self.assertNotIn("main %s" % b[:9] + " is deployed", out)

    def test_a_deployed_older_commit_leaves_only_the_newer_ones_on_the_clock(self):
        a = "a" * 40
        commits = [commit_at(SHA, S + 5 * M), commit_at(a, S - 60 * M)]
        rc, out, _ = self.run_main(deploy_runs=[deploy(a)], commits=commits, now=S + 125 * M)
        self.assertEqual(rc, 0, out)


class ReceiptTrustTest(AlarmCase):
    """R5: each rejected receipt leaves a stale head red."""

    REJECTED = {
        "run branch is not main": dict(branch="feature"),
        "workflow path is not cd-deploy.yml": dict(path=".github/workflows/other.yml"),
        "run is from another repository": dict(repo="evil/multica"),
        "release-receipt job branch is not main": dict(receipt_branch="feature"),
        "deploy job skipped": dict(deploy_job="skipped"),
        "deploy job failed": dict(deploy_job="failure"),
        "receipt job skipped": dict(receipt_job="skipped"),
        "receipt job failed": dict(receipt_job="failure"),
        "receipt step not success": dict(step="skipped"),
        "short SHA": dict(name=f"release-receipt old={'1' * 40} new={SHA[:7]} digest=sha256:{'a' * 64}"),
        "unevaluated expression": dict(name="release-receipt old=${{ needs.deploy.outputs.receipt_old }} new=${{ needs.deploy.outputs.receipt_new }} digest=${{ x }}"),
        "extra text after the digest": dict(name=receipt_name(SHA) + " extra"),
        "extra text before the receipt": dict(name="x " + receipt_name(SHA)),
        "uppercase SHA": dict(name=receipt_name(SHA.upper())),
        "bad digest": dict(name=f"release-receipt old={'1' * 40} new={SHA} digest=sha256:abc"),
    }

    def test_r5_every_rejected_receipt_leaves_a_stale_head_red(self):
        for label, override in self.REJECTED.items():
            with self.subTest(label):
                rc, out, _ = self.run_main(deploy_runs=[deploy(SHA, **override)], commits=[commit_at(SHA, S - 60 * M)], now=S + 120 * M)
                self.assertEqual(rc, 1)
                self.assertIn("::error::", out)

    def test_only_successful_main_runs_are_listed(self):
        _, _, seen = self.run_main(deploy_runs=[deploy(SHA)], commits=[commit_at(SHA, S - 60 * M)], now=S + 120 * M)
        listed = [a[-1] for a in seen if "cd-deploy.yml/runs" in a[-1] and "status=success" in a[-1]]
        self.assertEqual(len(listed), 1)
        self.assertIn("branch=main", listed[0])

    def test_the_title_is_never_a_receipt(self):
        run = deploy(SHA, deploy_job="skipped")
        run["display_title"] = f"CD deploy to C00 · {SHA}"
        self.assertEqual(self.run_main(deploy_runs=[run], commits=[commit_at(SHA, S - 60 * M)], now=S + 120 * M)[0], 1)

    def test_a_trusted_receipt_for_a_head_older_run_still_credits_after_rejected_ones(self):
        runs = [deploy(SHA, run_id=21, branch="feature"), deploy(SHA, run_id=22, deploy_job="failure"), deploy(SHA, run_id=23)]
        self.assertEqual(self.run_main(deploy_runs=runs, commits=[commit_at(SHA, S - 60 * M)], now=S + 120 * M)[0], 0)


class ScanBoundTest(AlarmCase):
    def test_r8_the_scan_stops_within_30_jobs_calls(self):
        runs = [deploy(SHA, run_id=100 + i, receipt_job="skipped") for i in range(40)]
        rc, _, seen = self.run_main(deploy_runs=runs, commits=[commit_at(SHA, S - 60 * M)], now=S + 120 * M)
        self.assertEqual(len(self.jobs_calls(seen)), fresh.SCAN_CAP)
        self.assertEqual(fresh.SCAN_CAP, 30)
        self.assertEqual(rc, 1)

    def test_r8_a_trusted_receipt_past_the_scan_cap_credits_nothing(self):
        runs = [deploy(SHA, run_id=100 + i, receipt_job="skipped") for i in range(30)] + [deploy(SHA, run_id=200)]
        rc, _, _ = self.run_main(deploy_runs=runs, commits=[commit_at(SHA, S - 60 * M)], now=S + 120 * M)
        self.assertEqual(rc, 1)

    def test_r8_a_receipt_for_a_sha_outside_the_listed_commits_credits_nothing(self):
        rc, out, _ = self.run_main(deploy_runs=[deploy("f" * 40)], commits=[commit_at(SHA, S - 60 * M), commit_at(OLD, S - 120 * M)], now=S + 120 * M)
        self.assertEqual(rc, 1)
        self.assertIn(OLD[:9], out)  # nothing credited: the oldest listed commit is the undeployed one


class ReceiptOrderTest(AlarmCase):
    def test_fp02_the_later_cutover_is_credited_and_rule_2_stays_green_past_the_deadline(self):
        # cd-deploy.yml queues runs (FIFO lock, queue: max): the run created first can cut over last. The newest-created
        # run deployed OLD at 06:10; the older-created run deployed the head at 06:30, so C00 runs the head.
        runs = [deploy(OLD, run_id=21, done="2026-11-05T06:10:00Z"), deploy(SHA, run_id=20, done="2026-11-05T06:30:00Z")]
        commits = [commit_at(SHA, S - 60 * M), commit_at(OLD, S - 600 * M)]
        rc, out, _ = self.run_main(deploy_runs=runs, commits=commits, now=S + 120 * M)
        self.assertEqual((rc, out), (0, "main %s is deployed to C00.\n" % SHA[:9]))

    def test_fp02_the_older_cutover_is_not_credited_when_it_completed_first(self):
        runs = [deploy(SHA, run_id=21, done="2026-11-05T06:10:00Z"), deploy(OLD, run_id=20, done="2026-11-05T06:30:00Z")]
        commits = [commit_at(SHA, S - 60 * M), commit_at(OLD, S - 600 * M)]
        rc, out, _ = self.run_main(deploy_runs=runs, commits=commits, now=S + 120 * M)
        self.assertEqual(rc, 1)
        self.assertIn(SHA[:9], out)


class FailurePathTest(AlarmCase):
    def test_fp01_r9_a_jobs_api_error_still_dispatches_reruns_and_reports_rule_2(self):
        boom = subprocess.CalledProcessError(1, "gh", stderr="HTTP 500: Server Error")
        runs = [deploy(SHA, error=boom)]
        rc, out, seen = self.run_main(qual_runs=[qual("failure")], deploy_runs=runs, commits=[commit_at(SHA, at(11, 30))], now=at(12, 0),
                                      schedule_runs=[], dispatch_fails=False)
        self.assertEqual(self.dispatches(seen), [self.DISPATCH])
        self.assertEqual(self.reruns(seen), [("run", "rerun", "10", "--failed", "-R", REPO)])
        self.assertIn("::error::rule 2 could not read deploy receipts: ", out)
        self.assertIn("HTTP 500", out)
        self.assertEqual(rc, 1)

    def test_a_deploy_run_list_error_is_reported_the_same_way(self):
        boom = subprocess.CalledProcessError(1, "gh", stderr="HTTP 502")
        rc, out, seen = self.run_main(qual_runs=[qual("failure")], deploy_list_error=boom, minutes_ago=30)
        self.assertEqual(rc, 1)
        self.assertIn("::error::rule 2 could not read deploy receipts: ", out)
        self.assertEqual(len(self.reruns(seen)), 1)

    def test_a_malformed_jobs_payload_is_reported_the_same_way(self):
        run = deploy(SHA)
        run["_jobs"] = [{"name": "deploy"}]
        rc, out, _ = self.run_main(deploy_runs=[run], minutes_ago=30)
        self.assertEqual(rc, 1)
        self.assertIn("::error::rule 2 could not read deploy receipts: ", out)

    def test_a_missing_run_field_is_reported_the_same_way(self):
        run = deploy(SHA)
        del run["id"]
        run["id"] = 20
        del run["path"]
        rc, out, _ = self.run_main(deploy_runs=[run], minutes_ago=30)
        self.assertEqual(rc, 1)
        self.assertIn("::error::rule 2 could not read deploy receipts: ", out)

    def test_rule_3_and_rule_1_run_before_rule_2(self):
        boom = subprocess.CalledProcessError(1, "gh", stderr="HTTP 500")
        _, _, seen = self.run_main(qual_runs=[qual("failure")], deploy_runs=[deploy(SHA, error=boom)], minutes_ago=30, schedule_runs=[])
        order = [i for i, a in enumerate(seen) if a[:2] in (("workflow", "run"), ("run", "rerun")) or self.jobs_calls([a])]
        kinds = ["dispatch" if seen[i][:2] == ("workflow", "run") else "rerun" if seen[i][:2] == ("run", "rerun") else "jobs" for i in order]
        self.assertEqual(kinds, ["dispatch", "rerun", "jobs"])


class MainTest(AlarmCase):
    def test_failed_build_is_rerun_once_and_not_yet_alerted(self):
        rc, out, seen = self.run_main([qual("failure")], [], 30)
        self.assertEqual(rc, 0)
        self.assertEqual(self.reruns(seen), [("run", "rerun", "10", "--failed", "-R", REPO)])

    def test_second_attempt_failure_after_the_deadline_alerts_with_run_url_and_first_error(self):
        rc, out, seen = self.run_main([qual("failure", attempt=2)], [], 400)
        self.assertEqual(rc, 1)
        self.assertEqual(self.reruns(seen), [])
        self.assertIn("::error::", out)
        self.assertIn("https://example.test/run/10", out)
        self.assertIn("buildx failed: boom", out)

    def test_healthy_deployed_head_is_silent(self):
        rc, out, _ = self.run_main([qual("success")], [deploy(SHA)], 300)
        self.assertEqual((rc, out), (0, "main %s is deployed to C00.\n" % SHA[:9]))

    def test_head_inside_the_window_is_not_an_alert(self):
        rc, out, _ = self.run_main([qual("success")], [], 30)
        self.assertEqual(rc, 0)
        self.assertIn("inside its window", out)

    def test_successful_run_with_skipped_deploy_job_still_alerts(self):
        rc, out, _ = self.run_main([qual("success")], [deploy(SHA, deploy_job="skipped")], 400)
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)

    def test_old_undeployed_head_with_no_error_line_still_alerts(self):
        rc, out, _ = self.run_main([qual("success")], [deploy(SHA, deploy_job="failure")], 400, log="nothing useful\n")
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)

    def test_no_qualification_run_at_all_alerts_after_the_deadline(self):
        rc, out, _ = self.run_main([], [], 400)
        self.assertEqual(rc, 1)
        self.assertIn("no qualification run found", out)

    def test_missed_deploy_slot_alerts_even_when_main_is_deployed(self):
        rc, out, _ = self.run_main([qual("success")], [deploy(SHA)], 300, schedule_runs=[])
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)
        self.assertIn("09:17 UTC", out)
        self.assertIn("16:17 ICT", out)

    def test_missed_slot_dispatches_cd_deploy_once_and_stays_red(self):
        rc, out, seen = self.run_main([qual("success")], [deploy(SHA)], 300, schedule_runs=[])
        self.assertEqual(self.dispatches(seen), [self.DISPATCH])
        self.assertEqual(rc, 1)
        self.assertIn("::error::cd-deploy.yml slot 09:17 UTC", out)
        self.assertIn("backup dispatch", out)

    def test_no_second_dispatch_when_a_cd_deploy_run_started_since_the_slot(self):
        # the run an earlier alarm dispatched (or a workflow_run deploy) is created after 09:17 UTC
        started = {"id": 40, "event": "workflow_dispatch", "created_at": "2026-11-05T09:50:00Z"}
        rc, out, seen = self.run_main([qual("success")], [deploy(SHA)], 300, schedule_runs=[], any_runs=[started])
        self.assertEqual(self.dispatches(seen), [])
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)

    def test_a_run_from_before_the_slot_does_not_block_the_dispatch(self):
        old = {"id": 41, "event": "schedule", "created_at": "2026-11-05T05:20:00Z"}
        _, _, seen = self.run_main([qual("success")], [deploy(SHA)], 300, schedule_runs=[], any_runs=[old])
        self.assertEqual(self.dispatches(seen), [self.DISPATCH])

    def test_no_dispatch_when_the_slot_ran(self):
        _, _, seen = self.run_main([qual("success")], [deploy(SHA)], 300)
        self.assertEqual(self.dispatches(seen), [])

    def test_failed_dispatch_keeps_the_alarm_and_the_rerun(self):
        rc, out, seen = self.run_main([qual("failure")], [], 30, schedule_runs=[], dispatch_fails=True)
        self.assertEqual(rc, 1)
        self.assertIn("backup dispatch failed", out)
        self.assertEqual(self.reruns(seen), [("run", "rerun", "10", "--failed", "-R", REPO)])


class LiveTest(AlarmCase):
    """Run history cannot see a rollback done on C00, so the live revision is the only proof of what runs now."""

    def run_live(self, live, commits, now):
        rc, out, _ = self.run_main(commits=commits, now=now, argv=["--live"], live=live)
        return rc, out

    def test_live_commit_is_read_from_api_config(self):
        resp = MagicMock(); resp.read.return_value = json.dumps({"server_commit": "d43d0c9"}).encode()
        resp.__enter__.return_value = resp
        with patch.object(fresh.urllib.request, "urlopen", return_value=resp) as op:
            self.assertEqual(fresh.live_commit("https://c00.test:19444/"), "d43d0c9")
        self.assertEqual(op.call_args.args[0], "https://c00.test:19444/api/config")

    def test_unreachable_endpoint_reads_as_none(self):
        with patch.object(fresh.urllib.request, "urlopen", side_effect=OSError("timed out")):
            self.assertIsNone(fresh.live_commit("https://c00.test:19444"))

    def test_running_the_head_is_silent(self):
        self.assertEqual(self.run_live(SHA[:7], [commit_at(SHA, S - 600 * M)], NOW), (0, "main %s runs on C00 (live revision %s).\n" % (SHA[:9], SHA[:7])))

    def test_r7_an_older_live_revision_inside_the_slot_window_is_green(self):
        # live runs OLD; the head merged at S + 5 and the next slot is S2, so the deadline is S2 + 90, not S + 95.
        rc, out = self.run_live(OLD[:7], [commit_at(SHA, S + 5 * M), commit_at(OLD, S - 600 * M)], S + 125 * M)
        self.assertEqual(rc, 0, out)
        self.assertNotIn("::error::", out)

    def test_r7_an_older_live_revision_after_the_slot_deadline_is_red(self):
        rc, out = self.run_live(OLD[:7], [commit_at(SHA, S + 5 * M), commit_at(OLD, S - 600 * M)], S2 + 90 * M)
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)
        self.assertIn(OLD[:7], out)

    def test_r7_rollback_control_alerts(self):
        # C00 rolled back to d43d0c9 a day after the head merged
        older = commit_at("d43d0c9" + "0" * 33, S - 3000 * M)
        rc, out = self.run_live("d43d0c9", [commit_at(SHA, S - 1440 * M), older], S + 120 * M)
        self.assertEqual(rc, 1)
        self.assertIn("d43d0c9", out)

    def test_live_alerts_on_the_oldest_undeployed_commit_not_the_newest(self):
        a = "a" * 40
        commits = [commit_at(SHA, S2 - 10 * M), commit_at(a, S - 60 * M), commit_at("d43d0c9" + "0" * 33, S - 300 * M)]
        rc, out = self.run_live("d43d0c9", commits, S + 100 * M)
        self.assertEqual(rc, 1)
        self.assertIn(a[:9], out)

    def test_fp03_unreadable_revision_after_an_older_deadline_stays_red_when_a_fresh_merge_lands(self):
        # A merged at S - 60 (deadline S + 90). C00 cannot be read. A fresh merge lands at S + 100 (deadline S2 + 90).
        # The head's window must not hide A's missed deadline.
        a = "a" * 40
        commits = [commit_at(SHA, S + 100 * M), commit_at(a, S - 60 * M)]
        rc, out = self.run_live(None, commits, S + 105 * M)
        self.assertEqual(rc, 1)
        self.assertIn("unreadable", out)
        self.assertIn(a[:9], out)

    def test_unreadable_revision_inside_the_window_is_green(self):
        rc, out = self.run_live(None, [commit_at(SHA, S - 60 * M)], S + 80 * M)
        self.assertEqual(rc, 0)
        self.assertIn("unreadable", out)


class MissedSlotTest(unittest.TestCase):
    """cd-deploy.yml slots: 01:17, 05:17, 09:17, 13:17, 17:17, 21:17 UTC (08:17 ... 04:17 ICT). CHE-5182 reopened."""

    def missed(self, now, runs):
        return fresh.missed_slot(now, runs)

    def test_slot_passed_over_30_minutes_with_no_schedule_run_is_missed(self):
        self.assertEqual(self.missed(at(13, 48), []), at(13, 17))

    def test_inside_the_30_minute_grace_nothing_is_missed(self):
        self.assertIsNone(self.missed(at(13, 46), [sched("2026-11-05T09:20:00Z")]))

    def test_a_run_created_since_the_slot_clears_it(self):
        self.assertIsNone(self.missed(at(13, 48), [sched("2026-11-05T13:22:00Z")]))

    def test_a_run_from_the_previous_slot_does_not_clear_the_new_one(self):
        self.assertEqual(self.missed(at(13, 48), [sched("2026-11-05T09:20:00Z")]), at(13, 17))

    def test_slot_before_midnight_is_found_after_midnight(self):
        self.assertEqual(self.missed(at(21, 50, day=5), []), at(21, 17))
        self.assertEqual(self.missed(at(0, 30, day=6), []), at(21, 17, day=5))

    def test_off_slot_hours_use_the_latest_slot(self):
        self.assertEqual(self.missed(at(3, 0), []), at(1, 17))


class WorkflowFileTest(unittest.TestCase):
    def read(self, name):
        return (Path(__file__).parents[2] / ".github/workflows" / name).read_text()

    def test_history_alarm_stays_off_x99(self):
        text = self.read("cd-freshness-watch.yml")
        self.assertIn("runs-on: ubuntu-24.04", text)
        self.assertNotIn("self-hosted", text)  # independent of X99 and C00 runners
        self.assertNotIn("cheese-x99", text)
        self.assertIn("actions: write", text)
        self.assertIn("cron:", text)
        self.assertNotIn("workflow_dispatch", text)
        self.assertIn("github.event_name != 'pull_request' && github.ref == 'refs/heads/main'", text)
        self.assertIn("python3 deploy/cd/test-check-deploy-freshness.py", text)
        self.assertIn("python3 deploy/cd/check-deploy-freshness.py", text)

    def test_live_revision_job_is_schedule_only_on_x99(self):
        # CHE-3634 security review: the tailnet-connected X99 job lives in its own file with `schedule` as the only
        # trigger, so a branch or a pull request cannot start it from this file (a schedule runs the default branch only).
        # An if: in branch YAML is not an admission control; runner-group admission is org-owner config (CHE-3644).
        text = self.read("cd-live-revision.yml")
        on = text.split("\njobs:")[0]
        self.assertIn("schedule:", on)
        for trigger in ("pull_request", "workflow_dispatch", "push", "workflow_run", "issue_comment"):
            self.assertNotIn(trigger + ":", on)
        self.assertIn("cheese-x99", text)
        self.assertIn("check-deploy-freshness.py --live", text)
        self.assertIn("permissions:\n  contents: read\n  actions: read", text)
        self.assertNotIn("actions: write", text)

    def test_r10_step_labels_state_the_slot_deadline(self):
        self.assertIn("- name: Rerun a failed main build once; alarm when main misses its deploy slot + 90 min\n", self.read("cd-freshness-watch.yml"))
        self.assertIn("- name: Alarm when C00 does not run main's head 90 min after its deploy slot\n", self.read("cd-live-revision.yml"))


if __name__ == "__main__":
    unittest.main()
