#!/usr/bin/env python3
"""CI rule against in-job waits (CHE-5175).

A job that sleeps, polls or uses `--watch` for more than 120 s while it holds a
runner fails this check. Wait with an environment wait timer or a workflow
wakeup instead; neither holds a runner.

An exception sits in .github/ci-policy.yml under `exceptions:` with
`rule: inline-wait`, `workflow`, `owner`, `root_fix` (an issue key such as
CHE-123, a URL or owner/repo#n), `added` and `expires`. `added` is not in the future, `expires` is at most 7
days after `added` and not in the past, or the exception is itself a violation.

Detection is a heuristic over each step's `run:` text:
- `--watch` (`gh run watch`, `gh pr checks --watch`);
- `sleep N` with N over 120 s (suffixes s, m, h, d);
- an integer argument over 120 passed to a script whose name has watch, wait,
  poll, readback or observe in it (the argument is its wait in seconds).
A wait that hides behind a loop or another script is not found; review catches it.

Usage: ci-wait-policy.py [repo-root]   (exit 1 when there is a violation)
"""
import re
import sys
from datetime import date, datetime
from pathlib import Path

import yaml

RULE = "inline-wait"
LIMIT_SECONDS = 120
MAX_EXCEPTION_DAYS = 7
UNITS = {"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}
COMMAND_START = r"(?:^|[;&|(]|\bthen\b|\bdo\b|\belse\b)\s*"
SLEEP = re.compile(COMMAND_START + r"sleep\s+(\d+(?:\.\d+)?)([smhd]?)(?=\s|$|[;&|)])")
WATCH = re.compile(COMMAND_START + r"(?:gh\s+run\s+watch\b|.*?\s--watch\b)")
WAIT_SCRIPT = re.compile(r"[\w./-]*(?:watch|wait|poll|readback|observe)[\w./-]*\.(?:sh|py)\b(.*)")
ROOT_FIX = re.compile(r"^(?:[A-Z][A-Z0-9]+-\d+|https?://\S+|[\w.-]+/[\w.-]+#\d+)$")


def waits_in(command):
    """Yield (description, seconds or None) for each wait found in one `run:` text."""
    for line in command.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        for amount, unit in SLEEP.findall(line):
            seconds = float(amount) * UNITS[unit]
            if seconds > LIMIT_SECONDS:
                yield f"sleep {amount}{unit} ({seconds:g} s)"
        if WATCH.search(line):
            yield "--watch"
        script = WAIT_SCRIPT.search(line)
        if script:
            for token in script.group(1).split():
                token = token.strip("\"'")
                if token.isdigit() and int(token) > LIMIT_SECONDS:
                    yield f"wait argument {token} s to a wait script"


def as_date(value):
    if isinstance(value, datetime):
        return value.date()
    if isinstance(value, date):
        return value
    try:
        return date.fromisoformat(str(value))
    except ValueError:
        return None


def exception_problem(entry, today):
    """Why an exception does not count (a sentence), or None when it is live."""
    if not str(entry.get("owner") or "").strip():
        return "no owner"
    if not ROOT_FIX.match(str(entry.get("root_fix") or "").strip()):
        return "root_fix is not an issue key, URL or owner/repo#n"
    added, expires = as_date(entry.get("added")), as_date(entry.get("expires"))
    if not added or not expires:
        return "added and expires must be dates"
    if added > today:
        return f"added {added} is in the future"
    if expires < today:
        return f"expired on {expires}"
    if (expires - added).days > MAX_EXCEPTION_DAYS:
        return f"expires {expires}, more than {MAX_EXCEPTION_DAYS} days after {added}"
    return None


def violations(document, workflow, exceptions, today):
    found = []
    for job, body in ((document or {}).get("jobs") or {}).items():
        for index, step in enumerate((body or {}).get("steps") or [], 1):
            for description in waits_in(str((step or {}).get("run") or "")):
                found.append(
                    f"{workflow}: job '{job}' step {index} holds a runner on a wait: {description}; limit {LIMIT_SECONDS} s"
                )
    if not found:
        return []
    mine = [e for e in exceptions if e.get("rule") == RULE and e.get("workflow") == workflow]
    problems = [(e, exception_problem(e, today)) for e in mine]
    if any(problem is None for _, problem in problems):
        return []
    return found + [f"{workflow}: {RULE} exception does not count: {problem}" for _, problem in problems]


def check_repo(root, today):
    root = Path(root)
    policy_file = root / ".github" / "ci-policy.yml"
    policy = yaml.safe_load(policy_file.read_text()) if policy_file.exists() else {}
    exceptions = (policy or {}).get("exceptions") or []
    found = []
    for path in sorted((root / ".github" / "workflows").glob("*.y*ml")):
        found += violations(yaml.safe_load(path.read_text()), path.name, exceptions, today)
    return found


def main():
    found = check_repo(sys.argv[1] if len(sys.argv) > 1 else ".", date.today())
    for line in found:
        print(f"::error::{line}")
    sys.exit(1 if found else 0)


if __name__ == "__main__":
    main()
