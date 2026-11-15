#!/usr/bin/env python3
"""Fixture tests for the C00 deploy freshness alarm (CHE-3634): rerun-once, 90-minute and rate-limit rules."""
import contextlib
import datetime as dt
import importlib.util
import io
import json
import subprocess
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

spec = importlib.util.spec_from_file_location("fresh", Path(__file__).with_name("check-deploy-freshness.py"))
assert spec is not None and spec.loader is not None
fresh = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fresh)

SHA = "d2593932fc23ade672b8d1962041ef9086762473"
NOW = dt.datetime(2026, 11, 5, 12, tzinfo=dt.timezone.utc)


def head_at(minutes_ago):
    return NOW - dt.timedelta(minutes=minutes_ago)


def qual(conclusion, attempt=1, run_id=10, created="2026-11-05T10:00:00Z"):
    return {"id": run_id, "head_sha": SHA, "conclusion": conclusion, "status": "completed" if conclusion else "in_progress",
            "run_attempt": attempt, "created_at": created, "html_url": f"https://example.test/run/{run_id}"}


def deploy(conclusion, sha=SHA, job="success"):
    # workflow_run deploys carry main's tip in head_sha; the qualified SHA is only in the run name (CHE-3634 review).
    # `job` is the conclusion of the run's `deploy` job: a run can succeed with that job skipped (ineligible preflight).
    return {"id": 20, "head_sha": SHA, "display_title": f"CD deploy to C00 · {sha}", "conclusion": conclusion, "status": "completed", "_job": job}


def ran(run):
    return run["_job"] == "success"


def decide(*args):
    return fresh.decide(*args, ran)


class DecideTest(unittest.TestCase):
    def test_failed_first_attempt_is_rerun_once(self):
        self.assertEqual(decide(SHA, head_at(10), NOW, [qual("failure")], [])["rerun"], 10)

    def test_second_attempt_failure_is_not_rerun_again(self):
        self.assertIsNone(decide(SHA, head_at(10), NOW, [qual("failure", attempt=2)], [])["rerun"])

    def test_only_the_newest_qualification_run_counts(self):
        old = qual("failure", run_id=1, created="2026-11-05T09:00:00Z")
        new = qual("success", run_id=2, created="2026-11-05T10:00:00Z")
        self.assertIsNone(decide(SHA, head_at(10), NOW, [old, new], [])["rerun"])
        self.assertIsNone(decide(SHA, head_at(10), NOW, [new, old], [])["rerun"])

    def test_success_running_and_cancelled_are_not_rerun(self):
        for conclusion in ("success", None, "cancelled", "skipped"):
            self.assertIsNone(decide(SHA, head_at(10), NOW, [qual(conclusion)], [])["rerun"], conclusion)

    def test_alert_exactly_at_90_minutes(self):
        self.assertFalse(decide(SHA, head_at(89), NOW, [qual("success")], [])["alert"])
        self.assertTrue(decide(SHA, head_at(90), NOW, [qual("success")], [])["alert"])

    def test_successful_deploy_of_head_clears_the_alarm(self):
        self.assertFalse(decide(SHA, head_at(600), NOW, [qual("success")], [deploy("success")])["alert"])

    def test_skipped_failed_other_sha_or_job_not_run_deploy_is_not_deployed(self):
        for runs in ([deploy("skipped")], [deploy("failure")], [deploy("success", sha="0" * 40)], [deploy("success", sha="07ce9f73b918f20d829c906fcb37db56e213e129")],
                     [deploy("success", job="skipped")], [deploy("success", job="failure")], [deploy("success", job=None)]):
            self.assertTrue(decide(SHA, head_at(600), NOW, [qual("success")], runs)["alert"], runs)

    def test_no_qualification_run_at_all_alerts_after_90_minutes(self):
        self.assertTrue(decide(SHA, head_at(91), NOW, [], [])["alert"])
        self.assertFalse(decide(SHA, head_at(5), NOW, [], [])["alert"])


class LiveTest(unittest.TestCase):
    """Run history cannot see a rollback done on C00, so the live revision is the only proof of what runs now."""

    def test_running_the_head_clears_the_alarm(self):
        self.assertEqual(fresh.decide_live(SHA, head_at(600), NOW, SHA[:7]), {"alert": False, "deployed": True})

    def test_rolled_back_to_an_older_commit_alerts_after_90_minutes(self):
        self.assertTrue(fresh.decide_live(SHA, head_at(120), NOW, "d43d0c9")["alert"])
        self.assertFalse(fresh.decide_live(SHA, head_at(89), NOW, "d43d0c9")["alert"])

    def test_unreadable_live_revision_alerts_after_90_minutes(self):
        self.assertTrue(fresh.decide_live(SHA, head_at(120), NOW, None)["alert"])
        self.assertFalse(fresh.decide_live(SHA, head_at(10), NOW, None)["alert"])

    def test_live_commit_is_read_from_api_config_and_compared_with_the_short_head(self):
        resp = MagicMock(); resp.read.return_value = json.dumps({"server_commit": "d43d0c9"}).encode()
        resp.__enter__.return_value = resp
        with patch.object(fresh.urllib.request, "urlopen", return_value=resp) as op:
            self.assertEqual(fresh.live_commit("https://c00.test:19444/"), "d43d0c9")
        self.assertEqual(op.call_args.args[0], "https://c00.test:19444/api/config")

    def test_unreachable_endpoint_reads_as_none(self):
        with patch.object(fresh.urllib.request, "urlopen", side_effect=OSError("timed out")):
            self.assertIsNone(fresh.live_commit("https://c00.test:19444"))

    def run_live(self, live, minutes_ago):
        commit = {"sha": SHA, "commit": {"committer": {"date": head_at(minutes_ago).isoformat()}}}
        out = io.StringIO()
        with patch.dict(fresh.os.environ, {"GITHUB_REPOSITORY": "cheese-work/multica", "C00_API_URL": "https://c00.test"}), \
                patch.object(fresh, "gh_api", return_value=json.dumps(commit)), patch.object(fresh, "utcnow", return_value=NOW), \
                patch.object(fresh, "live_commit", return_value=live), contextlib.redirect_stdout(out):
            rc = fresh.main(["--live"])
        return rc, out.getvalue()

    def test_main_live_rollback_after_a_successful_deploy_alerts(self):
        rc, out = self.run_live("d43d0c9", 120)
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)
        self.assertIn("d43d0c9", out)

    def test_main_live_healthy_is_silent(self):
        self.assertEqual(self.run_live(SHA[:7], 600), (0, "main %s runs on C00 (live revision %s).\n" % (SHA[:9], SHA[:7])))


class RateLimitTest(unittest.TestCase):
    def err(self, text):
        return subprocess.CalledProcessError(1, "gh", stderr=text)

    def test_rate_limit_waits_until_reset_then_retries(self):
        reset = int(NOW.timestamp()) + 120
        calls = [self.err("gh: API rate limit exceeded (HTTP 403)"), "42\n"]
        sleeps = []
        with patch.object(fresh, "_gh", side_effect=lambda *a: (_ for _ in ()).throw(calls.pop(0)) if isinstance(calls[0], Exception) else calls.pop(0)) as gh, \
                patch.object(fresh, "_reset_epoch", return_value=reset), patch.object(fresh.time, "time", return_value=NOW.timestamp()):
            self.assertEqual(fresh.gh_api("x", sleep=sleeps.append), "42\n")
        self.assertEqual(sleeps, [121])
        self.assertEqual(gh.call_count, 2)

    def test_429_is_also_a_rate_limit(self):
        calls = [self.err("HTTP 429: Too Many Requests"), "ok"]
        sleeps = []
        with patch.object(fresh, "_gh", side_effect=lambda *a: (_ for _ in ()).throw(calls.pop(0)) if isinstance(calls[0], Exception) else calls.pop(0)), \
                patch.object(fresh, "_reset_epoch", return_value=int(NOW.timestamp()) + 5), patch.object(fresh.time, "time", return_value=NOW.timestamp()):
            self.assertEqual(fresh.gh_api("x", sleep=sleeps.append), "ok")
        self.assertEqual(len(sleeps), 1)

    def test_other_errors_are_not_retried(self):
        with patch.object(fresh, "_gh", side_effect=self.err("HTTP 404: Not Found")) as gh:
            with self.assertRaises(subprocess.CalledProcessError):
                fresh.gh_api("x", sleep=lambda s: None)
        self.assertEqual(gh.call_count, 1)

    def test_wait_is_capped_and_gives_up_after_two_retries(self):
        sleeps = []
        with patch.object(fresh, "_gh", side_effect=self.err("API rate limit exceeded")) as gh, \
                patch.object(fresh, "_reset_epoch", return_value=int(NOW.timestamp()) + 99999), patch.object(fresh.time, "time", return_value=NOW.timestamp()):
            with self.assertRaises(subprocess.CalledProcessError):
                fresh.gh_api("x", sleep=sleeps.append)
        self.assertEqual(sleeps, [fresh.MAX_WAIT, fresh.MAX_WAIT])
        self.assertEqual(gh.call_count, 3)


def sched(created):
    return {"id": 30, "event": "schedule", "created_at": created, "html_url": "https://example.test/run/30"}


def at(h, m=0, day=5):
    return dt.datetime(2026, 11, day, h, m, tzinfo=dt.timezone.utc)


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


class MainTest(unittest.TestCase):
    def run_main(self, qual_runs, deploy_runs, minutes_ago, deploy_job="success", schedule_runs=(sched("2026-11-05T09:20:00Z"),), any_runs=(), dispatch_fails=False, log="2026-11-05T10:01:02Z ok\n2026-11-05T10:01:03Z ##[error]buildx failed: boom\n"):
        commit = {"sha": SHA, "commit": {"committer": {"date": head_at(minutes_ago).isoformat()}}}
        jobs = [{"id": 7, "name": "build-images", "conclusion": "failure"}, {"id": 8, "name": "cd-qualification", "conclusion": "skipped"}]
        seen = []

        def fake(*args):
            seen.append(args)
            path = args[-1]
            if args[:2] == ("workflow", "run") and dispatch_fails:
                raise subprocess.CalledProcessError(1, "gh", stderr="HTTP 403")
            if args[:2] in (("run", "rerun"), ("workflow", "run")):
                return ""
            if "/commits/main" in path:
                return json.dumps(commit)
            if "workflows/cd-qualification.yml/runs" in path:
                return json.dumps({"workflow_runs": qual_runs})
            if "workflows/cd-deploy.yml/runs" in path and "event=schedule" in path:
                return json.dumps({"workflow_runs": list(schedule_runs)})
            if "workflows/cd-deploy.yml/runs" in path and "branch=main" in path:
                return json.dumps({"workflow_runs": list(any_runs)})
            if "workflows/cd-deploy.yml/runs" in path:
                self.assertIn("status=success", path)
                return json.dumps({"workflow_runs": deploy_runs})
            if "/actions/runs/20/jobs" in path:
                return json.dumps({"jobs": [{"id": 9, "name": "deploy", "conclusion": deploy_job}]})
            if path.endswith("/jobs?per_page=100"):
                return json.dumps({"jobs": jobs})
            if path.endswith("/jobs/7/logs"):
                return log
            raise AssertionError(path)

        out = io.StringIO()
        with patch.dict(fresh.os.environ, {"GITHUB_REPOSITORY": "cheese-work/multica"}), \
                patch.object(fresh, "gh_api", side_effect=fake), patch.object(fresh, "utcnow", return_value=NOW), \
                contextlib.redirect_stdout(out):
            rc = fresh.main()
        return rc, out.getvalue(), seen

    def test_failed_build_is_rerun_once_and_not_yet_alerted(self):
        rc, out, seen = self.run_main([qual("failure")], [], 30)
        self.assertEqual(rc, 0)
        self.assertEqual([a for a in seen if a[:2] == ("run", "rerun")], [("run", "rerun", "10", "--failed", "-R", "cheese-work/multica")])

    def test_second_attempt_failure_after_90_minutes_alerts_with_run_url_and_first_error(self):
        rc, out, seen = self.run_main([qual("failure", attempt=2)], [], 95)
        self.assertEqual(rc, 1)
        self.assertEqual([a for a in seen if a[:2] == ("run", "rerun")], [])
        self.assertIn("::error::", out)
        self.assertIn("https://example.test/run/10", out)
        self.assertIn("buildx failed: boom", out)

    def test_healthy_deployed_head_is_silent(self):
        rc, out, seen = self.run_main([qual("success")], [deploy("success")], 300)
        self.assertEqual((rc, out), (0, "main %s is deployed to C00.\n" % SHA[:9]))

    def test_successful_run_with_skipped_deploy_job_still_alerts(self):
        rc, out, _ = self.run_main([qual("success")], [deploy("success")], 120, deploy_job="skipped")
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)

    def test_old_undeployed_head_with_no_error_line_still_alerts(self):
        rc, out, _ = self.run_main([qual("success")], [deploy("failure")], 120, log="nothing useful\n")
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)


    def test_missed_deploy_slot_alerts_even_when_main_is_deployed(self):
        rc, out, _ = self.run_main([qual("success")], [deploy("success")], 300, schedule_runs=[])
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)
        self.assertIn("09:17 UTC", out)
        self.assertIn("16:17 ICT", out)

    DISPATCH = ("workflow", "run", "cd-deploy.yml", "--ref", "main", "-f", "schedule_backup=true", "-R", "cheese-work/multica")

    @staticmethod
    def dispatches(seen):
        return [a for a in seen if a[:2] == ("workflow", "run")]

    def test_missed_slot_dispatches_cd_deploy_once_and_stays_red(self):
        rc, out, seen = self.run_main([qual("success")], [deploy("success")], 300, schedule_runs=[])
        self.assertEqual(self.dispatches(seen), [self.DISPATCH])
        self.assertEqual(rc, 1)
        self.assertIn("::error::cd-deploy.yml slot 09:17 UTC", out)
        self.assertIn("backup dispatch", out)

    def test_no_second_dispatch_when_a_cd_deploy_run_started_since_the_slot(self):
        # the run an earlier alarm dispatched (or a workflow_run deploy) is created after 09:17 UTC
        started = {"id": 40, "event": "workflow_dispatch", "created_at": "2026-11-05T09:50:00Z"}
        rc, out, seen = self.run_main([qual("success")], [deploy("success")], 300, schedule_runs=[], any_runs=[started])
        self.assertEqual(self.dispatches(seen), [])
        self.assertEqual(rc, 1)
        self.assertIn("::error::", out)

    def test_a_run_from_before_the_slot_does_not_block_the_dispatch(self):
        old = {"id": 41, "event": "schedule", "created_at": "2026-11-05T05:20:00Z"}
        _, _, seen = self.run_main([qual("success")], [deploy("success")], 300, schedule_runs=[], any_runs=[old])
        self.assertEqual(self.dispatches(seen), [self.DISPATCH])

    def test_no_dispatch_when_the_slot_ran(self):
        _, _, seen = self.run_main([qual("success")], [deploy("success")], 300)
        self.assertEqual(self.dispatches(seen), [])

    def test_failed_dispatch_keeps_the_alarm_and_the_rerun(self):
        rc, out, seen = self.run_main([qual("failure")], [], 30, schedule_runs=[], dispatch_fails=True)
        self.assertEqual(rc, 1)
        self.assertIn("backup dispatch failed", out)
        self.assertEqual([a for a in seen if a[:2] == ("run", "rerun")], [("run", "rerun", "10", "--failed", "-R", "cheese-work/multica")])


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


if __name__ == "__main__":
    unittest.main()
