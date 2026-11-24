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

Suspension (CHE-9223 S2): the walk classifies each finished run (`classify`). A
first-attempt run the quiescence preflight deferred (marker step in the `deploy` job) is
named in the warning, but it still suspends, exactly like a failure; nothing is skipped
yet (CHE-9330). `authorize-c00.py --explain` prints the walk and the deferral streak,
read only, and is not called by the workflow.

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
MARKER_STEP = "C00 deploy deferred by quiescence preflight"  # cd-deploy.yml, `deploy` job (CHE-9367)
MAX_DEFERRALS = 3  # only `--explain` uses it; nothing enforces it yet (CHE-9330)


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


def classify(run, jobs):
    """One class for a finished run: "deferral", "failure", "success" or "silent". Pure.

    A deferral is a first-attempt run the quiescence preflight deferred (CHE-9223): the
    `deploy` job failed, its marker step succeeded, and no readback job ran. Any other run
    with a failure in the deploy jobs is a failure (reruns, legacy unmarked deferrals,
    observation_failed, malformed cases). A success without a failure is a success;
    anything else (built and stopped, cancelled) is silent.
    """
    deploy_jobs = [j for j in jobs if j.get("name") in DEPLOY_JOBS]
    results = [j.get("conclusion") for j in deploy_jobs]
    if "failure" not in results:
        return "success" if "success" in results else "silent"
    deploy = [j for j in deploy_jobs if j.get("name") == "deploy"]
    readback = [j for j in deploy_jobs if j.get("name") != "deploy"]
    marked = any(
        step.get("name") == MARKER_STEP and step.get("conclusion") == "success"
        for job in deploy
        for step in job.get("steps") or []
    )
    if (
        run.get("run_attempt") == 1
        and deploy
        and all(j.get("conclusion") == "failure" for j in deploy)
        and marked
        and all(j.get("conclusion") == "skipped" for j in readback)
    ):
        return "deferral"
    return "failure"


def walk(repo, current_run_id):
    """Yield (run, class) for finished `main` runs, newest first, 100 a page, at most RUN_PAGES.

    The current run is skipped. Lazy: a consumer that stops reading makes no further API
    calls. Running out of history ends the walk; hitting the page cap yields (None, "unknown").
    """
    for page in range(1, RUN_PAGES + 1):
        runs = gh_api(
            f"repos/{repo}/actions/workflows/cd-deploy.yml/runs?branch=main&status=completed&per_page={RUNS_PER_PAGE}&page={page}"
        )["workflow_runs"]
        for run in runs:
            if str(run["id"]) == str(current_run_id):
                continue
            jobs = gh_api(f"repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100")["jobs"]
            yield run, classify(run, jobs)
        if len(runs) < RUNS_PER_PAGE:
            return
    yield None, "unknown"


def suspended_by(repo, current_run_id):
    """Why auto-deploy is suspended (a description), or None.

    Walks finished `main` runs newest first until one of them deployed: that run's deploy
    or readback job failed (suspended; a marked deferral is named as such, but still
    suspends) or both succeeded (not suspended). Runs that built and stopped (jobs skipped)
    and cancelled runs say nothing, however many there are. Running out of history is not
    suspended; hitting the page cap before finding a deploy is unknown, which stays suspended.
    """
    for run, kind in walk(repo, current_run_id):
        if kind == "deferral":
            return f"run {run['id']} was deferred by the quiescence preflight; nothing was cut over. Automatic retry is not active yet (CHE-9330)"
        if kind == "failure":
            return f"run {run['id']} failed after deploying"
        if kind == "success":
            return None
        if kind == "unknown":
            return f"no deploy found in the last {RUN_PAGES * RUNS_PER_PAGE} runs, so the state is unknown"
    return None


def explain(repo, current_run_id):
    """Print the walk and the deferral streak. Read only; writes no $GITHUB_OUTPUT.

    Walks past deferrals until the first failure or success, or MAX_DEFERRALS deferrals.
    `authorized` is what gate mode returns for this history: the first non-silent run decides.
    """
    streak, decided = 0, None
    for run, kind in walk(repo, current_run_id):
        if kind == "unknown":
            print(f"no deploy found in the last {RUN_PAGES * RUNS_PER_PAGE} runs, so the state is unknown")
            decided = decided if decided is not None else False
            break
        print(f"run {run['id']} attempt {run.get('run_attempt')} event {run.get('event')}: {kind}")
        if kind == "silent":
            continue
        if decided is None:
            decided = kind == "success"
        if kind == "deferral":
            streak += 1
            if streak == MAX_DEFERRALS:
                break
            continue
        break
    print(f"deferral streak {streak}/{MAX_DEFERRALS}")
    # Running out of history with no deploy is not suspended, as in gate mode.
    print(f"authorized={'true' if decided is None or decided else 'false'}")


def main():
    if sys.argv[1:] == ["--explain"]:
        explain(os.environ["GITHUB_REPOSITORY"], os.environ.get("GITHUB_RUN_ID", ""))
        return
    if sys.argv[1:]:
        sys.exit(f"usage: authorize-c00.py [--explain]; got {' '.join(sys.argv[1:])}")
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
