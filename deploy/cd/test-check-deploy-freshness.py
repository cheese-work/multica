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


class MainTest(unittest.TestCase):
    def run_main(self, qual_runs, deploy_runs, minutes_ago, deploy_job="success", log="2026-11-05T10:01:02Z ok\n2026-11-05T10:01:03Z ##[error]buildx failed: boom\n"):
        commit = {"sha": SHA, "commit": {"committer": {"date": head_at(minutes_ago).isoformat()}}}
        jobs = [{"id": 7, "name": "build-images", "conclusion": "failure"}, {"id": 8, "name": "cd-qualification", "conclusion": "skipped"}]
        seen = []

        def fake(*args):
            seen.append(args)
            path = args[-1]
            if args[:2] == ("run", "rerun"):
                return ""
            if "/commits/main" in path:
                return json.dumps(commit)
            if "workflows/cd-qualification.yml/runs" in path:
                return json.dumps({"workflow_runs": qual_runs})
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


class WorkflowFileTest(unittest.TestCase):
    def test_workflow_shape(self):
        text = (Path(__file__).parents[2] / ".github/workflows/cd-freshness-watch.yml").read_text()
        job = text.split("live-revision:")
        self.assertIn("runs-on: ubuntu-24.04", job[0])
        self.assertNotIn("self-hosted", job[0])  # the history alarm stays independent of X99 and C00 runners
        self.assertIn("cheese-x99", job[1])  # tailnet reach for the live revision
        self.assertIn("check-deploy-freshness.py --live", job[1])
        self.assertIn("actions: write", text)
        self.assertIn("cron:", text)
        self.assertIn("github.event_name != 'pull_request'", text)
        # CHE-3634 review P1: a repo writer can workflow_dispatch any branch. The tailnet-connected X99 job and the
        # write-token step must only run main's reviewed code, and the X99 job gets a read-only token.
        # Round 3: the guard lives in branch YAML, so also add no manual path: only the schedule (default branch) or a
        # PR (job skipped) can start this file. Runner-group admission is org-owner config, tracked separately.
        self.assertNotIn("workflow_dispatch", text)
        main_only = "github.ref == 'refs/heads/main'"
        live = job[1].split("steps:")[0]
        self.assertIn(main_only, live)
        self.assertIn("permissions:\n      contents: read\n      actions: read", live)
        self.assertNotIn("actions: write", job[1])
        rerun_step = text.split("Rerun a failed main build once")[1].split("run:")[0]
        self.assertIn(main_only, rerun_step)
        self.assertIn("python3 deploy/cd/test-check-deploy-freshness.py", text)
        self.assertIn("python3 deploy/cd/check-deploy-freshness.py", text)


if __name__ == "__main__":
    unittest.main()
