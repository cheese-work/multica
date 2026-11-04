#!/usr/bin/env python3
"""C00 deploy freshness alarm (CHE-3634). Independent of the deploy runner, like check-deploy-queue.py.

Two rules for the current main head:
1. Newest `CD qualification build` run failed on its first attempt -> `gh run rerun --failed`, once per
   run ID (run_attempt > 1 means it was already rerun).
2. Head merged >= 90 min ago and no successful `CD deploy to C00` run named for it whose `deploy` job succeeded
   (manual dispatches named by ref, not SHA, do not count; a run with the deploy job skipped does not count) -> ::error:: with the run
   URL and the first error line.
GitHub rate limits (403/429) wait for the reset time (capped) and retry instead of failing the alarm.
"""
import datetime as dt
import json
import os
import subprocess
import sys
import time

MAX_WAIT = 600  # seconds per rate-limit wait; the job's 15 min timeout bounds the total
RETRIES = 2
DEPLOY_DEADLINE = dt.timedelta(minutes=90)


def _gh(*args):
    return subprocess.run(["gh", *args], capture_output=True, text=True, check=True).stdout


def _reset_epoch():
    return int(_gh("api", "rate_limit", "--jq", ".resources.core.reset"))


def gh_api(*args, sleep=time.sleep):
    for attempt in range(RETRIES + 1):
        try:
            return _gh(*args)
        except subprocess.CalledProcessError as e:
            text = (e.stderr or "").lower()
            if attempt == RETRIES or not ("rate limit" in text or "429" in text):
                raise
            sleep(min(max(_reset_epoch() - int(time.time()), 0) + 1, MAX_WAIT))


def utcnow():
    return dt.datetime.now(dt.timezone.utc)


def parse(ts):
    return dt.datetime.fromisoformat(ts.replace("Z", "+00:00"))


def decide(sha, head_time, now, qual_runs, deploy_runs, ran_deploy):
    newest = max(qual_runs, key=lambda r: r["created_at"], default=None)
    rerun = newest["id"] if newest and newest["conclusion"] == "failure" and newest["run_attempt"] == 1 else None
    # A workflow_run deploy's head_sha is main's tip at trigger time, not the SHA it deployed. The qualified
    # SHA is only in the run name: "CD deploy to C00 · <sha>". Match that and a success conclusion. A run also
    # succeeds when candidate-preflight finds the commit ineligible and every later job is skipped, so ran_deploy(run)
    # must confirm that the `deploy` job itself succeeded (a baseline "skip" there means C00 already has the commit).
    deployed = any(r["conclusion"] == "success" and r.get("display_title", "").endswith(f" {sha}") and ran_deploy(r)
                   for r in deploy_runs)
    return {"rerun": rerun, "alert": not deployed and now - head_time >= DEPLOY_DEADLINE,
            "deployed": deployed, "run": newest}


def first_error(repo, run):
    """First ##[error] line of the first failed job, or a fallback text; never raises."""
    try:
        jobs = json.loads(gh_api("api", f"repos/{repo}/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs?per_page=100"))["jobs"]
        for job in (j for j in jobs if j["conclusion"] == "failure"):
            for line in gh_api("api", f"repos/{repo}/actions/jobs/{job['id']}/logs").splitlines():
                if "##[error]" in line:
                    return f"{job['name']}: {line.split('##[error]', 1)[1].strip()}"
    except (subprocess.CalledProcessError, ValueError, KeyError):
        pass
    return "no error line available"


def deploy_job_succeeded(repo, run):
    jobs = json.loads(gh_api("api", f"repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100"))["jobs"]
    return any(j["name"] == "deploy" and j["conclusion"] == "success" for j in jobs)


def main():
    repo = os.environ["GITHUB_REPOSITORY"]
    commit = json.loads(gh_api("api", f"repos/{repo}/commits/main"))
    sha, head_time, now = commit["sha"], parse(commit["commit"]["committer"]["date"]), utcnow()
    runs = lambda wf, query: json.loads(gh_api("api", f"repos/{repo}/actions/workflows/{wf}/runs?{query}&per_page=100"))["workflow_runs"]
    d = decide(sha, head_time, now, runs("cd-qualification.yml", f"head_sha={sha}"), runs("cd-deploy.yml", "status=success"),
               lambda run: deploy_job_succeeded(repo, run))
    if d["rerun"]:
        gh_api("run", "rerun", str(d["rerun"]), "--failed", "-R", repo)
        print(f"Reran failed jobs of qualification run {d['rerun']} (once per run ID).")
    if d["alert"]:
        age = int((now - head_time).total_seconds() // 60)
        run = d["run"]
        where = f"qualification run {run['html_url']} ({run['conclusion'] or run['status']}), {first_error(repo, run)}" if run else "no qualification run found"
        print(f"::error::main {sha[:9]} is not on C00 {age} min after merge (limit 90): {where}")
        return 1
    print(f"main {sha[:9]} is deployed to C00." if d["deployed"] else f"main {sha[:9]} not deployed yet, inside the 90 min window.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
