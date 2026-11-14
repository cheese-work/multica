"""CHE-3121: a new semgrep ERROR finding on a PR's added lines fails security-gate; baselined findings do not."""

from __future__ import annotations

import importlib.util
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("security_gate", ROOT / "scripts/security-gate/security_gate.py")
sg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sg)

HEAD = "a" * 40
BOT = 282619718
PR = {"head_sha": HEAD, "head_ref": "agent/x99-claude-sonnet/che-3121-x", "author_id": BOT,
      "repo": "cheese-work/multica-dotfiles", "number": 9, "base_ref": "main", "default_branch": "main"}
SQLI = 'cur.execute("SELECT * FROM u WHERE n = \'%s\'" % name)'
SAFE = 'cur.execute("SELECT * FROM u WHERE n = ?", (name,))'
FILE = "def q(cur, name):\n    {}\n"
HAS_SEMGREP = shutil.which(os.environ.get("SEMGREP_BIN", "semgrep")) is not None


def finding(path="app.py", start=2, rule="sqli", fp="r|app.py|1"):
    return {"fp": fp, "path": path, "start": start, "end": start, "rule": rule}


class NewSemgrepTest(unittest.TestCase):
    DIFF = "+++ b/app.py\n@@ -1 +2,1 @@\n+x\n"

    def test_finding_on_an_added_line_is_new(self):
        self.assertEqual(sg.new_semgrep([finding()], [], self.DIFF), ["app.py:2 sqli"])

    def test_finding_off_the_added_lines_is_ignored(self):
        self.assertEqual(sg.new_semgrep([finding(start=40)], [], self.DIFF), [])

    def test_baselined_finding_is_absorbed_once(self):
        two = [finding(), finding(start=3)]
        diff = "+++ b/app.py\n@@ -1 +2,2 @@\n+x\n+y\n"
        self.assertEqual(sg.new_semgrep(two, ["r|app.py|1"], diff), ["app.py:3 sqli"])

    def test_new_identical_finding_cannot_borrow_the_baseline_entry_of_an_untouched_one(self):
        old, new = finding(start=2), finding(start=9)
        diff = "+++ b/app.py\n@@ -8,0 +9,1 @@\n+x\n"
        self.assertEqual(sg.new_semgrep([old, new], ["r|app.py|1"], diff), ["app.py:9 sqli"])
        self.assertEqual(sg.new_semgrep([new, old], ["r|app.py|1"], diff), ["app.py:9 sqli"])
        before = finding(start=1)  # new finding sorts BEFORE the untouched baselined one
        diff = "+++ b/app.py\n@@ -0,0 +1,1 @@\n+x\n"
        self.assertEqual(sg.new_semgrep([before, finding(start=5)], ["r|app.py|1"], diff), ["app.py:1 sqli"])

    def test_evaluate_fails_on_new_findings_and_skips_when_not_run(self):
        args = (PR, [], {BOT}, "")
        self.assertEqual(sg.evaluate(*args, semgrep=["app.py:2 sqli"])[0], "failure")
        self.assertEqual(sg.evaluate(*args, semgrep=[])[0], "pending")
        self.assertEqual(sg.evaluate(*args)[0], "pending")


@unittest.skipUnless(HAS_SEMGREP, "semgrep not installed")
class GatePrWithSemgrepTest(unittest.TestCase):
    """Real git repo and real semgrep; only the GitHub API is faked."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = self.tmp.name

    def git(self, *a):
        return subprocess.run(["git", "-C", self.repo, "-c", "user.name=t", "-c", "user.email=t@t", *a],
                              check=True, capture_output=True, text=True).stdout.strip()

    def commit(self, text, msg):
        Path(self.repo, "app.py").write_text(text)
        self.git("add", "-A")
        self.git("commit", "-qm", msg)
        return self.git("rev-parse", "HEAD")

    def gate(self, base, head, baseline):
        verdict = "\n".join(["Security-Gate-Verdict: PASS", "Reviewer: x99-codex-sol (gpt-6.1-sol)",
                             "Signed: x99-codex-sol, 2026-11-02 18:00 ICT", "Repo: cheese-work/multica-dotfiles",
                             "PR: 9", "Base: main", f"SHA: {head}", "Findings: none"])

        def fake_gh(*args):
            if args[0] == "--paginate":
                return json.dumps({"id": 1, "author_id": BOT, "body": verdict, "updated_at": "2026-11-02T11:00:00Z"})
            return json.dumps({"head": {"sha": head, "ref": PR["head_ref"]}, "base": {"sha": base, "ref": "main"},
                               "user": {"id": BOT}})

        with mock.patch.object(sg, "_gh", fake_gh):
            return sg.gate_pr(self.repo, "cheese-work/multica-dotfiles", 9, "main", {BOT}, set(), baseline)[1:]

    def setup_base(self, body):
        self.git("init", "-qb", "main")
        return self.commit(FILE.format(body), "base")

    def test_planted_sql_injection_fails_and_the_same_branch_without_it_passes(self):
        base = self.setup_base(SAFE)
        planted = self.commit(FILE.format(SQLI), "planted")
        state, desc = self.gate(base, planted, [])
        self.assertEqual(state, "failure")
        self.assertIn("semgrep: 1 new ERROR", desc)
        clean = self.commit(FILE.format(SAFE) + "# note\n", "clean")
        self.assertEqual(self.gate(base, clean, [])[0], "success")

    def test_preexisting_finding_is_baselined_and_a_second_one_is_not(self):
        base = self.setup_base(SQLI)
        baseline = [f["fp"] for f in sg.semgrep_scan(self.repo, base)]
        self.assertTrue(baseline)
        reindented = self.commit(FILE.format(SQLI).replace("    cur", "        cur"), "touch the line")
        self.assertEqual(self.gate(base, reindented, baseline)[0], "success")
        added = self.commit(FILE.format(SQLI) + "def q2(cur, other):\n    " + SQLI.replace("name", "other") + "\n", "new")
        state, desc = self.gate(base, added, baseline)
        self.assertEqual(state, "failure")
        self.assertIn("app.py:4", desc)

    def test_nosem_comment_does_not_hide_a_finding(self):
        base = self.setup_base(SAFE)
        sneaky = self.commit(FILE.format(SQLI + "  # nosem"), "nosem")
        self.assertEqual(self.gate(base, sneaky, [])[0], "failure")

    def test_pr_authored_semgrepignore_cannot_hide_its_own_finding(self):
        base = self.setup_base(SAFE)
        Path(self.repo, ".semgrepignore").write_text("app.py\n")
        Path(self.repo, "sub").mkdir()
        Path(self.repo, "sub", ".semgrepignore").write_text("*\n")
        head = self.commit(FILE.format(SQLI), "ignore file plus vuln")
        self.git("checkout", "-q", base)  # the gate runs from a default-branch checkout, never the head
        self.assertEqual(self.gate(base, head, [])[0], "failure")

    def test_unusable_semgrep_is_pending_not_a_pass(self):
        base = self.setup_base(SAFE)
        head = self.commit(FILE.format(SAFE) + "# note\n", "clean")
        with mock.patch.dict(os.environ, {"SEMGREP_BIN": "/nonexistent/semgrep"}):
            state, desc = self.gate(base, head, [])
        self.assertEqual(state, "pending")
        self.assertIn("semgrep did not run", desc)


class WorkflowAndBaselineTest(unittest.TestCase):
    def test_baseline_file_is_committed_and_well_formed(self):
        data = json.loads((ROOT / sg.SEMGREP_BASELINE).read_text())
        self.assertEqual(data["findings"], sorted(data["findings"]))
        self.assertEqual(data["configs"], list(sg.SEMGREP_CONFIGS))


if __name__ == "__main__":
    unittest.main()
