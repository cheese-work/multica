#!/usr/bin/env python3
"""Read-only route proof for the CD image builds (CHE-9276, plan CHE-9208 step 2).

bind    find the proof run for a target commit (FP-02)
triage  say whether a queued job waits for capacity or for a missing selector (FP-03)

Both only read GitHub API JSON (`gh api`) and print a verdict as JSON. Nothing here
cancels a run, reverts a commit or changes a runner. Verdicts: bind prints BOUND, PENDING
(no proof yet, the 24 h window is still open) or NOT-RUN (no proof and the window is over);
triage prints capacity or missing-selector.
Exit codes: 0 BOUND or any triage verdict, 1 NOT-RUN, 2 bad input or API error, 3 PENDING.
"""
import argparse
import datetime as dt
import json
import subprocess
import sys
import urllib.parse

WORKFLOW_FILE = "cd-qualification.yml"
WORKFLOW_NAME = "CD qualification build"
LEG_NAMES = {"build-images (backend)", "build-images (web)"}


def _time(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def _labels(labels):
    """Lower-cased label names. Runner labels are objects, job labels are strings; GitHub ignores case (X64 vs x64)."""
    return {(label["name"] if isinstance(label, dict) else label).lower() for label in labels}


def _legs_prove(jobs, expected):
    legs = [job for job in jobs if job.get("name", "").startswith("build-images")]
    return (len(legs) == 2 and {job["name"] for job in legs} == LEG_NAMES
            and all(job.get("status") == "completed" and job.get("conclusion") == "success"
                    and _labels(job.get("labels") or []) == expected and len(job["labels"]) == len(expected)
                    for job in legs)), legs


def utcnow():
    return dt.datetime.now(dt.timezone.utc)


def bind_proof_run(runs, jobs_by_run, commits, target_time, expected_labels, window_hours=24, now=None):
    """First `CD qualification build` run on main that proves the route.

    runs: workflow runs (API objects), any order. jobs_by_run: {run_id: jobs} or a callable run_id -> jobs.
    commits: SHAs of the target commit and every later commit. target_time: the target commit's time.
    A run counts only when its head is in `commits`, it was created within window_hours of target_time,
    it is completed and not cancelled, and both build-images legs are success with exactly the expected labels.
    Without a proof: PENDING while now is inside the window (open_run_ids lists unfinished runs), NOT-RUN after it.
    now defaults to the real clock; tests pass it.
    """
    now = now or utcnow()
    get_jobs = jobs_by_run if callable(jobs_by_run) else jobs_by_run.get
    expected = _labels(expected_labels)
    deadline = target_time + dt.timedelta(hours=window_hours)
    read = sorted((run for run in runs
                   if run.get("name") == WORKFLOW_NAME and run.get("head_branch") == "main"
                   and run.get("head_sha") in commits and target_time <= _time(run["created_at"]) <= deadline),
                  key=lambda run: (_time(run["created_at"]), run["id"]))
    for run in read:
        if run.get("status") != "completed" or run.get("conclusion") == "cancelled":
            continue
        ok, legs = _legs_prove(get_jobs(run["id"]) or [], expected)
        if ok:
            return {"verdict": "BOUND", "run_id": run["id"], "head_sha": run["head_sha"], "created_at": run["created_at"],
                    "legs": [{"name": leg["name"], "runner": leg.get("runner_name"), "conclusion": leg["conclusion"]} for leg in legs]}
    return {"verdict": "PENDING" if now <= deadline else "NOT-RUN",
            "run_ids_read": [run["id"] for run in read],
            "open_run_ids": [run["id"] for run in read if run.get("status") != "completed"],
            "deadline": deadline.strftime("%Y-%m-%dT%H:%M:%SZ")}


def triage_queued_job(job_labels, runners):
    """capacity when an online runner carries every requested label (busy or not), else missing-selector."""
    wanted = _labels(job_labels)
    online = [runner for runner in runners if runner.get("status") == "online" and wanted <= _labels(runner.get("labels") or [])]
    return {"verdict": "capacity" if online else "missing-selector",
            "requested_labels": sorted(wanted),
            "online_labelled": sorted(runner["name"] for runner in online),
            "online_labelled_busy": sorted(runner["name"] for runner in online if runner.get("busy"))}


def gh_json(path):
    """One read-only `gh api` call."""
    return json.loads(subprocess.run(["gh", "api", path], check=True, capture_output=True, text=True).stdout)


def _all_runners(org):
    first = f"orgs/{org}/actions/runners?per_page=100"
    page = gh_json(first)
    runners = list(page["runners"])
    number = 1
    while len(runners) < page.get("total_count", 0):
        number += 1
        more = gh_json(f"{first}&page={number}")["runners"]
        if not more:
            break
        runners += more
    return runners


def _bind(args):
    base = f"repos/{args.repo}"
    commit = gh_json(f"{base}/commits/{args.target}")
    target_date = commit["commit"]["committer"]["date"]
    commits = {args.target, commit["sha"]} | {c["sha"] for c in gh_json(f"{base}/compare/{args.target}...main")["commits"]}
    created = urllib.parse.quote(">=" + target_date, safe="")
    runs = gh_json(f"{base}/actions/workflows/{WORKFLOW_FILE}/runs?branch=main&event=push&per_page=100&created={created}")["workflow_runs"]
    result = bind_proof_run(runs, lambda run_id: gh_json(f"{base}/actions/runs/{run_id}/jobs?per_page=100")["jobs"],
                            commits, _time(target_date), args.labels.split(","), args.window_hours)
    print(json.dumps(result, indent=2))
    return {"BOUND": 0, "NOT-RUN": 1, "PENDING": 3}[result["verdict"]]


def _triage(args):
    job = gh_json(f"repos/{args.repo}/actions/jobs/{args.job_id}")
    if job.get("status") != "queued":
        print(f"job {args.job_id} is {job.get('status')}, not queued: nothing to triage", file=sys.stderr)
        return 2
    result = triage_queued_job(job["labels"], _all_runners(args.repo.split("/")[0]))
    print(json.dumps(dict(result, job_id=job["id"]), indent=2))
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = parser.add_subparsers(dest="command", required=True)
    bind = sub.add_parser("bind", help="find the proof run for a target commit")
    bind.add_argument("--repo", required=True, help="owner/name")
    bind.add_argument("--target", required=True, help="target commit SHA")
    bind.add_argument("--labels", required=True, help="expected build-images labels, comma separated")
    bind.add_argument("--window-hours", type=float, default=24)
    triage = sub.add_parser("triage", help="classify a queued build-images job")
    triage.add_argument("--repo", required=True, help="owner/name; the org's runners are read")
    triage.add_argument("--job-id", required=True, help="job id of the queued build-images job")
    args = parser.parse_args(argv)
    try:
        return _bind(args) if args.command == "bind" else _triage(args)
    except (subprocess.CalledProcessError, KeyError, ValueError) as error:
        print(f"route-proof: {type(error).__name__}: {getattr(error, 'stderr', None) or error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
