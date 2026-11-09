"""CHE-4655: a new `Security-Gate-Verdict:` comment no longer starts a gate run; the 30 min sweep evaluates it.

Cheese chose the batch sweep on the CHE-4655 decision card (2ef385ab). It reverses the restored `created` trigger of
CHE-4385: a verdict takes effect up to 30 min late, in return for about 284 fewer `issue_comment` runs a day.
"""

from __future__ import annotations

import importlib.util
import json
import re
import subprocess
import unittest
from datetime import timedelta
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

    def test_verdict_posted_during_a_whole_20_minute_evaluation_is_still_selected(self):
        # Sol advisory on df9a311f1: the gate timeout is 20 min, so the margin must exceed it.
        got = st.targets([pr(1, H1)], {H1: {"state": "pending", "updated_at": "2026-11-09T01:25:00Z"}},
                         {1: [{"body": VERDICT, "updated_at": "2026-11-09T01:01:00Z"}]})
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


class QueueGate(unittest.TestCase):
    """Sol FAILs on df9a311f1 and 463bca589 (CWE-841): with no comment run, a FAIL posted after a queue entry went green
    reached it only at the next poll or sweep, so another required check could merge the rejected PR first. Both queue
    evaluators now post the green LAST: they hold it until every other required check on the queue commit is done, read the
    verdict again right before the post, and keep watching (cheap comment digest, full read on change) while the entry
    lives. A watch that expires, or a read that errors, fails closed."""

    FILES = [".github/workflows/security-gate.yml", ".github/workflows/security-gate-queue.yml"]

    def _step(self, rel):
        wf = yaml.safe_load((ROOT / rel).read_text())
        job = next(j for j in wf["jobs"].values() if "WATCH_SECONDS" in j.get("env", {}))
        run = next(s["run"] for s in job["steps"] if "queue-gate:begin" in s.get("run", ""))
        return job, run

    def _block(self, rel):
        run = self._step(rel)[1]
        return run[run.index("# queue-gate:begin"):run.index("# queue-gate:end")]

    def _drive(self, rel, verdicts, settled=("yes",), live=("refs/heads/q",), digests=("d",), required="backend\nsecurity-gate",
               watch=300, interval=15):
        """Run the gate block with stubbed `gh`, `sleep`, `read_verdict`, `post`; return the log. `settled` answers the
        required-check query per call (yes/no/err), `digests` the comment digest per call, `live` the queue ref list."""
        script = (
            "set -euo pipefail\nSECONDS=0\n"
            f"WATCH_SECONDS={watch} WATCH_INTERVAL={interval}\nGITHUB_REPOSITORY=o/r MERGE_GROUP_REF=refs/heads/q MERGE_GROUP_SHA=sha DEFAULT_BRANCH=main pr=1\n"
            "state=success desc=ok n=0\ncnt=$(mktemp -d)\n"
            # gh runs in a command substitution: a per-call answer needs a counter that survives the subshell
            'next() { local f="$cnt/$1" i=0; [ -f "$f" ] && i=$(<"$f"); echo $((i + 1)) > "$f"; echo "$i"; }\n'
            'sleep() { echo "SLEEP $1"; SECONDS=$((SECONDS + $1)); }\n'
            f"settled=({' '.join(settled)}) digests=({' '.join(digests)})\n"
            "gh() {\n"
            '  case "$*" in\n'
            f"    *rules/branches*) printf '{required}\\n' ;;\n"
            '    *check-runs*) i=$(next c); a="${settled[i]:-${settled[-1]}}"; [ "$a" = err ] && exit 1\n'
            '      [ "$a" = yes ] && printf "backend\\tdone\\n" || printf "backend\\topen\\n" ;;\n'
            "    *commits/*/status*) : ;;\n"
            f"    *matching-refs*) l=({' '.join(repr(x) for x in live)}); i=$(next l); a=\"${{l[i]:-${{l[-1]}}}}\"; [ \"$a\" = err ] && exit 1; printf '%s' \"$a\" ;;\n"
            '    *issues/*/comments*) i=$(next d); a="${digests[i]:-${digests[-1]}}"; [ "$a" = err ] && exit 1; echo "$a" ;;\n'
            "  esac\n}\n"
            f"vs=({' '.join(verdicts)})\n"
            'read_verdict() { state="${vs[n]:-success}"; desc="d$n"; echo "READ $state"; n=$((n + 1)); }\n'
            'post() { echo "POST $1 $2"; }\n' + self._block(rel) + "\nqueue_gate\n"
        )
        return subprocess.run(["bash", "-c", script], capture_output=True, text=True, check=True).stdout.splitlines()

    @staticmethod
    def _posts(log):
        return [l for l in log if l.startswith("POST")]

    def test_both_evaluators_carry_the_same_gate_block(self):
        a, b = (self._block(f) for f in self.FILES)
        self.assertEqual(a.split(), b.split())

    def test_the_watch_fits_the_job_timeout_and_a_pr_evaluation_stays_below_the_sweep_margin(self):
        for rel in self.FILES:
            job, _ = self._step(rel)
            self.assertLess(int(job["env"]["WATCH_SECONDS"]) + 300, 3600, rel)
        gate = yaml.safe_load((ROOT / self.FILES[0]).read_text())["jobs"]["security-gate"]
        timeout = gate["timeout-minutes"]
        self.assertIn("merge_group", str(timeout))
        pr_minutes = int(re.search(r"\|\|\s*(\d+)\s*\}\}", str(timeout)).group(1))
        self.assertLess(timedelta(minutes=pr_minutes), st.MARGIN)
        queue = yaml.safe_load((ROOT / self.FILES[1]).read_text())["jobs"]["security-gate-queue"]
        self.assertGreaterEqual(queue["timeout-minutes"] * 60, int(queue["env"]["WATCH_SECONDS"]) + 300)

    def test_green_is_held_until_every_other_required_check_is_done(self):
        log = self._drive(self.FILES[0], [], settled=["no", "no", "yes"], live=("refs/heads/q", "refs/heads/q", ""))
        self.assertEqual(self._posts(log), ["POST success d1"])
        self.assertEqual(log[:log.index("POST success d1")].count("SLEEP 15"), 2)

    def test_a_fail_while_the_green_is_held_is_posted_and_green_never_is(self):
        for rel in self.FILES:
            log = self._drive(rel, ["success", "failure"], settled=["no", "no"], digests=["a", "b"])
            self.assertEqual(self._posts(log), ["POST failure d1"], rel)

    def test_the_verdict_is_read_right_before_the_green_post_and_once_after(self):
        log = self._drive(self.FILES[0], [], settled=["yes"], live=("",))
        self.assertEqual(log[:4], ["READ success", "READ success", "POST success d1", "READ success"])

    def test_a_fail_between_the_last_read_and_the_post_is_not_published_as_green(self):
        for rel in self.FILES:
            log = self._drive(rel, ["success", "failure"], settled=["yes"])
            self.assertEqual(self._posts(log), ["POST failure d1"], rel)

    def test_a_fail_after_the_green_post_overwrites_it_within_one_poll_interval(self):
        for rel in self.FILES:
            log = self._drive(rel, ["success", "success", "success", "failure"], digests=["a", "a", "a", "b"],
                              live=("refs/heads/q", "refs/heads/q", "refs/heads/q", ""))
            self.assertEqual(self._posts(log), ["POST success d1", "POST failure d3"], rel)
            self.assertLessEqual(max(int(l.split()[1]) for l in log if l.startswith("SLEEP")), 15, rel)

    def test_an_unchanged_comment_digest_costs_no_verdict_read(self):
        log = self._drive(self.FILES[0], [], digests=["a"], live=("refs/heads/q",), watch=60)
        self.assertEqual(len([l for l in log if l.startswith("READ")]), 3, log)

    def test_a_digest_read_error_forces_a_verdict_read(self):
        log = self._drive(self.FILES[0], ["success", "success", "success", "failure"], digests=["a", "err"], watch=60)
        self.assertEqual(self._posts(log)[-1], "POST failure d3", log)

    def test_a_ref_listing_error_does_not_skip_the_evaluation(self):
        for rel in self.FILES:
            log = self._drive(rel, ["success", "success", "success", "failure"], digests=["a", "a", "a", "b"], live=("err",))
            self.assertEqual(self._posts(log), ["POST success d1", "POST failure d3"], rel)

    def test_the_watch_ends_when_the_queue_entry_is_gone(self):
        for rel in self.FILES:
            log = self._drive(rel, [], settled=["no"], live=("",))
            self.assertEqual(self._posts(log), [], rel)

    def test_a_required_check_query_error_holds_the_green_and_expiry_fails_closed(self):
        for rel in self.FILES:
            log = self._drive(rel, [], settled=["err"], watch=60)
            self.assertEqual([l.split(" ", 2)[1] for l in self._posts(log)], ["failure"], rel)

    def test_expiry_with_a_live_entry_revokes_the_green(self):
        for rel in self.FILES:
            log = self._drive(rel, [], settled=["yes"], watch=60)
            self.assertEqual([l.split(" ", 2)[1] for l in self._posts(log)], ["success", "failure"], rel)


if __name__ == "__main__":
    unittest.main()
