#!/usr/bin/env python3
"""ci-policy caller logic (D-6, eng review D10 and D14). Run from the BASE revision's bytes: the head is only read as
data (`git archive`), nothing from it is executed. Needs `git` and, for the API reads, `gh` with GH_TOKEN.

  pr          --repo OWNER/NAME --pr N --base SHA --head SHA [--private] [--default-branch main] [--root DIR]
  merge-group --repo OWNER/NAME --group-ref REF

`pr` runs check.py on the head's workflow files and policy file. A PR that edits `.github/ci-policy*` or the caller
workflow also fails `policy-changed` until a GitHub review APPROVED by a human who is not the author or a bot, with
write access, on the current head SHA. A new push moves the head, so it resets the approval. `merge-group` lists the PRs queued
in the group (the merge commits on the group head's first-parent chain), finds each by its recorded head and copies its latest
`ci-policy` check-run result; a commit whose PR the queue data does not identify fails closed."""
import argparse
import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check as ci_check  # noqa: E402

CHECK_NAME = "ci-policy"
WRITERS = {"OWNER", "MEMBER", "COLLABORATOR"}
VERDICTS = {"APPROVED", "CHANGES_REQUESTED", "DISMISSED"}  # a COMMENTED review changes nothing
COMPARE_CAP = 250  # the compare API lists at most this many commits
GROUP_REF = re.compile(r"^refs/heads/(gh-readonly-queue/(.+)/pr-(\d+)-[0-9a-f]{40})$")


def is_protected(path: str) -> bool:
    return path.startswith(".github/ci-policy") or path == ".github/workflows/ci-policy.yml"


def gh_api(path: str):
    """JSON for one object, or a list for a paginated array (`check_runs` unwrapped)."""
    jq = (".check_runs[]" if "/check-runs" in path else ".commits[]" if "/compare/" in path
          else ".[]" if path.endswith(("/reviews", "/pulls")) else ".")
    out = subprocess.run(["gh", "api", "--paginate", path, "--jq", jq], check=True, capture_output=True, text=True).stdout
    items = [json.loads(line) for line in out.splitlines() if line.strip()]
    return items if jq != "." else items[0]


def approver(reviews: list, head: str, author: str) -> str | None:
    latest = {}
    for r in reviews:  # chronological: the last verdict of each reviewer stands
        if r["state"] in VERDICTS:
            latest[r["user"]["login"]] = r
    return next((login for login, r in latest.items()
                 if r["state"] == "APPROVED" and r["commit_id"] == head and login != author
                 and r["user"]["type"] == "User" and r.get("author_association") in WRITERS), None)


def changed_paths(root: Path, base: str, head: str) -> list[str]:
    out = subprocess.run(["git", "-C", str(root), "-c", "core.quotepath=off", "diff", "--name-only", "--no-renames", f"{base}...{head}"],
                         check=True, capture_output=True, text=True).stdout
    return out.splitlines()


def extract_head(root: Path, head: str, dest: Path) -> None:
    """The head's `.github` tree as plain files; a repo without one yields an empty dir (the checker then reports D-2)."""
    archive = subprocess.run(["git", "-C", str(root), "archive", head, ".github"], capture_output=True)
    if archive.returncode == 0:
        subprocess.run(["tar", "-x", "-C", str(dest)], input=archive.stdout, check=True)


def check_pr(root: Path, repo: str, pr: int, base: str, head: str, private: bool, default_branch: str, api=gh_api) -> list:
    live = api(f"repos/{repo}/pulls/{pr}")
    if live["head"]["sha"] != head:
        return [ci_check.Violation("-", "head-moved", f"PR head is {live['head']['sha'][:12]}, not {head[:12]}: a newer run reports")]
    found = []
    touched = [p for p in changed_paths(root, base, head) if is_protected(p)]
    if touched and not approver(api(f"repos/{repo}/pulls/{pr}/reviews"), head, live["user"]["login"]):
        found.append(ci_check.Violation(touched[0], "policy-changed",
                                        f"PR edits {', '.join(touched)}: needs a non-author human APPROVED review on head {head[:12]}"))
    with tempfile.TemporaryDirectory() as tmp:
        extract_head(root, head, Path(tmp))
        violations, _ = ci_check.check(Path(tmp), repo, private, default_branch)
    return found + violations


def check_merge_group(repo: str, group_ref: str, api=gh_api) -> list:
    """Every PR queued in the group (the ref names only the last one) must have a successful `ci-policy` result.

    Only queue data GitHub itself records identifies a PR. Walk the first-parent chain from the group head down to the
    base branch (the compare response also lists each PR's own commits, which are not queue commits). Each chain commit
    must be a merge commit: its second parent is the PR head the queue merged, and `commits/{head}/pulls` names the one open PR that has that head. Titles and tree
    contents prove nothing about identity and are not read. A squashed commit records no head, so it fails closed."""
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
        runs = api(f"repos/{repo}/commits/{head}/check-runs?check_name={CHECK_NAME}&per_page=100")
        latest = max(runs, key=lambda r: r["id"], default=None)
        if latest is None or latest["status"] != "completed" or latest["conclusion"] != "success":
            state = latest and (latest["conclusion"] or latest["status"])
            found += bad(f"PR #{pr} head {head[:12]}: latest {CHECK_NAME} result is {state or 'missing'}, not success")
    return found


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
    args = ap.parse_args(argv)
    if args.cmd == "pr":
        found = check_pr(args.root, args.repo, args.pr, args.base, args.head, args.private, args.default_branch)
    else:
        found = check_merge_group(args.repo, args.group_ref)
    for v in found:
        print(f"{args.repo} {v.workflow} {v.rule} {v.reason}")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
