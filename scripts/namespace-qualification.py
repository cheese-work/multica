"""Require an immutable source and an explicit namespace PASS."""

import json
from pathlib import Path
import re
import sys

TEST = "TestBindingNestedNamespace"
PACKAGE = "github.com/multica-ai/multica/server/pkg/runbinding"


def check_source(source_sha):
    if not re.fullmatch(r"[0-9a-f]{40}", source_sha):
        raise ValueError("source must be a full lowercase 40-character commit SHA")


def verify(source_sha, actual_sha, events):
    check_source(source_sha)
    if actual_sha != source_sha:
        raise ValueError("source mismatch")
    results = [e["Action"] for e in events
               if e.get("Package") == PACKAGE and e.get("Test") == TEST
               and e.get("Action") in ("pass", "fail", "skip")]
    if results != ["pass"]:
        raise ValueError(f"explicit PASS required: {results!r}")


if __name__ == "__main__":
    try:
        if len(sys.argv) == 3 and sys.argv[1] == "check-source":
            check_source(sys.argv[2])
        elif len(sys.argv) == 5 and sys.argv[1] == "verify":
            source, actual, path = sys.argv[2:]
            events = [json.loads(line) for line in Path(path).read_text().splitlines()]
            verify(source, actual, events)
            print(f"QUALIFICATION PASS: {TEST} on source {source} (off-host only)")
        else:
            raise ValueError("usage: check-source SHA | verify SHA ACTUAL JSONL")
    except (ValueError, OSError, TypeError, KeyError) as error:
        sys.exit(f"Qualification incomplete: {error}")
