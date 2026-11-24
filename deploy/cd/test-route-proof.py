#!/usr/bin/env python3
"""Tests for deploy/cd/route-proof.py (CHE-9276, plan CHE-9208 step 2; FP-02, FP-03)."""
import contextlib
import copy
import datetime as dt
import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
FIX = HERE / "fixtures" / "route-proof"

spec = importlib.util.spec_from_file_location("route_proof", HERE / "route-proof.py")
assert spec is not None and spec.loader is not None
rp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rp)

OLD = ["self-hosted", "linux", "x64", "cheese-x99"]
EXPECTED = OLD + ["cheese-x99-homedisk"]
TARGET_TIME = dt.datetime(2026, 11, 22, 20, 0, tzinfo=dt.timezone.utc)

CANCELLED_ZERO_JOBS = 45069060254  # recorded: cancelled, 0 jobs
REPLACED = 45066486281  # recorded: replaced by the concurrency group, 0 jobs
WRONG_LABELS = 45114280355  # recorded: success, old selector
FAILED_RUN = 45060386606  # recorded run (legs rewritten per test)
GOOD = 45999999001  # synthetic: expected labels, both legs success
QUEUED = 45999999002  # synthetic: expected labels, still queued
DEADLINE = TARGET_TIME + dt.timedelta(hours=24)
AFTER_DEADLINE = DEADLINE + dt.timedelta(seconds=1)


def load(name):
    return json.loads((FIX / name).read_text())


def run(run_id):
    return load(f"run-{run_id}.json")


def jobs(run_id):
    return load(f"jobs/{run_id}.json")["jobs"]


def world(*run_ids):
    runs = sorted((run(i) for i in run_ids), key=lambda r: r["created_at"])
    return runs, {i: jobs(i) for i in run_ids}, {r["head_sha"] for r in runs}


def verdict(got):
    if not isinstance(got, dict):
        raise AssertionError(f"expected a verdict dict, got {got!r}")
    return got


def bind(runs, by_run, commits, **kw):
    kw.setdefault("now", AFTER_DEADLINE)  # never read the real clock: the fixtures are fixed in time
    return verdict(rp.bind_proof_run(runs, by_run, commits, TARGET_TIME, EXPECTED, **kw))


def triage(labels, runners):
    return verdict(rp.triage_queued_job(labels, runners))


class BindProofRun(unittest.TestCase):
    def test_fp02_skips_cancelled_replaced_and_jobless_runs(self):
        runs, by_run, commits = world(REPLACED, CANCELLED_ZERO_JOBS, WRONG_LABELS, GOOD)
        self.assertEqual(by_run[CANCELLED_ZERO_JOBS], [])
        self.assertEqual(run(CANCELLED_ZERO_JOBS)["conclusion"], "cancelled")
        got = bind(runs, by_run, commits)
        self.assertEqual(got["verdict"], "BOUND")
        self.assertEqual(got["run_id"], GOOD)

    def test_fp02_failed_or_missing_leg_is_not_proof(self):
        runs, by_run, commits = world(GOOD)
        one_failed = copy.deepcopy(by_run)
        one_failed[GOOD][1]["conclusion"] = "failure"
        self.assertEqual(bind(runs, one_failed, commits)["verdict"], "NOT-RUN")
        one_leg = copy.deepcopy(by_run)
        one_leg[GOOD] = one_leg[GOOD][:1]
        self.assertEqual(bind(runs, one_leg, commits)["verdict"], "NOT-RUN")
        # Same shape as the recorded failed run, labels made right: still not proof.
        failed_runs, failed_by_run, failed_commits = world(FAILED_RUN)
        for leg in failed_by_run[FAILED_RUN]:
            leg["labels"] = EXPECTED
        failed_by_run[FAILED_RUN][1]["conclusion"] = "failure"
        self.assertEqual(bind(failed_runs, failed_by_run, failed_commits)["verdict"], "NOT-RUN")

    def test_fp02_no_qualifying_run_in_24h_is_not_run(self):
        runs, by_run, commits = world(CANCELLED_ZERO_JOBS, REPLACED, WRONG_LABELS)
        got = bind(runs, by_run, commits)  # 24 h window, read one second after its end
        self.assertEqual(got["verdict"], "NOT-RUN")
        self.assertEqual(got["run_ids_read"], [REPLACED, CANCELLED_ZERO_JOBS, WRONG_LABELS])
        self.assertNotIn("run_id", got)

    def test_not_run_waits_for_the_24h_deadline(self):
        # Before the deadline an absent proof is PENDING, never NOT-RUN (the 2 h wakeup must not give up).
        none = bind([], {}, set(), now=TARGET_TIME + dt.timedelta(hours=2))
        self.assertEqual((none["verdict"], none["run_ids_read"]), ("PENDING", []))
        runs, by_run, commits = world(CANCELLED_ZERO_JOBS, QUEUED)  # QUEUED is unfinished
        self.assertEqual(by_run[QUEUED][0]["status"], "queued")
        early = bind(runs, by_run, commits, now=TARGET_TIME + dt.timedelta(hours=10))
        self.assertEqual(early["verdict"], "PENDING")
        self.assertEqual(early["run_ids_read"], [CANCELLED_ZERO_JOBS, QUEUED])
        self.assertEqual(early["open_run_ids"], [QUEUED])
        self.assertEqual(bind(runs, by_run, commits, now=DEADLINE)["verdict"], "PENDING")
        late = bind(runs, by_run, commits, now=AFTER_DEADLINE)  # the unfinished run is not proof either
        self.assertEqual(late["verdict"], "NOT-RUN")
        self.assertEqual(late["run_ids_read"], [CANCELLED_ZERO_JOBS, QUEUED])
        # A proof beats the clock: BOUND even when read early.
        runs, by_run, commits = world(GOOD)
        self.assertEqual(bind(runs, by_run, commits, now=TARGET_TIME + dt.timedelta(hours=10))["verdict"], "BOUND")

    def test_runs_created_after_the_window_are_not_read(self):
        runs, by_run, commits = world(CANCELLED_ZERO_JOBS, WRONG_LABELS, GOOD)
        got = bind(runs, by_run, commits, window_hours=9)  # GOOD (05:30 on 23 Nov) is created after 9 h
        self.assertEqual(got["verdict"], "NOT-RUN")
        self.assertEqual(got["run_ids_read"], [CANCELLED_ZERO_JOBS, WRONG_LABELS])

    def test_runs_before_the_target_never_bind(self):
        runs, by_run, _ = world(GOOD)
        self.assertEqual(bind(runs, by_run, {"b" * 40})["verdict"], "NOT-RUN")  # head not target-or-later


def runner(name, online, busy, *labels):
    return {"name": name, "status": "online" if online else "offline", "busy": busy,
            "labels": [{"name": label} for label in labels]}


class TriageQueuedJob(unittest.TestCase):
    def test_fp03_all_labelled_runners_busy_is_capacity(self):
        runners = load("runners.json")["runners"]
        runners = [r for r in runners if r["name"] != "cheese-x99-repo-9"]  # offline one removed
        self.assertTrue(all(r["busy"] for r in runners if "cheese-x99-homedisk" in [l["name"] for l in r["labels"]]))
        busy = triage(EXPECTED, runners)
        self.assertEqual(busy["verdict"], "capacity")
        freed = copy.deepcopy(runners)
        freed[0]["busy"] = False
        again = triage(EXPECTED, freed)
        self.assertEqual(again["verdict"], "capacity")
        self.assertNotEqual(again["verdict"], "missing-selector")

    def test_fp03_zero_online_labelled_runners_is_missing_selector(self):
        runners = load("runners.json")["runners"]
        for r in runners:  # offline ones, or online ones that lack a label, do not count
            r["status"] = "offline" if "homedisk" in json.dumps(r["labels"]) else r["status"]
        got = triage(EXPECTED, runners)
        self.assertEqual(got["verdict"], "missing-selector")
        self.assertEqual(got["online_labelled"], [])
        self.assertEqual(triage(EXPECTED, [])["verdict"], "missing-selector")

    def test_label_match_ignores_case(self):
        got = triage(["x64", "cheese-x99"], [runner("r", True, True, "X64", "cheese-x99")])
        self.assertEqual(got["verdict"], "capacity")


class Cli(unittest.TestCase):
    def call(self, argv, responses):
        out = io.StringIO()
        with patch.object(rp, "gh_json", side_effect=lambda path: responses[path]), contextlib.redirect_stdout(out):
            code = rp.main(argv)
        if not isinstance(code, int):
            raise AssertionError(f"expected an exit code, got {code!r}")
        return code, out.getvalue()

    def test_triage_prints_verdict_json(self):
        job = {"id": 7, "status": "queued", "labels": EXPECTED}
        responses = {"repos/o/r/actions/jobs/7": job, "orgs/o/actions/runners?per_page=100": load("runners.json")}
        code, text = self.call(["triage", "--repo", "o/r", "--job-id", "7"], responses)
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(text)["verdict"], "capacity")

    def test_triage_refuses_a_job_that_is_not_queued(self):
        job = {"id": 7, "status": "completed", "labels": EXPECTED}
        code, text = self.call(["triage", "--repo", "o/r", "--job-id", "7"], {"repos/o/r/actions/jobs/7": job})
        self.assertEqual(code, 2)

    def test_bind_prints_verdict_json(self):
        runs, by_run, commits = world(GOOD)
        sha = runs[0]["head_sha"]
        responses = {
            f"repos/o/r/commits/{sha}": {"sha": sha, "commit": {"committer": {"date": "2026-11-22T20:00:00Z"}}},
            f"repos/o/r/compare/{sha}...main": {"commits": [{"sha": sha}]},
            f"repos/o/r/actions/workflows/cd-qualification.yml/runs?branch=main&event=push&per_page=100&created=%3E%3D2026-11-22T20%3A00%3A00Z": {"workflow_runs": runs},
            f"repos/o/r/actions/runs/{GOOD}/jobs?per_page=100": {"jobs": by_run[GOOD]},
        }
        code, text = self.call(["bind", "--repo", "o/r", "--target", sha, "--labels", ",".join(EXPECTED)], responses)
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(text)["run_id"], GOOD)


if __name__ == "__main__":
    unittest.main()
