"""CHE-3610: a PASS is keyed to the PR's own diff; a head change that leaves that diff alone keeps it."""

from __future__ import annotations

import importlib.util
import io
import json
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
DIFF = "+++ b/app.py\n@@ -1 +1,2 @@\n+x = 1\n"


def verdict(sha, digest=None):
    lines = ["Security-Gate-Verdict: PASS", "Reviewer: x99-codex-sol (gpt-6.1-sol)", "Signed: x99-codex-sol, 2026-11-04 23:50 ICT",
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
        lines = [f"Security-Gate-Verdict: {result}", f"Reviewer: {agent} (gpt-6.1)", f"Signed: {agent}, 2026-11-04 23:55 ICT",
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
        got = self._eval([self._vd("PASS", "x99-codex-terra", self.h1, self.d1),
                          self._vd("FAIL", "x99-codex-sol", self.h1, self.d1),
                          self._vd("PASS", "x99-codex-sol", stale)], self.h1)
        self.assertEqual(got, "failure")

    def test_other_reviewers_digestless_fail_survives_an_unchanged_digest_head(self):
        got = self._eval([self._vd("PASS", "x99-codex-terra", self.h1, self.d1),
                          self._vd("FAIL", "x99-codex-sol", self.h1)], "e" * 40)
        self.assertEqual(got, "failure")

    def test_same_reviewers_newer_applicable_pass_supersedes_its_earlier_fail(self):
        got = self._eval([self._vd("FAIL", "x99-codex-sol", self.h1),
                          self._vd("PASS", "x99-codex-sol", "e" * 40, self.d1)], "e" * 40)
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

    def test_waiver_for_diff_survives_a_main_only_merge_and_dies_on_a_pr_edit(self):
        """CHE-4326: a test-first waiver recorded as `waived_for_diff` follows the diff like a PASS does."""
        waiver = {"waived_for_diff": self.d1}
        m = self.advance_main("other.txt", "o2\n")
        h2 = self.merge_main()
        self.assertTrue(sg.waiver_applies(waiver, h2, self.r.digest(m, h2)))
        self.r.write("pr.txt", "l1\nPR2\n" + "".join(f"l{i}\n" for i in range(3, 11)))
        h3 = self.r.commit("edit")
        self.assertFalse(sg.waiver_applies(waiver, h3, self.r.digest(m, h3)))

    def test_waiver_dies_when_a_main_merge_changes_a_pr_file(self):
        m = self.advance_main("pr.txt", "".join(f"l{i}\n" for i in range(1, 10)) + "main\n")
        h2 = self.merge_main()
        self.assertFalse(sg.waiver_applies({"waived_for_diff": self.d1}, h2, self.r.digest(m, h2)))

    def test_legacy_waived_for_head_still_needs_the_exact_head(self):
        self.assertTrue(sg.waiver_applies({"waived_for_head": self.h1}, self.h1, self.d1))
        self.assertFalse(sg.waiver_applies({"waived_for_head": self.h1}, "e" * 40, self.d1))

    def test_waiver_without_a_usable_key_or_digest_never_applies(self):
        self.assertFalse(sg.waiver_applies({}, self.h1, self.d1))
        self.assertFalse(sg.waiver_applies({"waived_for_diff": self.d1}, self.h1, None))
        self.assertFalse(sg.waiver_applies({"waived_for_diff": "xyz"}, self.h1, "xyz"))

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


if __name__ == "__main__":
    unittest.main()
