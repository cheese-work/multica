#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import datetime as dt
import unittest
from unittest.mock import patch
import contextlib
import io
import json

spec = importlib.util.spec_from_file_location("queue", Path(__file__).with_name("check-deploy-queue.py"))
assert spec is not None and spec.loader is not None
queue = importlib.util.module_from_spec(spec)
spec.loader.exec_module(queue)


class QueueAlarmTest(unittest.TestCase):
    def test_queue_states_and_exact_threshold(self):
        now = dt.datetime(2026, 9, 27, 12, tzinfo=dt.timezone.utc)
        for status in ("queued", "pending", "waiting", "in_progress", "completed"):
            for minutes in (29, 30, 1080):
                run = {"status": status, "created_at": (now - dt.timedelta(minutes=minutes)).isoformat()}
                self.assertEqual(bool(queue.stalled_runs([run], now)),
                                 status in ("queued", "pending", "waiting") and minutes >= 30)
        self.assertEqual(queue.stalled_runs([], now), [])

    def test_invalid_timestamp_fails_loudly(self):
        with self.assertRaises(ValueError):
            queue.stalled_runs([{"status": "queued", "created_at": "invalid"}], dt.datetime.now(dt.timezone.utc))

    def test_main_reads_every_state_and_page_and_deduplicates(self):
        run = {"id": 1, "run_attempt": 2, "status": "queued", "created_at": "2026-01-01T00:00:00Z", "html_url": "https://example.test/run/1"}
        job = dict(run, name="deploy")
        output = io.StringIO()
        with patch.dict(queue.os.environ, {"GITHUB_REPOSITORY": "cheese-work/multica"}), \
                patch.object(queue.subprocess, "check_output", side_effect=[json.dumps(run) + "\n" + json.dumps(run), "", "", "", json.dumps(job)]) as api, \
                contextlib.redirect_stdout(output):
            self.assertEqual(queue.main(), 1)
        self.assertEqual(api.call_count, 5)
        self.assertEqual(output.getvalue().count("::error::"), 1)
        for call, status in zip(api.call_args_list, ("queued", "pending", "waiting", "in_progress")):
            self.assertIn("--paginate", call.args[0])
            self.assertIn(f"status={status}", call.args[0][-1])
        self.assertIn("/attempts/2/jobs", api.call_args_list[-1].args[0][-1])

    def check_attempt(self, attempt, queued_at, old_job=False):
        run = {"id": 1, "run_attempt": attempt, "created_at": "2026-01-01T00:00:00Z"}
        job = {"name": "deploy", "run_attempt": attempt, "status": "queued",
               "created_at": queued_at, "html_url": "https://example.test/job/1"}
        jobs = [job]
        if old_job:
            jobs.append(dict(job, run_attempt=1, created_at="2026-01-01T00:00:00Z"))
        with patch.dict(queue.os.environ, {"GITHUB_REPOSITORY": "cheese-work/multica"}), \
                patch.object(queue.subprocess, "check_output", side_effect=[json.dumps(run), "", "", "", "\n".join(map(json.dumps, jobs))]), \
                contextlib.redirect_stdout(io.StringIO()):
            return queue.main()

    def test_one_minute_old_rerun_does_not_alarm(self):
        queued_at = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(minutes=1)).isoformat()
        self.assertEqual(self.check_attempt(2, queued_at, old_job=True), 0)

    def test_new_deploy_after_long_preparation_does_not_alarm(self):
        queued_at = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(minutes=1)).isoformat()
        self.assertEqual(self.check_attempt(1, queued_at), 0)

    def test_incident_deploy_still_alarms(self):
        self.assertEqual(self.check_attempt(1, "2026-09-26T17:43:04Z"), 1)

    def test_started_at_used_when_created_at_absent(self):
        now = dt.datetime(2026, 9, 27, 12, tzinfo=dt.timezone.utc)
        job = {"status": "queued", "created_at": None, "started_at": "2026-09-27T11:30:00Z"}
        self.assertEqual(queue.stalled_runs([job], now), [job])

    def test_api_failure_is_not_a_healthy_queue(self):
        with patch.dict(queue.os.environ, {"GITHUB_REPOSITORY": "cheese-work/multica"}), \
                patch.object(queue.subprocess, "check_output", side_effect=queue.subprocess.CalledProcessError(1, "gh")):
            with self.assertRaises(queue.subprocess.CalledProcessError):
                queue.main()

    def test_alarm_independent_of_deploy_capacity(self):
        workflow = (Path(__file__).parents[2] / ".github/workflows/cd-queue-watch.yml").read_text()
        self.assertIn("runs-on: ubuntu-latest", workflow)
        self.assertNotIn("concurrency:", workflow)
        self.assertNotIn("actions: write", workflow)
        self.assertIn("github.event_name != 'pull_request'", workflow)


if __name__ == "__main__":
    unittest.main()
