"""CHE-4671: a Dependabot-authored PR is not refused for its author; it still needs the normal verdict."""

from __future__ import annotations

import importlib.util
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("security_gate", ROOT / "scripts/security-gate/security_gate.py")
sg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sg)

HEAD = "a" * 40
BOT = 282619718
DEPENDABOT = 49699333  # dependabot[bot], GitHub's numeric user id
PR = {"head_sha": HEAD, "head_ref": "dependabot/npm_and_yarn/npm-version-updates-1a2b3c", "author_id": DEPENDABOT,
      "repo": "cheese-work/multica", "number": 1, "base_ref": "main", "default_branch": "main"}
DIFF = "+++ b/package.json\n@@ -1 +1,2 @@\n+x\n"


def verdict(reviewer="x99-claude-opus (claude-opus-5-5)", result="PASS"):
    return "\n".join([f"Security-Gate-Verdict: {result}", f"Reviewer: {reviewer}", "Signed: x, 2026-11-12 11:00 ICT",
                      "Repo: cheese-work/multica", "PR: 1", "Base: main", f"SHA: {HEAD}", "Findings: none"])


def comment(body):
    return {"id": 1, "author_id": BOT, "body": body, "updated_at": "2026-11-12T04:00:00Z"}


class DependabotAuthor(unittest.TestCase):
    def run_gate(self, comments):
        return sg.evaluate(PR, comments, {BOT}, DIFF, human_ids={129714106})

    def test_dependabot_author_is_not_refused_without_a_verdict(self):
        state, why = self.run_gate([])
        self.assertEqual(state, "pending")
        self.assertNotIn("not a registered", why)

    def test_dependabot_pr_succeeds_with_the_opus_pass(self):
        self.assertEqual(self.run_gate([comment(verdict())])[0], "success")

    def test_a_fail_verdict_still_blocks_a_dependabot_pr(self):
        body = verdict(result="FAIL").replace("Findings: none", "Findings:\n- package.json:1 x -> y")
        self.assertEqual(self.run_gate([comment(body)])[0], "failure")

    def test_other_unregistered_authors_still_fail_closed(self):
        state, why = sg.evaluate({**PR, "author_id": 1}, [], {BOT}, DIFF, human_ids={129714106})
        self.assertEqual(state, "failure")
        self.assertIn("not a registered", why)

    def test_dependabot_is_never_a_verdict_signer(self):
        body = {"id": 1, "author_id": DEPENDABOT, "body": verdict(), "updated_at": "2026-11-12T04:00:00Z"}
        self.assertEqual(self.run_gate([body])[0], "pending")


if __name__ == "__main__":
    unittest.main()
