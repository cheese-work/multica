"""CHE-4655: a new `Security-Gate-Verdict:` comment no longer starts a gate run; the 30 min sweep evaluates it.

Cheese chose the batch sweep on the CHE-4655 decision card (2ef385ab). It reverses the restored `created` trigger of
CHE-4385: a verdict takes effect up to 30 min late, in return for about 284 fewer `issue_comment` runs a day.
"""

from __future__ import annotations

import importlib.util
import json
import unittest
from pathlib import Path
from unittest import mock

import yaml

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("sweep_targets", ROOT / "scripts/security-gate/sweep_targets.py")
st = importlib.util.module_from_spec(spec)
spec.loader.exec_module(st)

H1, H2 = "a" * 40, "b" * 40
VERDICT = "Security-Gate-Verdict: PASS\n```json\n{}\n```"


def pr(n, head):
    return {"number": n, "head": head}


class SweepTargets(unittest.TestCase):
    def test_pending_head_with_a_newer_verdict_is_selected(self):
        got = st.targets([pr(1, H1)], {H1: {"state": "pending", "updated_at": "2026-11-09T01:00:00Z"}},
                         {1: [{"body": VERDICT, "updated_at": "2026-11-09T02:00:00Z"}]})
        self.assertEqual(got, [1])

    def test_green_head_with_a_newer_fail_verdict_is_selected(self):
        got = st.targets([pr(1, H1)], {H1: {"state": "success", "updated_at": "2026-11-09T01:00:00Z"}},
                         {1: [{"body": VERDICT, "updated_at": "2026-11-09T01:30:00Z"}]})
        self.assertEqual(got, [1])

    def test_head_without_any_status_and_with_a_verdict_is_selected(self):
        self.assertEqual(st.targets([pr(1, H1)], {}, {1: [{"body": VERDICT, "updated_at": "2026-11-09T01:00:00Z"}]}), [1])

    def test_verdict_already_older_than_the_status_is_not_reselected(self):
        got = st.targets([pr(1, H1)], {H1: {"state": "success", "updated_at": "2026-11-09T02:00:00Z"}},
                         {1: [{"body": VERDICT, "updated_at": "2026-11-09T01:00:00Z"}]})
        self.assertEqual(got, [])

    def test_verdict_posted_just_before_the_status_is_still_selected(self):
        # CHE-4389 race: a verdict can land after a run read the comments and before it wrote the status.
        got = st.targets([pr(1, H1)], {H1: {"state": "success", "updated_at": "2026-11-09T01:05:00Z"}},
                         {1: [{"body": VERDICT, "updated_at": "2026-11-09T01:00:00Z"}]})
        self.assertEqual(got, [1])

    def test_comment_without_the_verdict_header_is_ignored(self):
        got = st.targets([pr(1, H1)], {}, {1: [{"body": "looks fine", "updated_at": "2026-11-09T02:00:00Z"}]})
        self.assertEqual(got, [])

    def test_pr_without_comments_is_ignored(self):
        self.assertEqual(st.targets([pr(1, H1)], {}, {}), [])

    def test_result_is_sorted_and_unique(self):
        c = {"body": VERDICT, "updated_at": "2026-11-09T02:00:00Z"}
        self.assertEqual(st.targets([pr(5, H1), pr(2, H2)], {}, {5: [c, c], 2: [c]}), [2, 5])


class SweepListing(unittest.TestCase):
    def test_list_reads_open_prs_statuses_and_comments_through_gh(self):
        calls = []

        def fake_gh(*args):
            calls.append(args)
            path = args[-3] if "--jq" in args else args[-1]
            if "pulls?" in args[-3]:
                return '{"number": 7, "head": "%s"}\n' % H1
            if "/status" in args[-3]:
                return json.dumps({"state": "pending", "updated_at": "2026-11-09T01:00:00Z"})
            return json.dumps({"body": VERDICT, "updated_at": "2026-11-09T02:00:00Z"}) + "\n"

        with mock.patch.object(st, "_gh", fake_gh):
            self.assertEqual(st.list_targets("o/r", "main"), [7])
        self.assertTrue(any("base=main" in a[-3] for a in calls))


class WorkflowShape(unittest.TestCase):
    def setUp(self):
        self.gate = yaml.safe_load((ROOT / ".github/workflows/security-gate.yml").read_text())
        self.sweep_text = (ROOT / ".github/workflows/security-gate-sweep.yml").read_text()
        self.sweep = yaml.safe_load(self.sweep_text)

    def test_a_new_comment_starts_no_gate_run_but_edit_and_delete_do(self):
        self.assertEqual(self.gate[True]["issue_comment"]["types"], ["edited", "deleted"])

    def test_the_sweep_still_runs_every_30_minutes_and_by_hand(self):
        self.assertEqual(self.sweep[True]["schedule"], [{"cron": "*/30 * * * *"}])
        self.assertIn("workflow_dispatch", self.sweep[True])

    def test_the_list_job_selects_fresh_verdict_prs_from_the_default_branch_checkout(self):
        steps = self.sweep["jobs"]["list"]["steps"]
        self.assertEqual(steps[0]["uses"].split("@")[0], "actions/checkout")
        self.assertEqual(steps[0]["with"]["ref"], "${{ github.event.repository.default_branch }}")
        self.assertIs(steps[0]["with"]["persist-credentials"], False)
        fresh = [s for s in steps if "sweep_targets.py" in str(s.get("run", ""))]
        self.assertEqual(len(fresh), 1)
        self.assertEqual(fresh[0]["id"], "fresh")

    def test_the_sweep_matrix_covers_green_and_fresh_verdict_prs(self):
        jobs = self.sweep["jobs"]
        self.assertEqual(jobs["list"]["outputs"]["sweep"], "${{ steps.list.outputs.sweep }}")
        self.assertIn("needs.list.outputs.sweep != '[]'", jobs["sweep"]["if"])
        self.assertEqual(jobs["sweep"]["strategy"]["matrix"]["pr"], "${{ fromJSON(needs.list.outputs.sweep) }}")

    def test_the_sweep_keeps_the_gate_trust_boundary(self):
        self.assertNotIn("pull_request.head", self.sweep_text)
        self.assertNotIn("secrets.", self.sweep_text)
        self.assertEqual(self.sweep["permissions"], {"contents": "read", "pull-requests": "read", "statuses": "write"})


if __name__ == "__main__":
    unittest.main()
