#!/usr/bin/env python3
"""C00 deploy freshness alarm (CHE-3634). Independent of the deploy runner, like check-deploy-queue.py.

Rules (CHE-9243):
1. Newest `CD qualification build` run failed on its first attempt -> `gh run rerun --failed`, once per
   run ID (run_attempt > 1 means it was already rerun).
2. C = the oldest main commit newer than what C00 runs (CHE-7522: a newer merge never moves it). What C00 runs is the
   SHA of the trusted release receipt that completed last: a successful `cd-deploy.yml` run on main in this repository
   whose `deploy` and `release-receipt` jobs succeeded and whose receipt step is named exactly like
   `ghrun.ParseReleaseReceipt` reads it. Run titles carry no SHA and are never read. Alarm when C is undeployed and
   now >= the first deploy slot strictly after C's merge + 90 min. Qualification runs are diagnostic text only.
3. A `cd-deploy.yml` slot (17 past 01/05/09/13/17/21 UTC) passed >= 30 min ago and no `schedule` run was created since
   -> ::error:: (CHE-5182: GitHub can drop scheduled runs; the cron sits off the hour for the same reason), and one
   backup `gh workflow run cd-deploy.yml --ref main -f schedule_backup=true` (CHE-5353) unless a cd-deploy.yml run on main was created since the
   slot. That run, scheduled or dispatched, is the guard: later alarm runs find it, so a slot gets one dispatch.
Order: commits, rule 3, rule 1, rule 2, so a receipt lookup error cannot stop the dispatch or the rerun.
`--live`: compare the revision C00 reports on /api/config (env C00_API_URL, tailnet only, so it runs on an X99
runner) with main's commits and apply the same deadline to C; catches a rollback, which run history cannot see. An
unreadable revision proves nothing deployed, so C is the oldest listed commit.
GitHub rate limits (403/429) wait for the reset time (capped) and retry instead of failing the alarm.
"""
import datetime as dt
import json
import os
import re
import subprocess
import sys
import time
import urllib.request

MAX_WAIT = 600  # seconds per rate-limit wait; the job's 15 min timeout bounds the total
RETRIES = 2
DEPLOY_DEADLINE = dt.timedelta(minutes=90)
SLOT_HOURS_UTC, SLOT_MINUTE = (1, 5, 9, 13, 17, 21), 17  # keep equal to the cron in cd-deploy.yml (test-deploy-schedule.py)
SLOT_GRACE = dt.timedelta(minutes=30)
SCAN_CAP = 30  # successful cd-deploy.yml runs whose jobs are read per alarm run
DEPLOY_WORKFLOW_PATH = ".github/workflows/cd-deploy.yml"
RECEIPT_STEP = re.compile(r"release-receipt old=([0-9a-f]{40}) new=([0-9a-f]{40}) digest=sha256:[0-9a-f]{64}")  # ghrun/webhook.go receiptStep


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


def undeployed_commit(commits, deployed):
    """(sha, merge time) of the oldest commit newer than the newest deployed one, None when the head is deployed.
    commits: [(sha, time)] newest first. None deployed in the list -> the oldest listed commit."""
    oldest = None
    for sha, t in commits:
        if deployed(sha):
            break
        oldest = (sha, t)
    return oldest


def next_slot(t):
    """First cd-deploy.yml slot strictly after t."""
    return min(s for d in (t.date(), t.date() + dt.timedelta(days=1)) for h in SLOT_HOURS_UTC
               if (s := dt.datetime(d.year, d.month, d.day, h, SLOT_MINUTE, tzinfo=dt.timezone.utc)) > t)


def deadline(merged):
    """A merge is carried by the next deploy slot (CHE-5182: every 4 h), which gets DEPLOY_DEADLINE to finish."""
    return next_slot(merged) + DEPLOY_DEADLINE


def rerun_id(qual_runs):
    newest = max(qual_runs, key=lambda r: r["created_at"], default=None)
    return newest["id"] if newest and newest["conclusion"] == "failure" and newest["run_attempt"] == 1 else None


def trusted_receipt(repo, run):
    """(deployed SHA, receipt job completion time) of a successful main cd-deploy.yml run, else None. A run succeeds
    also when candidate-preflight finds the commit ineligible and every later job is skipped, and its title never
    names the SHA (`CD deploy to C00 · schedule`), so only the receipt step proves a deploy, readback and confirm."""
    if run["head_branch"] != "main" or run["path"] != DEPLOY_WORKFLOW_PATH or run["repository"]["full_name"] != repo:
        return None
    jobs = json.loads(gh_api("api", f"repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100"))["jobs"]
    if not any(j["name"] == "deploy" and j["conclusion"] == "success" for j in jobs):
        return None
    for job in (j for j in jobs if j["name"] == "release-receipt" and j["conclusion"] == "success" and j["head_branch"] == "main"):
        for step in job["steps"]:
            m = RECEIPT_STEP.fullmatch(step["name"])
            if m and step["conclusion"] == "success":
                return m.group(2), parse(job["completed_at"])
    return None


def credited_sha(repo, deploy_runs):
    """The SHA C00 runs by run history: among trusted receipts of the newest SCAN_CAP runs, the one whose receipt job
    completed last. Run creation order is not cutover order (cd-deploy.yml queues runs FIFO, `queue: max`)."""
    receipts = [r for r in (trusted_receipt(repo, run) for run in deploy_runs[:SCAN_CAP]) if r]
    return max(receipts, key=lambda r: r[1])[0] if receipts else None


def overdue(commit, now):
    return commit is not None and now >= deadline(commit[1])


def stamp(t):
    return f"{t:%d %b %H:%M} UTC ({t + dt.timedelta(hours=7):%H:%M} ICT)"


def late_text(head, commit, suffix=""):
    sha, merged = commit
    return (f"main {head[:9]} is not on C00: oldest undeployed {sha[:9]} (merged {stamp(merged)}) missed the {stamp(next_slot(merged))} slot "
            f"+ 90 min (deadline {stamp(deadline(merged))}){suffix}")


def missed_slot(now, schedule_runs):
    """Latest cd-deploy.yml slot at least SLOT_GRACE old that has no schedule run created since it, else None."""
    slots = [dt.datetime(d.year, d.month, d.day, h, SLOT_MINUTE, tzinfo=dt.timezone.utc)
             for d in (now - dt.timedelta(days=1), now) for h in SLOT_HOURS_UTC]
    slot = max((s for s in slots if now - s >= SLOT_GRACE), default=None)
    if slot and not any(parse(r["created_at"]) >= slot for r in schedule_runs):
        return slot
    return None


def run_since(slot, runs):
    return any(parse(r["created_at"]) >= slot for r in runs)


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


def main_live(sha, commits, now):
    # run history cannot see a rollback done on C00 (cutover.sh rollback leaves the old successful deploy run in the
    # API), so what runs now is the only proof. server_commit is the 7-char short SHA. Unreadable: nothing is proven
    # deployed, so C is the oldest listed commit, never the head (a new merge must not open a new window, CHE-9243).
    live = live_commit(os.environ["C00_API_URL"])
    c = undeployed_commit(commits, lambda s: live == s[:7])
    if overdue(c, now):
        print(f"::error::{late_text(sha, c, f"; live revision {live or 'unreadable'}")}")
        return 1
    if c is None:
        print(f"main {sha[:9]} runs on C00 (live revision {live}).")
    else:
        print(f"C00 runs {live or 'an unreadable revision'}, {c[0][:9]} is inside its window until {stamp(deadline(c[1]))}.")
    return 0


def backup_dispatch(repo, slot, deploy_runs):
    """The marker makes cd-deploy.yml run its scheduled path (resolve, preflight, merger and suspension checks) for this dispatch."""
    if run_since(slot, deploy_runs):
        print("A cd-deploy.yml run on main was created since the slot; no backup dispatch.")
        return
    try:
        gh_api("workflow", "run", "cd-deploy.yml", "--ref", "main", "-f", "schedule_backup=true", "-R", repo)
        print("Sent one backup dispatch of cd-deploy.yml (once per slot).")
    except subprocess.CalledProcessError as e:
        print(f"::error::backup dispatch failed: {(e.stderr or '').strip() or e}")


def rule_2(repo, sha, commits, now, qual_runs, runs):
    credited = credited_sha(repo, runs("cd-deploy.yml", "status=success&branch=main"))
    c = undeployed_commit(commits, lambda s: s == credited)
    if not overdue(c, now):
        print(f"main {sha[:9]} is deployed to C00." if c is None else f"main {sha[:9]} not deployed yet: {c[0][:9]} is inside its window until {stamp(deadline(c[1]))}.")
        return 0
    newest = max(qual_runs, key=lambda r: r["created_at"], default=None)
    where = f"head qualification run {newest['html_url']} ({newest['conclusion'] or newest['status']}), {first_error(repo, newest)}" if newest else "no qualification run found"
    print(f"::error::{late_text(sha, c)}: {where}")
    return 1


def main(argv=()):
    repo = os.environ["GITHUB_REPOSITORY"]
    # ponytail: newest 100 main commits; an older undeployed commit counts as the 100th (alarm fires either way).
    commits = [(c["sha"], parse(c["commit"]["committer"]["date"]))
               for c in json.loads(gh_api("api", f"repos/{repo}/commits?sha=main&per_page=100"))]
    sha, now = commits[0][0], utcnow()
    if "--live" in argv:
        return main_live(sha, commits, now)
    runs = lambda wf, query: json.loads(gh_api("api", f"repos/{repo}/actions/workflows/{wf}/runs?{query}&per_page=100"))["workflow_runs"]
    # Rule 3, then rule 1, then rule 2: rule 2 reads the most calls and fails last, so a lookup error cannot stop the dispatch or the rerun.
    # ponytail: one page (100) of schedule runs; slots are 6/day so the newest page always holds the latest slot.
    slot = missed_slot(now, runs("cd-deploy.yml", "event=schedule"))
    if slot:
        ict = slot + dt.timedelta(hours=7)
        print(f"::error::cd-deploy.yml slot {slot:%H:%M} UTC ({ict:%H:%M} ICT) passed over 30 min ago and no schedule run exists since")
        backup_dispatch(repo, slot, runs("cd-deploy.yml", "branch=main"))
    qual_runs = runs("cd-qualification.yml", f"head_sha={sha}")
    rerun = rerun_id(qual_runs)
    if rerun:
        gh_api("run", "rerun", str(rerun), "--failed", "-R", repo)
        print(f"Reran failed jobs of qualification run {rerun} (once per run ID).")
    try:
        late = rule_2(repo, sha, commits, now, qual_runs, runs)
    except (subprocess.CalledProcessError, ValueError, KeyError) as e:
        cause = (getattr(e, "stderr", None) or "").strip() or repr(e)
        print(f"::error::rule 2 could not read deploy receipts: {cause}")
        return 1
    return 1 if slot or late else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
