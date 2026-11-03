#!/usr/bin/env python3
"""pr-ready (CHE-3440): block a merge while the PR body says the work is unfinished.

  pull_request_target  the PR's live body must carry no unfinished marker (WIP, Status: Draft, do not merge, ...).
  merge_group          every PR in the queue group must pass that body check and, where REQUIRE_SECURITY_GATE=1,
                       carry a `security-gate` success status on its CURRENT head. security-gate is the trusted
                       judge of the signed cross-lab PASS; a PASS string in a body is never read here.

The group is rebuilt from the first-parent chain base_sha..head_sha: each entry is one queue commit, named by its
`Merge pull request #N` or `... (#N)` subject. An entry with no number, a missing commit or an empty group fails closed.

Run from a checkout of the DEFAULT branch only. This file never reads or runs anything from a PR head; the body is data.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import unicodedata

# ponytail: bare `blocked` is a deliberate false-positive trade for no false negative; the message names the marker
# so the author rewords. Upgrade path: scope it to a status line if authors keep tripping on it.
MARKERS = (
    ("status draft/wip/blocked", r"\bstatus\b *:? *(?:draft|wip|blocked|not ready|on hold|do not merge|in progress)\b"),
    ("wip", r"\bwip\b"),
    ("do not merge", r"\bdo[ -]?not[ -]merge\b|\bdon'?t[ -]merge\b"),
    ("not ready", r"\bnot[ -]ready\b"),
    ("keep in draft", r"\bkeep[ -](?:it[ -])?(?:in|as)?[ -]?draft\b"),
    ("draft candidate", r"\bdraft[ -]candidate\b"),
    ("stacked on", r"\bstacked[ -]on\b"),
    ("merge after", r"\bmerge[ -]after\b"),
    ("blocked", r"\bblocked\b"),
    ("review pending", r"\b(?:cross[ -]lab|independent)\b[^.]{0,30}\bpending\b|\bpending\b[^.]{0,20}\b(?:cross[ -]lab|independent)\b"),
)
MARKER_RES = [(name, re.compile(rx)) for name, rx in MARKERS]
QUEUE_SUBJECT = (re.compile(r"^Merge pull request #(\d+) "), re.compile(r"\(#(\d+)\)$"))
GATE_CONTEXT, GATE_CREATOR = "security-gate", "github-actions[bot]"


def unfinished(body: str) -> str | None:
    """The first unfinished marker in `body` (any markdown form), else None. Markdown punctuation and invisible
    characters are dropped and whitespace is collapsed, so `**Status:** Draft` and `## Status\\n\\nDraft` both read
    `status draft`. HTML comments and ~~struck~~ text are not read by a human, so they are ignored."""
    text = re.sub(r"<!--.*?-->|~~.*?~~", " ", body or "", flags=re.S)
    text = "".join(c for c in unicodedata.normalize("NFKC", text) if unicodedata.category(c) != "Cf")
    text = " ".join(re.sub(r"[*_`~#>|\[\]()]", " ", text).lower().split())
    return next((name for name, rx in MARKER_RES if rx.search(text)), None)


def gate_problem(statuses: list[dict]) -> str | None:
    """Why `statuses` (of one head SHA) do not prove a security-gate PASS, else None."""
    gate = next((s for s in statuses if s["context"] == GATE_CONTEXT), None)
    if gate is None:
        return "no security-gate status on the current head"
    if gate["state"] != "success":
        return f"security-gate is {gate['state']} on the current head"
    if gate["creator"]["login"] != GATE_CREATOR:
        return f"security-gate status was posted by {gate['creator']['login']}, not {GATE_CREATOR}"
    return None


def queue_prs(commits: list[dict], base_sha: str, head_sha: str, head_ref: str) -> tuple[set[int], list[str]]:
    """(PR numbers in the group, problems). `commits` are compare-API commits."""
    by_sha = {c["sha"]: c for c in commits}
    nums, problems, sha = set(), [], head_sha
    while sha != base_sha:
        c = by_sha.get(sha)
        if c is None or not c["parents"]:
            problems.append(f"queue commit {sha[:12]} is not in the base..head range")
            break
        subject = c["commit"]["message"].splitlines()[0] if c["commit"]["message"] else ""
        m = next((m for m in (rx.search(subject) for rx in QUEUE_SUBJECT) if m), None)
        if m:
            nums.add(int(m[1]))
        else:
            problems.append(f"queue commit {sha[:12]} names no PR: {subject[:60]!r}")
        sha = c["parents"][0]["sha"]
    m = re.search(r"/pr-(\d+)-", head_ref)
    if m:
        nums.add(int(m[1]))
    if not nums:
        problems.append("no PR found in the merge group")
    return nums, problems


def check_pr(gh, repo: str, number: int, require_gate: bool) -> list[str]:
    pull = gh(f"repos/{repo}/pulls/{number}")
    out = []
    if pull["state"] != "open":
        out.append(f"#{number} is {pull['state']}")
    marker = unfinished(pull["body"])
    if marker:
        out.append(f"#{number} body says unfinished ({marker}): set Status to Ready after the PASS lands at the final head")
    if require_gate:
        why = gate_problem(gh(f"repos/{repo}/commits/{pull['head']['sha']}/status?per_page=100")["statuses"])
        if why:
            out.append(f"#{number}: {why}")
    return out


def group_commits(gh, repo: str, base_sha: str, head_sha: str) -> list[dict]:
    commits, page = [], 1
    while True:
        d = gh(f"repos/{repo}/compare/{base_sha}...{head_sha}?per_page=100&page={page}")
        commits += d["commits"]
        if not d["commits"] or len(commits) >= d.get("total_commits", len(commits)):
            return commits
        page += 1


def run(env: dict, gh) -> list[str]:
    """Problems for this event; empty means pass."""
    repo, require_gate = env["REPO"], env.get("REQUIRE_SECURITY_GATE") == "1"
    if env["EVENT"] == "merge_group":
        nums, problems = queue_prs(group_commits(gh, repo, env["BASE_SHA"], env["HEAD_SHA"]),
                                   env["BASE_SHA"], env["HEAD_SHA"], env["HEAD_REF"])
        return problems + [p for n in sorted(nums) for p in check_pr(gh, repo, n, require_gate)]
    if env["EVENT"] == "pull_request_target":
        # Only the body is judged on a PR event: security-gate is its own required check and runs beside this one.
        return check_pr(gh, repo, int(env["PR_NUMBER"]), False)
    return [f"unsupported event {env['EVENT']}"]


def _gh(path: str) -> dict:
    return json.loads(subprocess.run(["gh", "api", path], check=True, capture_output=True, text=True).stdout)


def main() -> int:
    try:
        problems = run(os.environ, _gh)
    except (subprocess.CalledProcessError, KeyError, ValueError) as e:  # cannot read: never a silent pass
        problems = [f"cannot evaluate: {type(e).__name__}: {str(e)[-120:]}"]
    for p in problems:
        print(f"::error::{p}")
    print("pr-ready: " + ("FAIL" if problems else "ok"))
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
