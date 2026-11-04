#!/usr/bin/env python3
"""pr-ready (CHE-3440): a PR may merge only when its body says it is finished AND a trusted cross-lab reviewer
signed a PASS for its current head.

  pull_request_target  judges the PR's live body and, for a PR into the default branch, its verdict comments.
  merge_group          judges every PR the merge queue really holds (same two gates).

Gate 1: the body carries no unfinished marker (WIP, Status: Draft, do not merge, ...), in the raw or rendered text.
Gate 2: a `Security-Gate-Verdict: PASS` comment by a registered workspace bot names this repo, PR, base (any
        base, stacked PRs included) and the full current head SHA, and its reviewer is a REGISTERED agent of a
        different lab than the registered author (RGS-1). One shared bot posts every verdict, so the reviewer name
        in it is a claim: an unregistered name, or a model of another lab than the registry says, is rejected. A PASS
        string in a PR body or by any other commenter is never read. For default-base PRs this is the same judge as
        scripts/security-gate/security_gate.py (a differential test keeps the two equal); it reads the comments
        itself rather than trust a commit status that any writer with a token could post.

issue_comment  a verdict comment fires no PR event, so a comment by the workspace bot re-runs the latest completed
               pr-ready run of the PR's current head; the re-run reads the live comments.

Group membership comes from the merge queue (GraphQL `mergeQueue.entries`), never from commit subjects or titles
(a PR author controls its title, so a subject can name another PR). Each commit of the first-parent chain
base_sha..head_sha must be the head commit of one entry, the queue ref must name the entry at head_sha, and the
commit must carry that PR's revision: a merge commit holds the PR head as its second parent; a squash or rebase
commit must change exactly the files, with exactly the blobs, that the PR changes. A missing queue, entry, commit or
PR, or an unrecognized ref, fails closed.

Run from a checkout of the DEFAULT branch only. This file never reads or runs anything from a PR head; the body is data.
"""

from __future__ import annotations

import html
import json
import os
import re
import subprocess
import sys
import time
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
# Agents allowed to author or review, with their lab. One shared GitHub App posts every verdict, so GitHub cannot say
# which agent wrote it; this list stops an invented name and ties the name to a lab. Upgrade path: one App identity
# per agent (owner-only), then drop the name check.
AGENT_LABS = {
    **dict.fromkeys(("x99-claude-opus", "x99-claude-sonnet", "x99-claude-sonnet-200k", "x99-claude-sonnet-design",
                     "x99-claude-gstack", "mbp-claude-opus", "mbp-claude-sonnet", "Tofu"), "anthropic"),
    **dict.fromkeys(("x99-codex-sol", "x99-codex-sol-200k", "x99-codex-terra", "x99-codex-luna", "x99-codex-luna-2",
                     "x99-codex-luna-design", "x99-codex-luna-android-lead", "x99-codex-luna-avd", "mbp-codex-sol",
                     "mbp-codex-luna", "mbp-codex-luna-lead", "mbp-codex-luna-browser", "mbp-codex-luna-pixel6pro",
                     "mbp-codex-luna-pixel3a", "c00-hermes-devops", "OpenCodeReview"), "openai"),
    **dict.fromkeys(("x99-agy-gemini", "x99-agy-gemini-ux"), "google"),
}
NO_QUEUE = "no merge queue found for the base branch"
RUN_NAME = "pr-ready-{}"  # the pr-ready.yml `run-name`; the recheck matches runs to a PR by it
WAIT_POLLS, POLL_SECONDS = 40, 15
MAX_TREE_DIRS = 100
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
FINDING_RE = re.compile(r"^- (\S+:\d+)\s+\S")
REVIEWER_RE = re.compile(r"^(\S+) \(([^)]+)\)$")
QUEUE_QUERY = ("query($owner:String!,$name:String!,$branch:String!){repository(owner:$owner,name:$name){"
               "mergeQueue(branch:$branch){entries(first:100){pageInfo{hasNextPage} "
               "nodes{position headCommit{oid} pullRequest{number headRefOid}}}}}}")


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
    """Lab of a MODEL string (`gpt-6.1-sol`); agent names go through AGENT_LABS."""
    n = name.lower()
    for lab, words in (("anthropic", ("claude",)), ("openai", ("codex", "gpt", "hermes")), ("google", ("gemini",))):
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
    security_gate.evaluate for a default-base PR, without its diff scans, and adds the registry check; any base
    needs its own verdict (`Base:` must name it)."""
    head, number, base = pull["head"]["sha"], pull["number"], pull["base"]["ref"]
    author_id = pull["user"]["id"]
    if author_id in HUMAN_IDS:
        author, author_lab = None, "human"
    elif author_id in TRUSTED_BOT_IDS:
        m = re.match(r"^agent/([^/]+)/", pull["head"]["ref"])
        author = m.group(1) if m else None
        author_lab = AGENT_LABS.get(author or "")
        if author_lab is None:
            return f"author agent unknown ({author or pull['head']['ref']}): not registered, fail closed"
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
        lab = AGENT_LABS.get(agent)
        if lab is None or lab_of(v["model"]) != lab:
            why.append(f"reviewer {agent} ({v['model']}): not a registered agent of that model's lab")
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
    """({PR number: (queued head SHA, queue commit)}, problems). Every commit of the first-parent chain base..head
    must be the head commit of one queue entry; `commits` are compare-API commits, `entries` are mergeQueue nodes."""
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
            members[pr["number"]] = (pr["headRefOid"], c)
        sha = c["parents"][0]["sha"]
    m = re.search(r"/pr-(\d+)-", head_ref)
    if not m:
        problems.append(f"group ref {head_ref!r} names no PR: cannot tie the group to a queue entry")
    elif entry_of.get(head_sha, {}).get("number") != int(m[1]):
        problems.append(f"group ref names #{m[1]}, not the queue entry at {head_sha[:12]}")
    if not members:
        problems.append("no PR found in the merge group")
    return members, problems


def changed_files(gh, repo: str, spec: str) -> dict | None:
    """{path: (status, blob sha, previous path)} of a compare, or None when the API truncated the file list. The
    previous path is part of the change: two renames to one destination from different sources differ."""
    files = gh(f"repos/{repo}/compare/{spec}")["files"]
    if len(files) >= 300:
        return None
    return {f["filename"]: (f["status"], f.get("sha"), f.get("previous_filename")) for f in files}


def modes_at(gh, repo: str, commit: str, paths: list[str]) -> dict | None:
    """{path: (mode, blob sha) or None if absent} at `commit`, from the git trees API (the compare API has no mode);
    None when a listing is truncated or there are too many directories to read."""
    dirs = sorted({p.rpartition("/")[0] for p in paths})
    if len(dirs) > MAX_TREE_DIRS:
        return None
    entries: dict[str, tuple] = {}
    for d in dirs:
        try:
            listing = gh(f"repos/{repo}/git/trees/{commit}:{d}")
        except subprocess.CalledProcessError:  # the directory does not exist in this commit
            continue
        if listing.get("truncated"):
            return None
        entries.update({f"{d}/{t['path']}" if d else t["path"]: (t["mode"], t["sha"]) for t in listing["tree"]})
    return {p: entries.get(p) for p in paths}


def bind_problem(gh, repo: str, pull: dict, commit: dict) -> str | None:
    """Why the queue `commit` does not carry this PR's current revision, else None. A merge-method commit holds the PR
    head as its second parent. A squash or rebase commit has no such parent, so its whole tree change must equal the
    PR's: same paths, statuses, rename sources, blobs and modes. A base that edited the same files makes it differ,
    which fails closed (a false fail, never a false pass)."""
    head, parents = pull["head"]["sha"], commit["parents"]
    if len(parents) >= 2:
        return None if parents[1]["sha"] == head else f"queue commit {commit['sha'][:12]} does not hold the PR head {head[:12]}"
    queued = changed_files(gh, repo, f"{parents[0]['sha']}...{commit['sha']}")
    own = changed_files(gh, repo, f"{pull['base']['ref']}...{head}")
    if queued is None or own is None:
        return "too many changed files to bind the queue commit to the PR head"
    if queued != own:
        return f"queue commit {commit['sha'][:12]} changes other files or content than PR head {head[:12]}"
    kept = [path for path, (status, *_) in own.items() if status != "removed"]
    in_queue, in_head = modes_at(gh, repo, commit["sha"], kept), modes_at(gh, repo, head, kept)
    if in_queue is None or in_head is None:
        return "tree listing too large to compare file modes of the queue commit and the PR head"
    if in_queue != in_head:
        return f"queue commit {commit['sha'][:12]} differs from PR head {head[:12]} in file mode or tree content"
    return None


def list_paged(gh, path: str) -> list[dict]:
    out, page = [], 1
    while True:
        batch = gh(f"{path}{'&' if '?' in path else '?'}per_page=100&page={page}")
        out += batch
        if len(batch) < 100:
            return out
        page += 1


def check_pr(gh, repo: str, number: int, default_branch: str, queued: tuple | None = None) -> list[str]:
    """Problems for one PR. `queued` = (head SHA the queue entry holds, the queue commit) inside a merge group."""
    pull = gh(f"repos/{repo}/pulls/{number}")
    out = []
    if pull["state"] != "open":
        out.append(f"#{number} is {pull['state']}")
    if queued is not None:
        queued_head, queue_commit = queued
        if queued_head != pull["head"]["sha"]:
            out.append(f"#{number}: queued at {queued_head[:12]} but the PR head is {pull['head']['sha'][:12]}")
        elif (why := bind_problem(gh, repo, pull, queue_commit)):
            out.append(f"#{number}: {why}")
        if pull["base"]["ref"] != default_branch:
            out.append(f"#{number}: base {pull['base']['ref']} is not {default_branch}")
    marker = unfinished(pull["body"])
    if marker:
        out.append(f"#{number} body says unfinished ({marker}): set Status to Ready after the PASS lands at the final head")
    why = pass_problem(pull, list_paged(gh, f"repos/{repo}/issues/{number}/comments"), default_branch, repo)
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


def queue_entries(gql, repo: str, base_ref: str) -> tuple[list[dict], list[str]]:
    owner, name = repo.split("/", 1)
    q = (gql(QUEUE_QUERY, owner=owner, name=name, branch=base_ref.removeprefix("refs/heads/")) or {}).get("data", {})
    queue = (q.get("repository") or {}).get("mergeQueue")
    if not queue:
        return [], [NO_QUEUE]
    entries = queue["entries"]
    if entries.get("pageInfo", {}).get("hasNextPage"):
        return [], ["merge queue holds more than 100 entries: cannot read the whole group"]
    return entries["nodes"], []


def rerun_when_done(gh, post, sleep, repo: str, run: dict, polls: list[int]) -> str | None:
    """Re-run `run` once it has finished, so the new attempt reads the comments as they are now. A run that is still
    going may already have read the old verdict and would finish on it, and a run cannot be re-run while it goes.
    `polls` is the poll budget left for the whole recheck. Returns a problem if the run never finishes."""
    while True:
        status = gh(f"repos/{repo}/actions/runs/{run['id']}")["status"]
        if status == "completed":
            post(f"repos/{repo}/actions/runs/{run['id']}/rerun")
            return None
        if polls[0] <= 0:
            return f"run {run['id']} is still {status} after {WAIT_POLLS} polls: cannot re-evaluate it"
        polls[0] -= 1
        sleep(POLL_SECONDS)


def latest(runs: list[dict]) -> dict | None:
    return max(runs, key=lambda r: r["created_at"], default=None)


def recheck(env: dict, gh, gql, post, sleep) -> list[str]:
    """A verdict comment fires no pull_request_target or merge_group event, so re-run the checks it can change: the
    PR's own latest run for its current head (matched by `run-name`, because two PRs can share a head), and the queue
    checks of every merge-queue entry at or behind the PR (each is stacked on the PR's commit). A new, edited or deleted
    PASS then replaces the old result. Only the workspace bot's comments count."""
    if int(env["COMMENT_USER_ID"]) not in TRUSTED_BOT_IDS:
        return []
    repo, number = env["REPO"], int(env["PR_NUMBER"])
    polls = [WAIT_POLLS]
    head = gh(f"repos/{repo}/pulls/{number}")["head"]["sha"]
    runs = gh(f"repos/{repo}/actions/workflows/pr-ready.yml/runs?event=pull_request_target&head_sha={head}")["workflow_runs"]
    mine = latest([r for r in runs if r.get("display_title") == RUN_NAME.format(number)])
    problems = []
    if mine is None:
        problems.append(f"no pr-ready run found for #{number} at {head[:12]}: nothing to re-run")
    elif (why := rerun_when_done(gh, post, sleep, repo, mine, polls)):
        problems.append(why)
    entries, queue_problems = queue_entries(gql, repo, env["DEFAULT_BRANCH"])
    problems += [p for p in queue_problems if p != NO_QUEUE]
    here = next((e["position"] for e in entries if e["pullRequest"]["number"] == number), None)
    for e in entries if here is not None else []:
        if e["position"] < here:
            continue
        oid = e["headCommit"]["oid"]
        queued = gh(f"repos/{repo}/actions/workflows/pr-ready-queue.yml/runs?event=merge_group&head_sha={oid}")["workflow_runs"]
        if (run := latest(queued)) and (why := rerun_when_done(gh, post, sleep, repo, run, polls)):
            problems.append(why)
    return problems


def run(env: dict, gh, gql=None, post=None, sleep=None) -> list[str]:
    """Problems for this event; empty means pass."""
    repo, default = env["REPO"], env["DEFAULT_BRANCH"]
    if env["EVENT"] == "merge_group":
        entries, problems = queue_entries(gql, repo, env["BASE_REF"])
        members, more = queue_prs(group_commits(gh, repo, env["BASE_SHA"], env["HEAD_SHA"]), entries,
                                  env["BASE_SHA"], env["HEAD_SHA"], env["HEAD_REF"])
        return problems + more + [p for n, queued in sorted(members.items())
                                  for p in check_pr(gh, repo, n, default, queued)]
    if env["EVENT"] == "pull_request_target":
        return check_pr(gh, repo, int(env["PR_NUMBER"]), default)
    if env["EVENT"] == "issue_comment":
        return recheck(env, gh, gql, post, sleep or time.sleep)
    return [f"unsupported event {env['EVENT']}"]


def _gh(path: str):
    return json.loads(subprocess.run(["gh", "api", path], check=True, capture_output=True, text=True).stdout)


def _post(path: str) -> None:
    subprocess.run(["gh", "api", "-X", "POST", path], check=True, capture_output=True, text=True)


def _gql(query: str, **variables):
    args = ["gh", "api", "graphql", "-f", f"query={query}"] + [a for k, v in variables.items() for a in ("-f", f"{k}={v}")]
    return json.loads(subprocess.run(args, check=True, capture_output=True, text=True).stdout)


def main() -> int:
    try:
        problems = run(os.environ, _gh, _gql, _post)
    except (subprocess.CalledProcessError, KeyError, ValueError, TypeError) as e:  # cannot read: never a silent pass
        problems = [f"cannot evaluate: {type(e).__name__}: {str(e)[-120:]}"]
    for p in problems:
        print(f"::error::{p}")
    print("pr-ready: " + ("FAIL" if problems else "ok"))
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
