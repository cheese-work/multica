#!/usr/bin/env python3
"""C00 deploy freshness alarm (CHE-3634). Independent of the deploy runner, like check-deploy-queue.py.

Two rules for the current main head:
1. Newest `CD qualification build` run failed on its first attempt -> `gh run rerun --failed`, once per
   run ID (run_attempt > 1 means it was already rerun).
2. Head merged >= 90 min ago and no successful `CD deploy to C00` run named for it whose `deploy` job succeeded
   (manual dispatches named by ref, not SHA, do not count; a run with the deploy job skipped does not count) -> ::error:: with the run
   URL and the first error line.
3. Qualified head whose deploy run was skipped (MAIN_RED_PAUSED was set when it started) -> `gh run rerun`, once per
   15 min tick up to MAX_RESUME_ATTEMPTS, because clearing the variable starts no run (CHE-4500).
`--live`: compare the revision C00 reports on /api/config (env C00_API_URL, tailnet only, so it runs on an X99
runner) with main's head; catches a rollback, which run history cannot see.
GitHub rate limits (403/429) wait for the reset time (capped) and retry instead of failing the alarm.
"""
import datetime as dt
import json
import os
import subprocess
import sys
import time
import urllib.request

MAX_WAIT = 600  # seconds per rate-limit wait; the job's 15 min timeout bounds the total
RETRIES = 2
DEPLOY_DEADLINE = dt.timedelta(minutes=90)
MAX_RESUME_ATTEMPTS = 12  # one rerun per 15 min tick: about 3 h of pause; the 90 min alarm still fires


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


def decide_resume(sha, qual_runs, skipped_deploy_runs, deployed):
    """Run ID of the skipped `CD deploy to C00` run to rerun, or None (CHE-4500).

    A run with the skipped conclusion never started candidate-preflight: its `if` was false. With a successful
    qualification that means MAIN_RED_PAUSED was set. Clearing the variable starts nothing, so rerun the run
    (all jobs: the preflight is the skipped one) to re-evaluate the flag. Still paused -> skipped again, next tick retries."""
    newest = max(qual_runs, key=lambda r: r["created_at"], default=None)
    if deployed or not newest or newest["conclusion"] != "success":
        return None
    named = [r for r in skipped_deploy_runs if r.get("display_title", "").endswith(f" {sha}")]
    run = max(named, key=lambda r: r.get("created_at", ""), default=None)
    if run is None or run["run_attempt"] >= MAX_RESUME_ATTEMPTS:
        return None
    return run["id"]


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


def live_commit(base_url):
    """Short commit C00's server reports on /api/config, or None when it cannot be read (tailnet only)."""
    try:
        with urllib.request.urlopen(f"{base_url.rstrip('/')}/api/config", timeout=15) as resp:
            return json.loads(resp.read()).get("server_commit") or None
    except (OSError, ValueError):
        return None


def decide_live(sha, head_time, now, live):
    # Run history cannot see a rollback done on C00 (cutover.sh rollback leaves the old successful deploy run in
    # the API), so what runs now is the only proof. server_commit is the 7-char short SHA.
    deployed = live == sha[:7]
    return {"alert": not deployed and now - head_time >= DEPLOY_DEADLINE, "deployed": deployed}


def deploy_job_succeeded(repo, run):
    jobs = json.loads(gh_api("api", f"repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100"))["jobs"]
    return any(j["name"] == "deploy" and j["conclusion"] == "success" for j in jobs)


def main_live(sha, head_time, now):
    live = live_commit(os.environ["C00_API_URL"])
    d = decide_live(sha, head_time, now, live)
    if d["alert"]:
        age = int((now - head_time).total_seconds() // 60)
        print(f"::error::main {sha[:9]} is not what C00 runs {age} min after merge (limit 90): live revision {live or 'unreadable'}")
        return 1
    print(f"main {sha[:9]} runs on C00 (live revision {live})." if d["deployed"] else f"C00 runs {live or 'an unreadable revision'}, inside the 90 min window.")
    return 0


def main(argv=()):
    repo = os.environ["GITHUB_REPOSITORY"]
    commit = json.loads(gh_api("api", f"repos/{repo}/commits/main"))
    sha, head_time, now = commit["sha"], parse(commit["commit"]["committer"]["date"]), utcnow()
    if "--live" in argv:
        return main_live(sha, head_time, now)
    runs = lambda wf, query: json.loads(gh_api("api", f"repos/{repo}/actions/workflows/{wf}/runs?{query}&per_page=100"))["workflow_runs"]
    qual_runs = runs("cd-qualification.yml", f"head_sha={sha}")
    d = decide(sha, head_time, now, qual_runs, runs("cd-deploy.yml", "status=success"),
               lambda run: deploy_job_succeeded(repo, run))
    if d["rerun"]:
        gh_api("run", "rerun", str(d["rerun"]), "--failed", "-R", repo)
        print(f"Reran failed jobs of qualification run {d['rerun']} (once per run ID).")
    resume = None if d["deployed"] else decide_resume(sha, qual_runs, runs("cd-deploy.yml", "status=skipped"), d["deployed"])
    if resume:
        gh_api("run", "rerun", str(resume), "-R", repo)
        print(f"Reran skipped deploy run {resume}: MAIN_RED_PAUSED may have cleared.")
    if d["alert"]:
        age = int((now - head_time).total_seconds() // 60)
        run = d["run"]
        where = f"qualification run {run['html_url']} ({run['conclusion'] or run['status']}), {first_error(repo, run)}" if run else "no qualification run found"
        print(f"::error::main {sha[:9]} is not on C00 {age} min after merge (limit 90): {where}")
        return 1
    print(f"main {sha[:9]} is deployed to C00." if d["deployed"] else f"main {sha[:9]} not deployed yet, inside the 90 min window.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
