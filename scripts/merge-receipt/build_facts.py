#!/usr/bin/env python3
"""CHE-5594: turn a mechanical-tier receipt into the body of PUT /api/github/merge-receipts/ci.

Usage: build_facts.py RECEIPT.json --pr N --head SHA --base-ref REF --security-gate STATE --labels JSON --run-url URL

Only facts the receipt or GitHub prove are claimed. The cross-lab PASS and its time are not here: the server derives
them from signed verdicts. A fact the receipt does not prove becomes null or false, and the server's evaluator
treats that as a refusal. A receipt for another head is an error, not a fact.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def build(r: dict, pr: int, head: str, base_ref: str, security_gate: str, labels: list, run_url: str) -> dict:
    if r.get("head") != head:
        raise SystemExit(f"receipt head {r.get('head')} is not the PR head {head}")
    tier = r.get("tier")
    tier_ok = tier in (0, 1, 2) and not isinstance(tier, bool) and not r.get("error")
    cover = (r.get("coverage") or {}).get("complete") is True
    trusted = (r.get("classifier") or {}).get("trusted") is True
    ci = r.get("ci") or {}
    prb = ci.get("pr") or {}
    bound = (ci.get("source") == "github-api" and prb.get("head_sha") == head and prb.get("base_sha") == r.get("base")
             and prb.get("number") == pr and not r.get("error"))
    paths = sorted({p for row in r.get("paths") or [] for p in (row.get("old"), row.get("new")) if p})
    return {
        "pr_number": pr, "head_sha": head, "base_ref": base_ref,
        "tier": tier if tier_ok else None,
        "classifier_trusted": trusted, "coverage_complete": cover, "pr_bound": bound,
        # "disabled" needs the rulesets read back; this producer does not read them, so it never claims it.
        "security_gate": "success" if security_gate == "success" else "",
        # The server's evaluator still refuses an escalation path or label; this only says the diff was classified.
        "scope_classified": bool(tier_ok and cover),
        # A Multica-only veto lives in the server; a GitHub-side veto drops the receipt there.
        "gate_vetoed": False,
        "paths": paths, "labels": labels,
        "evidence": f"mechanical-tier receipt {r.get('receipt_sha256')} via {run_url}",
    }


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(allow_abbrev=False)
    ap.add_argument("receipt")
    ap.add_argument("--pr", type=int, required=True)
    ap.add_argument("--head", required=True)
    ap.add_argument("--base-ref", required=True)
    ap.add_argument("--security-gate", default="")
    ap.add_argument("--labels", default="[]")
    ap.add_argument("--run-url", required=True)
    a = ap.parse_args(argv)
    labels = json.loads(a.labels)
    if not isinstance(labels, list) or not all(isinstance(x, str) for x in labels):
        raise SystemExit("--labels must be a JSON array of strings")
    out = build(json.loads(Path(a.receipt).read_text()), a.pr, a.head, a.base_ref, a.security_gate, labels, a.run_url)
    json.dump(out, sys.stdout, sort_keys=True)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
