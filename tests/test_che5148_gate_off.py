"""CHE-5148: while the security gate is off (CHE-5126), the security-gate workflows only post `success`.

No verdict logic runs, no schedule starts a run, and the files stay so a revert switches the gate back on.
"""

from __future__ import annotations

import unittest
from pathlib import Path

import yaml

WF = Path(__file__).resolve().parents[1] / ".github/workflows"
FILES = ["security-gate.yml", "security-gate-queue.yml", "security-gate-sweep.yml"]
DESC = "security gate off (CHE-5126)"


def load(name: str) -> dict:
    doc = yaml.safe_load((WF / name).read_text())
    doc["on"] = doc.pop(True, doc.get("on"))  # PyYAML reads the bare key `on` as True
    return doc


class GateOff(unittest.TestCase):
    def test_no_verdict_logic_runs(self):
        for name in FILES:
            text = (WF / name).read_text()
            for banned in ("security_gate.py", "semgrep", "Security-Gate-Verdict:", "create-github-app-token"):
                with self.subTest(file=name, banned=banned):
                    code = "\n".join(l for l in text.splitlines() if not l.lstrip().startswith("#"))
                    self.assertNotIn(banned, code)

    def test_each_file_posts_success_with_the_off_description(self):
        for name in FILES:
            text = (WF / name).read_text()
            with self.subTest(file=name):
                self.assertIn("context=security-gate", text)
                self.assertIn("state=success", text)
                self.assertIn(DESC, text)
                self.assertNotIn("state=failure", text)

    def test_no_security_gate_file_has_a_schedule(self):
        for name in FILES:
            with self.subTest(file=name):
                self.assertNotIn("schedule", load(name)["on"])

    def test_sweep_is_dispatch_only(self):
        self.assertEqual(set(load("security-gate-sweep.yml")["on"]), {"workflow_dispatch"})

    def test_gate_has_no_comment_trigger(self):
        self.assertEqual(
            set(load("security-gate.yml")["on"]), {"workflow_call", "workflow_dispatch", "merge_group"}
        )

    def test_gate_job_name_is_stable(self):
        jobs = load("security-gate.yml")["jobs"]
        self.assertEqual(jobs["security-gate"]["name"], "security-gate-eval")
        self.assertEqual(jobs["queue-handoff"]["name"], "queue-handoff")
        self.assertIn("mechanical-tier", jobs)

    def test_ci_policy_still_lists_security_gate(self):
        text = (WF.parent / "ci-policy.yml").read_text()
        self.assertIn("security-gate", text)


if __name__ == "__main__":
    unittest.main()
