#!/usr/bin/env python3
"""Pre-merge security gate (CHE-2671).

Sets the `security-gate` commit status on a PR's exact head SHA:
  success  a valid cross-lab PASS verdict for this SHA, no FAIL, no secret in the added lines, no new semgrep finding
  failure  a valid FAIL verdict, an RGS-1 violation, a secret in the added lines, or a new semgrep ERROR (CHE-3121)
  pending  no verdict for this SHA, or only malformed ones (a new push lands here); semgrep could not run

Verdict = a PR comment by a trusted workspace bot:
  Security-Gate-Verdict: PASS|FAIL
  Reviewer: <agent-name> (<model>)
  Signed: <agent-name>, <date time ICT>
  Repo: <owner/repo>
  PR: <number>
  Base: <target branch>
  SHA: <40 hex>
  Diff-Digest: <64 hex>   (optional; `pr_diff_digest` of the PR at that SHA, CHE-3610)
  Findings: none          (FAIL: lines `- path:line <exploit path>`)

A verdict counts only for the repo, PR number, base branch and head SHA it names. A verdict that also names a
`Diff-Digest` keeps counting on a later head whose digest is identical: a clean merge from the default branch that
leaves every PR file alone is no reason to review again; any change to a PR file changes the digest (CHE-3610). Only PRs into the default
branch are gated; a PR into another base is pending ("not gated yet") until it is retargeted. The PR author
must be a registered agent (lab from the `agent/<name>/` branch) or a registered human; anyone else fails closed.

A PR whose own status is not `success` also flips any merge-queue head carrying it to failure (`revoke_queue`,
CHE-3240), so a success carried onto a queue commit cannot outlive its verdict.

Run from a checkout of the DEFAULT branch only; the head is fetched for its diff, never executed.
"""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
import re
import subprocess
import sys
import tarfile
import tempfile
import time

CONTEXT = "security-gate"
TRUSTED_AUTHORS = "skills/multica-cheese-work-github/scripts/trusted-authors.json"
SEMGREP_BASELINE = "scripts/security-gate/semgrep-baseline.json"
SEMGREP_CONFIGS = ("p/default", "p/golang", "p/gosec", "p/typescript", "p/python", "p/kotlin")
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
DIGEST_RE = re.compile(r"^[0-9a-f]{64}$")
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


def pr_diff_digest(repo: str, base: str, head: str) -> str:
    """SHA-256 of the full `git diff <merge-base> <head>` over the files the PR changes, `index` lines (blob hashes)
    included and headers unstripped. `--raw` adds both blob ids and modes for every path: a mode-only change has no
    `index` line in the patch, so without it a main-branch edit to that file would not move the digest.
    A merge from the default branch that changes a PR file moves the merge-base blob on the `-` side, so it changes
    the digest too."""
    def git(*a: str) -> bytes:  # raw bytes: text mode would turn a \r in a file name into \n and drop the file
        return subprocess.run(["git", "--literal-pathspecs", "-C", repo, *a], check=True, capture_output=True).stdout
    mb = git("merge-base", base, head).decode().strip()
    names = [n for n in git("diff", "--name-only", "-z", "--no-renames", mb, head).split(b"\0") if n]
    full = git("diff", "--raw", "--patch", "--full-index", "--no-color", "--no-ext-diff", "--no-renames", mb, head,
               "--", *names)
    return hashlib.sha256(full).hexdigest()


def lab_of(name: str) -> str | None:
    n = name.lower()
    for lab, words in (("anthropic", ("claude",)), ("openai", ("codex", "gpt", "hermes")), ("google", ("gemini", "agy"))):
        if any(w in n for w in words):
            return lab
    return None


def author_agent(head_ref: str) -> str | None:
    m = re.match(r"^agent/([^/]+)/", head_ref)
    return m.group(1) if m else None


def added_lines(diff_text: str):
    """Yield (path, line, text) for every added line of a `-U0` diff. Header lines are recognised only outside a
    hunk: an added line `++ 1` renders as `+++ 1` and must not be mistaken for a file header (it would skip the scan)."""
    path, line, old_left, new_left = "?", 0, 0, 0
    for raw in diff_text.splitlines():
        if old_left or new_left:  # inside a hunk: the counts from `@@ -a,b +c,d @@` say what is body
            if raw.startswith("+"):
                new_left -= 1
                line += 1
                yield path, line, raw
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


def scan_secrets(diff_text: str) -> list[str]:
    return [f"{path}:{line}" for path, line, raw in added_lines(diff_text) if any(r.search(raw) for r in SECRET_RES)]


class SemgrepError(Exception):
    pass


def semgrep_scan(repo: str, rev: str, paths: list[str] | None = None) -> list[dict]:
    """ERROR-severity semgrep findings of `rev`: the whole tree, or only `paths`. Static scan of an extracted copy;
    nothing from the revision runs. Only the default branch's .semgrepignore is used (the revision's are deleted), `--disable-nosem` stops a PR from
    silencing a rule inline, and the `data` tar filter refuses links that point outside the copy."""
    with tempfile.TemporaryDirectory() as root:
        chunks = [[]] if paths is None else [paths[i:i + 200] for i in range(0, len(paths), 200)]
        for chunk in chunks:
            try:
                tar = subprocess.run(["git", "--literal-pathspecs", "-C", repo, "archive", rev, "--", *chunk],
                                     check=True, capture_output=True).stdout
                with tarfile.open(fileobj=io.BytesIO(tar)) as t:
                    t.extractall(root, filter="data")
            except (subprocess.CalledProcessError, tarfile.TarError) as e:
                raise SemgrepError(f"cannot read {rev[:12]}: {type(e).__name__}") from e
        for dirpath, _, names in os.walk(root):  # an ignore file from the revision is attacker data: drop all of them
            if ".semgrepignore" in names:
                os.unlink(os.path.join(dirpath, ".semgrepignore"))
        ignore = os.path.join(repo, ".semgrepignore")
        if os.path.exists(ignore):
            with open(ignore, "rb") as src, open(os.path.join(root, ".semgrepignore"), "wb") as dst:
                dst.write(src.read())
        cmd = [os.environ.get("SEMGREP_BIN", "semgrep"), "scan", "--metrics=off", "--disable-version-check",
               "--disable-nosem", "--quiet", "--json", "--timeout", "30",
               *(a for c in SEMGREP_CONFIGS for a in ("--config", c)), "."]
        try:
            proc = subprocess.run(cmd, cwd=root, capture_output=True, text=True, timeout=480)
        except (OSError, subprocess.TimeoutExpired) as e:
            raise SemgrepError(type(e).__name__) from e
        if proc.returncode != 0:
            raise SemgrepError(f"exit {proc.returncode}: {proc.stderr.strip()[-80:]}")
        found = []
        for r in json.loads(proc.stdout)["results"]:
            if r["extra"]["severity"] != "ERROR":
                continue
            start, end = r["start"]["line"], r["end"]["line"]
            with open(os.path.join(root, r["path"]), errors="replace") as f:
                text = " ".join(l.strip() for l in f.read().splitlines()[start - 1:end])
            fp = f"{r['check_id']}|{r['path']}|{hashlib.sha256(text.encode()).hexdigest()[:16]}"
            found.append({"fp": fp, "path": r["path"], "start": start, "end": end, "rule": r["check_id"].rsplit(".", 1)[-1]})
        return found


def new_semgrep(findings: list[dict], baseline: list[str], diff_text: str) -> list[str]:
    """Findings on lines this PR adds that the committed baseline does not already absorb (a multiset: each
    baseline entry covers one finding)."""
    added = {(p, n) for p, n, _ in added_lines(diff_text)}
    left = list(baseline)
    on_added = lambda f: any((f["path"], n) in added for n in range(f["start"], f["end"] + 1))
    ordered = sorted(findings, key=lambda f: (f["path"], f["start"]))
    # Untouched findings consume their baseline entries first, so an identical finding the PR adds in the same
    # file cannot borrow one that belongs to code the PR did not touch.
    out = []
    for f in [f for f in ordered if not on_added(f)] + [f for f in ordered if on_added(f)]:
        if f["fp"] in left:
            left.remove(f["fp"])
        elif on_added(f):
            out.append(f"{f['path']}:{f['start']} {f['rule']}")
    return sorted(out)


def load_baseline(repo: str) -> list[str]:
    try:
        with open(f"{repo}/{SEMGREP_BASELINE}") as f:
            return json.load(f)["findings"]
    except (OSError, ValueError, KeyError) as e:
        raise SemgrepError(f"baseline unreadable: {type(e).__name__}") from e


def parse_verdict(body: str) -> tuple[dict | None, str]:
    lines = body.replace("\r", "").splitlines()
    start = next((i for i, l in enumerate(lines) if l.startswith("Security-Gate-Verdict:")), None)
    if start is None:
        return None, ""
    fields, findings, in_findings = {}, [], False
    for l in lines[start:]:
        key, _, val = l.partition(":")
        if key in ("Security-Gate-Verdict", "Reviewer", "Signed", "Repo", "PR", "Base", "SHA", "Diff-Digest", "Findings"):
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
    if "Diff-Digest" in fields and not DIGEST_RE.match(fields["Diff-Digest"]):
        return None, "Diff-Digest must be 64 hex"
    if not (fields.get("Repo") and fields.get("PR", "").isdigit() and fields.get("Base")):
        return None, "missing Repo, PR or Base"
    if result == "PASS" and (fields.get("Findings", "").lower() != "none" or findings):
        return None, "PASS needs `Findings: none`"
    if result == "FAIL" and not any(FINDING_RE.match(f) for f in findings):
        return None, "FAIL needs `- path:line <exploit path>`"
    return {"result": result, "agent": rev.group(1), "model": rev.group(2), "sha": fields["SHA"],
            "repo": fields["Repo"], "pr": int(fields["PR"]), "base": fields["Base"], "digest": fields.get("Diff-Digest"), "findings": findings}, ""


def evaluate(pr: dict, comments: list[dict], trusted_ids: set[int], diff_text: str,
             human_ids: frozenset[int] | set[int] = frozenset(), semgrep: list[str] | None = None,
             digest: str | None = None) -> tuple[str, str]:
    """`digest` = `pr_diff_digest` of the current head; None = not computed, so only an exact SHA match counts."""
    head, why = pr["head_sha"], []
    if pr["base_ref"] != pr["default_branch"]:
        # A stacked layer is gated when it is retargeted to the default branch (the `edited` event re-runs this).
        # Pending, not failure: no red check on a layer that merges into its parent, and a missed retarget event
        # still cannot let the head reach the default branch.
        return _out("pending", f"not gated yet: base {pr['base_ref']} is not {pr['default_branch']}; gated on retarget")
    secrets = scan_secrets(diff_text)
    if secrets:
        return _out("failure", f"secret in added lines: {', '.join(secrets[:3])}")
    if semgrep:  # None = semgrep not run for this call
        return _out("failure", f"semgrep: {len(semgrep)} new ERROR finding(s): {', '.join(semgrep[:2])}")
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
        if (v["repo"], v["pr"], v["base"]) != (pr["repo"], pr["number"], pr["base_ref"]):
            why.append(f"verdict is for {v['repo']}#{v['pr']} into {v['base']}, not this PR")
            continue
        applies = v["sha"] == head or bool(digest and v["digest"] == digest)
        # A verdict that does not apply to this head/diff never replaces an applicable one from the same reviewer
        # (a delayed stale PASS must not erase a current FAIL). A FAIL stands unless its digest names another diff:
        # a later FAIL must not be skipped, and a digest-less FAIL keeps blocking until its reviewer posts a newer
        # applicable verdict.
        if applies or (v["result"] == "FAIL" and not v["digest"]):
            latest[v["agent"]] = v
        else:
            why.append(f"verdict is for {v['sha'][:12]}, head is {head[:12]}")
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


RATE_LIMIT_EXIT = 75  # EX_TEMPFAIL: the workflow maps it to a `pending` status, never `failure`
RATE_LIMIT_MAX_WAIT = 480  # seconds of backoff for the whole run; the job timeout is 20 min
RATE_LIMIT_RE = re.compile(r"rate limit|HTTP 429|too many requests", re.I)
RATE_LIMIT_WRITE_RESERVE = 120  # seconds of the budget kept for the status write and the queue revocation
_rate_limit_waited = 0


def _reserve_write_budget() -> None:
    """Reads may spend the budget, but a verdict that is already known must still get its writes attempted."""
    global _rate_limit_waited
    _rate_limit_waited = min(_rate_limit_waited, RATE_LIMIT_MAX_WAIT - RATE_LIMIT_WRITE_RESERVE)


class RateLimited(subprocess.CalledProcessError):
    """The installation's API quota stayed exhausted for the whole wait budget (CHE-4242)."""


def _gh(*args: str) -> str:
    """`gh api`, waiting out a 403/429 rate limit with a doubling backoff (5 s up to 60 s) from a budget shared by
    every call of the run. Any other error is raised at once."""
    global _rate_limit_waited
    delay = 5
    while True:
        try:
            return subprocess.run(["gh", "api", *args], check=True, capture_output=True, text=True).stdout
        except subprocess.CalledProcessError as e:
            if not RATE_LIMIT_RE.search(str(e.stderr or "")):
                # CHE-4234: capture_output hid the API error; show it in the job log
                print(f"gh api {args[0]} failed (exit {e.returncode}): {(e.stderr or '').strip()}", file=sys.stderr)
                raise
            if _rate_limit_waited + delay > RATE_LIMIT_MAX_WAIT:
                raise RateLimited(e.returncode, e.cmd, e.output, e.stderr) from e
            time.sleep(delay)
            _rate_limit_waited += delay
            delay = min(delay * 2, 60)


def _post(github: str, sha: str, state: str, desc: str, run_url: str) -> None:
    _gh(f"repos/{github}/statuses/{sha}", "-f", f"state={state}", "-f", f"context={CONTEXT}",
        "-f", f"description={desc}", *(["-f", f"target_url={run_url}"] if run_url else []))


def _group_chain(github: str, base: str, head: str, strict: bool = True) -> list[dict]:
    """The first-parent chain of merge-queue commits from `head` down to `base` (compare also lists PR branch commits).
    `strict=False` takes `base` as a branch name and stops where the chain leaves the compare range."""
    rows = [json.loads(l) for l in _gh("--paginate", f"repos/{github}/compare/{base}...{head}", "--jq",
                                       ".commits[] | {sha, parents: [.parents[].sha], message: .commit.message}").splitlines()]
    by_sha, chain, sha = {r["sha"]: r for r in rows}, [], head
    while sha != base and sha in by_sha:
        chain.append(by_sha[sha])
        sha = by_sha[sha]["parents"][0]
    return chain if sha == base or not strict else []


def _heads_of(lookups: tuple[tuple[str, ...], ...]) -> list[str]:
    """First lookup that answers wins; only when every one fails does the last error propagate."""
    for i, args in enumerate(lookups):
        try:
            return _gh(*args).split()
        except subprocess.CalledProcessError:
            if i == len(lookups) - 1:
                raise
    return []


def revoke_queue(github: str, number: int, default_branch: str, run_url: str) -> None:
    """A PR whose gate is no longer `success` must not ride a success already carried onto a queue head: flip every
    queue head whose group contains the PR to `failure`, so GitHub drops the entry. A group that cannot be read is
    failed too (fail closed). Only when both queue lookups keep failing for the whole retry period does it raise: no head
    is known to flip then and the job goes red (accepted residual risk, CHE-3240: dequeue the PR by hand). Each head's
    write is retried (3 attempts, short backoff) and a head that still fails never skips the later heads; every head left
    unflipped is named in the error that fails the job (residual risk: a write that fails on every bounded retry cannot
    be forced, so dequeue the entry or re-run the job). The PR status
    was written first, so a group evaluated after this point reads the revocation itself."""
    prefix = f"gh-readonly-queue/{default_branch}/"
    lookups = (  # two different endpoints: an outage of one must not leave a carried success in place
        ("--paginate", f"repos/{github}/git/matching-refs/heads/{prefix}", "--jq", ".[].object.sha"),
        ("--paginate", f"repos/{github}/branches", "--jq", f'.[] | select(.name | startswith("{prefix}")) | .commit.sha'))
    for attempt in range(3):
        try:
            heads = _heads_of(lookups)
            break
        except subprocess.CalledProcessError:
            if attempt == 2:
                raise
            time.sleep(2 * (attempt + 1))
    unflipped = []
    for head in heads:
        try:
            carries = any(re.match(rf"Merge pull request #{number} ", r["message"])
                          for r in _group_chain(github, default_branch, head, strict=False))
            desc = f"#{number}: {CONTEXT} no longer success on its head"
        except Exception as e:  # unreadable group: block it rather than leave a carried success
            carries, desc = True, f"cannot read queue group: {type(e).__name__}"
        if carries:
            for attempt in range(3):  # a transient write error must not leave the carried success in place
                try:
                    _post(github, head, "failure", desc, run_url)
                    break
                except subprocess.CalledProcessError:
                    if attempt == 2:
                        unflipped.append(head)
                    else:
                        time.sleep(2 * (attempt + 1))
    if unflipped:
        raise RuntimeError(f"could not revoke security-gate on queue head(s): {' '.join(unflipped)}")


def gate_pr(repo: str, github: str, number: int, default_branch: str, trusted: set[int],
            humans: set[int], baseline: list[str] | None = None) -> tuple[str, str, str]:
    """`baseline` None = no semgrep scan."""
    pull = json.loads(_gh(f"repos/{github}/pulls/{number}"))
    head, base = pull["head"]["sha"], pull["base"]["sha"]
    # no --slurp: the X99 gh (2.45) lacks it; --jq emits one compact object per line across pages
    lines = _gh("--paginate", f"repos/{github}/issues/{number}/comments", "--jq",
                '.[] | {id, author_id: .user.id, body: (.body // ""), updated_at}').splitlines()
    comments = [json.loads(l) for l in lines]
    diff, semgrep, digest = "", None, None  # a non-default base returns before the diff is read; its base may not be in this checkout
    if pull["base"]["ref"] == default_branch:
        diff = subprocess.run(["git", "-C", repo, "diff", "-U0", "--no-color", f"{base}...{head}"],
                              check=True, capture_output=True, text=True).stdout
        digest = pr_diff_digest(repo, base, head)
        if baseline is not None:
            try:
                names = subprocess.run(["git", "-C", repo, "diff", "--name-only", "-z", "--diff-filter=d",
                                        f"{base}...{head}"], check=True, capture_output=True, text=True).stdout
                semgrep = new_semgrep(semgrep_scan(repo, head, [n for n in names.split("\0") if n]), baseline, diff)
            except (SemgrepError, subprocess.CalledProcessError) as e:  # cannot scan: never a silent pass
                return head, "pending", _out("pending", f"semgrep did not run: {e}")[1]
    state, desc = evaluate({"head_sha": head, "head_ref": pull["head"]["ref"], "base_ref": pull["base"]["ref"],
                            "default_branch": default_branch, "author_id": pull["user"]["id"],
                            "repo": github, "number": number}, comments, trusted, diff, humans, semgrep, digest)
    return head, state, desc


def main() -> int:
    try:
        return _main()
    except RateLimited:
        # Not a verdict: the caller posts `pending` (or retries), never `failure` for a rate limit.
        print("rate limited, retry", file=sys.stderr)
        return RATE_LIMIT_EXIT


def _main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--repo", default=".", help="DEFAULT-branch checkout with the head objects fetched")
    ap.add_argument("--github", required=True, help="owner/repo")
    ap.add_argument("--pr", type=int, required=True)
    ap.add_argument("--default-branch", required=True)
    ap.add_argument("--run-url", default="")
    ap.add_argument("--semgrep", action="store_true", help="scan the PR's added lines against the committed baseline")
    ap.add_argument("--write-baseline", action="store_true", help="scan the default branch HEAD and rewrite the baseline")
    ap.add_argument("--print-digest", action="store_true",
                    help="print the PR's head and diff digest as JSON, for any lane that records a PASS (CHE-3610)")
    ap.add_argument("--post", action="store_true", help="write the commit status (default: print only)")
    a = ap.parse_args()
    if a.write_baseline:
        fps = sorted(f["fp"] for f in semgrep_scan(a.repo, "HEAD"))
        with open(f"{a.repo}/{SEMGREP_BASELINE}", "w") as f:
            json.dump({"configs": list(SEMGREP_CONFIGS), "findings": fps}, f, indent=1)
            f.write("\n")
        print(f"baseline: {len(fps)} ERROR finding(s)")
        return 0
    if a.print_digest:
        pull = json.loads(_gh(f"repos/{a.github}/pulls/{a.pr}"))
        head = pull["head"]["sha"]
        print(json.dumps({"head": head, "digest": pr_diff_digest(a.repo, pull["base"]["sha"], head)}))
        return 0
    with open(f"{a.repo}/{TRUSTED_AUTHORS}") as f:
        registry = json.load(f)
    trusted = {b["id"] for b in registry["workspace_bots"]}
    humans = {h["id"] for h in registry["instruction_authors"]}
    try:
        baseline = load_baseline(a.repo) if a.semgrep else None
    except SemgrepError as e:
        sys.exit(f"semgrep baseline: {e}")  # fail the job: no status is posted, the required check stays pending
    head, state, desc = gate_pr(a.repo, a.github, a.pr, a.default_branch, trusted, humans, baseline)
    # The API also lists PRs that merely contain the commit (descendants, forks) with a different head. Their
    # head objects may not be fetched, so they are filtered here, before any diff is read.
    rows = [json.loads(l) for l in _gh(f"repos/{a.github}/commits/{head}/pulls", "--jq",
                                       '.[] | select(.state=="open") | {number, head: .head.sha}').splitlines()]
    results = [(state, desc)]
    for n in sorted({r["number"] for r in rows if r["head"] == head} - {a.pr}):
        try:
            h, st, d = gate_pr(a.repo, a.github, n, a.default_branch, trusted, humans, baseline)
        except RateLimited:
            # No verdict for the sibling: it holds the head at `pending`. A known failure still wins in combine().
            h, st, d = head, "pending", "rate limited, retry"
        except Exception as e:  # an unreadable sibling must block, never skip the status write
            h, st, d = head, "failure", f"cannot evaluate: {type(e).__name__}"
        if h == head:
            results.append((st, f"#{n}: {d}"))
    state, desc = _out(*combine(results))
    print(json.dumps({"context": CONTEXT, "sha": head, "state": state, "description": desc}))
    if a.post:
        _reserve_write_budget()
        limited = None
        for write in (lambda: _post(a.github, head, state, desc, a.run_url),
                      lambda: revoke_queue(a.github, a.pr, a.default_branch, a.run_url) if state != "success" else None):
            try:
                write()
            except RateLimited as e:  # attempt the other write too, then report the rate limit
                limited = e
        if limited:
            raise limited
    return 0


if __name__ == "__main__":
    sys.exit(main())
