#!/usr/bin/env python3
"""Emit the push path filters each workflow declares, keyed by check name.

Some required checks are path-filtered and legitimately do not run on a commit
that touches none of their paths (`mobile` is one). admission.mjs needs to tell
"excluded by a filter" apart from "missing for an unknown reason", so it
re-derives that itself from raw evidence. This script supplies one half of that
evidence: the filter each check declares, read straight out of the workflow
that defines it. It records no judgement about whether a check may be skipped.

Output: {"<check name>": ["<glob>", ...], ...} on stdout.

Parsed with PyYAML rather than regexes on purpose. GitHub's `on:` blocks vary
enough in formatting that pattern-matching silently misses filters, and a
filter missed here reads downstream as an unexplained absence. When PyYAML is
unavailable this prints an empty object, which leaves every required check
unconditionally required in admission.mjs — that can only refuse a deploy,
never admit one.
"""

import json
import pathlib
import sys

try:
    import yaml
except ImportError:
    print("{}")
    sys.exit(0)


def main() -> int:
    workflows = pathlib.Path(".github/workflows")
    filters: dict[str, list[str]] = {}

    for path in sorted(workflows.glob("*.y*ml")):
        try:
            document = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
        except Exception:
            # An unparseable workflow contributes nothing; every check it
            # defines stays unconditionally required.
            continue
        if not isinstance(document, dict):
            continue

        # PyYAML resolves an unquoted `on:` key to the boolean True.
        triggers = document.get("on", document.get(True)) or {}
        if not isinstance(triggers, dict):
            continue
        push = triggers.get("push")
        if not isinstance(push, dict):
            continue
        paths = push.get("paths")
        if not isinstance(paths, list) or not paths:
            continue
        globs = [p for p in paths if isinstance(p, str)]
        if not globs:
            continue

        jobs = document.get("jobs")
        if not isinstance(jobs, dict):
            continue
        for job_id, job in jobs.items():
            # A check run is named after the job's `name` when it has a static
            # one, and after the job id otherwise. Record both spellings; a
            # templated name cannot be resolved to a check name here.
            if isinstance(job, dict):
                name = job.get("name")
                if isinstance(name, str) and "${{" not in name:
                    filters[name] = globs
            if isinstance(job_id, str):
                filters[job_id] = globs

    print(json.dumps(filters, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
