#!/usr/bin/env python3
"""Pre-merge security gate (CHE-2671).

Sets the `security-gate` commit status on a PR's exact head SHA:
  success  a valid cross-lab PASS verdict for this SHA, no FAIL, no secret in the added lines
  failure  a valid FAIL verdict, an RGS-1 violation, or a secret in the added lines
  pending  no verdict for this SHA, or only malformed ones (a new push lands here)

Verdict = a PR comment by a trusted workspace bot:
  Security-Gate-Verdict: PASS|FAIL
  Reviewer: <agent-name> (<model>)
  Signed: <agent-name>, <date time ICT>
  Repo: <owner/repo>
  PR: <number>
  Base: <target branch>
  SHA: <40 hex>
  Findings: none          (FAIL: lines `- path:line <exploit path>`)

A verdict counts only for the repo, PR number, base branch and head SHA it names. Only PRs into the default
branch are supported. The PR author must be a registered agent (lab from the `agent/<name>/` branch) or a
registered human; anyone else fails closed.

Run from a checkout of the DEFAULT branch only; the head is fetched for its diff, never executed.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys

CONTEXT = "security-gate"
TRUSTED_AUTHORS = "skills/multica-cheese-work-github/scripts/trusted-authors.json"
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
HUNK_RE = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@")
FINDING_RE = re.compile(r"^- (\S+:\d+)\s+\S")
REVIEWER_RE = re.compile(r"^(\S+) \(([^)]+)\)$")
# ponytail: regex floor, not a scanner; swap in gitleaks when the X99 runner ships it.
SECRET_RES = [re.compile(p) for p in (
    r"-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----",
    r"\bAKIA[0-9A-Z]{16}\b",
    r"\bgh[pousr]_[A-Za-z0-9]{36,}\b",
    r"\bxox[abprs]-[A-Za-z0-9-]{10,}",
    r"\bsk-[A-Za-z0-9_-]{32,}",
)]


def lab_of(name: str) -> str | None:
    n = name.lower()
    for lab, words in (("anthropic", ("claude",)), ("openai", ("codex", "gpt", "hermes")), ("google", ("gemini", "agy"))):
        if any(w in n for w in words):
            return lab
    return None


def author_agent(head_ref: str) -> str | None:
    m = re.match(r"^agent/([^/]+)/", head_ref)
    return m.group(1) if m else None


def scan_secrets(diff_text: str) -> list[str]:
    """Scan every added line of a `-U0` diff. Header lines are recognised only outside a hunk: an added
    line `++ 1` renders as `+++ 1` and must not be mistaken for a file header (it would skip the scan)."""
    hits, path, line, old_left, new_left = [], "?", 0, 0, 0
    for raw in diff_text.splitlines():
        if old_left or new_left:  # inside a hunk: the counts from `@@ -a,b +c,d @@` say what is body
            if raw.startswith("+"):
                new_left -= 1
                line += 1
                if any(r.search(raw) for r in SECRET_RES):
                    hits.append(f"{path}:{line}")
            elif raw.startswith("-"):
                old_left -= 1
            elif not raw.startswith("\\"):
                old_left, new_left, line = old_left - 1, new_left - 1, line + 1
        elif raw.startswith("+++ "):
            path = raw[4:].removeprefix("b/")
        elif raw.startswith("@@"):
            m = HUNK_RE.match(raw)
            if m:
                old_left, new_left = int(m[2] or 1), int(m[4] or 1)
                line = int(m[3]) - 1
    return hits


def parse_verdict(body: str) -> tuple[dict | None, str]:
    lines = body.replace("\r", "").splitlines()
    start = next((i for i, l in enumerate(lines) if l.startswith("Security-Gate-Verdict:")), None)
    if start is None:
        return None, ""
    fields, findings, in_findings = {}, [], False
    for l in lines[start:]:
        key, _, val = l.partition(":")
        if key in ("Security-Gate-Verdict", "Reviewer", "Signed", "Repo", "PR", "Base", "SHA", "Findings"):
            fields[key] = val.strip()
            in_findings = key == "Findings"
        elif in_findings and l.startswith("- "):
            findings.append(l.strip())
    result, rev = fields.get("Security-Gate-Verdict"), REVIEWER_RE.match(fields.get("Reviewer", ""))
    if result not in ("PASS", "FAIL"):
        return None, "result must be PASS or FAIL"
    if not rev:
        return None, "Reviewer must be `<agent> (<model>)`"
    if not fields.get("Signed"):
        return None, "missing Signed"
    if not SHA_RE.match(fields.get("SHA", "")):
        return None, "SHA must be 40 hex"
    if not (fields.get("Repo") and fields.get("PR", "").isdigit() and fields.get("Base")):
        return None, "missing Repo, PR or Base"
    if result == "PASS" and (fields.get("Findings", "").lower() != "none" or findings):
        return None, "PASS needs `Findings: none`"
    if result == "FAIL" and not any(FINDING_RE.match(f) for f in findings):
        return None, "FAIL needs `- path:line <exploit path>`"
    return {"result": result, "agent": rev.group(1), "model": rev.group(2), "sha": fields["SHA"],
            "repo": fields["Repo"], "pr": int(fields["PR"]), "base": fields["Base"], "findings": findings}, ""


def evaluate(pr: dict, comments: list[dict], trusted_ids: set[int], diff_text: str,
             human_ids: frozenset[int] | set[int] = frozenset()) -> tuple[str, str]:
    head, why = pr["head_sha"], []
    if pr["base_ref"] != pr["default_branch"]:
        return _out("failure", f"unsupported target {pr['base_ref']}: only {pr['default_branch']} is gated")
    secrets = scan_secrets(diff_text)
    if secrets:
        return _out("failure", f"secret in added lines: {', '.join(secrets[:3])}")
    # Authorship comes from the PR author's registered identity, never from a branch name alone.
    if pr["author_id"] in human_ids:
        author, author_lab = None, "human"
    elif pr["author_id"] in trusted_ids:
        author = author_agent(pr["head_ref"])
        author_lab = lab_of(author) if author else None
        if author_lab is None:
            return _out("failure", f"author agent unknown ({author or pr['head_ref']}): no lab, fail closed")
    else:
        return _out("failure", f"PR author {pr['author_id']} is not a registered agent or human: fail closed")
    latest: dict[str, dict] = {}
    for c in sorted((c for c in comments if c["author_id"] in trusted_ids), key=lambda c: (c["updated_at"], c["id"])):
        v, err = parse_verdict(c["body"])
        if v is None:
            if err:
                why.append(err)
            continue
        if v["sha"] != head:
            why.append(f"verdict is for {v['sha'][:12]}, head is {head[:12]}")
            continue
        if (v["repo"], v["pr"], v["base"]) != (pr["repo"], pr["number"], pr["base_ref"]):
            why.append(f"verdict is for {v['repo']}#{v['pr']} into {v['base']}, not this PR")
            continue
        latest[v["agent"]] = v
    passes = []
    for agent, v in latest.items():
        lab = lab_of(agent)
        if lab is None or lab_of(v["model"]) not in (None, lab):
            why.append(f"reviewer {agent}: lab unknown or model mismatch")
        elif agent == author or lab == author_lab:
            why.append(f"reviewer {agent} fails RGS-1 (author lab {author_lab})")
            latest[agent] = {**v, "rgs1": True}
        elif v["result"] == "FAIL":
            find = next(f for f in v["findings"] if FINDING_RE.match(f))
            return _out("failure", f"FAIL by {agent}: {find[2:]}")
        else:
            passes.append(agent)
    if passes:
        return _out("success", f"PASS on {head[:12]} by {', '.join(passes)}")
    if any(v.get("rgs1") for v in latest.values()):
        return _out("failure", why[-1])
    return _out("pending", f"no valid verdict for {head[:12]}" + (f" ({why[-1]})" if why else ""))


def combine(results: list[tuple[str, str]]) -> tuple[str, str]:
    """One commit status per SHA, but several open PRs can share a head: the worst state wins, so a PASS
    earned on one PR never unblocks another."""
    order = {"failure": 0, "pending": 1, "success": 2}
    return min(results, key=lambda r: order[r[0]])


def _out(state: str, desc: str) -> tuple[str, str]:
    return state, desc[:140]  # GitHub caps a status description at 140 chars


def _gh(*args: str) -> str:
    return subprocess.run(["gh", "api", *args], check=True, capture_output=True, text=True).stdout


def gate_pr(repo: str, github: str, number: int, default_branch: str, trusted: set[int],
            humans: set[int]) -> tuple[str, str, str]:
    pull = json.loads(_gh(f"repos/{github}/pulls/{number}"))
    head, base = pull["head"]["sha"], pull["base"]["sha"]
    # no --slurp: the X99 gh (2.45) lacks it; --jq emits one compact object per line across pages
    lines = _gh("--paginate", f"repos/{github}/issues/{number}/comments", "--jq",
                '.[] | {id, author_id: .user.id, body: (.body // ""), updated_at}').splitlines()
    comments = [json.loads(l) for l in lines]
    diff = ""  # an unsupported target fails before the diff is read; its base may not be in this checkout
    if pull["base"]["ref"] == default_branch:
        diff = subprocess.run(["git", "-C", repo, "diff", "-U0", "--no-color", f"{base}...{head}"],
                              check=True, capture_output=True, text=True).stdout
    state, desc = evaluate({"head_sha": head, "head_ref": pull["head"]["ref"], "base_ref": pull["base"]["ref"],
                            "default_branch": default_branch, "author_id": pull["user"]["id"],
                            "repo": github, "number": number}, comments, trusted, diff, humans)
    return head, state, desc


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--repo", default=".", help="DEFAULT-branch checkout with the head objects fetched")
    ap.add_argument("--github", required=True, help="owner/repo")
    ap.add_argument("--pr", type=int, required=True)
    ap.add_argument("--default-branch", required=True)
    ap.add_argument("--run-url", default="")
    ap.add_argument("--post", action="store_true", help="write the commit status (default: print only)")
    a = ap.parse_args()
    with open(f"{a.repo}/{TRUSTED_AUTHORS}") as f:
        registry = json.load(f)
    trusted = {b["id"] for b in registry["workspace_bots"]}
    humans = {h["id"] for h in registry["instruction_authors"]}
    head, state, desc = gate_pr(a.repo, a.github, a.pr, a.default_branch, trusted, humans)
    # The API also lists PRs that merely contain the commit (descendants, forks) with a different head. Their
    # head objects may not be fetched, so they are filtered here, before any diff is read.
    rows = [json.loads(l) for l in _gh(f"repos/{a.github}/commits/{head}/pulls", "--jq",
                                       '.[] | select(.state=="open") | {number, head: .head.sha}').splitlines()]
    results = [(state, desc)]
    for n in sorted({r["number"] for r in rows if r["head"] == head} - {a.pr}):
        try:
            h, st, d = gate_pr(a.repo, a.github, n, a.default_branch, trusted, humans)
        except Exception as e:  # an unreadable sibling must block, never skip the status write
            h, st, d = head, "failure", f"cannot evaluate: {type(e).__name__}"
        if h == head:
            results.append((st, f"#{n}: {d}"))
    state, desc = _out(*combine(results))
    print(json.dumps({"context": CONTEXT, "sha": head, "state": state, "description": desc}))
    if a.post:
        _gh(f"repos/{a.github}/statuses/{head}", "-f", f"state={state}", "-f", f"context={CONTEXT}",
            "-f", f"description={desc}", *(["-f", f"target_url={a.run_url}"] if a.run_url else []))
    return 0


if __name__ == "__main__":
    sys.exit(main())
