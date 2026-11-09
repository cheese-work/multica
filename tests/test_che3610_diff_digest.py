"""CHE-3610: a PASS is keyed to the PR's own diff; a head change that leaves that diff alone keeps it."""

from __future__ import annotations

import importlib.util
import io
import json
import os
import re
import shlex
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("security_gate", ROOT / "scripts/security-gate/security_gate.py")
sg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sg)

BOT = 282619718
PR = {"head_ref": "agent/x99-claude-sonnet/che-3610-1c5d373a16a5", "author_id": BOT,
      "repo": "cheese-work/multica", "number": 918, "base_ref": "main", "default_branch": "main"}
DIGEST_FOR_SERVER = r"^[0-9a-f]{64}$"  # sha256HexPattern, server/internal/handler/decision_bound_write.go
DIFF = "+++ b/app.py\n@@ -1 +1,2 @@\n+x = 1\n"


def verdict(sha, digest=None):
    lines = ["Security-Gate-Verdict: PASS", "Reviewer: x99-claude-opus (claude-opus-5-5)", "Signed: x99-claude-opus, 2026-11-04 23:50 ICT",
             "Repo: cheese-work/multica", "PR: 918", "Base: main", f"SHA: {sha}", "Findings: none"]
    if digest:
        lines.append(f"Diff-Digest: {digest}")
    return "\n".join(lines)


class Repo:
    """A throwaway git repo: `main` plus a PR branch."""

    def __init__(self, root: str):
        self.root = root
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.email", "t@t")
        self.git("config", "user.name", "t")
        self.write("pr.txt", "".join(f"l{i}\n" for i in range(1, 11)))
        self.write("other.txt", "o\n")
        self.commit("base")

    def git(self, *a):
        return subprocess.run(["git", "-C", self.root, *a], check=True, capture_output=True, text=True).stdout.strip()

    def write(self, name, text):
        (Path(self.root) / name).write_text(text)

    def commit(self, msg):
        self.git("add", "-A")
        self.git("commit", "-q", "-m", msg)
        return self.git("rev-parse", "HEAD")

    def digest(self, base, head):
        return sg.pr_diff_digest(self.root, base, head)


class DiffDigestTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.r = Repo(self.tmp.name)
        self.base = self.r.git("rev-parse", "main")
        self.r.git("checkout", "-q", "-b", "pr")
        self.r.write("pr.txt", "l1\nPR\n" + "".join(f"l{i}\n" for i in range(3, 11)))
        self.h1 = self.r.commit("pr change")
        self.d1 = self.r.digest(self.base, self.h1)

    def advance_main(self, name, text):
        self.r.git("checkout", "-q", "main")
        self.r.write(name, text)
        sha = self.r.commit("main moves")
        self.r.git("checkout", "-q", "pr")
        return sha

    def merge_main(self):
        self.r.git("merge", "-q", "--no-edit", "main")
        return self.r.git("rev-parse", "HEAD")

    def test_digest_is_sha256_of_the_full_diff_with_blob_hashes(self):
        self.assertRegex(self.d1, r"^[0-9a-f]{64}$")
        out = subprocess.run(["git", "-C", self.tmp.name, "diff", "--raw", "--patch", "--full-index", "--no-color", self.base, self.h1],
                             capture_output=True, text=True, check=True).stdout
        self.assertIn("index ", out)  # the blob-hash line is part of what is hashed
        self.assertEqual(self.d1, sg.hashlib.sha256(out.encode()).hexdigest())

    def test_clean_merge_from_main_that_touches_no_pr_file_keeps_the_digest(self):
        m = self.advance_main("other.txt", "o2\n")
        h2 = self.merge_main()
        self.assertNotEqual(h2, self.h1)
        self.assertEqual(self.r.digest(m, h2), self.d1)

    def test_editing_a_pr_file_changes_the_digest(self):
        self.r.write("pr.txt", "l1\nPR2\n" + "".join(f"l{i}\n" for i in range(3, 11)))
        h2 = self.r.commit("edit")
        self.assertNotEqual(self.r.digest(self.base, h2), self.d1)

    def test_merge_from_main_that_changes_a_pr_file_changes_the_digest(self):
        m = self.advance_main("pr.txt", "".join(f"l{i}\n" for i in range(1, 10)) + "main\n")
        h2 = self.merge_main()
        self.assertNotEqual(self.r.digest(m, h2), self.d1)

    def test_merge_from_main_that_edits_a_mode_only_pr_file_changes_the_digest(self):
        # a mode-only change has no `index` line in the patch, so the blob ids must come from somewhere else
        (Path(self.tmp.name) / "other.txt").chmod(0o755)
        h1 = self.r.commit("mode only")
        d1 = self.r.digest(self.base, h1)
        self.r.git("checkout", "-q", "main")
        self.r.write("other.txt", "o2\n")
        m = self.r.commit("main edits other")
        self.r.git("checkout", "-q", "pr")
        h2 = self.merge_main()
        self.assertNotEqual(self.r.digest(m, h2), d1)

    def test_editing_a_file_with_a_carriage_return_in_its_name_changes_the_digest(self):
        # text-mode git output turns \r in a NUL-delimited name into \n, so the file silently left the digest
        name = "a\rb.txt"
        self.r.write(name, "one\n")
        h1 = self.r.commit("add cr file")
        d1 = self.r.digest(self.base, h1)
        self.r.write(name, "two\n")
        h2 = self.r.commit("edit cr file")
        self.assertNotEqual(self.r.digest(self.base, h2), d1)

    def test_later_digestless_fail_is_not_resurrected_by_a_head_with_the_same_digest(self):
        fail = "\n".join(["Security-Gate-Verdict: FAIL", "Reviewer: x99-codex-sol (gpt-6.1-sol)",
                          "Signed: x99-codex-sol, 2026-11-04 23:55 ICT", "Repo: cheese-work/multica", "PR: 918",
                          "Base: main", f"SHA: {self.h1}", "Findings:", "- app.py:1 injects"])
        c = [{"id": 1, "author_id": BOT, "body": verdict(self.h1, self.d1), "updated_at": "2026-11-04T16:50:00Z"},
             {"id": 2, "author_id": BOT, "body": fail, "updated_at": "2026-11-04T16:55:00Z"}]
        pr = {**PR, "head_sha": "e" * 40}
        self.assertNotEqual(sg.evaluate(pr, c, {BOT}, DIFF, human_ids=set(), digest=self.d1)[0], "success")

    def _vd(self, result, agent, sha, digest=None):
        lines = [f"Security-Gate-Verdict: {result}", f"Reviewer: {agent} ({'claude-opus-5-5' if 'opus' in agent else 'gpt-6.1'})", f"Signed: {agent}, 2026-11-04 23:55 ICT",
                 "Repo: cheese-work/multica", "PR: 918", "Base: main", f"SHA: {sha}",
                 "Findings:" if result == "FAIL" else "Findings: none"]
        if result == "FAIL":
            lines.append("- app.py:1 injects")
        if digest:
            lines.append(f"Diff-Digest: {digest}")
        return "\n".join(lines)

    def _eval(self, comments, head):
        c = [{"id": i, "author_id": BOT, "body": b, "updated_at": f"2026-11-04T16:5{i}:00Z"} for i, b in enumerate(comments, 1)]
        return sg.evaluate({**PR, "head_sha": head}, c, {BOT}, DIFF, human_ids=set(), digest=self.d1)[0]

    def test_delayed_stale_pass_does_not_erase_the_same_reviewers_current_fail(self):
        stale = "a" * 40
        got = self._eval([self._vd("PASS", "x99-claude-opus", self.h1, self.d1),
                          self._vd("FAIL", "x99-codex-sol", self.h1, self.d1),
                          self._vd("PASS", "x99-codex-sol", stale)], self.h1)
        self.assertEqual(got, "failure")

    def test_other_reviewers_digestless_fail_survives_an_unchanged_digest_head(self):
        got = self._eval([self._vd("PASS", "x99-claude-opus", self.h1, self.d1),
                          self._vd("FAIL", "x99-codex-sol", self.h1)], "e" * 40)
        self.assertEqual(got, "failure")

    def test_same_reviewers_newer_applicable_pass_supersedes_its_earlier_fail(self):
        got = self._eval([self._vd("FAIL", "x99-claude-opus", self.h1),
                          self._vd("PASS", "x99-claude-opus", "e" * 40, self.d1)], "e" * 40)
        self.assertEqual(got, "success")

    def test_pass_survives_only_while_the_digest_matches(self):
        c = [{"id": 1, "author_id": BOT, "body": verdict(self.h1, self.d1), "updated_at": "2026-11-04T16:50:00Z"}]
        pr = {**PR, "head_sha": "e" * 40}
        ev = lambda digest: sg.evaluate(pr, c, {BOT}, DIFF, human_ids=set(), digest=digest)[0]
        self.assertEqual(ev(self.d1), "success")
        self.assertEqual(ev("f" * 64), "pending")
        self.assertEqual(ev(None), "pending")

    def test_verdict_without_digest_still_needs_the_exact_head(self):
        c = [{"id": 1, "author_id": BOT, "body": verdict(self.h1), "updated_at": "2026-11-04T16:50:00Z"}]
        pr = {**PR, "head_sha": "e" * 40}
        self.assertEqual(sg.evaluate(pr, c, {BOT}, DIFF, human_ids=set(), digest=self.d1)[0], "pending")
        pr = {**PR, "head_sha": self.h1}
        self.assertEqual(sg.evaluate(pr, c, {BOT}, DIFF, human_ids=set(), digest=self.d1)[0], "success")

    def test_malformed_digest_is_not_a_valid_verdict(self):
        v, err = sg.parse_verdict(verdict(self.h1, "xyz"))
        self.assertIsNone(v)
        self.assertIn("Diff-Digest", err)

    def test_print_digest_gives_lanes_the_digest_to_record_and_compare(self):
        """Browser/device lanes record this beside their PASS; the same value on a later head keeps it."""
        def run(base, head):
            pull = json.dumps({"head": {"sha": head}, "base": {"sha": base}})
            argv = ["x", "--repo", self.tmp.name, "--github", "o/r", "--pr", "918", "--default-branch", "main", "--print-digest"]
            out = io.StringIO()
            with mock.patch.object(sg, "_gh", return_value=pull), mock.patch("sys.argv", argv), redirect_stdout(out):
                self.assertEqual(sg.main(), 0)
            return json.loads(out.getvalue())

        first = run(self.base, self.h1)
        self.assertEqual(first, {"head": self.h1, "digest": self.d1})
        m = self.advance_main("other.txt", "o2\n")
        h2 = self.merge_main()
        self.assertEqual(run(m, h2), {"head": h2, "digest": self.d1})  # clean merge: same digest, PASS kept

    def _cli(self, base, head, *extra):
        """Run the published command: `security_gate.py --github o/r --pr 918 --default-branch main --print-digest`."""
        pull = json.dumps({"head": {"sha": head}, "base": {"sha": base}})
        argv = ["x", "--repo", self.tmp.name, "--github", "o/r", "--pr", "918", "--default-branch", "main",
                "--print-digest", *extra]
        out = io.StringIO()
        with mock.patch.object(sg, "_gh", return_value=pull), mock.patch("sys.argv", argv), redirect_stdout(out):
            rc = sg.main()
        return rc, json.loads(out.getvalue())

    def _record(self, body):
        f = tempfile.NamedTemporaryFile("w", suffix=".md", delete=False)
        self.addCleanup(os.unlink, f.name)
        f.write(body)
        f.close()
        return f.name

    def test_human_waiver_follows_the_diff_through_a_main_merge_and_dies_on_a_pr_edit(self):
        """CHE-4326. A human test-first waiver is a decision record that names the PR diff digest. The command says
        whether it still applies: yes after a main-only merge, no after any change to a PR file."""
        rc, first = self._cli(self.base, self.h1)
        self.assertEqual(rc, 0)
        self.assertRegex(first["digest"], DIGEST_FOR_SERVER)
        record = self._record(f"Cheese chose \"Waive this PR diff\".\nStatus: WAIVED, not PASS.\nWaiver-Diff-Digest: {first['digest']}\n")
        rc, out = self._cli(self.base, self.h1, "--waiver-record", record)
        self.assertEqual((rc, out["waiver_applies"]), (0, True))
        m = self.advance_main("other.txt", "o2\n")
        h2 = self.merge_main()
        rc, out = self._cli(m, h2, "--waiver-record", record)
        self.assertEqual((rc, out["waiver_applies"]), (0, True))  # head moved, diff did not
        self.r.write("pr.txt", "l1\nPR2\n" + "".join(f"l{i}\n" for i in range(3, 11)))
        h3 = self.r.commit("edit")
        rc, out = self._cli(m, h3, "--waiver-record", record)
        self.assertEqual((rc, out["waiver_applies"]), (3, False))  # the PR's own diff changed

    def test_record_without_a_scope_line_never_applies_even_if_it_names_the_head(self):
        """CHE-4326 round 3. Prose is not a scope. A waiver recorded before the line existed names a head in prose,
        and the prose may as well reject that head. The command fails closed; the owner re-records it once with the
        `Waiver-Diff-Digest:` line (a human reads the old record, the checker never parses it)."""
        for body in (f"Cheese waived the test-first red for head `{self.h1[:8]}` only. Status: WAIVED, not PASS.\n",
                     f"Cheese did NOT waive head {self.h1}. Status: REJECTED.\n",
                     f"Not waived for {self.h1[:12]}\n"):
            for head_base in ((self.base, self.h1),):
                rc, out = self._cli(*head_base, "--waiver-record", self._record(body))
                self.assertEqual((rc, out["waiver_applies"]), (3, False), body)

    def test_ambiguous_or_malformed_scope_lines_fail_closed(self):
        """A valid digest line beside a second, malformed, repeated or indented scope line is ambiguous: no waiver."""
        _, first = self._cli(self.base, self.h1)
        good = f"Waiver-Diff-Digest: {first['digest']}"
        other = "Waiver-Diff-Digest: " + "1" * 64
        for body in (f"{good}\n{other}\n", f"{other}\n{good}\n", f"{good}\n{good}\n", f"{good}\nWaiver-Diff-Digest: xyz\n",
                     f"{good}\nWaiver-Diff-Digest:\n", f"{good} \n", f" {good}\n", f"{good}\nwaiver-diff-digest: {first['digest']}\n",
                     f"{good}0\n", f"Waiver-Diff-Digest:{first['digest']}\n"):
            rc, out = self._cli(self.base, self.h1, "--waiver-record", self._record(body))
            self.assertEqual((rc, out["waiver_applies"]), (3, False), repr(body))
        rc, out = self._cli(self.base, self.h1, "--waiver-record", self._record(f"Status: WAIVED\r\n{good}\r\n"))
        self.assertEqual((rc, out["waiver_applies"]), (0, True))  # one exact line, CRLF record

    def test_unrelated_or_malformed_records_never_apply(self):
        _, first = self._cli(self.base, self.h1)
        for body in ("", "no scope here\n", "Waiver-Diff-Digest: xyz\n", f"Waiver-Diff-Digest: {first['digest'].upper()}\n",
                     f"Waiver-Diff-Digest: {'0' * 64}\n", f"see {first['digest']} in prose\n"):
            rc, out = self._cli(self.base, self.h1, "--waiver-record", self._record(body))
            self.assertEqual((rc, out["waiver_applies"]), (3, False), body)

    def test_docs_publish_a_command_that_parses_and_the_record_line(self):
        agent = (ROOT / "docs/agent-decisions.md").read_text()
        records = (ROOT / "docs/decision-records.md").read_text()
        record = self._record(f"Waiver-Diff-Digest: {self.d1}\n")
        for doc in (agent, records):
            lines = re.findall(r"^python3 scripts/security-gate/security_gate\.py .*--print-digest.*$", doc, re.M)
            self.assertTrue(lines, "no published command")
            for line in lines:
                argv = shlex.split(line.replace("<owner/repo>", "o/r").replace("<n>", "918").replace("<record.md>", record))
                pull = json.dumps({"head": {"sha": self.h1}, "base": {"sha": self.base}})
                with mock.patch.object(sg, "_gh", return_value=pull), mock.patch("sys.argv", ["x", "--repo", self.tmp.name, *argv[2:]]), \
                        redirect_stdout(io.StringIO()) as out:
                    self.assertEqual(sg.main(), 0)  # argparse accepts the published command and the record applies
                self.assertEqual(json.loads(out.getvalue())["digest"], self.d1)
        self.assertIn("Waiver-Diff-Digest:", records)
        self.assertIn("--waiver-record", records)
        self.assertIn("re-record it once", records)
        self.assertIn("main-only merge", agent)
        self.assertIn("main-only merge", records)


if __name__ == "__main__":
    unittest.main()
