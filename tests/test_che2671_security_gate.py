"""CHE-2671: the security-gate decision is PASS on the exact head or the merge stays blocked."""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("security_gate", ROOT / "scripts/security-gate/security_gate.py")
sg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sg)

HEAD = "a" * 40
OLD = "b" * 40
BOT = 282619718
HUMAN = 129714106
TRUSTED = {BOT}
HUMANS = {HUMAN}
PR = {"head_sha": HEAD, "head_ref": "agent/x99-claude-sonnet/che-2671-1c5d373a16a5", "author_id": BOT,
      "repo": "cheese-work/multica-dotfiles", "number": 405, "base_ref": "main", "default_branch": "main"}
CLEAN_DIFF = "+++ b/app.py\n@@ -1 +1,2 @@\n+x = 1\n"
FAKE_KEY = "AKIA" + "ABCDEFGHIJKLMNOP"  # split so this file does not trip the gate's own secret scan
VULN_DIFF = f'+++ b/app.py\n@@ -1 +1,2 @@\n+aws = "{FAKE_KEY}"\n'


def verdict(result="PASS", reviewer="x99-codex-sol (gpt-6.1-sol)", sha=HEAD, findings="none", signed=True,
            repo="cheese-work/multica-dotfiles", pr=405, base="main"):
    lines = [f"Security-Gate-Verdict: {result}", f"Reviewer: {reviewer}"]
    if signed:
        lines.append("Signed: x99-codex-sol, 2026-10-30 23:50 ICT")
    lines += [f"Repo: {repo}", f"PR: {pr}", f"Base: {base}", f"SHA: {sha}", f"Findings: {findings}"]
    return "\n".join(lines)


def comment(body, author=BOT, cid=1, updated="2026-10-30T16:50:00Z"):
    return {"id": cid, "author_id": author, "body": body, "updated_at": updated}


class GateDecisionTest(unittest.TestCase):
    def run_gate(self, comments, diff=CLEAN_DIFF, pr=PR):
        return sg.evaluate(pr, comments, TRUSTED, diff, human_ids=HUMANS)

    def test_clean_pr_with_cross_lab_pass_on_head_succeeds(self):
        state, _ = self.run_gate([comment(verdict())])
        self.assertEqual(state, "success")

    def test_no_verdict_is_pending(self):
        self.assertEqual(self.run_gate([])[0], "pending")

    def test_push_after_pass_clears_the_verdict(self):
        state, why = self.run_gate([comment(verdict(sha=OLD))])
        self.assertEqual(state, "pending")
        self.assertIn(HEAD[:12], why)

    def test_planted_secret_fails_even_with_pass(self):
        state, why = self.run_gate([comment(verdict())], diff=VULN_DIFF)
        self.assertEqual(state, "failure")
        self.assertIn("app.py:1", why)

    def test_fail_verdict_blocks_and_names_file_line_exploit(self):
        body = verdict("FAIL", findings="\n- app.py:7 shell=True on user input -> command injection via ?name=")
        state, why = self.run_gate([comment(body)])
        self.assertEqual(state, "failure")
        self.assertIn("app.py:7", why)

    def test_fail_without_file_line_is_not_a_valid_verdict_and_still_blocks(self):
        state, _ = self.run_gate([comment(verdict("FAIL", findings="looks bad"))])
        self.assertEqual(state, "pending")

    def test_same_lab_reviewer_is_rejected(self):
        state, why = self.run_gate([comment(verdict(reviewer="x99-claude-opus (claude-opus-5-5)"))])
        self.assertEqual(state, "failure")
        self.assertIn("lab", why)

    def test_author_cannot_review_itself(self):
        state, _ = self.run_gate([comment(verdict(reviewer="x99-claude-sonnet (claude-sonnet-5-5)"))])
        self.assertEqual(state, "failure")

    def test_untrusted_commenter_is_ignored(self):
        self.assertEqual(self.run_gate([comment(verdict(), author=999)])[0], "pending")

    def test_unsigned_pass_is_pending(self):
        self.assertEqual(self.run_gate([comment(verdict(signed=False))])[0], "pending")

    def test_pass_with_findings_is_pending(self):
        self.assertEqual(self.run_gate([comment(verdict(findings="app.py:1 meh"))])[0], "pending")

    def test_any_reviewer_fail_on_head_beats_another_pass(self):
        fail = verdict("FAIL", reviewer="x99-codex-luna (gpt-6-luna)", findings="\n- a.py:3 sqli via id -> dump table")
        state, _ = self.run_gate([comment(verdict(), cid=1), comment(fail, cid=2)])
        self.assertEqual(state, "failure")

    def test_reviewer_latest_verdict_replaces_their_earlier_one(self):
        fail = verdict("FAIL", findings="\n- a.py:3 sqli via id -> dump table")
        state, _ = self.run_gate([
            comment(fail, cid=1, updated="2026-10-30T16:00:00Z"),
            comment(verdict(), cid=2, updated="2026-10-30T17:00:00Z"),
        ])
        self.assertEqual(state, "success")

    def test_human_author_gets_any_agent_reviewer(self):
        pr = {**PR, "head_ref": "feature/x", "author_id": HUMAN}
        self.assertEqual(self.run_gate([comment(verdict())], pr=pr)[0], "success")

    def test_unregistered_pr_author_cannot_pick_its_own_reviewer(self):
        # finding 2: a non-agent branch used to mean "external author", so the author could sign their own PASS
        pr = {**PR, "head_ref": "feature/x", "author_id": BOT}
        state, why = self.run_gate([comment(verdict(reviewer="x99-claude-opus (claude-opus-5-5)"))], pr=pr)
        self.assertEqual(state, "failure")
        self.assertIn("author", why)
        pr = {**PR, "head_ref": "dependabot/npm/x", "author_id": 49699333}
        self.assertEqual(self.run_gate([comment(verdict())], pr=pr)[0], "failure")

    def test_verdict_for_another_pr_with_the_same_head_does_not_pass(self):
        self.assertEqual(self.run_gate([comment(verdict(pr=406))])[0], "pending")

    def test_verdict_for_another_repo_does_not_pass(self):
        self.assertEqual(self.run_gate([comment(verdict(repo="cheese-work/other"))])[0], "pending")

    def test_retarget_clears_the_verdict(self):
        # finding 3: PASS was given for base main; the PR now targets another base
        pr = {**PR, "base_ref": "release", "default_branch": "release"}
        self.assertEqual(self.run_gate([comment(verdict(base="main"))], pr=pr)[0], "pending")

    def test_stacked_base_is_pending_not_failed_and_never_passes(self):
        # CHE-3035: a stacked layer is not gated yet. Pending (not failure) keeps the PR free of a red check, and
        # not success keeps the head from reaching the default branch if the retarget event is ever missed.
        pr = {**PR, "base_ref": "evil", "default_branch": "main"}
        for comments in ([], [comment(verdict(base="evil"))]):
            state, why = self.run_gate(comments, pr=pr)
            self.assertEqual(state, "pending")
            self.assertIn("not gated", why)
            self.assertIn("main", why)

    def test_retarget_to_default_runs_the_real_gate(self):
        # CHE-3035: the same head, now based on main, is gated as before: a planted secret fails, no verdict is pending
        self.assertEqual(self.run_gate([], diff=VULN_DIFF)[0], "failure")
        self.assertEqual(self.run_gate([])[0], "pending")
        self.assertEqual(self.run_gate([comment(verdict())])[0], "success")

    def test_verdict_missing_binding_fields_is_not_valid(self):
        old = "\n".join(l for l in verdict().splitlines() if not l.startswith(("Repo:", "PR:", "Base:")))
        self.assertEqual(self.run_gate([comment(old)])[0], "pending")

    def test_combine_takes_the_worst_state_across_prs_sharing_a_head(self):
        self.assertEqual(sg.combine([("success", "ok"), ("pending", "no verdict")])[0], "pending")
        self.assertEqual(sg.combine([("success", "ok"), ("failure", "bad"), ("pending", "x")])[0], "failure")
        self.assertEqual(sg.combine([("success", "ok")])[0], "success")

    def test_plus_plus_added_line_is_scanned_not_taken_for_a_file_header(self):
        # finding 5: added line "++ 1 # <token>" renders as "+++ 1 # <token>"
        diff = f'+++ b/app.py\n@@ -1 +1,2 @@\n+++ 1 # {FAKE_KEY}\n'
        self.assertEqual(sg.scan_secrets(diff), ["app.py:1"])

    def test_secret_after_a_plus_plus_line_keeps_the_right_file_and_line(self):
        diff = (f"+++ b/a.py\n@@ -0,0 +1,2 @@\n+++ 1\n+k = '{FAKE_KEY}'\n"
                "diff --git a/b.py b/b.py\n+++ b/b.py\n@@ -0,0 +1 @@\n+ok = 1\n")
        self.assertEqual(sg.scan_secrets(diff), ["a.py:2"])

    def test_unknown_agent_author_fails_closed(self):
        pr = {**PR, "head_ref": "agent/mystery-bot/x"}
        self.assertEqual(self.run_gate([comment(verdict())], pr=pr)[0], "failure")

    def test_description_fits_github_limit(self):
        body = verdict("FAIL", findings="\n- " + "a.py:3 " + "x" * 400)
        self.assertLessEqual(len(self.run_gate([comment(body)])[1]), 140)


class WrapperTest(unittest.TestCase):
    """main() with the GitHub API and git faked: the posted status is the contract."""

    OTHER = "c" * 40

    def run_main(self, sibling_head, sibling_diff_error=True):
        fail = verdict("FAIL", findings="\n- app.py:7 shell=True on user input -> command injection")
        posted = []

        def pull(n, head, ref):
            return json.dumps({"head": {"sha": head, "ref": ref}, "base": {"sha": "d" * 40, "ref": "main"},
                               "user": {"id": BOT}})

        def fake_gh(*args):
            path = " ".join(args)
            if args[0] == "--paginate":
                return json.dumps(comment(fail)) if "issues/405/comments" in path else ""
            if "commits/" in path:
                rows = [{"number": 405, "head": HEAD}, {"number": 406, "head": sibling_head}]
                return "\n".join(json.dumps(r) for r in rows)
            if "pulls/405" in path:
                return pull(405, HEAD, PR["head_ref"])
            if "pulls/406" in path:
                return pull(406, sibling_head, "agent/x99-claude-sonnet/other")
            if "statuses/" in path:
                posted.append(args)
                return ""
            return ""

        def fake_run(cmd, **kw):
            if sibling_head in os.fsdecode(cmd[-1]) and sibling_diff_error:
                raise subprocess.CalledProcessError(128, cmd)  # fork head objects were never fetched
            return subprocess.CompletedProcess(cmd, 0, stdout=CLEAN_DIFF if kw.get("text") else CLEAN_DIFF.encode())

        argv = ["x", "--repo", str(ROOT), "--github", "cheese-work/multica-dotfiles", "--pr", "405",
                "--default-branch", "main", "--post"]
        with mock.patch.object(sg, "_gh", fake_gh), mock.patch.object(sg.subprocess, "run", fake_run), \
                mock.patch("sys.argv", argv), mock.patch("builtins.print"):
            code = sg.main()
        return code, posted

    def test_primary_fail_is_posted_even_when_an_associated_fork_head_cannot_be_diffed(self):
        # round 2 finding: a descendant PR with a different, unfetched head crashed main() before any status write
        code, posted = self.run_main(sibling_head=self.OTHER)
        self.assertEqual(code, 0)
        self.assertEqual(len(posted), 1)
        self.assertIn("state=failure", posted[0])

    def test_pr_with_a_different_head_is_not_evaluated_at_all(self):
        calls = []
        with mock.patch.object(sg, "gate_pr", side_effect=lambda *a: calls.append(a[2]) or (HEAD, "success", "ok")):
            self.run_main(sibling_head=self.OTHER, sibling_diff_error=False)
        self.assertEqual(calls, [405])

    def test_an_associated_pr_that_cannot_be_evaluated_blocks_instead_of_crashing(self):
        def boom(repo, github, n, *rest):
            if n == 406:
                raise RuntimeError("cannot read PR")
            return HEAD, "success", "PASS"
        with mock.patch.object(sg, "gate_pr", side_effect=boom):
            code, posted = self.run_main(sibling_head=HEAD, sibling_diff_error=False)
        self.assertEqual(code, 0)
        self.assertIn("state=failure", posted[0])


class MergeGroupStepTest(unittest.TestCase):
    """The merge queue requires `security-gate` on its own commit, which no PR event ever reaches. The job runs the
    DEFAULT-branch script, which on the first queue run is still the old one (`--pr` only), so the workflow step
    re-evaluates the queued PR with that script unchanged and posts the state on the queue commit itself."""

    QUEUE = "e" * 40
    REF = f"refs/heads/gh-readonly-queue/main/pr-405-{'d' * 40}"
    STUB_GH = ('#!/bin/bash\n'
               'case "$*" in\n'
               '  *commits/*) printf "%s\\n" $PARENTS ;;\n'
               '  *) echo "gh $*" >> "$LOG" ;;\n'
               'esac\n')
    STUB_GATE = ('#!/bin/bash\n'  # the old script: --pr N required (an empty N is an argparse error), verdict read live
                 'echo "gate $*" >> "$LOG"\n'
                 'case " $* " in *" --pr "[0-9]*) ;; *) exit 2 ;; esac\n'
                 'n=$(grep -c "^gate " "$LOG")\n'  # this is the Nth read: STATES answers per read, FAIL_AT makes one read error out
                 '[ "$n" = "${FAIL_AT:-}" ] && exit 1\n'
                 'if [ -n "${STATES:-}" ]; then STATE=$(echo $STATES | cut -d" " -f$n); fi\n'
                 'echo \'{"context": "security-gate", "sha": "\'$HEAD\'", "state": "\'$STATE\'", "description": "PASS on x"}\'\n')

    def step_run(self):
        steps = yaml.safe_load((ROOT / ".github/workflows/security-gate.yml").read_text())["jobs"]["security-gate"]["steps"]
        gate = [s for s in steps if "security_gate.py" in s.get("run", "")]
        self.assertEqual(len(gate), 1)
        return gate[0]["run"]

    def run_step(self, ref=REF, parents=("d" * 40, HEAD), state="success", script="", states="", fail_at=""):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "bin").mkdir()
            (tmp / "scripts/security-gate").mkdir(parents=True)
            (tmp / "bin/gh").write_text(self.STUB_GH)
            (tmp / "scripts/security-gate/gate.sh").write_text(self.STUB_GATE)
            (tmp / "scripts/security-gate/security_gate.py").write_text(script)  # the step runs it via the shadowed python3
            (tmp / "bin/python3").write_text(
                '#!/bin/bash\n[ "${1##*/}" = security_gate.py ] && exec bash "$(dirname "$1")/gate.sh" "${@:2}"\n'
                f'exec {sys.executable} "$@"\n')
            for path in (tmp / "bin/gh", tmp / "bin/python3", tmp / "scripts/security-gate/gate.sh"):
                path.chmod(0o755)
            env = {**os.environ, "PATH": f"{tmp / 'bin'}:{os.environ['PATH']}", "LOG": str(tmp / "log"), "HEAD": HEAD,
                   "STATE": state, "STATES": states, "FAIL_AT": str(fail_at), "PARENTS": " ".join(parents), "MERGE_GROUP_REF": ref, "MERGE_GROUP_SHA": self.QUEUE,
                   "PR_NUMBER": "", "GH_TOKEN": "t", "DEFAULT_BRANCH": "main", "GITHUB_REPOSITORY": "o/r",
                   "GITHUB_SERVER_URL": "https://github.com", "GITHUB_RUN_ID": "7"}
            done = subprocess.run(["bash", "-c", self.step_run()], cwd=tmp, env=env, capture_output=True, text=True)
            log = (tmp / "log").read_text().splitlines() if (tmp / "log").exists() else []
        return done, log

    def posted(self, log):
        return [l for l in log if l.startswith("gh ") and f"statuses/{self.QUEUE}" in l]

    def test_queued_pr_verdict_is_posted_on_the_queue_commit(self):
        done, log = self.run_step()
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(len(self.posted(log)), 1, log)
        self.assertIn("state=success", self.posted(log)[0])
        self.assertIn("--pr 405", " ".join(l for l in log if l.startswith("gate ")))
        self.assertNotIn("--post", " ".join(log))  # the old script would post on the PR head, not the queue commit

    def test_queue_run_scans_with_semgrep_once_the_gate_script_supports_it(self):
        done, log = self.run_step(script='ap.add_argument("--semgrep")')
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn("--semgrep", " ".join(l for l in log if l.startswith("gate ")))

    def test_queue_run_omits_semgrep_for_an_older_gate_script(self):
        done, log = self.run_step()
        self.assertNotIn("--semgrep", " ".join(l for l in log if l.startswith("gate ")))

    def test_verdict_is_read_fresh_at_queue_time(self):
        done, log = self.run_step(state="pending")
        self.assertIn("state=pending", self.posted(log)[0])

    def test_a_verdict_revoked_after_the_first_read_never_ends_as_success(self):
        # a late success must not overwrite a revocation failure: a success is read again before and after it is posted
        for states, last in (("success pending", "pending"), ("success success pending", "pending"),
                             ("success success success", "success")):
            done, log = self.run_step(states=states)
            self.assertIn(f"state={last}", self.posted(log)[-1], (states, log))

    def test_a_success_is_read_again_before_and_after_it_is_published(self):
        done, log = self.run_step(states="success success success")
        self.assertEqual(len([l for l in log if l.startswith("gate ")]), 3, log)

    def test_a_failed_read_after_the_success_was_posted_fails_closed(self):
        # round 7 P1: an error in the post-write read left the cached success in place under set -e
        done, log = self.run_step(fail_at=3)
        self.assertIn("state=success", self.posted(log)[0], log)
        self.assertIn("state=failure", self.posted(log)[-1], (done.stderr, log))

    def test_a_failed_read_before_the_success_is_posted_posts_a_failure_not_a_stall(self):
        for fail_at in (1, 2):
            done, log = self.run_step(fail_at=fail_at)
            self.assertIn("state=failure", self.posted(log)[-1], (fail_at, done.stderr, log))

    def test_pr_head_that_is_not_a_parent_fails(self):
        done, log = self.run_step(parents=("d" * 40, OLD))
        self.assertIn("state=failure", self.posted(log)[0])

    def test_queue_for_another_branch_fails_without_evaluating(self):
        done, log = self.run_step(ref=f"refs/heads/gh-readonly-queue/dev/pr-405-{'d' * 40}")
        self.assertIn("state=failure", self.posted(log)[0])
        self.assertFalse([l for l in log if l.startswith("gate ")])


class QueueRevocationTest(unittest.TestCase):
    """CHE-3240: a success carried onto a queue head must not outlive the PR-head verdict that earned it."""

    BASE, G1, G2, OTHER = "1" * 40, "2" * 40, "3" * 40, "6" * 40
    QUEUE_HEADS = {405: G1, 999: G2}  # queue head per tip PR number

    def run_pr(self, comments, queue_heads=None, queue_error=False, compare_error=False, post_fail=(), stacked=False):
        queue_heads = self.QUEUE_HEADS if queue_heads is None else queue_heads
        posted, lookups = [], []
        # a set fails every write to those heads; a dict maps head -> number of writes that fail before one succeeds
        fails = dict(post_fail) if isinstance(post_fail, dict) else dict.fromkeys(post_fail, 10**9)
        chains = {self.G1: [{"sha": self.G1, "parents": [self.BASE, HEAD], "message": "Merge pull request #405 from x/y"}],
                  self.G2: [{"sha": self.G2, "parents": [self.OTHER, "7" * 40], "message": "Merge pull request #999 from x/z"}]}
        if stacked:  # the second group sits on top of the first, so both carry the PR
            chains[self.G2] = [{"sha": self.G2, "parents": [self.G1, "7" * 40], "message": "Merge pull request #999 from x/z"}, *chains[self.G1]]

        def fake_gh(*args):
            path = " ".join(args)
            if "git/matching-refs/heads/gh-readonly-queue/main/" in path or "/branches" in path:
                lookups.append(path)
                if queue_error is True or (queue_error == "primary" and "matching-refs" in path):
                    raise subprocess.CalledProcessError(1, ["gh"])
                return "\n".join(queue_heads.values())
            if "compare/main..." in path:
                if compare_error:
                    raise subprocess.CalledProcessError(1, ["gh"])
                return "\n".join(json.dumps(c) for c in chains[path.split("...")[1].split()[0]])
            if args[0] == "--paginate":
                return json.dumps(c) if (c := comments[0]) and "issues/405/comments" in path else ""
            if "commits/" in path:
                return json.dumps({"number": 405, "head": HEAD})
            if "pulls/405" in path:
                return json.dumps({"head": {"sha": HEAD, "ref": PR["head_ref"]}, "base": {"sha": "d" * 40, "ref": "main"},
                                   "user": {"id": BOT}})
            if "statuses/" in path:
                posted.append(args)
                sha = path.split("statuses/")[1].split()[0]
                if fails.get(sha, 0) > 0:
                    fails[sha] -= 1
                    raise subprocess.CalledProcessError(1, ["gh"])
            return ""

        argv = ["x", "--repo", str(ROOT), "--github", "cheese-work/multica", "--pr", "405",
                "--default-branch", "main", "--post"]
        fake_run = lambda cmd, **kw: subprocess.CompletedProcess(cmd, 0, stdout=CLEAN_DIFF if kw.get("text") else CLEAN_DIFF.encode())
        with mock.patch.object(sg, "_gh", fake_gh), mock.patch.object(sg.subprocess, "run", fake_run), \
                mock.patch.object(sg.time, "sleep"), mock.patch("sys.argv", argv), mock.patch("builtins.print"):
            code = sg.main()
        self.lookups = lookups
        return code, posted

    def test_a_revoked_verdict_flips_the_queue_head_that_carries_the_pr_to_failure(self):
        code, posted = self.run_pr([comment("no verdict any more")])
        self.assertEqual(code, 0)
        by_sha = {a[0].rsplit("/", 1)[1]: a for a in posted if "statuses/" in a[0]}
        self.assertIn(HEAD, by_sha)  # the PR head status itself
        self.assertIn("state=pending", by_sha[HEAD])
        self.assertIn("state=failure", by_sha[self.G1])
        self.assertIn("#405", " ".join(by_sha[self.G1]))
        self.assertNotIn(self.G2, by_sha)  # a group without the PR is untouched

    def test_a_still_green_pr_leaves_the_queue_alone(self):
        _, posted = self.run_pr([comment(verdict("PASS", repo="cheese-work/multica"))])
        self.assertEqual([a[0].rsplit("/", 1)[1] for a in posted], [HEAD])

    def test_a_failing_ref_lookup_falls_back_to_the_branch_listing_and_still_flips_the_head(self):
        # review round 4 finding 1: one failing endpoint must not leave the carried success on the queue head
        with mock.patch.object(sg.time, "sleep"):
            _, posted = self.run_pr([comment("no verdict any more")], queue_error="primary")
        by_sha = {a[0].rsplit("/", 1)[1]: a for a in posted}
        self.assertIn("state=failure", by_sha[self.G1])
        self.assertTrue(any("/branches" in l for l in self.lookups))

    def test_both_lookups_failing_is_retried_then_raises(self):
        with mock.patch.object(sg.time, "sleep"), self.assertRaises(subprocess.CalledProcessError):
            self.run_pr([comment("no verdict any more")], queue_error=True)

    def test_a_group_that_cannot_be_read_is_failed_closed_in_the_persisted_status(self):
        # review round 3 finding 1: a compare error must not leave the carried success on that queue head
        with mock.patch.object(sg.time, "sleep"):
            _, posted = self.run_pr([comment("no verdict any more")], compare_error=True)
        heads = {a[0].rsplit("/", 1)[1]: a for a in posted}
        for g in (self.G1, self.G2):
            self.assertIn("state=failure", heads[g])

    def test_a_failed_status_write_on_one_head_does_not_skip_the_later_heads_and_fails_the_job(self):
        # review round 8: one failed write must not leave later queue heads on a carried success
        with self.assertRaises(RuntimeError) as ctx:
            self.run_pr_capture([comment("no verdict any more")], post_fail={self.G1})
        self.assertIn(self.G1, str(ctx.exception))
        self.assertNotIn(self.G2, str(ctx.exception))
        self.assertIn("state=failure", self.captured[self.G2])

    def test_every_failed_head_is_named_when_all_status_writes_fail(self):
        with self.assertRaises(RuntimeError) as ctx:
            self.run_pr_capture([comment("no verdict any more")], post_fail={self.G1, self.G2})
        self.assertIn(self.G1, str(ctx.exception))
        self.assertIn(self.G2, str(ctx.exception))

    def test_a_failed_write_after_a_good_one_still_raises_and_keeps_the_good_flip(self):
        with self.assertRaises(RuntimeError) as ctx:
            self.run_pr_capture([comment("no verdict any more")], post_fail={self.G2})
        self.assertIn(self.G2, str(ctx.exception))
        self.assertNotIn(self.G1, str(ctx.exception))
        self.assertIn("state=failure", self.captured[self.G1])

    def writes_to(self, posted, sha):
        return [a for a in posted if a[0].endswith(f"statuses/{sha}")]

    def test_a_transient_status_write_failure_is_retried_and_the_head_ends_on_failure(self):
        # review round 9: one failed attempt must not leave the carried success while the API has recovered
        code, posted = self.run_pr([comment("no verdict any more")], post_fail={self.G1: 2}, stacked=True)
        self.assertEqual(code, 0)
        self.assertEqual(len(self.writes_to(posted, self.G1)), 3)
        self.assertIn("state=failure", self.writes_to(posted, self.G1)[-1])
        self.assertEqual(len(self.writes_to(posted, self.G2)), 1)

    def test_a_write_that_fails_on_every_bounded_retry_is_named_and_the_other_heads_are_still_flipped(self):
        with self.assertRaises(RuntimeError) as ctx:
            self.run_pr([comment("no verdict any more")], post_fail={self.G1: 10**9}, stacked=True)
        self.assertIn(self.G1, str(ctx.exception))
        self.assertNotIn(self.G2, str(ctx.exception))

    def test_retries_are_bounded_at_three_attempts_per_head(self):
        attempts = []
        real = sg._post

        def spy(github, sha, *rest):
            attempts.append(sha)
            return real(github, sha, *rest)

        with mock.patch.object(sg, "_post", spy), self.assertRaises(RuntimeError):
            self.run_pr([comment("no verdict any more")], post_fail={self.G1: 10**9, self.G2: 10**9}, stacked=True)
        self.assertEqual([attempts.count(self.G1), attempts.count(self.G2)], [3, 3])

    def run_pr_capture(self, comments, post_fail):
        # run_pr raises before it can return `posted`; capture the attempted writes through the mock instead
        seen = {}
        real = sg._post

        def spy(github, sha, state, desc, run_url):
            seen[sha] = (f"state={state}",)
            return real(github, sha, state, desc, run_url)

        self.captured = seen
        with mock.patch.object(sg, "_post", spy):
            return self.run_pr(comments, post_fail=post_fail, stacked=True)


class WorkflowTest(unittest.TestCase):
    def setUp(self):
        self.text = (ROOT / ".github/workflows/security-gate.yml").read_text()
        self.doc = yaml.safe_load(self.text)

    def test_runs_from_base_and_never_checks_out_head(self):
        # CHE-4385: the file runs only when pr-guards calls it (pull_request_target, base definition), on a dispatch
        # of the default branch, or on the merge queue. It has no event of its own for a PR or a comment.
        self.assertEqual(set(self.doc[True]), {"workflow_call", "workflow_dispatch", "issue_comment", "merge_group"})
        self.assertIn("ref: ${{ github.event.repository.default_branch }}", self.text)
        self.assertNotIn("pull_request.head.ref", self.text)

    def test_privileged_job_runs_the_default_branch_not_the_pr_base(self):
        # finding 1: base.sha of a PR into an unprotected branch is attacker-influenced code
        self.assertIn("github.event.repository.default_branch", self.text)
        self.assertNotIn(".base.sha", self.text)
        self.assertIn("--default-branch", self.text)

    def test_pr_guards_calls_the_gate_on_every_pr_event_that_retargets_or_edits(self):
        # findings 3 and 4, moved by CHE-4385: `edited` (retarget) is a pr-guards trigger now
        guards = yaml.safe_load((ROOT / ".github/workflows/pr-guards.yml").read_text())
        self.assertIn("edited", guards[True]["pull_request_target"]["types"])
        self.assertEqual(guards["jobs"]["security-gate"]["uses"], "./.github/workflows/security-gate.yml")

    def test_dispatch_takes_one_digits_only_pull_request_number(self):
        self.assertEqual(list(self.doc[True]["workflow_dispatch"]["inputs"]), ["pr"])
        self.assertIn("github.event.inputs.pr", self.doc["jobs"]["security-gate"]["env"]["PR_NUMBER"])
        self.assertIn("pr must be digits only", self.text)

    def test_only_an_edited_or_deleted_verdict_comment_revokes_without_a_dispatch(self):
        # CHE-4385: `created` is the run-budget cost and is left to the poster's dispatch; edit/delete revoke a PASS.
        self.assertEqual(sorted(self.doc[True]["issue_comment"]["types"]), ["deleted", "edited"])
        cond = self.doc["jobs"]["security-gate"]["if"]
        self.assertIn("github.event_name != 'issue_comment'", cond)
        self.assertIn("Security-Gate-Verdict:", cond)
        self.assertIn("github.event.changes.body.from", cond)
        self.assertIn("OWNER", cond)
        self.assertIn("282619718", cond)
        self.assertIn("github.event.issue.number", self.doc["jobs"]["security-gate"]["env"]["PR_NUMBER"])

    def test_job_group_differs_from_the_caller_group(self):
        # A called workflow cannot share its caller's concurrency group (pr-guards-<n>); a dispatch and a push for the
        # same PR share this one, so the newest read of the verdict wins.
        job = self.doc["jobs"]["security-gate"]
        self.assertTrue(job["concurrency"]["group"].startswith("security-gate-"))
        self.assertIn("github.event.merge_group.head_sha", job["concurrency"]["group"])

    def test_merge_queue_commit_gets_the_status(self):
        self.assertIn("checks_requested", self.doc[True]["merge_group"]["types"])

    def test_only_status_write_is_added(self):
        self.assertEqual(self.doc["permissions"], {"contents": "read", "pull-requests": "read", "statuses": "write"})

if __name__ == "__main__":
    unittest.main()
