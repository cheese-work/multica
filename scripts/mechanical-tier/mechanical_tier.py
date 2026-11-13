#!/usr/bin/env python3
"""Trusted mechanical-tier classifier (CHE-1298, phase 2 of CHE-1286). Standard library plus `git` and `gh`.

PROPOSED, NOT ACTIVE: no tier waiver applies until the CHE-1286 owner rollout is read back.

Contract
  classify --repo DIR --base SHA --head SHA [--github OWNER/REPO --pr N] [--out FILE]
  verify   --repo DIR --receipt FILE|-       [--github OWNER/REPO --pr N]
There is no tier, CI, coverage or check-exclusion option: a caller cannot claim or exclude any of them.
The base and head are bound to the pull request: `--github` needs `--pr`, the base and head must equal the pull request's
own base SHA and head SHA read from the GitHub API, and a caller base that differs is rejected by classify and verify.
Without that binding no tier below 2 is issued. The only excluded check is the constant SELF_CHECK_NAME (trusted bytes).

Trust model
  Run this file from the base revision's bytes. The registry is read from `base:scripts/mechanical-tier/registry.json`,
  never from the working tree or the head, and this file must equal `base:scripts/mechanical-tier/mechanical_tier.py`
  byte for byte, else the receipt is untrusted and the tier is 2. Edits to either file are themselves Tier 2 through a
  built-in floor, so a candidate cannot classify itself with its own replacement bytes. The registry can only add
  protection to the floor.

Tiers (evaluated Tier 2 first, Tier 0 second, Tier 1 last; the highest match wins)
  2  protected path or capability, malformed path, incomplete coverage, untrusted classifier/registry, empty change
  0  every changed old/new path is on the inert allowlist, nothing else matched, no symlink/submodule, AND every
     applicable required check reported success on the exact head SHA (at least one exists)
  1  everything else with complete coverage (including Tier-0-eligible work whose CI evidence is not a clean success)

Coverage limit: this classifier implements path rules only. It has no language scanner. Coverage is complete for a
path only if the path is on the Tier 0 allowlist or inside a registry `covered_scopes` entry (analysis "path-registry",
a reviewed claim that the path decides the capability). An unsupported analysis name makes the registry malformed.
A receipt's digest is integrity, not authenticity: `verify` recomputes everything from git and the GitHub API.
"""
import argparse
import hashlib
import json
import posixpath
import re
import subprocess
import sys
from pathlib import Path

SCHEMA = "mechanical-tier-receipt/1"
CLASSIFIER_PATH = "scripts/mechanical-tier/mechanical_tier.py"
REGISTRY_PATH = "scripts/mechanical-tier/registry.json"
POLICY_STATE = "proposed; no waiver is active until the CHE-1286 owner rollout is read back"
# Built-in floor: the registry can add to it, never remove from it.
FLOOR_RULES = [
    {"id": "floor-classifier", "capability": "Trusted classifier and registry", "globs": ["scripts/mechanical-tier/**"]},
    {"id": "floor-ci", "capability": "CI execution, release or production deployment",
     "globs": [".github/workflows/**", ".github/actions/**"]},
]
FLOOR_TOKENS = frozenset("auth authentication authorization billing deploy deployment release releasing secret secrets "
                         "privacy migration migrations migrate".split())
SELF_CHECK_NAME = "mechanical-tier"     # the one check the classifier cannot use as its own evidence; callers cannot change it
RENAME_LIMIT = 1000                     # git's inexact rename/copy search limit; exceeding it makes coverage incomplete
SUPPORTED_ANALYSES = frozenset({"path-registry"})
RECEIPT_KEYS = frozenset("schema policy_state classifier registry base head paths capability_matches coverage malformed "
                         "ci tier tier_reasons gates merge_authorized error receipt_sha256".split())
SHA_RX = re.compile(r"[0-9a-f]{40}(?:[0-9a-f]{24})?\Z")
CI_PRIORITY = ["failed", "cancelled", "pending", "incomplete", "stale_head", "absent", "skipped", "neutral", "success"]
CI_BLOCKING = {"failed", "cancelled", "pending", "incomplete"}


class Malformed(Exception):
    """A path, registry or receipt that cannot be trusted. Always fails closed to Tier 2."""


# ------------------------------------------------------------------ paths and globs

def normalize_path(raw):
    """Repository-relative path from Git bytes. Case and bytes are preserved; unsafe forms are rejected."""
    try:
        p = raw.decode("utf-8")
    except UnicodeDecodeError:
        raise Malformed(f"path is not UTF-8: {raw.hex()}")
    if not p or p.startswith("/") or "\\" in p or any(ord(c) < 32 or ord(c) == 127 for c in p):
        raise Malformed(f"unsafe path: {p!r}")
    for part in p.split("/"):
        if part in ("", ".", "..") or part.lower() == ".git":
            raise Malformed(f"unsafe path component in {p!r}")
    return p


def glob_re(glob, ignore_case=False):
    """Anchored full-path glob: `**` crosses directories (a trailing `**` needs one more segment), `*`/`?` stay in a segment."""
    parts, rx = glob.split("/"), []
    for i, seg in enumerate(parts):
        last = i == len(parts) - 1
        if seg == "**":
            rx.append(".+" if last else "(?:[^/]+/)*")
        else:
            body = "".join("[^/]*" if c == "*" else "[^/]" if c == "?" else re.escape(c) for c in seg)
            rx.append(body if last else body + "/")
    return re.compile("".join(rx) + r"\Z", re.S | (re.I if ignore_case else 0))


def _globs(v, what):
    if (not isinstance(v, list) or not all(isinstance(g, str) and g and not g.startswith("/")
                                           and ".." not in g.split("/") for g in v)):
        raise Malformed(f"registry {what} must be a list of relative globs")
    return v


class Registry:
    def __init__(self, text):
        try:
            d = json.loads(text)
        except ValueError as e:
            raise Malformed(f"registry is not JSON: {e}")
        if not isinstance(d, dict) or d.get("schema") != 1 or not set(d) <= {"schema", "note", "tier2", "sensitive_tokens",
                                                                              "tier0_allowlist", "covered_scopes"}:
            raise Malformed("registry has an unknown schema or key")
        try:
            self.rules = []
            for r in FLOOR_RULES + d["tier2"]:
                if set(r) - {"id", "capability", "globs", "pure_exceptions"} or not isinstance(r["id"], str) \
                        or not isinstance(r["capability"], str) or not r["globs"]:
                    raise Malformed("registry tier2 rule is malformed")
                self.rules.append((r["id"], r["capability"], [glob_re(g, True) for g in _globs(r["globs"], "tier2 globs")],
                                   [glob_re(g) for g in _globs(r.get("pure_exceptions", []), "pure_exceptions")]))
            toks = d["sensitive_tokens"]
            if not isinstance(toks, list) or not all(isinstance(t, str) and re.fullmatch(r"[a-z0-9]+", t) for t in toks):
                raise Malformed("registry sensitive_tokens must be lowercase words")
            self.tokens = FLOOR_TOKENS | set(toks)
            self.allow = [glob_re(g) for g in _globs(d["tier0_allowlist"], "tier0_allowlist")]
            self.covered = []
            for s in d["covered_scopes"]:
                if set(s) != {"id", "glob", "analysis"} or s["analysis"] not in SUPPORTED_ANALYSES:
                    raise Malformed(f"unsupported or malformed covered scope: {s!r}")
                self.covered.append(glob_re(_globs([s["glob"]], "covered_scopes")[0]))
        except (KeyError, TypeError) as e:
            raise Malformed(f"registry is malformed: {e!r}")

    def tier2(self, path):
        """[(rule id, capability, exceptable)] for every protected match of `path` (case-insensitive)."""
        hits = [(rid, cap, any(x.match(path) for x in exc))
                for rid, cap, rxs, exc in self.rules if any(x.match(path) for x in rxs)]
        for tok in sorted(set(re.split(r"[^a-z0-9]+", path.lower())) & self.tokens):   # any non-alphanumeric delimits (a superset of r2's)
            hits.append((f"token:{tok}", "Always-protected domain (docs and tests included)", False))
        return hits

    def inert(self, path):
        return any(x.match(path) for x in self.allow)

    def covered_by_scope(self, path):
        return self.inert(path) or any(x.match(path) for x in self.covered)


# ------------------------------------------------------------------------- git

def git(repo, *args, check=True):
    r = subprocess.run(["git", "-C", str(repo), "-c", "core.quotepath=off", *args], capture_output=True)
    if check and r.returncode:
        raise Malformed(f"git {args[0]} failed: {r.stderr.decode(errors='replace').strip()}")
    return r


def revision(repo, rev):
    if not isinstance(rev, str) or not SHA_RX.match(rev):
        raise Malformed(f"revision must be a full lowercase commit SHA: {rev!r}")
    r = git(repo, "rev-parse", "--verify", "--quiet", f"{rev}^{{commit}}", check=False)
    if r.returncode or r.stdout.decode().strip() != rev:
        raise Malformed(f"not a commit in this repository: {rev}")
    return rev


def blob(repo, rev, path):
    r = git(repo, "cat-file", "blob", f"{rev}:{path}", check=False)
    return r.stdout if r.returncode == 0 else None


def diff_entries(repo, base, head):
    """(entries, copy_detection_skipped). `--find-copies-harder` also finds a copy of an UNCHANGED file; a copy lists its source."""
    r = git(repo, "diff", "--raw", "-z", "-M", "-C", "--find-copies-harder", f"-l{RENAME_LIMIT}", "--full-index",
            "--no-ext-diff", "--no-textconv", base, head, "--")
    skipped = bool(re.search(rb"too many files|was skipped", r.stderr))      # git warns when the limit cuts the search short
    toks, entries, i = r.stdout.split(b"\0"), [], 0
    while i < len(toks) and toks[i]:
        om, nm, osha, nsha, st = toks[i][1:].decode().split(" ")
        n = 2 if st[0] in "RC" else 1
        raw = toks[i + 1:i + 1 + n]
        entries.append({"status": st, "old_mode": om, "new_mode": nm, "old_sha": osha, "new_sha": nsha,
                        "old_raw": None if st[0] == "A" else raw[0], "new_raw": None if st[0] == "D" else raw[-1]})
        i += 1 + n
    return entries, skipped


def resolve_symlink(repo, tree, path, blob_sha):
    """In-repo regular-file target path of a symlink, or None when it cannot be resolved (absolute, escaping,
    dangling, a directory, a submodule or another symlink)."""
    try:
        target = git(repo, "cat-file", "blob", blob_sha).stdout.decode("utf-8")
        if target.startswith("/") or "\\" in target or not target:
            return None
        joined = posixpath.normpath(posixpath.join(posixpath.dirname(path), target))
        if joined in (".", "") or joined.startswith(".."):
            return None
        resolved = normalize_path(joined.encode())
    except (Malformed, UnicodeDecodeError):
        return None
    ls = git(repo, "ls-tree", "-z", tree, "--", resolved, check=False).stdout
    mode = ls.split(b" ", 1)[0].decode() if ls else ""
    return resolved if mode in ("100644", "100755") else None


# ---------------------------------------------------------------------- classify

def _digest_input(r):
    return json.dumps({k: v for k, v in r.items() if k != "receipt_sha256"}, sort_keys=True, separators=(",", ":"))


def receipt_digest(r):
    return hashlib.sha256(_digest_input(r).encode()).hexdigest()


def digest_ok(r):
    return isinstance(r, dict) and r.get("receipt_sha256") == receipt_digest(r)


def _gates(tier):
    return {"rgs1_signer": "waived" if tier == 0 else "required", "ocr": "not_required" if tier == 0 else "required",
            "human_merge": "required", "specialist_and_safety_gates": "retained"}


def _ci_block(ci, head):
    if ci is None:
        return {"source": "none", "head_sha": head, "state": "not_collected", "passed": False, "blocking_state": None,
                "required": [], "checks": [], "notes": ["no GitHub evidence collected; no caller assertion is accepted"]}
    if ci.get("head_sha") != head:
        return {**ci, "head_sha": head, "state": "stale_head", "passed": False, "blocking_state": None,
                "notes": [f"evidence was collected for {ci.get('head_sha')}, not this head"]}
    return ci


def classify(repo, base, head, ci=None):
    """`ci` is evidence from fetch_ci, None, or a callable `head -> evidence`. A callable runs inside the fail-closed
    guard after both revisions validate, so a collection error still yields a Tier 2 receipt."""
    r = {"schema": SCHEMA, "policy_state": POLICY_STATE, "base": base, "head": head, "paths": [], "capability_matches": [],
         "coverage": {"complete": False, "uncovered": [], "reasons": []}, "malformed": [], "tier_reasons": [], "error": None,
         "classifier": {"path": CLASSIFIER_PATH, "sha256": None, "trusted": False, "revision": None, "blob_id": None},
         "registry": {"path": REGISTRY_PATH, "sha256": None}, "ci": _ci_block(None, head), "merge_authorized": False}
    try:
        revision(repo, base), revision(repo, head)
        r["ci"] = _ci_block(ci(head) if callable(ci) else ci, head)
        _classify(repo, base, head, r)
    except Exception as e:      # any failure to classify is Tier 2, never a pass
        r["error"] = f"{type(e).__name__}: {e}"
        r["tier"], r["tier_reasons"] = 2, ["fail closed: " + r["error"]]
    r["gates"] = _gates(r["tier"])
    r["receipt_sha256"] = receipt_digest(r)
    return r


def _classify(repo, base, head, r):
    revision(repo, base), revision(repo, head)
    own = Path(__file__).read_bytes()
    r["classifier"]["sha256"] = hashlib.sha256(own).hexdigest()
    r["classifier"]["revision"] = base
    base_clf = blob(repo, base, CLASSIFIER_PATH)
    r["classifier"]["blob_id"] = git(repo, "rev-parse", f"{base}:{CLASSIFIER_PATH}", check=False).stdout.decode().strip() or None
    r["classifier"]["trusted"] = base_clf == own
    reg_bytes = blob(repo, base, REGISTRY_PATH)
    reasons, why_incomplete = [], []
    ci, pr = r["ci"], r["ci"].get("pr")
    if pr and pr.get("base_sha") != base:
        raise Malformed(f"base {base} differs from the pull request base {pr.get('base_sha')}")
    if not (pr and ci.get("source") == "github-api" and pr.get("head_sha") == head):
        reasons.append("base and head are not bound to a pull request read from GitHub: no tier below 2 is issued")
    if not r["classifier"]["trusted"]:
        reasons.append("classifier is not the base revision's bytes: untrusted")
    if reg_bytes is None:
        reasons.append("no registry at the base revision: untrusted")
        reg = None
    else:
        r["registry"]["sha256"] = hashlib.sha256(reg_bytes).hexdigest()
        try:
            reg = Registry(reg_bytes.decode("utf-8"))
        except (Malformed, UnicodeDecodeError) as e:
            reg = None
            r["malformed"].append(f"registry: {e}")
            reasons.append("registry at the base revision is malformed")
    if reg is None:
        r["tier"], r["tier_reasons"] = 2, reasons
        return
    entries, copy_skipped = diff_entries(repo, base, head)
    if copy_skipped:
        why_incomplete.append("copy detection was skipped (too many files): a copied protected source may be unseen")
    if not entries:
        reasons.append("empty change: nothing to classify")
    items, special = [], False          # items: (path, side)
    for e in entries:
        row = {"status": e["status"], "old": None, "new": None, "old_mode": e["old_mode"], "new_mode": e["new_mode"]}
        for side, raw_key, mode, sha, tree in (("old", "old_raw", e["old_mode"], e["old_sha"], base),
                                               ("new", "new_raw", e["new_mode"], e["new_sha"], head)):
            raw = e[raw_key]
            if raw is None:
                continue
            try:
                p = normalize_path(raw)
            except Malformed as m:
                r["malformed"].append(f"{side} path: {m}")
                continue
            row[side] = p
            items.append((p, side))
            if mode == "160000":
                special = True
                why_incomplete.append(f"submodule at {p}: cannot be analysed")
            elif mode == "120000":
                special = True
                t = resolve_symlink(repo, tree, p, sha)
                if t is None:
                    why_incomplete.append(f"unresolved symlink at {p}")
                else:
                    items.append((t, "symlink_target"))
        r["paths"].append(row)
    if r["malformed"]:
        why_incomplete.append("malformed path or registry entry")
    uniq = list(dict.fromkeys(items))
    all_inert = bool(uniq) and all(reg.inert(p) for p, _ in uniq) and not r["malformed"]
    hits = [(p, side, *h) for p, side in uniq for h in reg.tier2(p)]
    hard = [h for h in hits if not h[4]]
    pure = all_inert and not hard
    effective = hits if not pure else hard
    r["capability_matches"] = [{"path": p, "side": side, "rule": rid, "capability": cap} for p, side, rid, cap, _ in effective]
    uncovered = sorted({p for p, _ in uniq if not reg.covered_by_scope(p)})
    r["coverage"] = {"complete": not (uncovered or why_incomplete or reasons), "uncovered": uncovered,
                     "reasons": why_incomplete + reasons}
    ci = r["ci"]
    if reasons or effective or not r["coverage"]["complete"]:
        r["tier"] = 2
        if effective:
            reasons.append("protected path or capability: " + ", ".join(sorted({m["rule"] for m in r["capability_matches"]})))
        reasons += why_incomplete + ([f"no registered coverage: {', '.join(uncovered)}"] if uncovered else [])
    elif all_inert and not special and ci["passed"]:
        r["tier"] = 0
        reasons.append("every old/new path is inert and every required check succeeded on the exact head")
    else:
        r["tier"] = 1
        reasons.append("complete coverage, no Tier 2 match" if not all_inert or special else
                       f"Tier-0-eligible paths, but required-check evidence is {ci['state']}: fallback to Tier 1")
    r["tier_reasons"] = reasons


# ----------------------------------------------------------------- required checks

def _json_docs(text):
    dec, i, docs = json.JSONDecoder(), 0, []
    text = text.strip()
    while i < len(text):
        doc, i = dec.raw_decode(text, i)
        docs.append(doc)
        while i < len(text) and text[i].isspace():
            i += 1
    return docs


def _run_state(run, head):
    if run.get("status") != "completed":
        return "pending"
    return {"success": "success", "failure": "failed", "timed_out": "failed", "action_required": "failed",
            "cancelled": "cancelled", "skipped": "skipped", "neutral": "neutral", "stale": "stale_head"}.get(
        run.get("conclusion"), "incomplete")


def _app(v):
    return v if isinstance(v, int) and not isinstance(v, bool) and v > 0 else None      # null, 0 and -1 mean any producer


def fetch_ci(gh, slug, head, base_branch, pr=None):
    """Required-check evidence for the exact head, read from the GitHub API through `gh(path) -> (rc, out, err)`.
    With `pr` it also records the pull request's own base and head SHA, which `classify` and `verify` bind to.
    A required check is a context plus, when registered, its producing app. Only SELF_CHECK_NAME is excluded."""
    ev = {"source": "github-api", "repo": slug, "head_sha": head, "base_branch": base_branch, "pr": None, "required": [],
          "excluded_self": [], "checks": [], "state": "incomplete", "passed": False, "blocking_state": "incomplete", "notes": []}

    def done(state, note=None):
        ev["state"], ev["passed"] = state, state == "success"
        ev["blocking_state"] = state if state in CI_BLOCKING else None
        if note:
            ev["notes"].append(note)
        return ev

    if pr is not None:
        rc, out, err = gh(f"repos/{slug}/pulls/{pr}")
        try:
            doc = json.loads(out) if rc == 0 else None
            now, branch, bsha = doc["head"]["sha"], doc["base"]["ref"], doc["base"]["sha"]
        except (ValueError, KeyError, TypeError):
            return done("incomplete", f"pull request {pr} unreadable: {err.strip()[:120]}")
        ev["pr"] = {"number": pr, "base_sha": bsha, "head_sha": now}
        caller_branch, ev["base_branch"] = base_branch, branch
        if caller_branch and caller_branch != branch:
            return done("incomplete", f"caller base branch {caller_branch} differs from the pull request base branch {branch}")
        base_branch = branch
        if now != head:
            return done("stale_head", f"pull request head is {now}, not {head}")
    if not base_branch:
        return done("incomplete", "base branch unknown")
    required = set()                                                    # {(context, app id or None)}
    rc, out, err = gh(f"repos/{slug}/rules/branches/{base_branch}")
    try:
        for rule in json.loads(out) if rc == 0 else None:
            if rule.get("type") == "required_status_checks":
                required |= {(c["context"], _app(c.get("integration_id"))) for c in rule["parameters"]["required_status_checks"]}
    except (ValueError, KeyError, TypeError):
        return done("incomplete", f"branch rules unreadable: {err.strip()[:120]}")
    # `branches/<b>` shows classic protection to any reader; the `/protection` endpoint needs admin and 403s for CI tokens.
    rc, out, err = gh(f"repos/{slug}/branches/{base_branch}")
    try:
        rsc = json.loads(out)["protection"]["required_status_checks"] if rc == 0 else None
        required |= {(c, None) for c in rsc.get("contexts") or []}
        required |= {(c["context"], _app(c.get("app_id"))) for c in rsc.get("checks") or []}
    except (ValueError, KeyError, TypeError, AttributeError):
        return done("incomplete", f"branch protection unreadable: {err.strip()[:120]}")
    ev["excluded_self"] = sorted({c for c, _ in required if c == SELF_CHECK_NAME})
    applicable = sorted(((c, a) for c, a in required if c != SELF_CHECK_NAME), key=lambda t: (t[0], t[1] is not None, t[1] or 0))   # a context may be registered with and without an app
    ev["required"] = [c if a is None else f"{c}@{a}" for c, a in applicable]
    if not applicable:
        return done("no_required_checks", "no applicable required check exists for the base branch")
    runs, statuses = [], []
    for path, key, sink in ((f"repos/{slug}/commits/{head}/check-runs?per_page=100", "check_runs", runs),
                            (f"repos/{slug}/commits/{head}/status?per_page=100", "statuses", statuses)):
        rc, out, err = gh(path)
        try:
            for doc in _json_docs(out) if rc == 0 else None:
                sink += doc[key]
        except (ValueError, KeyError, TypeError):
            return done("incomplete", f"{key} unreadable: {err.strip()[:120]}")
    for name, app in applicable:
        mine = [x for x in runs if x.get("name") == name and (app is None or (x.get("app") or {}).get("id") == app)]
        on_head = sorted((x for x in mine if x.get("head_sha") == head), key=lambda x: x.get("id", 0))
        latest = {(x.get("app") or {}).get("id"): x for x in on_head}       # newest run per producer
        states = [_run_state(x, head) for x in latest.values()]
        if mine and not on_head:
            states.append("stale_head")
        if app is None:                                                    # a legacy status has no producer to match
            states += [{"success": "success", "pending": "pending"}.get(s.get("state"), "failed")
                       for s in statuses if s.get("context") == name]
        state = next((s for s in CI_PRIORITY if s in states), "absent")     # worst across producers
        ev["checks"].append({"name": name, "app_id": app, "state": state})
    states = {c["state"] for c in ev["checks"]}
    return done(next(s for s in CI_PRIORITY if s in states))


# ------------------------------------------------------------------- verify / CLI

def _load_receipt(receipt):
    if isinstance(receipt, (str, bytes)):
        try:
            receipt = json.loads(receipt)
        except ValueError:
            raise Malformed("receipt is not JSON")
    if not isinstance(receipt, dict) or set(receipt) != RECEIPT_KEYS:
        raise Malformed("receipt keys are not exactly the receipt schema")
    if receipt["schema"] != SCHEMA or receipt["tier"] not in (0, 1, 2) or isinstance(receipt["tier"], bool):
        raise Malformed("receipt schema or tier is invalid")
    if not digest_ok(receipt):
        raise Malformed("receipt digest does not match its content")
    return receipt


def tier_of(receipt):
    """Structure-only tier of a receipt: any malformed receipt is Tier 2. Authentic only after `verify`."""
    try:
        return _load_receipt(receipt)["tier"]
    except Malformed:
        return 2


def verify(receipt, repo, ci=None):
    """Recompute the classification from git (and the supplied live evidence) and require equality."""
    try:
        rec = _load_receipt(receipt)
    except Malformed as e:
        return False, [str(e)]
    try:
        ci = ci(rec["head"]) if callable(ci) else ci
    except Exception as e:
        return False, [f"evidence collection failed: {type(e).__name__}: {e}"]
    pr = (ci or {}).get("pr")
    if pr and pr.get("base_sha") != rec["base"]:
        return False, [f"receipt base {rec['base']} differs from the pull request base {pr.get('base_sha')}"]
    if rec["error"]:
        return False, ["receipt records a classification error"]
    fresh = classify(repo, rec["base"], rec["head"], ci=ci)
    diff = sorted(k for k in RECEIPT_KEYS if fresh.get(k) != rec.get(k))
    return (not diff), [f"recomputed {k} differs from the receipt" for k in diff]


def gh_api(path):
    r = subprocess.run(["gh", "api", "--paginate", path], capture_output=True, text=True)
    return r.returncode, r.stdout, r.stderr


def _live_ci(a):
    """A collector for classify/verify (None without --github). Called inside their fail-closed guards."""
    return (lambda head: fetch_ci(gh_api, a.github, head, None, pr=a.pr)) if a.github else None


def main(argv):
    ap = argparse.ArgumentParser(prog="mechanical_tier.py", allow_abbrev=False, description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("classify", "verify"):
        p = sub.add_parser(name, allow_abbrev=False)
        p.add_argument("--repo", required=True)
        p.add_argument("--github", help="OWNER/REPO: read the pull request and its required-check evidence from the GitHub API")
        p.add_argument("--pr", type=int, help="pull request number (required with --github)")
        if name == "classify":
            p.add_argument("--base", required=True)
            p.add_argument("--head", required=True)
            p.add_argument("--out")
        else:
            p.add_argument("--receipt", required=True)
    a = ap.parse_args(argv)
    if a.github and not re.fullmatch(r"[\w.-]+/[\w.-]+", a.github):
        ap.error("--github has an unsafe form")
    if bool(a.github) != (a.pr is not None):
        ap.error("--github and --pr go together: the base and head are bound to the pull request")
    if a.cmd == "classify":
        r = classify(a.repo, a.base, a.head, ci=_live_ci(a))
        text = json.dumps(r, indent=2, sort_keys=True) + "\n"
        if a.out:
            Path(a.out).write_text(text)
        sys.stdout.write(text)
        print(f"mechanical-tier: tier={r['tier']} ci={r['ci']['state']} trusted={r['classifier']['trusted']}", file=sys.stderr)
        return 1 if r["error"] else 0
    text = ""
    try:
        text = sys.stdin.read() if a.receipt == "-" else Path(a.receipt).read_text()
        ok, problems = verify(text, a.repo, ci=_live_ci(a))
    except Exception as e:      # an unreadable receipt or any other failure is invalid, never a traceback
        ok, problems = False, [f"{type(e).__name__}: {e}"]
    print(json.dumps({"valid": ok, "tier": tier_of(text) if ok else 2, "problems": problems}, indent=2))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
