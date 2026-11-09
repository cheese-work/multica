#!/usr/bin/env python3
"""List the open PRs whose `security-gate` status may not have read their newest verdict comment (CHE-4655).

A new `Security-Gate-Verdict:` comment no longer starts a gate run; the scheduled sweep evaluates it. A PR is selected when
a comment that holds the header was updated after its head's status (minus a margin for the CHE-4389 race: a verdict can
land after a run read the comments and before it wrote the status). Selection is cheap and loose on purpose: the gate
script still checks author trust, exact head SHA, base and signers, and an extra evaluation is idempotent.

Prints a JSON array of PR numbers. Read-only: it writes nothing. Edited and deleted comments still start the gate at once.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from datetime import datetime, timedelta

HEADER = "Security-Gate-Verdict:"
CONTEXT = "security-gate"
MARGIN = timedelta(minutes=30)  # gate timeout is 20 min: a verdict can land during a whole evaluation


def _instant(ts: str) -> datetime:
    return datetime.fromisoformat(ts.replace("Z", "+00:00"))


def targets(prs: list[dict], statuses: dict[str, dict], comments: dict[int, list[dict]]) -> list[int]:
    """`prs`: [{number, head}]; `statuses`: head sha -> {state, updated_at} of the `security-gate` context (absent = none);
    `comments`: PR number -> [{body, updated_at}]."""
    out = set()
    for pr in prs:
        status = statuses.get(pr["head"])
        floor = _instant(status["updated_at"]) - MARGIN if status else None
        for c in comments.get(pr["number"], []):
            if HEADER in (c.get("body") or "") and (floor is None or _instant(c["updated_at"]) > floor):
                out.add(pr["number"])
                break
    return sorted(out)


def _gh(*args: str) -> str:
    return subprocess.run(["gh", "api", *args], check=True, capture_output=True, text=True).stdout


def list_targets(github: str, default_branch: str) -> list[int]:
    # Each read is its own call so a failed API request fails the run; an empty list is never assumed (no `|| true`).
    prs = [json.loads(l) for l in _gh("--paginate", f"repos/{github}/pulls?state=open&base={default_branch}&per_page=100",
                                      "--jq", '.[] | {number, head: .head.sha}').splitlines()]
    statuses, comments = {}, {}
    for pr in prs:
        raw = _gh(f"repos/{github}/commits/{pr['head']}/status", "--jq",
                  f'[.statuses[] | select(.context == "{CONTEXT}")][0] // empty | {{state, updated_at}}')
        if raw.strip():
            statuses[pr["head"]] = json.loads(raw)
        comments[pr["number"]] = [json.loads(l) for l in _gh(
            "--paginate", f"repos/{github}/issues/{pr['number']}/comments?per_page=100", "--jq",
            '.[] | {body: (.body // ""), updated_at}').splitlines()]
    return targets(prs, statuses, comments)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--github", required=True, help="owner/repo")
    ap.add_argument("--default-branch", required=True)
    a = ap.parse_args()
    print(json.dumps(list_targets(a.github, a.default_branch)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
