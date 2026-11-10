"""CHE-4415: a security-gate success must not outlive a revoked PASS, a sibling race, a retarget or a failed downgrade.

A stateful fake GitHub runs the real `main()`. A hook fires after the Nth read of a path and changes the fake, which is
how a verdict, a retarget or a new sibling lands in the middle of an evaluation. Design: .plans/CHE-4415-gate-freshness-design.md
"""

from __future__ import annotations

import importlib.util
import json
import subprocess
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("security_gate", ROOT / "scripts/security-gate/security_gate.py")
sg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sg)

HEAD, BASE = "a" * 40, "d" * 40
BOT = 282619718
GH = "cheese-work/multica"
CLEAN_DIFF = "+++ b/app.py\n@@ -1 +1,2 @@\n+x = 1\n"
REF = "agent/x99-claude-sonnet/che-4415-x"


def verdict(result="PASS", reviewer="x99-claude-opus (claude-opus-5-5)", pr=405, base="main"):
    findings = "none" if result == "PASS" else "\n- app.py:7 shell=True on user input -> command injection"
    return "\n".join([f"Security-Gate-Verdict: {result}", f"Reviewer: {reviewer}", "Signed: x99-claude-opus, 2026-11-10 10:00 ICT",
                      f"Repo: {GH}", f"PR: {pr}", f"Base: {base}", f"SHA: {HEAD}", f"Findings: {findings}"])


class FakeGitHub:
    """PRs on one head, their comments, the head's gate status, and the status writes."""

    def __init__(self, prs=(405,), fail_writes=()):
        self.pulls = {n: {"head": {"sha": HEAD, "ref": REF}, "base": {"sha": BASE, "ref": "main"}, "user": {"id": BOT}}
                      for n in prs}
        self.comments = {n: [{"id": 1, "author_id": BOT, "body": verdict(pr=n), "updated_at": "2026-11-10T03:00:00Z"}]
                         for n in prs}
        self.status = ""
        self.posts: list[tuple[str, str]] = []  # (sha, state) per attempted status write
        self.fail_writes = set(fail_writes)  # shas whose status write raises
        self.hooks: list[list] = []  # [needle, remaining reads, action]
        self.next_id = 100

    def after_read(self, needle, nth, action):
        self.hooks.append([needle, nth, action])

    def add_comment(self, n, body):
        self.next_id += 1
        self.comments[n].append({"id": self.next_id, "author_id": BOT, "body": body, "updated_at": f"2026-11-10T04:00:{self.next_id % 60:02d}Z"})

    def gh(self, *args):
        path = " ".join(args)
        out = self._answer(args, path)
        for hook in self.hooks:
            if hook[0] in path and hook[1] > 0:
                hook[1] -= 1
                if hook[1] == 0:
                    hook[2]()
        return out

    def _answer(self, args, path):
        if "git/matching-refs" in path or "/branches" in path:
            return ""
        if "/statuses/" in path:
            sha, state = path.split("/statuses/")[1].split()[0], next(a for a in args if a.startswith("state=")).split("=")[1]
            self.posts.append((sha, state))
            if sha in self.fail_writes:
                raise subprocess.CalledProcessError(1, ["gh"], stderr="HTTP 500")
            return ""
        if "/comments" in path:
            n = int(path.split("issues/")[1].split("/")[0])
            if "author_id" in path:
                return "\n".join(json.dumps(c) for c in self.comments[n])
            return "\n".join(f"{c['id']} {c['updated_at']}" for c in self.comments[n])
        if path.endswith("/status") or "/status " in path:
            return self.status
        if "/pulls" in path and "commits/" in path:
            return "\n".join(json.dumps({"number": n, "head": p["head"]["sha"]}) for n, p in self.pulls.items())
        if "/pulls/" in path:
            return json.dumps(self.pulls[int(path.split("/pulls/")[1].split()[0])])
        raise AssertionError(path)

    def run(self, post=True, sleeps=None):
        argv = ["x", "--repo", str(ROOT), "--github", GH, "--pr", "405", "--default-branch", "main", *(["--post"] if post else [])]
        fake_run = lambda cmd, **kw: subprocess.CompletedProcess(cmd, 0, stdout=CLEAN_DIFF if kw.get("text") else CLEAN_DIFF.encode())
        printed = []
        with mock.patch.object(sg, "_gh", self.gh), mock.patch.object(sg.subprocess, "run", fake_run), \
                mock.patch.object(sg.time, "sleep", sleeps or mock.Mock()), mock.patch("sys.argv", argv), \
                mock.patch("builtins.print", lambda *a, **k: printed.append(a[0]) if a else None):
            try:
                self.code, self.error = sg.main(), None
            except BaseException as e:  # SystemExit or a failed write: the job goes red
                self.code, self.error = None, e
        self.printed = [json.loads(p) for p in printed if str(p).startswith("{")]
        return self

    def states(self, sha=HEAD):
        return [s for h, s in self.posts if h == sha]


class FinalRefresh(unittest.TestCase):
    def test_a_fail_verdict_landing_on_a_sibling_during_evaluation_never_ends_as_success(self):
        # Findings 1 and 2: the sibling's comments were outside the re-check, so its FAIL was missed.
        fake = FakeGitHub(prs=(405, 406))
        fake.after_read("issues/406/comments --jq .[] | {id, author_id", 1, lambda: fake.add_comment(406, verdict("FAIL", pr=406)))
        fake.run()
        self.assertNotIn("success", fake.states())
        self.assertEqual(fake.states()[-1], "failure")

    def test_a_sibling_opened_during_evaluation_forces_a_new_evaluation(self):
        # CHARACTERIZATION PIN: green on main already (siblings are listed after the primary is gated). Not red evidence.
        fake = FakeGitHub(prs=(405,))
        def open_sibling():
            fake.pulls[406] = json.loads(json.dumps(fake.pulls[405]))
            fake.comments[406] = []  # no verdict on the new sibling: the worst state wins
        fake.after_read("issues/405/comments --jq .[] | {id, author_id", 1, open_sibling)
        fake.run()
        self.assertNotIn("success", fake.states())

    def test_a_revoked_pass_on_a_sibling_survives_no_refresh_of_the_other_sibling(self):
        # Finding 2. CHARACTERIZATION PIN: green on main already (main re-evaluates every PR on a refresh). Not red evidence.
        # It keeps the full re-evaluation from regressing into a per-PR cache.
        fake = FakeGitHub(prs=(405, 406))
        fake.after_read("issues/405/comments --jq .[] | {id, author_id", 1, lambda: fake.add_comment(405, "unrelated note"))
        fake.after_read("issues/406/comments --jq .[] | {id, author_id", 1, lambda: fake.add_comment(406, verdict("FAIL", pr=406)))
        fake.run()
        self.assertNotIn("success", fake.states())

    def test_a_printed_success_is_refreshed_too_not_only_a_posted_one(self):
        # Finding 1: without --post the script returned success from the first evaluation unchecked.
        fake = FakeGitHub(prs=(405,))
        fake.after_read("issues/405/comments --jq .[] | {id, author_id", 1, lambda: fake.add_comment(405, verdict("FAIL")))
        fake.run(post=False)
        self.assertEqual([p["state"] for p in fake.printed], ["failure"])

    def test_an_unreadable_final_refresh_posts_pending_instead_of_crashing_with_the_old_success(self):
        fake = FakeGitHub(prs=(405,))
        fake.status = "success"
        reads = []
        real = fake._answer
        def flaky(args, path):
            if path.split()[0].endswith("/status"):
                reads.append(path)
                if len(reads) % 2 == 0:  # every second snapshot: right before the success would be posted
                    raise subprocess.CalledProcessError(1, ["gh"], stderr="HTTP 502")
            return real(args, path)
        fake._answer = flaky
        fake.run()
        self.assertNotIn("success", fake.states())
        self.assertIn("pending", fake.states())

    def test_a_pr_that_moves_on_every_evaluation_ends_pending_not_in_a_silent_exit(self):
        fake = FakeGitHub(prs=(405,))
        fake.status = "success"  # the old success must not stay the last word
        # every snapshot read of the comments moves them (a PR that never settles)
        real = fake._answer
        def moving(args, path):
            if "/comments" in path and "author_id" not in path:
                fake.add_comment(405, "tick")
            return real(args, path)
        fake._answer = moving
        fake.run()
        self.assertEqual(fake.states(), ["pending"])
        self.assertNotIn("success", fake.states())


class BaseBinding(unittest.TestCase):
    def test_a_pr_retargeted_after_a_success_was_decided_does_not_get_that_success(self):
        # Finding 3: the base was not in the re-check, so a success for `main` was posted for a PR now aimed elsewhere.
        fake = FakeGitHub(prs=(405,))
        def retarget():
            fake.pulls[405]["base"]["ref"] = "feature"
        fake.after_read("issues/405/comments --jq .[] | {id, author_id", 1, retarget)
        fake.run()
        self.assertNotIn("success", fake.states())

    def test_a_pr_retargeted_to_main_after_a_skipped_scan_is_decided_again_with_its_scans(self):
        # CHARACTERIZATION PIN: green on main already (no cache of the skip). Not red evidence.
        fake = FakeGitHub(prs=(405,))
        fake.pulls[405]["base"]["ref"] = "feature"
        def retarget():
            fake.pulls[405]["base"]["ref"] = "main"
        fake.after_read("issues/405/comments --jq .[] | {id, author_id", 1, retarget)
        diffs = []
        fake_run = lambda cmd, **kw: (diffs.append(cmd), subprocess.CompletedProcess(cmd, 0, stdout=CLEAN_DIFF if kw.get("text") else CLEAN_DIFF.encode()))[1]
        with mock.patch.object(sg.subprocess, "run", fake_run):
            fake.run()
        # whatever is posted, a `success` is only legal after a diff of the retargeted PR was read
        if "success" in fake.states():
            self.assertTrue([c for c in diffs if "diff" in c and f"{BASE}...{HEAD}" in c])


class FailedDowngradeWrite(unittest.TestCase):
    QUEUE_HEAD = "9" * 40

    def revoked(self, **kw):
        fake = FakeGitHub(prs=(405,), **kw)
        fake.status = "success"  # recorded earlier
        fake.comments[405].append({"id": 7, "author_id": BOT, "body": verdict("FAIL"), "updated_at": "2026-11-10T04:00:00Z"})
        return fake

    def test_the_downgrade_write_is_retried_and_the_job_still_fails_when_it_never_lands(self):
        fake = self.revoked(fail_writes={HEAD}).run()
        self.assertEqual(fake.states(), ["failure"] * 3)
        self.assertIsInstance(fake.error, subprocess.CalledProcessError)

    def test_queue_revocation_runs_even_when_the_downgrade_write_failed(self):
        # Finding 4: the exception from the status write skipped revoke_queue.
        fake = self.revoked(fail_writes={HEAD})
        with mock.patch.object(sg, "revoke_queue") as revoke:
            fake.run()
        revoke.assert_called_once_with(GH, 405, "main", "")

    def test_a_downgrade_write_that_lands_on_a_later_attempt_ends_clean(self):
        fake = self.revoked()
        real, tries = fake._answer, [0]
        def flaky(args, path):
            if "/statuses/" in path:
                tries[0] += 1
                if tries[0] < 3:
                    fake.posts.append((HEAD, "failure"))
                    raise subprocess.CalledProcessError(1, ["gh"], stderr="HTTP 500")
            return real(args, path)
        fake._answer = flaky
        with mock.patch.object(sg, "revoke_queue") as revoke:
            fake.run()
        self.assertIsNone(fake.error)
        self.assertEqual(fake.code, 0)
        revoke.assert_called_once()

    def test_a_failing_queue_revocation_does_not_hide_a_failed_status_write(self):
        # CHARACTERIZATION PIN: green on main (the status write raises first).
        fake = self.revoked(fail_writes={HEAD})
        with mock.patch.object(sg, "revoke_queue", side_effect=RuntimeError("queue")):
            fake.run()
        self.assertIsInstance(fake.error, (subprocess.CalledProcessError, RuntimeError))


class SuccessWriteIsNotBlindlyRetried(unittest.TestCase):
    def test_a_failed_success_write_is_not_sent_again_without_a_fresh_decision(self):
        # Sol e21567e3. CHARACTERIZATION PIN: green on main (no retry exists there). Guards the new _publish from adding one.
        fake = FakeGitHub(prs=(405,), fail_writes={HEAD})
        sleeps = mock.Mock()
        fake.run(sleeps=sleeps)
        self.assertEqual(fake.states(), ["success"])
        sleeps.assert_not_called()
        self.assertIsInstance(fake.error, subprocess.CalledProcessError)

    def test_a_good_pr_still_ends_as_exactly_one_success(self):
        # CHARACTERIZATION PIN: green on main.
        fake = FakeGitHub(prs=(405, 406)).run()
        self.assertEqual(fake.states(), ["success"])
        self.assertEqual(fake.code, 0)


if __name__ == "__main__":
    unittest.main()
