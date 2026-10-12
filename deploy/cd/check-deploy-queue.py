#!/usr/bin/env python3
"""Read-only queue alarm, independent of the deploy runner and concurrency lock."""
import datetime as dt
import json
import os
import subprocess
import sys


def stalled_runs(runs, now, max_age_minutes=30):
    return [run for run in runs if run["status"] in ("queued", "pending", "waiting")
            and now - dt.datetime.fromisoformat((run.get("created_at") or run["started_at"]).replace("Z", "+00:00"))
            >= dt.timedelta(minutes=max_age_minutes)]


def main():
    repo = os.environ["GITHUB_REPOSITORY"]
    # Fetch every active queue state independently; a completed-run backlog must
    # not hide the oldest stalled run behind the first page.
    runs = {}
    for status in ("queued", "pending", "waiting", "in_progress"):
        output = subprocess.check_output([
            "gh", "api", "--paginate", "--jq", ".workflow_runs[] | @json",
            f"repos/{repo}/actions/workflows/cd-deploy.yml/runs?status={status}&per_page=100",
        ], text=True)
        for line in output.splitlines():
            run = json.loads(line)
            runs[run["id"]] = run
    jobs = []
    for run in runs.values():
        # Attempt-specific jobs exclude earlier reruns. Workflow creation is
        # never a queue clock: preparation may legitimately take over an hour.
        output = subprocess.check_output([
            "gh", "api", "--paginate", "--jq", ".jobs[] | @json",
            f"repos/{repo}/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs?per_page=100",
        ], text=True)
        jobs.extend(job for line in output.splitlines()
                    if (job := json.loads(line))["name"] == "deploy"
                    and job["run_attempt"] == run["run_attempt"])
    stalled = stalled_runs(jobs, dt.datetime.now(dt.timezone.utc))
    for job in stalled:
        queued_at = job.get("created_at") or job["started_at"]
        print(f"::error::C00 deployment stalled in {job['status']} since {queued_at}: {job['html_url']}")
    if stalled:
        print("Inspect cheese-c00-deploy runner registration/authentication and the cd-deploy-c00 lock. "
              "Do not revive the runner, cancel, or redeploy without reconciling production admission.")
        return 1
    print("No C00 deployment queued/pending/waiting for 30 minutes.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
