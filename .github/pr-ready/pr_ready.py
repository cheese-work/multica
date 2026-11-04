#!/usr/bin/env python3
"""pr-ready (CHE-3440): a PR may merge only when its body says it is finished AND a trusted cross-lab reviewer
signed a PASS for its current head.

  pull_request_target  judges the PR's live body and, for a PR into the default branch, its verdict comments.
  merge_group          judges every PR the merge queue really holds (same two gates).

Gate 1: the body carries no unfinished marker (WIP, Status: Draft, do not merge, ...), in the raw or rendered text.
Gate 2: a `Security-Gate-Verdict: PASS` comment by a registered workspace bot names this repo, PR, base and the
        full current head SHA, and its reviewer is of a different lab than the author (RGS-1). A PASS string in a
        PR body or by any other commenter is never read. This is the same judge as scripts/security-gate/
        security_gate.py (a differential test keeps the two equal); it reads the comments itself rather than trust
        a commit status that any writer with a token could post.

Group membership comes from the merge queue (GraphQL `mergeQueue.entries`), never from commit subjects or titles
(a PR author controls its title, so a subject can name another PR). Each commit of the first-parent chain
base_sha..head_sha must be the head commit of one entry; the entry's PR head must equal the live PR head. A
missing queue, entry, commit or PR fails closed.

Run from a checkout of the DEFAULT branch only. This file never reads or runs anything from a PR head; the body is data.
"""

from __future__ import annotations

import html
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
# Cyrillic/Greek letters that render as Latin ones; ponytail: a short list, not the full Unicode confusables table.
LOOKALIKES = str.maketrans("аеорсхуіѕјԁοα",
                           "aeopcxyisjdoa")
# Registered identities, same as skills/multica-cheese-work-github/scripts/trusted-authors.json (id, not login).
TRUSTED_BOT_IDS, HUMAN_IDS = {282619718}, {129714106}
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
FINDING_RE = re.compile(r"^- (\S+:\d+)\s+\S")
REVIEWER_RE = re.compile(r"^(\S+) \(([^)]+)\)$")
QUEUE_QUERY = ("query($owner:String!,$name:String!,$branch:String!){repository(owner:$owner,name:$name){"
               "mergeQueue(branch:$branch){entries(first:100){pageInfo{hasNextPage} "
               "nodes{headCommit{oid} pullRequest{number headRefOid}}}}}}")


def _rendered(body: str, tag_sep: str, punct_sep: str) -> str:
    text = re.sub(r"<!--.*?-->|~~.*?~~|<(s|del|strike)>.*?</\1>", " ", body or "", flags=re.S | re.I)
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)  # a link shows its text
    text = re.sub(r"<[^>]*>", tag_sep, html.unescape(text))
    text = unicodedata.normalize("NFKD", text)
    text = "".join(c for c in text if unicodedata.category(c) not in ("Cf", "Mn", "Cc") or c in "\n\t")
    text = unicodedata.normalize("NFKC", text).translate(LOOKALIKES).lower()
    text = re.sub(r"[*_`~\\]", punct_sep, text)
    return " ".join(re.sub(r"[^a-z0-9']+", " ", text).split())


def unfinished(body: str) -> str | None:
    """The first unfinished marker in `body`, else None. Two renderings are read: markup removed so `Dr**aft` and
    `<b>Dr</b>aft` join into one word, and markup replaced by a space so `Do<br>not` stays two words. Entities,
    accents, invisible characters and look-alike letters are folded first; HTML comments and struck text are not
    read by a human, so they are ignored."""
    forms = (_rendered(body, "", ""), _rendered(body, " ", " "))
    return next((name for name, rx in MARKER_RES if any(rx.search(f) for f in forms)), None)


def lab_of(name: str) -> str | None:
    n = name.lower()
    for lab, words in (("anthropic", ("claude",)), ("openai", ("codex", "gpt", "hermes")), ("google", ("gemini", "agy"))):
        if any(w in n for w in words):
            return lab
    return None


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


def pass_problem(pull: dict, comments: list[dict], default_branch: str, repo: str) -> str | None:
    """Why `comments` do not prove a trusted cross-lab PASS at the pull's current head, else None. Mirrors
    security_gate.evaluate without its diff scans."""
    head, number, base = pull["head"]["sha"], pull["number"], pull["base"]["ref"]
    if base != default_branch:
        return f"base is {base}, not {default_branch}"
    author_id = pull["user"]["id"]
    if author_id in HUMAN_IDS:
        author, author_lab = None, "human"
    elif author_id in TRUSTED_BOT_IDS:
        m = re.match(r"^agent/([^/]+)/", pull["head"]["ref"])
        author = m.group(1) if m else None
        author_lab = lab_of(author) if author else None
        if author_lab is None:
            return f"author agent unknown ({author or pull['head']['ref']}): no lab, fail closed"
    else:
        return f"PR author {author_id} is not a registered agent or human: fail closed"
    why: list[str] = []
    latest: dict[str, dict] = {}
    for c in sorted((c for c in comments if c["user"]["id"] in TRUSTED_BOT_IDS), key=lambda c: (c["updated_at"], c["id"])):
        v, err = parse_verdict(c["body"] or "")
        if v is None:
            if err:
                why.append(err)
            continue
        if v["sha"] != head:
            why.append(f"verdict is for {v['sha'][:12]}, head is {head[:12]}")
            continue
        if (v["repo"], v["pr"], v["base"]) != (repo, number, base):
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
        elif v["result"] == "FAIL":
            return f"FAIL by {agent}: {next(f for f in v['findings'] if FINDING_RE.match(f))[2:]}"
        else:
            passes.append(agent)
    if passes:
        return None
    return f"no trusted cross-lab PASS for {head[:12]}" + (f" ({why[-1]})" if why else "")


def queue_prs(commits: list[dict], entries: list[dict], base_sha: str, head_sha: str, head_ref: str):
    """({PR number: queued head SHA}, problems). Every commit of the first-parent chain base..head must be the head
    commit of one queue entry; `commits` are compare-API commits, `entries` are mergeQueue nodes."""
    by_sha = {c["sha"]: c for c in commits}
    entry_of = {e["headCommit"]["oid"]: e["pullRequest"] for e in entries if e.get("headCommit") and e.get("pullRequest")}
    members, problems, sha = {}, [], head_sha
    while sha != base_sha:
        c = by_sha.get(sha)
        if c is None or not c["parents"]:
            problems.append(f"queue commit {sha[:12]} is not in the base..head range")
            break
        pr = entry_of.get(sha)
        if pr is None:
            problems.append(f"queue commit {sha[:12]} is not the head of any merge queue entry")
        else:
            members[pr["number"]] = pr["headRefOid"]
        sha = c["parents"][0]["sha"]
    m = re.search(r"/pr-(\d+)-", head_ref)
    if m and entry_of.get(head_sha, {}).get("number") != int(m[1]):
        problems.append(f"group ref names #{m[1]}, not the queue entry at {head_sha[:12]}")
    if not members:
        problems.append("no PR found in the merge group")
    return members, problems


def list_paged(gh, path: str) -> list[dict]:
    out, page = [], 1
    while True:
        batch = gh(f"{path}{'&' if '?' in path else '?'}per_page=100&page={page}")
        out += batch
        if len(batch) < 100:
            return out
        page += 1


def check_pr(gh, repo: str, number: int, default_branch: str, queued_head: str | None = None, in_group=False) -> list[str]:
    pull = gh(f"repos/{repo}/pulls/{number}")
    out = []
    if pull["state"] != "open":
        out.append(f"#{number} is {pull['state']}")
    if queued_head is not None and queued_head != pull["head"]["sha"]:
        out.append(f"#{number}: queued at {queued_head[:12]} but the PR head is {pull['head']['sha'][:12]}")
    marker = unfinished(pull["body"])
    if marker:
        out.append(f"#{number} body says unfinished ({marker}): set Status to Ready after the PASS lands at the final head")
    # A stacked PR into a non-default branch has no cross-lab gate yet (security-gate supports only the default
    # branch); inside a merge group the base is the default branch, so any other base is wrong.
    if pull["base"]["ref"] == default_branch:
        why = pass_problem(pull, list_paged(gh, f"repos/{repo}/issues/{number}/comments"), default_branch, repo)
        if why:
            out.append(f"#{number}: {why}")
    elif in_group:
        out.append(f"#{number}: base {pull['base']['ref']} is not {default_branch}")
    return out


def group_commits(gh, repo: str, base_sha: str, head_sha: str) -> list[dict]:
    commits, page = [], 1
    while True:
        d = gh(f"repos/{repo}/compare/{base_sha}...{head_sha}?per_page=100&page={page}")
        commits += d["commits"]
        if not d["commits"] or len(commits) >= d.get("total_commits", len(commits)):
            return commits
        page += 1


def queue_entries(gql, repo: str, base_ref: str) -> tuple[list[dict], list[str]]:
    owner, name = repo.split("/", 1)
    q = (gql(QUEUE_QUERY, owner=owner, name=name, branch=base_ref.removeprefix("refs/heads/")) or {}).get("data", {})
    queue = (q.get("repository") or {}).get("mergeQueue")
    if not queue:
        return [], ["no merge queue found for the base branch"]
    entries = queue["entries"]
    if entries.get("pageInfo", {}).get("hasNextPage"):
        return [], ["merge queue holds more than 100 entries: cannot read the whole group"]
    return entries["nodes"], []


def run(env: dict, gh, gql=None) -> list[str]:
    """Problems for this event; empty means pass."""
    repo, default = env["REPO"], env["DEFAULT_BRANCH"]
    if env["EVENT"] == "merge_group":
        entries, problems = queue_entries(gql, repo, env["BASE_REF"])
        members, more = queue_prs(group_commits(gh, repo, env["BASE_SHA"], env["HEAD_SHA"]), entries,
                                  env["BASE_SHA"], env["HEAD_SHA"], env["HEAD_REF"])
        return problems + more + [p for n, head in sorted(members.items())
                                  for p in check_pr(gh, repo, n, default, head, in_group=True)]
    if env["EVENT"] == "pull_request_target":
        return check_pr(gh, repo, int(env["PR_NUMBER"]), default)
    return [f"unsupported event {env['EVENT']}"]


def _gh(path: str):
    return json.loads(subprocess.run(["gh", "api", path], check=True, capture_output=True, text=True).stdout)


def _gql(query: str, **variables):
    args = ["gh", "api", "graphql", "-f", f"query={query}"] + [a for k, v in variables.items() for a in ("-f", f"{k}={v}")]
    return json.loads(subprocess.run(args, check=True, capture_output=True, text=True).stdout)


def main() -> int:
    try:
        problems = run(os.environ, _gh, _gql)
    except (subprocess.CalledProcessError, KeyError, ValueError, TypeError) as e:  # cannot read: never a silent pass
        problems = [f"cannot evaluate: {type(e).__name__}: {str(e)[-120:]}"]
    for p in problems:
        print(f"::error::{p}")
    print("pr-ready: " + ("FAIL" if problems else "ok"))
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
