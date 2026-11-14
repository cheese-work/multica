#!/usr/bin/env python3
"""ci-policy checker: enforces rules R1 to R8 and decisions D-1, D-3, D-7, D-8 of
standards/general/ci.md. Reads only .github/ci-policy.yml and .github/workflows (no token)."""
import argparse
import itertools
import re
import sys
from datetime import date
from pathlib import Path
from typing import NamedTuple

import yaml

CLASSES = {"pr-validation", "main-build", "deploy", "gate", "scheduled", "manual"}
POLICY = ".github/ci-policy.yml"
SELECTOR = "changes"  # D-5: the job that decides relevance
HEADER = re.compile(r"^#\s*ci-policy:\s*class=([\w-]+)", re.M)


class Violation(NamedTuple):
    workflow: str
    rule: str
    reason: str


class Workflow(NamedTuple):
    file: str
    cls: str | None
    on: dict
    doc: dict


def triggers(raw) -> dict:
    if isinstance(raw, str):
        return {raw: {}}
    if isinstance(raw, list):
        return {event: {} for event in raw}
    return {event: cfg if isinstance(cfg, dict) else {} for event, cfg in (raw or {}).items()}


def load_workflows(root: Path) -> tuple[list[Workflow], list[Violation]]:
    workflows, problems = [], []
    for path in sorted((root / ".github/workflows").glob("*.y*ml")):
        text = path.read_text()
        try:
            doc = yaml.safe_load(text) or {}
        except yaml.YAMLError as err:
            problems.append(Violation(path.name, "parse", f"invalid YAML: {err}"))
            continue
        header = HEADER.search(text)
        # PyYAML (YAML 1.1) reads the key `on` as boolean True
        workflows.append(Workflow(path.name, header and header[1], triggers(doc.get("on", doc.get(True))), doc))
    return workflows, problems


def load_policy(root: Path) -> dict:
    return yaml.safe_load((root / POLICY).read_text()) or {}


def evaluate(cond, event, known=None, funcs=None):
    """Three-valued evaluation of a workflow `if:`/`cancel-in-progress` value for one event.

    Only `github.event_name`, the `known` atoms and the `funcs` (name -> value of a call without arguments) are known; any
    other atom or call is unknown (None), so callers treat "not provably False" as admitted. Parse trouble, and any token
    left over after the expression, also yield unknown."""
    if cond is None or isinstance(cond, bool):
        return cond
    expr = re.sub(r"^\s*\$\{\{(.*)\}\}\s*$", r"\1", str(cond), flags=re.S)
    toks = re.findall(r"'(?:[^']|'')*'|&&|\|\||==|!=|[()!,]|[^\s()!,&|=']+", expr)
    pos = 0

    def peek():
        return toks[pos] if pos < len(toks) else None

    def take():
        nonlocal pos
        pos += 1
        return toks[pos - 1]

    def truth(v):
        return v if v is None or isinstance(v, bool) else bool(v)

    def or_():
        left = and_()
        while peek() == "||":
            take()
            a, b = truth(left), truth(and_())
            left = True if a or b else (None if a is None or b is None else False)
        return left

    def and_():
        left = cmp_()
        while peek() == "&&":
            take()
            a, b = truth(left), truth(cmp_())
            left = False if a is False or b is False else (None if a is None or b is None else True)
        return left

    def cmp_():
        left = unary()
        if peek() in ("==", "!="):
            op, right = take(), unary()
            return None if left is None or right is None else (left == right) == (op == "==")
        return left

    def unary():
        if peek() == "!":
            take()
            v = truth(unary())
            return None if v is None else not v
        return atom()

    def atom():
        tok = take()
        if tok == "(":
            v = or_()
            if take() != ")":
                raise ValueError("unbalanced")
            return v
        if tok.startswith("'"):
            return tok[1:-1].replace("''", "'")
        if peek() == "(":  # function call: skip the balanced argument list
            depth, start = 0, pos
            while True:
                t = take()
                depth += (t == "(") - (t == ")")
                if depth == 0:
                    return (funcs or {}).get(tok) if pos - start == 2 else None
        return {"github.event_name": event, "true": True, "false": False, **(known or {})}.get(tok)

    try:
        value = truth(or_())
    except (IndexError, ValueError):
        return None
    return value if pos == len(toks) else None  # a suffix the parser did not read (`< 3`, a second `==`) makes it unknown


ALWAYS = {"always": True, "cancelled": False}  # how a no-argument always() and cancelled() read when a job is judged by event
MAX_MATRIX = 256  # GitHub itself rejects a matrix with more jobs than this


def matrix_size(matrix: dict) -> int:
    """Upper bound on the jobs a matrix makes, computed from list lengths without building the product."""
    size = 1
    for k, v in matrix.items():
        if isinstance(v, list) and k not in ("include", "exclude"):
            size *= len(v)
            if size > MAX_MATRIX:
                return size
    include = matrix.get("include")
    return size + (len(include) if isinstance(include, list) else 0)


def job_names(job_id: str, job: dict, by_file: dict) -> list[tuple[str, dict]]:
    """(check name, job that runs it) pairs a job produces (D-3); a reusable call yields the callee's jobs.
    A name ending in ' / ' marks a remote callee that cannot be read."""
    name = job.get("name") or job_id
    uses = job.get("uses")
    if uses:
        callee = by_file.get(Path(uses).name) if uses.startswith("./") else None
        if callee is None:
            return [(f"{name} / ", job)]
        return [(f"{name} / {n}", leaf) for cid, cjob in (callee.doc.get("jobs") or {}).items()
                for n, leaf in job_names(cid, cjob, {})]
    matrix = (job.get("strategy") or {}).get("matrix")
    if not isinstance(matrix, dict):
        return [(name, job)]
    axes = {k: v for k, v in matrix.items() if isinstance(v, list) and k not in ("include", "exclude")}
    if matrix_size(matrix) > MAX_MATRIX:  # a PR controls the axes: never expand an oversized product
        return [(f"{name} / ", job)]
    combos = [dict(zip(axes, vals)) for vals in itertools.product(*axes.values())] if axes else []
    subset = lambda part, whole: all(k in whole and whole[k] == v for k, v in part.items())
    combos = [c for c in combos if not any(subset(x, c) for x in matrix.get("exclude") or [])]
    labels = [list(c.values()) for c in combos]  # default names use the original axis values only
    original = len(combos)  # an include only extends original combos, never ones added by an earlier include
    for inc in (i for i in matrix.get("include") or [] if isinstance(i, dict)):
        # it extends every original combo whose axis values it does not overwrite, else it is a new combo
        hits = [n for n in range(original) if all(combos[n][k] == inc[k] for k in axes if k in inc)]
        for n in hits:
            combos[n] = {**combos[n], **{k: v for k, v in inc.items() if k not in axes}}
        if not hits:
            combos.append(inc)
            labels.append(list(inc.values()))
    if not combos:
        return [(name, job)]
    if job.get("name"):
        return [(re.sub(r"\$\{\{\s*matrix\.([\w-]+)\s*\}\}", lambda m, c=c: str(c.get(m[1], m[0])), name), job) for c in combos]
    return [(f"{job_id} ({', '.join(map(str, lab))})", job) for lab in labels]


def name_matches(required: str, produced: str) -> bool:
    return required == produced


def concurrency(doc: dict) -> dict:
    conc = doc.get("concurrency")
    return {"group": conc} if isinstance(conc, str) else conc or {}


def only_default_branch(cfg: dict, default_branch: str) -> bool:
    branches = cfg.get("branches")
    if branches:
        return set([branches] if isinstance(branches, str) else branches) <= {default_branch}
    return "tags" in cfg and "branches-ignore" not in cfg  # a tags-only push never runs for a branch push


def deploy_markers(w: Workflow, overrides: dict) -> list[str]:
    markers = [f"file name {w.file}"] if re.search(r"deploy|release", w.file) else []
    for jid, job in (w.doc.get("jobs") or {}).items():
        if overrides.get(jid) == "deploy":  # D9: the job is declared a deploy job
            continue
        if "environment" in job:
            markers.append(f"job {jid} environment")
        if re.search(r"-deploy\b", str(job.get("runs-on"))):
            markers.append(f"job {jid} *-deploy runner label")
    return markers


def workflow_rules(w: Workflow, producing: bool, mq: bool, private: bool, default_branch: str,
                   overrides: dict) -> list[Violation]:
    out = []

    def bad(rule, reason):
        out.append(Violation(w.file, rule, reason))

    for jid, job in (w.doc.get("jobs") or {}).items():
        matrix = (job.get("strategy") or {}).get("matrix") if isinstance(job, dict) and isinstance(job.get("strategy"), dict) else None
        if isinstance(matrix, dict) and matrix_size(matrix) > MAX_MATRIX:
            bad("D-3", f"job {jid} matrix expands to more than {MAX_MATRIX} jobs: the check names cannot be verified")
    if w.cls not in CLASSES:
        bad("D-1", "unclassified workflow: add `# ci-policy: class=<" + "|".join(sorted(CLASSES)) + ">`"
            if w.cls is None else f"unknown class {w.cls!r}")
    elif w.cls != "deploy" and (markers := deploy_markers(w, overrides)):
        bad("D-1", f"class is {w.cls} but it has deploy markers ({'; '.join(markers)}): class must be deploy")

    jobs = w.doc.get("jobs") or {}
    conc = concurrency(w.doc)
    if "push" in w.on:  # R1
        if mq and producing:
            bad("R1", "merge-queue repo: workflow produces a required check or status and must not run on push (D-4)")
        elif (w.cls == "pr-validation" or producing) and not only_default_branch(w.on["push"], default_branch):
            bad("R1", f"push trigger must be limited to branches: [{default_branch}]")
    pr_events = [e for e in ("pull_request", "pull_request_target") if e in w.on]
    if w.cls == "pr-validation" and pr_events:  # R2
        if not conc.get("group"):
            bad("R2", "no workflow-level concurrency group")
        elif not re.search(r"github\.(event\.pull_request\.number|head_ref|ref)\b", str(conc["group"])):
            bad("R2", "concurrency group is not per PR (use github.event.pull_request.number or github.ref)")
        for event in pr_events if conc.get("group") else []:  # omitted cancel-in-progress defaults to false
            cancel = conc.get("cancel-in-progress")
            if cancel is None or evaluate(cancel, event) is False:
                bad("R2", f"cancel-in-progress must be true for {event} events")
    if w.cls == "main-build":  # R3
        cancel = conc.get("cancel-in-progress")
        if not conc.get("group"):
            bad("R3", "no concurrency group: every push starts a parallel run")
        elif re.search(r"github\.(sha|run_id|run_number)\b", str(conc["group"])):
            bad("R3", "concurrency group is per run, not a stable group per ref")
        if cancel is not None and evaluate(cancel, None) is not False:
            bad("R3", "cancel-in-progress must be false (one running, one pending)")
    deploys = list(jobs) if w.cls == "deploy" else [j for j in jobs if overrides.get(j) == "deploy"]  # D9 overrides count
    if deploys and w.cls in CLASSES:  # R4: the workflow-level group also cancels an overridden deploy job
        for where, c in [("workflow", conc)] + [(f"job {j}", concurrency(jobs[j])) for j in deploys]:
            if c.get("cancel-in-progress") is not None and evaluate(c["cancel-in-progress"], None) is not False:
                bad("R4", f"{where}: deploy must never cancel in progress")
    if private:  # R6
        for jid, job in jobs.items():
            runs = job.get("runs-on")
            if runs is not None and "self-hosted" not in str(runs) and not (isinstance(runs, dict) and "group" in runs):
                bad("R6", f"job {jid} runs on {runs!r}, not a self-hosted runner")
    return out


def required_check_rules(w: Workflow, name: str, job_id: str, job: dict, mq: bool) -> list[Violation]:
    out = []
    for gated in ([("pull_request", "pull_request_target")] + ([("merge_group",)] if mq else [])):
        event = next((e for e in gated if e in w.on), None)
        if event is None:
            out.append(Violation(w.file, "R5", f"produces required check {name!r} but has no {gated[0]} trigger"))
            continue
        if {"paths", "paths-ignore"} & w.on[event].keys():
            out.append(Violation(w.file, "R5", f"required check {name!r}: {event} trigger has a paths filter (check can hang)"))
        if evaluate(job.get("if"), event, None, ALWAYS) is False:  # the same reading of always() as R8's skip_witness
            out.append(Violation(w.file, "R5", f"required check {name!r}: job {job_id} `if:` excludes {event}"))
    return out


BAD_RESULTS = ("failure", "cancelled", "skipped")


def skip_covered(cond, producer_if, need: str) -> bool:
    """True when `cond` fires on `needs.<need>.result == skipped` for every value of the selector's outputs that
    leaves the producer job relevant (its `if:` not provably False). The outputs are tried over the string literals
    of both conditions plus an empty and an unrelated value. Anything still unknown after that counts as not covered."""
    both = f"{cond} {producer_if or ''}"
    atoms = sorted(set(re.findall(rf"needs\.{SELECTOR}\.outputs\.[\w\-]+", both)))
    if len(atoms) > 4:
        return False
    values = sorted({m.replace("''", "'") for m in re.findall(r"'((?:[^']|'')*)'", both)} | {"", "\0other"})
    for combo in itertools.product(values, repeat=len(atoms)):
        known = dict(zip(atoms, combo))
        if evaluate(producer_if, None, known) is not False and \
                evaluate(cond, None, {**known, f"needs.{need}.result": "skipped"}) is not True:
            return False
    return True


def failing_states(job: dict, need: str, producer_if=None) -> set[str]:
    """Results of `need` on which a step fails the job (R8). Only two step shapes are understood: a single
    `test`/`[` assertion `= success` with no `if:`, and `run: exit N` under an `if:` that is evaluated per result.
    A step with `continue-on-error`, or an assertion with extra shell (`|| true`), proves nothing. `skipped` may
    also be guarded by the selector's relevance: `skip_covered` proves the condition fires whenever the producer
    job's own `if:` leaves it relevant."""
    ref = rf"\$\{{\{{\s*needs\.{re.escape(need)}\.result\s*\}}\}}"
    assertion = re.compile(rf'(?:test\s+"?{ref}"?\s+==?\s+"?success"?|\[\s+"?{ref}"?\s+==?\s+"?success"?\s+\])')
    states = set()
    for step in job.get("steps") or []:
        if step.get("continue-on-error") not in (None, False, "false"):
            continue
        run, cond = str(step.get("run") or "").strip(), step.get("if")
        if cond is None and assertion.fullmatch(run):
            states |= set(BAD_RESULTS)
        elif cond is not None and re.fullmatch(r"(exit\s+[1-9]|false)", run):
            for state in BAD_RESULTS:
                fires = evaluate(cond, None, {f"needs.{need}.result": state})
                if fires is True or (state == "skipped" and skip_covered(cond, producer_if, need)):
                    states.add(state)
    return states


AGGREGATE_EVENTS = ("pull_request", "pull_request_target", "merge_group")


def skip_witness(cond, needs: list, w: Workflow) -> str | None:
    """A case in which the aggregate's `if:` is not provably true on an event that runs the required check, or None.
    `always()` alone is not enough: `always() && needs.changes.result == 'success'` skips the job (a passing check) when
    the selector fails. Every combination of the needed jobs' results is tried; any other atom is unknown and so a witness."""
    if len(needs) > 6:
        return "it needs more than 6 jobs"
    events = [e for e in AGGREGATE_EVENTS if e in w.on] or [None]
    for event in events:
        if evaluate(cond, event, None, ALWAYS) is False:
            continue  # excluded on this event whatever the needs are: R5 reports that
        for states in itertools.product(("success", "failure", "cancelled", "skipped"), repeat=len(needs)):
            known = {f"needs.{n}.result": s for n, s in zip(needs, states)}
            if evaluate(cond, event, known, ALWAYS) is not True:
                return f"{event or 'the event'} runs with " + (", ".join(f"{n} {s}" for n, s in zip(needs, states)) or "no needs")
    return None


def aggregate_rules(w: Workflow, name: str, job_id: str, job: dict, jobs: dict) -> list[Violation]:
    """R8 (D-5, D13): a required check is an aggregate job that cannot pass when the selector failed or a job was skipped."""
    out = []

    def bad(reason):
        out.append(Violation(w.file, "R8", f"required check {name!r}: job {job_id} {reason}"))

    needs = job.get("needs") or []
    needs = [needs] if isinstance(needs, str) else needs
    cond = job.get("if")
    if not re.search(r"\balways\(\)", re.sub(r"^\s*\$\{\{(.*)\}\}\s*$", r"\1", str(cond or ""), flags=re.S)):
        bad("`if:` must use always(): a skipped or failed selector would otherwise pass the check")
    elif skippable := skip_witness(cond, needs, w):
        bad(f"`if:` can skip the aggregate when {skippable}: a skipped job passes the check, so only always() may gate it")
    if SELECTOR not in needs:
        bad(f"must need the `{SELECTOR}` selector job")
    if job.get("continue-on-error") not in (None, False, "false"):
        bad("must not set continue-on-error: the check would pass when the job fails")
    for need in needs:
        if missing := [r for r in BAD_RESULTS if r not in failing_states(job, need, (jobs.get(need) or {}).get("if"))]:
            bad(f"does not fail the job when needs.{need}.result is {', '.join(missing)}"
                f" (add a step `test \"${{{{ needs.{need}.result }}}}\" = success`)")
    return out


def job_class_rules(overrides: dict, by_file: dict) -> list[Violation]:
    """D9: every `job_classes` entry names an existing job and a known class."""
    out = []
    for file, jobs in overrides.items():
        existing = (by_file[file].doc.get("jobs") or {}) if file in by_file else {}
        for jid, cls in (jobs or {}).items():
            if jid not in existing:
                out.append(Violation(POLICY, "D-1", f"job_classes: no job {jid!r} in {file}"))
            elif cls not in CLASSES:
                out.append(Violation(POLICY, "D-1", f"job_classes: {file} {jid} has unknown class {cls!r}"))
    return out


def status_rules(ctx: dict, by_file: dict, mq: bool) -> list[Violation]:
    name, file, targets = ctx.get("name"), ctx.get("workflow"), ctx.get("targets") or {}
    w = by_file.get(file)
    if w is None:
        return [Violation(POLICY, "R7", f"status {name!r}: workflow {file!r} not found")]
    out = []
    for events, want in [(("pull_request", "pull_request_target"), "pr-head")] + ([(("merge_group",), "merge-group-head")] if mq else []):
        if not any(e in w.on for e in events):
            out.append(Violation(w.file, "R7", f"status {name!r}: no {events[0]} trigger"))
        if next((targets[e] for e in events if e in targets), None) != want:
            out.append(Violation(w.file, "R7", f"status {name!r}: declare target {want} for {events[0]}"))
    return out


def apply_exceptions(found: list[Violation], exceptions: list, today: date, repo: str) -> tuple[list[Violation], list[Violation]]:
    """D-7 and D-8: a valid exception removes matching findings; an invalid or expired one adds its own."""
    warnings, extra = [], []
    for e in exceptions:
        rule, file = e.get("rule"), e.get("workflow") or "-"
        need = ["repo", "workflow", "rule", "reason", "owner", "expires"] + (["decision"] if rule in ("R4", "R6", "R7") else [])
        errors = []
        if missing := [k for k in need if not e.get(k)]:
            errors.append(Violation(file, "D-7", f"exception for {rule}: missing {', '.join(missing)}"))
        if e.get("repo") and e["repo"] != repo:
            errors.append(Violation(file, "D-7", f"exception for {rule} is for repo {e['repo']}, not {repo}"))
        try:
            expires = e["expires"] if isinstance(e.get("expires"), date) else date.fromisoformat(str(e.get("expires")))
        except ValueError:
            expires = None
            errors.append(Violation(file, "D-7", f"exception for {rule}: invalid expires {e.get('expires')!r}"))
        if expires and (expires - today).days > 60:
            errors.append(Violation(file, "D-7", f"exception for {rule}: expires {expires} is more than 60 days out"))
        if expires and expires < today:
            late = Violation(file, "D-8", f"exception for {rule} expired {expires}")
            (errors if (today - expires).days > 7 else warnings).append(late)
        if errors:
            extra += errors
        else:
            found = [v for v in found if (v.workflow, v.rule) != (file, rule)]
    return found + extra, warnings


def check(root: Path, repo: str, private: bool = False, default_branch: str = "main",
          today: date | None = None) -> tuple[list[Violation], list[Violation]]:
    """Return (violations, warnings)."""
    if not (root / POLICY).exists():
        return [Violation(POLICY, "D-2", "policy file missing")], []
    workflows, found = load_workflows(root)
    policy = load_policy(root)
    mq = bool(policy.get("merge_queue"))
    required = policy.get("required_checks") or []
    contexts = policy.get("status_contexts") or []
    by_file = {w.file: w for w in workflows}
    overrides = policy.get("job_classes") or {}
    found += job_class_rules(overrides, by_file)
    producers = [(n, w, jid, job, leaf) for w in workflows for jid, job in (w.doc.get("jobs") or {}).items()
                 for n, leaf in job_names(jid, job, by_file)]
    producing = {w.file for n, w, *_ in producers if any(name_matches(r, n) for r in required)}
    producing |= {c.get("workflow") for c in contexts}
    for w in workflows:
        found += workflow_rules(w, w.file in producing, mq, private, default_branch, overrides.get(w.file) or {})
    for req in required:
        hits = [p for p in producers if name_matches(req, p[0])]
        if not hits:
            remote = any(req.startswith(n) for n, *_ in producers if n.endswith(" / "))
            found.append(Violation(POLICY, "R5", f"required check {req!r} is not produced by any job"
                                   + (" (remote reusable workflow names cannot be verified: add an R5 exception)" if remote else "")))
        for _, w, jid, job, leaf in hits:
            found += required_check_rules(w, req, jid, job, mq)
            siblings = next((x.doc["jobs"] for x in workflows if any(j is leaf for j in (x.doc.get("jobs") or {}).values())), {})
            found += aggregate_rules(w, req, jid, leaf, siblings)  # leaf: the callee job for a reusable call
    for ctx in contexts:
        found += status_rules(ctx, by_file, mq)
    found, warnings = apply_exceptions(list(dict.fromkeys(found)), policy.get("exceptions") or [], today or date.today(), repo)
    return found, warnings


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--root", type=Path, default=Path("."))
    ap.add_argument("--repo", required=True, help="owner/name; exceptions for other repos are rejected")
    ap.add_argument("--private", action="store_true", help="repo is private (R6)")
    ap.add_argument("--default-branch", default="main")
    ap.add_argument("--today", type=date.fromisoformat, default=None)
    ap.add_argument("--report-only", action="store_true", help="print violations, always exit 0")
    args = ap.parse_args(argv)
    violations, warnings = check(args.root, args.repo, args.private, args.default_branch, args.today)
    for v in warnings:
        print(f"WARN {args.repo} {v.workflow} {v.rule} {v.reason}")
    for v in violations:
        print(f"{args.repo} {v.workflow} {v.rule} {v.reason}")
    return 1 if violations and not args.report_only else 0


if __name__ == "__main__":
    sys.exit(main())
