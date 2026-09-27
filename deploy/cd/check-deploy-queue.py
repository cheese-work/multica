#!/usr/bin/env python3
"""Read-only queue alarm, independent of the deploy runner and concurrency lock."""
import datetime as dt
import json
import os
import subprocess
import sys


def stalled_runs(runs, now, max_age_minutes=30):
    return [run for run in runs if run["status"] in ("queued", "pending", "waiting")
            and now - dt.datetime.fromisoformat(run["created_at"].replace("Z", "+00:00"))
            >= dt.timedelta(minutes=max_age_minutes)]


def main():
    repo = os.environ["GITHUB_REPOSITORY"]
    # Fetch every active queue state independently; a completed-run backlog must
    # not hide the oldest stalled run behind the first page.
    runs = {}
    for status in ("queued", "pending", "waiting"):
        output = subprocess.check_output([
            "gh", "api", "--paginate", "--jq", ".workflow_runs[] | @json",
            f"repos/{repo}/actions/workflows/cd-deploy.yml/runs?status={status}&per_page=100",
        ], text=True)
        for line in output.splitlines():
            run = json.loads(line)
            runs[run["id"]] = run
    stalled = stalled_runs(runs.values(), dt.datetime.now(dt.timezone.utc))
    for run in stalled:
        print(f"::error::C00 deployment stalled in {run['status']} since {run['created_at']}: {run['html_url']}")
    if stalled:
        print("Inspect cheese-c00-deploy runner registration/authentication and the cd-deploy-c00 lock. "
              "Do not revive the runner, cancel, or redeploy without reconciling production admission.")
        return 1
    print("No C00 deployment queued/pending/waiting for 30 minutes.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
