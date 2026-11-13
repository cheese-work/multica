#!/usr/bin/env python3
"""Pre-merge security gate (CHE-2671).

Sets the `security-gate` commit status on a PR's exact head SHA:
  success  a valid PASS from every required signer for this SHA, no FAIL, no secret in the added lines, no new semgrep finding
           (CHE-4596: Opus signs; a sensitive path also needs Sol; an Opus-authored PR needs Sol only; nobody signs their own PR)
  failure  a valid FAIL verdict, a reviewer who signs their own PR, a secret in the added lines, or a new semgrep ERROR (CHE-3121)
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
from pathlib import Path

CONTEXT = "security-gate"
# CHE-4671: dependabot[bot] (GitHub numeric user id) may AUTHOR a PR. It is not a signer and not an instruction author;
# the PR still needs the normal Opus (and Sol on a sensitive path) verdict, so registration only skips the author refusal.
PR_ONLY_AUTHOR_IDS = frozenset({49699333})
TRUSTED_AUTHORS = "skills/multica-cheese-work-github/scripts/trusted-authors.json"
SEMGREP_BASELINE = "scripts/security-gate/semgrep-baseline.json"
SEMGREP_CONFIGS = ("p/default", "p/golang", "p/gosec", "p/typescript", "p/python", "p/kotlin")
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
DIGEST_RE = re.compile(r"^[0-9a-f]{64}$")
WAIVER_SCOPE_LINE_RE = re.compile(r"waiver-diff-digest", re.I)
WAIVER_DIGEST_LINE_RE = re.compile(r"Waiver-Diff-Digest: ([0-9a-f]{64})")
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


# CHE-3122 / CHE-4322: a PR that changes the gate, its rules or its instruction text is a gate-path PR (Cheese still merges it).
# Match on the whole path, never a substring: `notCODEOWNERS` and `docs/github/workflows/` are ordinary files.
# Sensitive set (CHE-4596; named TWO_SIGNER_RES since CHE-3122): the gate itself, credential, auth and secrets, CD/deploy, DB
# migration, and permission and trust-boundary paths. Needs Opus PASS and Sol PASS (see `required_signers`). This list is the
# sensitive-path glob list the PR reviews; docs/cheese-work/SECURITY-GATE.md mirrors it.
TWO_SIGNER_RES = [re.compile(p) for p in (
    r"^scripts/security-gate/",  # verdict signing, trusted-author check, secret regexes, the sweep
    r"(?:^|/)trusted-authors\.json$",  # who may sign
    r"^\.github/rulesets/",
    r"^\.github/workflows/security-gate[^/]*\.ya?ml$",
    r"^\.github/workflows/mechanical-tier\.ya?ml$",  # runs with the shared bot token
    r"^scripts/(?:mechanical-tier|agent-merge-brake)/",  # merge authority
    r"^\.github/workflows/skill-sync\.ya?ml$",  # passes the workspace-admin MULTICA_TOKEN (CHE-4322, Sol P1)
    r"^scripts/skill-sync/",  # runs with that token
    r"^scripts/instruction-manifest/",  # instruction_drift.py puts the whole directory first on sys.path while holding that token: a new `datetime.py` shadows a stdlib import (CHE-4322, Sol P1)
    r"^\.github/workflows/ocr-review\.ya?ml$",  # reads the runner-mounted /run/secrets/ocr-sub2api-key and hands it to a third-party action (CHE-4322, Opus P1)
    r"^\.github/scripts/",  # helpers every workflow step runs; they can write env and PATH for later steps that hold secrets
    r"^scripts/(?:(?:run-)?in-review-gate|stack-state)$",  # stack-state runs inside the gate with its token (CHE-4005, Sol)
    r"(?:^|/)\.semgrepignore$",  # the semgrep gate reads the default branch's copy; a widened ignore hides later findings
    # Credential, auth and secrets paths in any repo that copies this gate (e.g. `server/internal/auth/jwt.go`). Whole path
    # components or file stems only: `authoring.md`, `author/` and `tokenizer/` stay ordinary. Prose (`.md`) never matches.
    r"(?:^|/)(?:auth|authn|authz|oauth|secrets?|credentials?|jwt)/(?!.*\.md$)",
    r"(?:^|/)(?:[^/]*[._-])?(?:auth|authn|authz|oauth|secrets?|credentials?|jwt|tokens?|passwords?)(?:[._-][^/]*)?\.(?:go|py|kt|java|rs|js|ts|tsx|ya?ml|json|toml|sql|sh)$",  # the word may lead or trail: `daemon_auth.go`, `personal_access_token.go`
    r"(?:^|/)\.env(?:\.[^/]*)?$",
    r"\.(?:pem|key|p12|pfx|jks|keystore)$",
    # CD and deploy (CHE-4596): they ship to the live hosts.
    r"(?:^|/)deploy/(?!.*\.md$)",
    r"^\.github/workflows/(?:[^/]*[-_])?(?:deploy|release|cd)(?:[-_.][^/]*)?\.ya?ml$",
    # DB migrations (CHE-4596): irreversible schema changes.
    r"(?:^|/)(?:migrations?|db/migrate)/(?!.*\.md$)",
    # Permission and trust-boundary code (CHE-4596): whole components or stems, so `trustworthy.py` and `acl_docs.md` stay plain.
    r"(?:^|/)(?:permissions?|rbac|acl|trust)/(?!.*\.md$)",
    r"(?:^|/)(?:[^/]*[._-])?(?:permissions?|rbac|acl|trust)(?:[._-][^/]*)?\.(?:go|py|kt|java|rs|js|ts|tsx|ya?ml|json|toml|sql|sh)$",
)]


def waiver_applies(record: str, head: str, digest: str) -> bool:
    """Does a human waiver record (the decision-record comment, CHE-4326) cover this PR state? Only one exact
    `Waiver-Diff-Digest: <64 lowercase hex>` line is a scope: it covers every head whose PR diff digest equals it, so a
    main-only merge keeps the waiver and any change to a PR file voids it. Fail closed on anything else: no such
    line, a second scope line, or a line that names the key but is not exact (case, indent, trailing text, bad hex).
    Prose, including a head SHA, is never a scope: a record from before CHE-4326 is re-recorded once with the line.
    `head` is unused on purpose: the scope is the diff. The caller still reads that the record is the server's
    answered human decision (`multica decision get`): this checks scope only."""
    scope_lines = [ln for ln in record.splitlines() if WAIVER_SCOPE_LINE_RE.search(ln)]
    if len(scope_lines) != 1:
        return False
    m = WAIVER_DIGEST_LINE_RE.fullmatch(scope_lines[0])
    return bool(m) and m.group(1) == digest


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


# CHE-4596 signer roles, matched on whole name tokens (`x99-claude-opus`, `mbp-claude-opus`, `x99-codex-sol-200k`).
# CHE-5097: Sol's review work moved to Gemini, so `x99-agy-gemini` holds the `sol` role too.
SIGNER_RES = {"opus": re.compile(r"(?:^|-)claude-opus(?:$|-)"),
              "sol": re.compile(r"(?:^|-)(?:codex-sol|agy-gemini)(?:$|-)")}


def signer_role(name: str | None) -> str | None:
    return next((r for r, rx in SIGNER_RES.items() if name and rx.search(name)), None)


def required_signers(author: str | None, sensitive: bool) -> set[str]:
    """Roles that must PASS. Opus signs every PR it did not author; a sensitive path adds Sol; an Opus PR is signed by
    Sol only. Nobody signs their own PR, so an author's own role is never required (a Sol PR on a sensitive path
    is left with Opus alone)."""
    role = signer_role(author)
    if role == "opus":
        return {"sol"}
    return ({"opus", "sol"} if sensitive else {"opus"}) - {role}


def author_agent(head_ref: str) -> str | None:
    m = re.match(r"^agent/([^/]+)/", head_ref)
    return m.group(1).casefold() if m else None


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


def unquote_path(raw: str) -> str:
    """Decode a Git C-quoted path (`"caf\\303\\251.py"`, `"a\\"b"`); an unquoted path is returned as is. Semgrep reports
    the decoded name, so a quoted one would never match it and a finding on an added line would be dropped."""
    if not (len(raw) >= 2 and raw.startswith('"') and raw.endswith('"')):
        return raw
    out, i, body = bytearray(), 0, raw[1:-1]
    simple = {"a": 7, "b": 8, "f": 12, "n": 10, "r": 13, "t": 9, "v": 11, "\\": 92, '"': 34}
    while i < len(body):
        c = body[i]
        if c == "\\" and i + 1 < len(body):
            m = re.match(r"[0-7]{3}", body[i + 1:i + 4])
            if m:
                out.append(int(m[0], 8) & 0xFF)
                i += 4
                continue
            out.append(simple.get(body[i + 1], ord(body[i + 1])))
            i += 2
            continue
        out.extend(c.encode())
        i += 1
    return out.decode("utf-8", errors="replace")


def header_path(raw: str) -> str:
    """Path of a `+++ ` header value. Git quotes a name with special characters, and appends a TAB to an unquoted name
    that has a space; an unquoted name never contains a TAB itself."""
    return unquote_path(raw) if raw.startswith('"') else raw.split("\t")[0]


def changed_paths(diff_text: str) -> set[str]:
    """Paths named by `diff --git` headers (covers deletes and renames) and `+++ b/` lines. A hunk line can only
    spoof an extra path, which makes the gate stricter, never looser."""
    paths = set()
    for raw in diff_text.splitlines():
        m = re.match(r"^diff --git a/(.+) b/(.+)$", raw)
        q = re.match(r'^diff --git ("a/(?:[^"\\]|\\.)*") ("b/(?:[^"\\]|\\.)*")$', raw)
        if q:  # Git quotes a name with non-ASCII, quote, backslash or control characters
            paths.update(unquote_path(g)[2:] for g in q.groups())
        elif m:
            paths.update(m.groups())
        elif raw.startswith("+++ "):
            name = header_path(raw[4:])
            if name.startswith("b/"):
                paths.add(name[2:])
    return paths


def diff_paths(repo: str, base: str, head: str) -> set[str]:
    """Changed paths from NUL-delimited git output: never quoted, so a non-ASCII or odd name cannot hide."""
    out = subprocess.run(["git", "-C", repo, "diff", "--name-only", "-z", "--no-renames", f"{base}...{head}"],
                         check=True, capture_output=True, text=True).stdout
    return {p for p in out.split("\0") if p}


def two_signer_paths(diff_text: str, paths=()) -> list[str]:
    """The subset of gate paths that needs 2 distinct PASS verdicts (CHE-4322)."""
    return sorted(p for p in changed_paths(diff_text) | set(paths) if any(r.search(p) for r in TWO_SIGNER_RES))


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
    # casefold: `X99-CODEX-SOL` and `x99-codex-sol` are one reviewer, never two
    return {"result": result, "agent": rev.group(1).casefold(), "model": rev.group(2), "sha": fields["SHA"],
            "repo": fields["Repo"], "pr": int(fields["PR"]), "base": fields["Base"], "digest": fields.get("Diff-Digest"), "findings": findings}, ""


def evaluate(pr: dict, comments: list[dict], trusted_ids: set[int], diff_text: str,
             human_ids: frozenset[int] | set[int] = frozenset(), semgrep: list[str] | None = None,
             digest: str | None = None, paths=()) -> tuple[str, str]:
    """`digest` = `pr_diff_digest` of the current head; None = not computed, so only an exact SHA match counts.
    `paths` = NUL-delimited changed paths (`diff_paths`); the diff headers are a stricter-only backstop."""
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
    elif pr["author_id"] in PR_ONLY_AUTHOR_IDS:
        author, author_lab = None, "bot"
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
    gated = two_signer_paths(diff_text, paths)
    need, passed, author_role = required_signers(author, bool(gated)), set(), signer_role(author)
    for agent, v in latest.items():
        role, lab = signer_role(agent), lab_of(agent)
        if lab is None or lab_of(v["model"]) not in (None, lab) or (role == "opus" and "opus" not in v["model"].casefold()):
            why.append(f"reviewer {agent}: lab unknown or model mismatch")
        elif role is None and not (v["result"] == "FAIL" and agent != author and lab != author_lab):
            # A PASS is limited to the signer roles. A non-signer FAIL still blocks when its reviewer is not the author
            # and comes from another lab than the author (base RGS-1 behaviour); any other non-signer verdict is ignored.
            why.append(f"reviewer {agent}: not an allowed signer (Opus or Sol)")
        elif agent == author or (role and role == author_role):
            why.append(f"reviewer {agent} cannot sign its own PR (author {author})")
            latest[agent] = {**v, "rgs1": True}
        elif v["result"] == "FAIL":
            find = next(f for f in v["findings"] if FINDING_RE.match(f))
            return _out("failure", f"FAIL by {agent}: {find[2:]}")
        else:
            passed.add(role)
    if need <= passed:
        return _out("success", f"PASS on {head[:12]} by {', '.join(sorted(passed))}")
    if passed & need:
        missing = " and ".join(r.capitalize() for r in sorted(need - passed))
        return _out("pending", f"sensitive path {gated[0]}: {missing} PASS still needed" if gated
                    else f"{missing} PASS still needed")
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
    try:
        return subprocess.run(["gh", "api", *args], check=True, capture_output=True, text=True).stdout
    except subprocess.CalledProcessError as e:  # CHE-4234: capture_output hid the API error; show it in the job log
        print(f"gh api {args[0]} failed (exit {e.returncode}): {(e.stderr or '').strip()}", file=sys.stderr)
        raise


FRESHNESS_TRIES = 3


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
    diff, semgrep, digest, paths = "", None, None, ()  # a non-default base returns before the diff is read; its base may not be in this checkout
    if pull["base"]["ref"] == default_branch:
        diff = subprocess.run(["git", "-C", repo, "diff", "-U0", "--no-color", "--no-renames", f"{base}...{head}"],
                              check=True, capture_output=True, text=True).stdout
        digest = pr_diff_digest(repo, base, head)
        paths = diff_paths(repo, base, head)
        if baseline is not None:
            try:
                names = subprocess.run(["git", "-C", repo, "diff", "--name-only", "-z", "--diff-filter=d",
                                        f"{base}...{head}"], check=True, capture_output=True, text=True).stdout
                semgrep = new_semgrep(semgrep_scan(repo, head, [n for n in names.split("\0") if n]), baseline, diff)
            except (SemgrepError, subprocess.CalledProcessError) as e:  # cannot scan: never a silent pass
                return head, "pending", _out("pending", f"semgrep did not run: {e}")[1]
    state, desc = evaluate({"head_sha": head, "head_ref": pull["head"]["ref"], "base_ref": pull["base"]["ref"],
                            "default_branch": default_branch, "author_id": pull["user"]["id"],
                            "repo": github, "number": number}, comments, trusted, diff, humans, semgrep, digest, paths)
    return head, state, desc


def _fingerprint(github: str, number: int) -> tuple:
    """What a success depends on: the PR head, every comment id + edit time, and the current gate status."""
    head = json.loads(_gh(f"repos/{github}/pulls/{number}"))["head"]["sha"]
    comments = _gh("--paginate", f"repos/{github}/issues/{number}/comments", "--jq", r'.[] | "\(.id) \(.updated_at)"')
    status = _gh(f"repos/{github}/commits/{head}/status", "--jq",
                 f'[.statuses[] | select(.context=="{CONTEXT}") | .state] | first // ""').strip()
    return head, comments, status


def _evaluate(a: argparse.Namespace, trusted: set[int], humans: set[int], baseline: list[str] | None) -> tuple[str, str, str]:
    head, state, desc = gate_pr(a.repo, a.github, a.pr, a.default_branch, trusted, humans, baseline)
    # The API also lists PRs that merely contain the commit (descendants, forks) with a different head. Their
    # head objects may not be fetched, so they are filtered here, before any diff is read.
    rows = [json.loads(l) for l in _gh(f"repos/{a.github}/commits/{head}/pulls", "--jq",
                                       '.[] | select(.state=="open") | {number, head: .head.sha}').splitlines()]
    results = [(state, desc)]
    for n in sorted({r["number"] for r in rows if r["head"] == head} - {a.pr}):
        try:
            h, st, d = gate_pr(a.repo, a.github, n, a.default_branch, trusted, humans, baseline)
        except Exception as e:  # an unreadable sibling must block, never skip the status write
            h, st, d = head, "failure", f"cannot evaluate: {type(e).__name__}"
        if h == head:
            results.append((st, f"#{n}: {d}"))
    state, desc = _out(*combine(results))
    return head, state, desc


def main() -> int:
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
    ap.add_argument("--waiver-record", metavar="FILE",
                    help="with --print-digest: check a human waiver record (file, or - for stdin) against the PR; exit 3 if it does not apply")
    ap.add_argument("--post", action="store_true", help="write the commit status (default: print only)")
    ap.add_argument("--revoke-only", action="store_true",
                    help="only flip the merge-queue heads that carry the PR to failure, without reading a verdict (sweep retry)")
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
        out = {"head": head, "digest": pr_diff_digest(a.repo, pull["base"]["sha"], head)}
        if a.waiver_record:
            record = sys.stdin.read() if a.waiver_record == "-" else Path(a.waiver_record).read_text()
            out["waiver_applies"] = waiver_applies(record, head, out["digest"])
        print(json.dumps(out))
        return 3 if out.get("waiver_applies") is False else 0
    if a.revoke_only:
        # The sweep chose this PR as non-success some time ago; a gate run may have turned it green since.
        # Read the live head status right before the flip so a recovered PR keeps its queue entry.
        head = json.loads(_gh(f"repos/{a.github}/pulls/{a.pr}"))["head"]["sha"]
        status = _gh(f"repos/{a.github}/commits/{head}/status", "--jq",
                     f'[.statuses[] | select(.context=="{CONTEXT}") | .state] | first // ""').strip()
        if status != "success":
            revoke_queue(a.github, a.pr, a.default_branch, a.run_url)
        return 0
    with open(f"{a.repo}/{TRUSTED_AUTHORS}") as f:
        registry = json.load(f)
    trusted = {b["id"] for b in registry["workspace_bots"]}
    humans = {h["id"] for h in registry["instruction_authors"]}
    try:
        baseline = load_baseline(a.repo) if a.semgrep else None
    except SemgrepError as e:
        sys.exit(f"semgrep baseline: {e}")  # fail the job: no status is posted, the required check stays pending
    for _ in range(FRESHNESS_TRIES):
        before = _fingerprint(a.github, a.pr)
        head, state, desc = _evaluate(a, trusted, humans, baseline)
        # CHE-4385: the scheduled sweep can run for minutes. A FAIL verdict, a new head or a status change that
        # landed meanwhile must not be overwritten by a success: re-read, and evaluate again if anything moved.
        if not a.post or state != "success" or _fingerprint(a.github, a.pr) == before:
            break
    else:
        sys.exit("security-gate: the PR changed on every evaluation; no success posted")
    print(json.dumps({"context": CONTEXT, "sha": head, "state": state, "description": desc}))
    if a.post:
        _post(a.github, head, state, desc, a.run_url)
        if state != "success":
            revoke_queue(a.github, a.pr, a.default_branch, a.run_url)
    return 0


if __name__ == "__main__":
    sys.exit(main())
