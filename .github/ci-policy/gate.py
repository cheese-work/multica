#!/usr/bin/env python3
"""ci-policy caller logic (D-6, eng review D10 and D14). Run from the BASE revision's bytes: the head is only read as
data (`git archive`), nothing from it is executed. Needs `git` and, for the API reads, `gh` with GH_TOKEN.

  pr          --repo OWNER/NAME --pr N --base SHA --head SHA [--private] [--default-branch main] [--root DIR]
  merge-group --repo OWNER/NAME --group-ref REF
  rerun-pr    --repo OWNER/NAME --head SHA   (after a review change: re-run that PR's check)

`pr` runs check.py on the head's workflow files and policy file. A PR that edits `.github/ci-policy*` or the caller
workflow also fails `policy-changed` until a GitHub review APPROVED by a human who is not the author or a bot, with
write access, on the current head SHA. A new push moves the head, so it resets the approval. `merge-group` lists the PRs queued
in the group (the merge commits on the group head's first-parent chain), finds each by its recorded head and runs the PR
check again on that head; a commit whose PR the queue data does not identify fails closed."""
import argparse
import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check as ci_check  # noqa: E402

WRITE_PERMISSIONS = {"write", "admin"}  # effective repository permission; author_association says nothing about it (CWE-863)
VERDICTS = {"APPROVED", "CHANGES_REQUESTED", "DISMISSED"}  # a COMMENTED review changes nothing
COMPARE_CAP = 250  # the compare API lists at most this many commits
HANDOFF = ".github/workflows/ci-policy-handoff.yml"
GROUP_REF = re.compile(r"^refs/heads/(gh-readonly-queue/(.+)/pr-(\d+)-[0-9a-f]{40})$")


def is_protected(path: str) -> bool:
    return path.startswith((".github/ci-policy", ".github/workflows/ci-policy"))


def gh_api(path: str):
    """JSON for one object, or a list for a paginated array (`check_runs` unwrapped)."""
    base = path.split("?")[0]
    jq = (".check_runs[]" if "/check-runs" in path else ".commits[]" if "/compare/" in path
          else ".workflow_runs[]" if base.endswith("/runs")
          else ".[]" if base.endswith(("/reviews", "/pulls")) else ".")
    out = subprocess.run(["gh", "api", "--paginate", path, "--jq", jq], check=True, capture_output=True, text=True).stdout
    items = [json.loads(line) for line in out.splitlines() if line.strip()]
    return items if jq != "." else items[0]


def gh_post(path: str) -> None:
    subprocess.run(["gh", "api", "-X", "POST", path], check=True, capture_output=True, text=True)


def can_write(repo: str, login: str, api) -> bool:
    """Effective permission from the collaborators API; an unreadable answer is no permission (fail closed)."""
    try:
        return api(f"repos/{repo}/collaborators/{login}/permission").get("permission") in WRITE_PERMISSIONS
    except (subprocess.CalledProcessError, AttributeError, KeyError, ValueError):
        return False


def approver(reviews: list, head: str, author: str, repo: str, api=gh_api) -> str | None:
    latest = {}
    for r in reviews:  # chronological: the last verdict of each reviewer stands
        if r["state"] in VERDICTS:
            latest[r["user"]["login"]] = r
    return next((login for login, r in latest.items()
                 if r["state"] == "APPROVED" and r["commit_id"] == head and login != author
                 and r["user"]["type"] == "User" and can_write(repo, login, api)), None)


def changed_paths(root: Path, base: str, head: str) -> list[str]:
    out = subprocess.run(["git", "-C", str(root), "-c", "core.quotepath=off", "diff", "--name-only", "--no-renames", f"{base}...{head}"],
                         check=True, capture_output=True, text=True).stdout
    return out.splitlines()


def extract_head(root: Path, head: str, dest: Path) -> list[str]:
    """Write the head's `.github` regular files under `dest` and return the paths it could not (symlinks, submodules).
    Reads the tree object by object: `git archive` would honour an `export-ignore` the PR itself put in `.gitattributes`
    and so let the PR hide a workflow from the checker (CWE-807)."""
    listing = subprocess.run(["git", "-C", str(root), "ls-tree", "-r", "-z", head, "--", ".github"],
                             capture_output=True, check=True).stdout.decode().split("\0")
    skipped = []
    for entry in filter(None, listing):
        meta, path = entry.split("\t", 1)
        mode, _, sha = meta.split()
        if mode not in ("100644", "100755"):
            skipped.append(path)
            continue
        target = dest / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(subprocess.run(["git", "-C", str(root), "cat-file", "blob", sha], capture_output=True, check=True).stdout)
    return skipped


HANDOFF_DOC = {  # the parsed `.github/workflows/ci-policy-handoff.yml` (key `on` as a string); a test pins it to the shipped file
    "name": "ci-policy-handoff",
    "on": {"pull_request_review": {"types": ["submitted", "dismissed"]}, "merge_group": None},
    "permissions": {},
    "concurrency": {
        "group": "ci-policy-handoff-${{ github.event_name }}-${{ github.event.pull_request.number || github.event.merge_group.head_sha }}",
        "cancel-in-progress": True,
    },
    "jobs": {"handoff": {
        "name": "${{ github.event_name == 'merge_group' && 'queue-handoff' || 'review-handoff' }}",
        "runs-on": "${{ github.repository_owner == 'cheese-work' && fromJSON('[\"self-hosted\",\"Linux\",\"X64\",\"cheese-x99-gate\"]') || fromJSON('[\"ubuntu-latest\"]') }}",
        "timeout-minutes": 2,
        "steps": [{"run": 'echo "ci-policy-queue.yml follows this run"'}],
    }},
}


def first_difference(want, got, path="") -> str | None:
    if isinstance(want, dict) and isinstance(got, dict):
        for key in sorted(set(want) | set(got), key=str):
            if key not in got or key not in want:
                return f"{path}/{key}"
            if diff := first_difference(want[key], got[key], f"{path}/{key}"):
                return diff
        return None
    if isinstance(want, list) and isinstance(got, list) and len(want) == len(got):
        return next((d for i, (x, y) in enumerate(zip(want, got)) if (d := first_difference(x, y, f"{path}/{i}"))), None)
    return None if type(want) is type(got) and want == got else (path or "/")


def handoff_violations(root: Path, base: str, tree: Path) -> list:
    """The review/queue handoff is the only signal that revokes a cached success (an approval withdrawn without a push), and
    its definition comes from the PR. Any edit can delay or drop it: a removed trigger, a narrowed type, a job `if:`, a
    runner label that never picks the job up, a long timeout or a queueing concurrency group. A human approval of such an
    edit would not undo a withdrawal later. So the head must keep the whole contract: the parsed workflow equals
    `HANDOFF_DOC` (comments aside). A repo whose base does not carry the file yet is not enrolled, so a missing file is not
    judged there. Changing the contract takes a change to this script first."""
    enrolled = subprocess.run(["git", "-C", str(root), "cat-file", "-e", f"{base}:{HANDOFF}"], capture_output=True).returncode == 0
    path = tree / HANDOFF
    if not path.exists():
        return [ci_check.Violation(HANDOFF, "handoff-disabled", "the handoff workflow is missing: a withdrawn approval would not be seen")] if enrolled else []

    def bad(reason):
        return [ci_check.Violation(HANDOFF, "handoff-disabled", reason)]

    try:
        doc = yaml.safe_load(path.read_text())
    except yaml.YAMLError:
        return bad("not valid YAML")
    if not isinstance(doc, dict):
        return bad("not a workflow")
    doc = {("on" if k is True else k): v for k, v in doc.items()}  # PyYAML (YAML 1.1) reads the key `on` as True
    if diff := first_difference(HANDOFF_DOC, doc):
        return bad(f"differs from the trusted handoff contract at {diff}: a changed trigger, job, runner, timeout or concurrency can delay a withdrawn approval")
    return []


def check_pr(root: Path, repo: str, pr: int, base: str, head: str, private: bool, default_branch: str, api=gh_api) -> list:
    live = api(f"repos/{repo}/pulls/{pr}")
    if live["head"]["sha"] != head:
        return [ci_check.Violation("-", "head-moved", f"PR head is {live['head']['sha'][:12]}, not {head[:12]}: a newer run reports")]
    found = []
    touched = [p for p in changed_paths(root, base, head) if is_protected(p)]
    if touched and not approver(api(f"repos/{repo}/pulls/{pr}/reviews"), head, live["user"]["login"], repo, api):
        found.append(ci_check.Violation(touched[0], "policy-changed",
                                        f"PR edits {', '.join(touched)}: needs a non-author human APPROVED review on head {head[:12]}"))
    with tempfile.TemporaryDirectory() as tmp:
        skipped = extract_head(root, head, Path(tmp))
        violations, _ = ci_check.check(Path(tmp), repo, private, default_branch)
        found += handoff_violations(root, base, Path(tmp))
    found += [ci_check.Violation(p, "head-unreadable", "not a regular file: the checker cannot read it") for p in skipped]
    return found + violations


def check_merge_group(repo: str, group_ref: str, api=gh_api, evaluate=None) -> list:
    """Every PR queued in the group must pass `evaluate(pr, head)` (the PR check run again, here, on the queued head).

    Nothing is copied from a check-run: a check name is not authenticated, so any workflow could post a green
    `ci-policy` (CWE-345). Only queue data GitHub itself records identifies a PR. Walk the first-parent chain from the
    group head down to the base branch (the compare response also lists each PR's own commits, which are not queue
    commits). Each chain commit must be a merge commit: its second parent is the PR head the queue merged, and
    `commits/{head}/pulls` names the one open PR that has that head. Titles and tree contents prove nothing about
    identity and are not read. A squashed commit records no head, so it fails closed."""
    def bad(reason):
        return [ci_check.Violation("-", "merge-group", reason)]

    if not (m := GROUP_REF.match(group_ref)):
        return bad(f"cannot read the PR number from {group_ref!r}")
    branch, base_branch, last = m[1], m[2], int(m[3])
    group_head = api(f"repos/{repo}/git/ref/heads/{branch}")["object"]["sha"]
    commits = api(f"repos/{repo}/compare/{base_branch}...{group_head}?per_page=100")
    if len(commits) >= COMPARE_CAP:
        return bad(f"binding unavailable: compare lists {len(commits)} commits, so it may be truncated")
    by_sha = {c["sha"]: c for c in commits}  # compare also lists each PR's own commits; only the first-parent chain is the queue's
    queued = {}  # PR number -> the head the queue merged
    sha = group_head
    while sha in by_sha:
        c = by_sha[sha]
        parents = c["parents"]
        if len(parents) != 2:
            return bad(f"binding unavailable: group commit {c['sha'][:12]} is not a merge commit, so it records no PR head")
        head = parents[1]["sha"]
        owners = [p["number"] for p in api(f"repos/{repo}/commits/{head}/pulls")
                  if p["state"] == "open" and p["head"]["sha"] == head and p["base"]["ref"] == base_branch]
        if len(owners) != 1:
            return bad(f"binding unavailable: queued head {head[:12]} belongs to {len(owners)} open PRs, not one")
        if owners[0] in queued:
            return bad(f"PR #{owners[0]} is queued by two group commits")
        queued[owners[0]] = head
        sha = parents[0]["sha"]
    if last not in queued:
        return bad(f"binding unavailable: PR #{last} from the ref is not among the group's merged heads")
    found = []
    for pr, head in sorted(queued.items()):
        for v in evaluate(pr, head):
            found += bad(f"PR #{pr} head {head[:12]}: {v.rule} {v.workflow}: {v.reason}")
    return found


def rerun_pr_check(repo: str, head: str, api=gh_api, post=gh_post) -> str:
    """A review was submitted or dismissed: re-run the PR check of the open PR at `head`, so approval withdrawal flips the
    cached verdict. Runs in the trusted receiver; the only input is a head SHA, matched against the repo's own open PRs.
    A re-run keeps the original (trusted, base-branch) definition and re-reads the reviews."""
    prs = [p["number"] for p in api(f"repos/{repo}/pulls?state=open&per_page=100") if p["head"]["sha"] == head]
    if not prs:
        return f"no open PR at {head[:12]}: nothing to re-run"
    runs = [r for r in api(f"repos/{repo}/actions/workflows/ci-policy.yml/runs?event=pull_request_target&per_page=100")
            if r["head_sha"] == head or any(p["number"] in prs for p in r.get("pull_requests", []))]
    if not runs:
        raise RuntimeError(f"no ci-policy run found for PR #{prs[0]} at {head[:12]}")
    run = max(runs, key=lambda r: r["id"])["id"]
    post(f"repos/{repo}/actions/runs/{run}/rerun")
    return f"re-ran ci-policy run {run} for PR #{prs[0]}"


def queue_check(args) -> list:
    """Run in the trusted queue workflow: the group ref is fetched as data; each queued head is checked like a PR."""
    base_branch = m[2] if (m := GROUP_REF.match(args.group_ref)) else args.default_branch

    def evaluate(pr, head):
        return check_pr(args.root, args.repo, pr, f"refs/remotes/origin/{base_branch}", head, args.private, args.default_branch)

    return check_merge_group(args.repo, args.group_ref, evaluate=evaluate)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    pr = sub.add_parser("pr")
    pr.add_argument("--root", type=Path, default=Path("."))
    pr.add_argument("--repo", required=True)
    pr.add_argument("--pr", type=int, required=True)
    pr.add_argument("--base", required=True)
    pr.add_argument("--head", required=True)
    pr.add_argument("--private", action="store_true")
    pr.add_argument("--default-branch", default="main")
    mg = sub.add_parser("merge-group")
    mg.add_argument("--repo", required=True)
    mg.add_argument("--group-ref", required=True)
    mg.add_argument("--root", type=Path, default=Path("."))
    mg.add_argument("--private", action="store_true")
    mg.add_argument("--default-branch", default="main")
    rr = sub.add_parser("rerun-pr")
    rr.add_argument("--repo", required=True)
    rr.add_argument("--head", required=True)
    args = ap.parse_args(argv)
    if args.cmd == "rerun-pr":
        print(rerun_pr_check(args.repo, args.head))
        return 0
    if args.cmd == "pr":
        found = check_pr(args.root, args.repo, args.pr, args.base, args.head, args.private, args.default_branch)
    else:
        found = queue_check(args)
    for v in found:
        print(f"{args.repo} {v.workflow} {v.rule} {v.reason}")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
