#!/usr/bin/env python3
import contextlib
import datetime as dt
import importlib.util
import io
import json
import unittest
from pathlib import Path
from unittest.mock import patch

HERE = Path(__file__).parent
spec = importlib.util.spec_from_file_location("watch", HERE / "merge-queue-watch.py")
assert spec is not None and spec.loader is not None
watch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(watch)

NOW = dt.datetime(2026, 11, 3, 6, tzinfo=dt.timezone.utc)
REQUIRED = ["security-gate"]


def fixture(name):
    return json.loads((HERE / "fixtures" / "merge-queue-watch" / f"{name}.json").read_text())


class FindStallsTest(unittest.TestCase):
    def test_healthy_queue_passes(self):
        self.assertEqual(watch.find_stalls(fixture("healthy"), REQUIRED, NOW), [])

    def test_empty_or_absent_queue_passes(self):
        self.assertEqual(watch.find_stalls(fixture("empty"), REQUIRED, NOW), [])
        self.assertEqual(watch.find_stalls(fixture("no-queue"), REQUIRED, NOW), [])

    def test_entry_awaiting_checks_over_30_minutes_fails(self):
        stalls = watch.find_stalls(fixture("stuck-awaiting-checks"), REQUIRED, NOW)
        self.assertEqual(len(stalls), 1)
        self.assertIn("AWAITING_CHECKS", stalls[0])
        self.assertIn("pull/101", stalls[0])

    def test_threshold_is_exactly_30_minutes_from_the_head_commit(self):
        response = fixture("stuck-awaiting-checks")
        head = response["data"]["repository"]["mergeQueue"]["entries"]["nodes"][0]["headCommit"]
        committed = dt.datetime.fromisoformat(head["committedDate"].replace("Z", "+00:00"))
        self.assertEqual(len(watch.find_stalls(response, REQUIRED, committed + dt.timedelta(minutes=30))), 1)
        self.assertEqual(watch.find_stalls(response, REQUIRED, committed + dt.timedelta(minutes=29, seconds=59)), [])

    def test_long_queued_entry_with_a_new_head_passes(self):
        # enqueuedAt is 3 h old, but the merge-group head and the checks wait are 5 min old.
        self.assertEqual(watch.find_stalls(fixture("long-queued-new-head"), REQUIRED, NOW), [])

    def test_long_queued_entry_without_a_head_passes(self):
        self.assertEqual(watch.find_stalls(fixture("long-queued-no-head"), REQUIRED, NOW), [])

    def test_headless_awaiting_checks_beyond_30_minutes_is_inconclusive_not_green(self):
        stalls = watch.find_stalls(fixture("headless-awaiting-checks"), REQUIRED, NOW)
        self.assertEqual(len(stalls), 1)
        self.assertIn("inconclusive", stalls[0])
        self.assertIn("AWAITING_CHECKS", stalls[0])

    def test_headless_awaiting_checks_younger_than_30_minutes_passes(self):
        self.assertEqual(watch.find_stalls(fixture("headless-awaiting-checks-young"), REQUIRED, NOW), [])

    def test_required_check_missing_on_old_head_fails(self):
        stalls = watch.find_stalls(fixture("missing-required-check"), REQUIRED, NOW)
        self.assertEqual(len(stalls), 1)
        self.assertIn("security-gate", stalls[0])
        self.assertIn("c1", stalls[0])

    def test_required_check_missing_on_young_head_passes(self):
        self.assertEqual(watch.find_stalls(fixture("missing-check-too-young"), REQUIRED, NOW), [])

    def test_commit_status_satisfies_required_check(self):
        self.assertEqual(watch.find_stalls(fixture("status-context-satisfies"), REQUIRED, NOW), [])

    def test_truncated_rollup_is_inconclusive_not_green(self):
        stalls = watch.find_stalls(fixture("rollup-truncated"), REQUIRED, NOW)
        self.assertEqual(len(stalls), 1)
        self.assertIn("inconclusive", stalls[0])

    def test_truncated_queue_is_inconclusive_not_green(self):
        stalls = watch.find_stalls(fixture("queue-truncated"), REQUIRED, NOW)
        self.assertEqual(len(stalls), 1)
        self.assertIn("inconclusive", stalls[0])

    def test_invalid_timestamp_fails_loudly(self):
        response = fixture("stuck-awaiting-checks")
        response["data"]["repository"]["mergeQueue"]["entries"]["nodes"][0]["headCommit"]["committedDate"] = "invalid"
        with self.assertRaises(ValueError):
            watch.find_stalls(response, REQUIRED, NOW)


class MainTest(unittest.TestCase):
    def run_main(self, name, required=REQUIRED):
        rules = [{"type": "deletion"},
                 {"type": "required_status_checks", "parameters": {"required_status_checks": [{"context": c} for c in required]}}]
        output = io.StringIO()
        with patch.dict(watch.os.environ, {"GITHUB_REPOSITORY": "cheese-work/multica"}), \
                patch.object(watch, "now", return_value=NOW), \
                patch.object(watch.subprocess, "check_output", side_effect=[json.dumps(fixture(name)), json.dumps(rules)]) as api, \
                contextlib.redirect_stdout(output):
            code = watch.main()
        return code, output.getvalue(), api

    def test_stuck_queue_exits_red_with_annotation(self):
        code, out, _ = self.run_main("stuck-awaiting-checks")
        self.assertEqual(code, 1)
        self.assertIn("::error::", out)

    def test_healthy_queue_exits_green(self):
        self.assertEqual(self.run_main("healthy")[0], 0)

    def test_reads_graphql_queue_and_branch_rules(self):
        _, _, api = self.run_main("healthy")
        graphql, rules = (call.args[0] for call in api.call_args_list)
        self.assertIn("graphql", graphql)
        self.assertTrue(any("mergeQueue" in part for part in graphql))
        self.assertIn("repos/cheese-work/multica/rules/branches/main", rules)

    def test_repo_without_required_checks_still_alarms_on_age(self):
        self.assertEqual(self.run_main("stuck-awaiting-checks", required=[])[0], 1)
        self.assertEqual(self.run_main("missing-required-check", required=[])[0], 0)

    def test_api_failure_is_not_a_healthy_queue(self):
        with patch.dict(watch.os.environ, {"GITHUB_REPOSITORY": "cheese-work/multica"}), \
                patch.object(watch.subprocess, "check_output", side_effect=watch.subprocess.CalledProcessError(1, "gh")):
            with self.assertRaises(watch.subprocess.CalledProcessError):
                watch.main()


if __name__ == "__main__":
    unittest.main()
