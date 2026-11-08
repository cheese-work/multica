#!/usr/bin/env python3
"""Deploy gate for cd-deploy.yml (CHE-4495, CHE-4582).

Who may deploy to C00: `congvc-dev` plus the logins in EXTRA_AUTHORIZERS (comma
separated; the repository variable C00_DEPLOY_AUTHORIZERS, set by CHE-3123 for
the agent merge identity). The owner is hard-coded so an empty or edited
variable can never lock the owner out or widen the owner's own rights.

- workflow_dispatch: the triggering actor must be allowed, otherwise exit 1
  before any C00 step. Outputs authorized=true and the candidate_sha input.
- workflow_run (a successful qualification of a `main` push): the merger of the
  pull request whose merge commit is HEAD_SHA must be allowed, and the latest
  automatic deploy must not have failed. Anything else is not an error: the
  run builds and stops (authorized=false). A GitHub API error fails the job.

Outputs go to $GITHUB_OUTPUT. `gh` and its token come from the environment.
"""
import json
import os
import subprocess
import sys

OWNER = "congvc-dev"
DEPLOY_JOBS = ("deploy", "readback")
RUNS_SCANNED = 20


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
    """Login that merged the pull request whose merge commit on `main` is `sha`, or None."""
    for pr in gh_api(f"repos/{repo}/commits/{sha}/pulls"):
        if pr.get("merged_at") and pr.get("merge_commit_sha") == sha and pr.get("base", {}).get("ref") == "main":
            return (pr.get("merged_by") or {}).get("login") or None
    return None


def suspended_by(repo, current_run_id):
    """Run id of the latest finished deploy when it failed, else None.

    A run counts only if its deploy or readback job succeeded or failed; runs
    that built and stopped (jobs skipped) and cancelled runs say nothing.
    """
    runs = gh_api(f"repos/{repo}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page={RUNS_SCANNED}")
    for run in runs["workflow_runs"]:
        if str(run["id"]) == str(current_run_id):
            continue
        jobs = gh_api(f"repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100")["jobs"]
        results = [j.get("conclusion") for j in jobs if j.get("name") in DEPLOY_JOBS]
        if "failure" in results:
            return run["id"]
        if "success" in results:
            return None
    return None


def main():
    event = os.environ["EVENT_NAME"]
    allowed = allowed_logins()

    if event == "workflow_dispatch":
        actor = os.environ.get("TRIGGERING_ACTOR", "")
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
    failed_run = suspended_by(repo, os.environ.get("GITHUB_RUN_ID", ""))
    if failed_run:
        print(f"::warning title=C00 auto-deploy suspended::run {failed_run} failed after deploying; {sha} was built, not deployed. A successful dispatch by the owner lifts the suspension.")
        write_outputs(False)
        return
    write_outputs(True, sha)


if __name__ == "__main__":
    main()
