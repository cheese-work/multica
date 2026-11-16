"""CHE-5594: the producer turns a mechanical-tier receipt into the facts a merge_eligibility rule reads.

It asserts nothing GitHub cannot show: the PASS and its time are the server's. Anything the receipt does not
prove becomes null or false, which the server's evaluator treats as a refusal.
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/merge-receipt/build_facts.py"
HEAD, BASE = "h" * 40, "b" * 40


def receipt(**over):
    r = {
        "tier": 1, "error": None, "base": BASE, "head": HEAD, "receipt_sha256": "d" * 64,
        "classifier": {"trusted": True}, "coverage": {"complete": True},
        "ci": {"source": "github-api", "pr": {"number": 9, "base_sha": BASE, "head_sha": HEAD}},
        "paths": [{"old": "a.go", "new": "a.go"}, {"old": None, "new": "b/c.md"}, {"old": "gone.txt", "new": None}],
    }
    r.update(over)
    return r


def build(rec, gate="success", labels=("ready",)):
    with tempfile.TemporaryDirectory() as d:
        p = Path(d) / "receipt.json"
        p.write_text(json.dumps(rec))
        out = subprocess.run(
            [sys.executable, "-I", str(SCRIPT), str(p), "--pr", "9", "--head", HEAD, "--base-ref", "main",
             "--security-gate", gate, "--labels", json.dumps(list(labels)), "--run-url", "https://example.test/run/1"],
            capture_output=True, text=True)
    return out


class BuildFacts(unittest.TestCase):
    def facts(self, rec, **kw):
        out = build(rec, **kw)
        self.assertEqual(out.returncode, 0, out.stderr)
        return json.loads(out.stdout)

    def test_a_clean_receipt_gives_the_full_fact_set(self):
        f = self.facts(receipt())
        self.assertEqual(
            {k: f[k] for k in ("pr_number", "head_sha", "base_ref", "tier", "classifier_trusted", "coverage_complete",
                               "pr_bound", "security_gate", "scope_classified", "gate_vetoed", "labels")},
            {"pr_number": 9, "head_sha": HEAD, "base_ref": "main", "tier": 1, "classifier_trusted": True,
             "coverage_complete": True, "pr_bound": True, "security_gate": "success", "scope_classified": True,
             "gate_vetoed": False, "labels": ["ready"]})
        self.assertEqual(f["paths"], ["a.go", "b/c.md", "gone.txt"])
        self.assertIn("https://example.test/run/1", f["evidence"])
        self.assertIn("d" * 64, f["evidence"])

    def test_it_never_asserts_a_pass(self):
        f = self.facts(receipt())
        self.assertNotIn("cross_lab_pass", f)
        self.assertNotIn("last_pass_at", f)

    def test_what_the_receipt_does_not_prove_is_not_claimed(self):
        cases = {
            "classification error": (receipt(error="boom"), {"tier": None, "pr_bound": False, "scope_classified": False}),
            "untrusted classifier": (receipt(classifier={"trusted": False}), {"classifier_trusted": False}),
            "incomplete coverage": (receipt(coverage={"complete": False}), {"coverage_complete": False, "scope_classified": False}),
            "no GitHub evidence": (receipt(ci={"source": "none", "pr": None}), {"pr_bound": False}),
            "receipt for another head": (receipt(ci={"source": "github-api", "pr": {"number": 9, "base_sha": BASE, "head_sha": "x" * 40}}), {"pr_bound": False}),
            "receipt base differs from the PR base": (receipt(ci={"source": "github-api", "pr": {"number": 9, "base_sha": "y" * 40, "head_sha": HEAD}}), {"pr_bound": False}),
            "invalid tier": (receipt(tier=7), {"tier": None}),
        }
        for name, (rec, want) in cases.items():
            with self.subTest(name):
                f = self.facts(rec)
                for k, v in want.items():
                    self.assertEqual(f[k], v, f"{name}: {k}")

    def test_the_security_gate_is_claimed_only_when_success(self):
        self.assertEqual(self.facts(receipt(), gate="success")["security_gate"], "success")
        for state in ("failure", "pending", "", "disabled"):
            with self.subTest(state):
                self.assertEqual(self.facts(receipt(), gate=state)["security_gate"], "")

    def test_a_receipt_for_another_head_is_refused(self):
        out = build(receipt(head="z" * 40))
        self.assertNotEqual(out.returncode, 0)


class ProducerWorkflow(unittest.TestCase):
    """The producer holds an OIDC identity, so only the default branch's schedule and a manual run may start it."""

    def test_triggers_and_head_isolation(self):
        import yaml
        text = (ROOT / ".github/workflows/merge-receipt-producer.yml").read_text()
        doc = yaml.safe_load(text)
        on = doc.get("on", doc.get(True))
        self.assertEqual(set(on), {"schedule", "workflow_dispatch"})
        self.assertEqual(doc["permissions"]["id-token"], "write")
        self.assertIn("multica-merge-receipt", text)
        # Nothing from a PR head is checked out: the head is fetched as objects only.
        checkouts = [s for s in doc["jobs"]["produce"]["steps"] if str(s.get("uses", "")).startswith("actions/checkout")]
        self.assertTrue(checkouts)
        for s in checkouts:
            self.assertNotIn("ref", s.get("with", {}))
            self.assertIs(s["with"]["persist-credentials"], False)


if __name__ == "__main__":
    unittest.main()
