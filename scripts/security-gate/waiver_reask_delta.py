#!/usr/bin/env python3
"""CHE-4760 A3: list the PR files changed between a waived head and a new head, and mark each waived case whose file is in the list.

A waiver re-ask runs this and pastes the output; it replaces any prose "the files are unchanged" claim.
  python3 scripts/security-gate/waiver_reask_delta.py --github cheese-work/multica --pr 1016 \
    --old 41543b913 --new 0effbd706 --case "lazy schema gate=packages/core/api/schemas.ts"
Exit 0: list complete. When GitHub caps the compare at 300 files (a big main merge), the helper compares each PR file's blob id
at both heads through one GraphQL read. Exit 2: neither read worked, so the list may miss files: do not claim "unchanged"."""
from __future__ import annotations

import argparse
import json
import subprocess
import sys

COMPARE_FILE_CAP = 300  # GitHub's compare endpoint returns at most this many files


def _gh(*args: str) -> str:
    return subprocess.run(["gh", "api", *args], check=True, capture_output=True, text=True).stdout


def blob_changed_files(github: str, pr_files: list[str], old: str, new: str) -> list[str]:
    """PR files whose blob id differs between the two heads (a missing file counts as its own value). One GraphQL read."""
    owner, name = github.split("/", 1)
    parts = []
    for i, f in enumerate(pr_files):
        for alias, sha in (("a", old), ("b", new)):
            parts.append(f"{alias}{i}: object(expression: {json.dumps(f'{sha}:{f}')}) {{ oid }}")
    query = f"query {{ repository(owner: {json.dumps(owner)}, name: {json.dumps(name)}) {{ {' '.join(parts)} }} }}"
    repo = json.loads(_gh("graphql", "-f", f"query={query}"))["data"]["repository"]
    oid = lambda k: (repo[k] or {}).get("oid")
    return [f for i, f in enumerate(pr_files) if oid(f"a{i}") != oid(f"b{i}")]


def parse_compare(lines: list[str]) -> list[tuple[str, str]]:
    """Compare lines are `filename` or `filename<TAB>previous_filename` (a rename between the two heads)."""
    return [(n, prev) for n, _, prev in (line.partition("\t") for line in lines if line)]


def delta(compare: list[tuple[str, str]], pr_files: list[str], cases: dict[str, str]) -> tuple[list[str], dict[str, bool | None]]:
    """PR files that also changed between the two heads (compare order), and per case: True if its path is among them
    (current or pre-rename name), False if the path was seen and is not, None (unknown) if the path is neither a PR file
    nor in the compare, e.g. a waived file renamed or removed where the compare cannot show it."""
    in_pr = set(pr_files)
    hit = [(n, prev) for n, prev in compare if n in in_pr or prev in in_pr]
    changed = [n for n, _ in hit]
    names = {x for pair in hit for x in pair if x}
    seen = names | in_pr | {x for pair in compare for x in pair if x}
    return changed, {name: True if path in names else (False if path in seen else None) for name, path in cases.items()}


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--github", required=True, help="owner/repo")
    ap.add_argument("--pr", type=int, required=True)
    ap.add_argument("--old", required=True, help="the waived head")
    ap.add_argument("--new", required=True, help="the new head")
    ap.add_argument("--case", action="append", default=[], metavar="NAME=PATH", help="a waived case and its file (repeatable)")
    a = ap.parse_args(argv)
    cases = {}
    for c in a.case:
        name, sep, path = c.partition("=")
        if not (sep and name.strip() and path.strip()):
            ap.error(f"--case needs NAME=PATH, got {c!r}")
        cases[name.strip()] = path.strip()
    compare = _gh(f"repos/{a.github}/compare/{a.old}...{a.new}", "--paginate", "--jq", ".files[]? | [.filename, (.previous_filename // \"\")] | @tsv").splitlines()
    pr_files = _gh(f"repos/{a.github}/pulls/{a.pr}/files", "--paginate", "--jq", ".[].filename").splitlines()
    compare = parse_compare(compare)
    changed, marks = delta(compare, pr_files, cases)
    capped, how = len(compare) >= COMPARE_FILE_CAP, "compare"
    if capped:
        try:
            changed, marks = delta([(f, "") for f in blob_changed_files(a.github, pr_files, a.old, a.new)], pr_files, cases)
            capped, how = False, f"blob ids (compare hit the {COMPARE_FILE_CAP}-file cap)"
        except (subprocess.CalledProcessError, KeyError, TypeError, ValueError) as e:
            print(f"blob-id read failed: {type(e).__name__}", file=sys.stderr)
    print(f"Re-ask delta {a.github} PR {a.pr}: {a.old}...{a.new}")
    print(f"PR files changed since the waived head ({len(changed)}), by {how}:")
    print("\n".join(f"- {f}" for f in changed) or "- none")
    if capped:
        print(f"INCOMPLETE: compare returned {len(compare)} files, the GitHub cap. The list may miss PR files; case marks are unknown.")
    else:
        print("Waived cases:")
        print("\n".join(f"- {n}: { {True: 'CHANGED', False: 'not in the list', None: 'UNKNOWN'}[hit] } ({cases[n]})" for n, hit in marks.items()) or "- none given")
    print(f"Command: gh api repos/{a.github}/compare/{a.old}...{a.new} --paginate --jq '.files[]? | [.filename, (.previous_filename // \"\")] | @tsv'"
          + ("; then one GraphQL `object(expression: \"<sha>:<path>\") { oid }` pair per PR file" if "blob" in how else ""))
    return 2 if capped else 0


if __name__ == "__main__":
    sys.exit(main())
