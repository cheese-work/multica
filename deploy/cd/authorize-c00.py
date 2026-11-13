#!/usr/bin/env python3
"""Deploy gate for cd-deploy.yml (CHE-4495, CHE-4582).

Who may deploy to C00: `congvc-dev` plus the logins in EXTRA_AUTHORIZERS (comma
separated; the repository variable C00_DEPLOY_AUTHORIZERS, set by CHE-3123 for
the agent merge identity). The owner is hard-coded so an empty or edited
variable can never lock the owner out or widen the owner's own rights.

- workflow_dispatch: the triggering actor must be allowed, otherwise exit 1
  before any C00 step. Outputs authorized=true and the candidate_sha input.
- schedule (the newest qualified `main` commit, resolved by schedule-resolve; CHE-5182): the merger of the
  pull request whose merge commit is HEAD_SHA must be allowed, and the latest
  automatic deploy must not have failed. Anything else is not an error: the
  run builds and stops (authorized=false). A GitHub API error fails the job.

- backup dispatch (CHE-5353): the freshness alarm dispatches cd-deploy.yml with
  `schedule_backup=true` when GitHub drops a schedule slot. Only the workflow's
  own robot (ROBOT) may send the marker; any other actor is refused. It then
  takes the schedule rules above: same merger check, same suspension.

Outputs go to $GITHUB_OUTPUT. `gh` and its token come from the environment.
"""
import json
import os
import subprocess
import sys

OWNER = "congvc-dev"
ROBOT = "github-actions[bot]"
DEPLOY_JOBS = ("deploy", "readback", "readback-delay", "readback-confirm")
RUNS_PER_PAGE = 100
RUN_PAGES = 5


def gh_api(path):
    result = subprocess.run(["gh", "api", path], capture_output=True, text=True)
    if result.returncode:
        print(f"::error::GitHub API read failed ({path}): {result.stderr.strip()}", file=sys.stderr)
        sys.exit(1)
    return json.loads(result.stdout)


def allowed_logins():
    extra = [name.strip() for name in os.environ.get("EXTRA_AUTHORIZERS", "").split(",")]
    return {OWNER, *[name for name in extra if name]}


def write_outputs(authorized, candidate_sha=""):
    with open(os.environ["GITHUB_OUTPUT"], "a") as out:
        out.write(f"authorized={'true' if authorized else 'false'}\ncandidate_sha={candidate_sha}\n")


def merger_of(repo, sha):
    """Login that merged the pull request whose merge commit on `main` is `sha`, or None.

    The commit's pull list carries no `merged_by`; only the pull's own record
    does, so each candidate is re-read and judged on that record.
    """
    for listed in gh_api(f"repos/{repo}/commits/{sha}/pulls"):
        if listed.get("merge_commit_sha") != sha or not listed.get("number"):
            continue
        pr = gh_api(f"repos/{repo}/pulls/{listed['number']}")
        if pr.get("merged_at") and pr.get("merge_commit_sha") == sha and pr.get("base", {}).get("ref") == "main":
            return (pr.get("merged_by") or {}).get("login") or None
    return None


def suspended_by(repo, current_run_id):
    """Why auto-deploy is suspended (a description), or None.

    Walks finished `main` runs newest first until one of them deployed: that
    run's deploy or readback job failed (suspended) or both succeeded (not
    suspended). Runs that built and stopped (jobs skipped) and cancelled runs
    say nothing, however many there are. Running out of history is not
    suspended; hitting the page cap before finding a deploy is unknown, which
    stays suspended.
    """
    for page in range(1, RUN_PAGES + 1):
        runs = gh_api(
            f"repos/{repo}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page={RUNS_PER_PAGE}&page={page}"
        )["workflow_runs"]
        for run in runs:
            if str(run["id"]) == str(current_run_id):
                continue
            jobs = gh_api(f"repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100")["jobs"]
            results = [j.get("conclusion") for j in jobs if j.get("name") in DEPLOY_JOBS]
            if "failure" in results:
                return f"run {run['id']} failed after deploying"
            if "success" in results:
                return None
        if len(runs) < RUNS_PER_PAGE:
            return None
    return f"no deploy found in the last {RUN_PAGES * RUNS_PER_PAGE} runs, so the state is unknown"


def main():
    event = os.environ["EVENT_NAME"]
    allowed = allowed_logins()

    actor = os.environ.get("TRIGGERING_ACTOR", "")
    backup = event == "workflow_dispatch" and os.environ.get("INPUT_SCHEDULE_BACKUP", "") != ""
    if backup and actor != ROBOT:
        print(f"::error::backup dispatch by '{actor}' refused; only {ROBOT} may send schedule_backup", file=sys.stderr)
        sys.exit(1)

    if event == "workflow_dispatch" and not backup:
        if actor not in allowed:
            print(f"::error::dispatch by '{actor}' refused; only {', '.join(sorted(allowed))} may deploy to C00", file=sys.stderr)
            sys.exit(1)
        write_outputs(True, os.environ.get("INPUT_CANDIDATE_SHA", ""))
        return

    repo, sha = os.environ["GITHUB_REPOSITORY"], os.environ["HEAD_SHA"]
    merger = merger_of(repo, sha)
    if merger not in allowed:
        print(f"::notice title=C00 auto-deploy::{sha} merged by '{merger or 'nobody found'}'; built, not deployed. The owner may dispatch it.")
        write_outputs(False)
        return
    reason = suspended_by(repo, os.environ.get("GITHUB_RUN_ID", ""))
    if reason:
        print(f"::warning title=C00 auto-deploy suspended::{reason}; {sha} was built, not deployed. A successful dispatch by the owner lifts the suspension.")
        write_outputs(False)
        return
    write_outputs(True, sha)


if __name__ == "__main__":
    main()
