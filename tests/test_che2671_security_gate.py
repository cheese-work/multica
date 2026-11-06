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
            if sibling_head in cmd[-1] and sibling_diff_error:
                raise subprocess.CalledProcessError(128, cmd)  # fork head objects were never fetched
            return subprocess.CompletedProcess(cmd, 0, stdout=CLEAN_DIFF)

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


class RateLimitTest(unittest.TestCase):
    """CHE-4242: `gh api` answers 403 `API rate limit exceeded` when the installation is spent. Wait and retry
    up to a bounded budget, then raise RateLimited; main() maps that to exit 75 (workflow posts pending), never a failure."""

    LIMIT = "gh: API rate limit exceeded for installation. If you reach out to support (HTTP 403)"

    def fake_run(self, outcomes):
        calls = iter(outcomes)

        def run(cmd, **kw):
            code, out, err = next(calls)
            if code:
                raise subprocess.CalledProcessError(code, cmd, output=out, stderr=err)
            return subprocess.CompletedProcess(cmd, 0, stdout=out, stderr="")
        return run

    def setUp(self):
        sg._rate_limit_waited = 0

    def test_a_rate_limited_call_is_retried_and_returns_the_answer(self):
        run = self.fake_run([(1, "", self.LIMIT), (1, "", self.LIMIT), (0, "ok", "")])
        with mock.patch.object(sg.subprocess, "run", run), mock.patch.object(sg.time, "sleep") as sleep:
            self.assertEqual(sg._gh("repos/o/r/pulls/1"), "ok")
        self.assertEqual(sleep.call_count, 2)

    def test_429_is_a_rate_limit_too(self):
        run = self.fake_run([(1, "", "gh: Too Many Requests (HTTP 429)"), (0, "ok", "")])
        with mock.patch.object(sg.subprocess, "run", run), mock.patch.object(sg.time, "sleep"):
            self.assertEqual(sg._gh("x"), "ok")

    def test_a_call_still_rate_limited_after_the_budget_raises_rate_limited(self):
        run = self.fake_run([(1, "", self.LIMIT)] * 100)
        with mock.patch.object(sg.subprocess, "run", run), mock.patch.object(sg.time, "sleep") as sleep:
            with self.assertRaises(sg.RateLimited):
                sg._gh("x")
        self.assertLessEqual(sum(c.args[0] for c in sleep.call_args_list), sg.RATE_LIMIT_MAX_WAIT)

    def test_the_wait_budget_is_shared_across_calls(self):
        run = self.fake_run([(1, "", self.LIMIT)] * 100)
        with mock.patch.object(sg.subprocess, "run", run), mock.patch.object(sg.time, "sleep") as sleep:
            for _ in range(3):
                with self.assertRaises(sg.RateLimited):
                    sg._gh("x")
        self.assertLessEqual(sum(c.args[0] for c in sleep.call_args_list), sg.RATE_LIMIT_MAX_WAIT)

    def test_any_other_error_is_raised_at_once_without_waiting(self):
        run = self.fake_run([(1, "", "gh: Not Found (HTTP 404)")])
        with mock.patch.object(sg.subprocess, "run", run), mock.patch.object(sg.time, "sleep") as sleep:
            with self.assertRaises(subprocess.CalledProcessError) as ctx:
                sg._gh("x")
        self.assertNotIsInstance(ctx.exception, sg.RateLimited)
        sleep.assert_not_called()

    def test_main_exits_75_and_posts_no_failure_when_the_read_stays_rate_limited(self):
        posted = []
        argv = ["x", "--repo", str(ROOT), "--github", "o/r", "--pr", "405", "--default-branch", "main", "--post"]
        with mock.patch.object(sg, "gate_pr", side_effect=sg.RateLimited(1, ["gh"])), \
                mock.patch.object(sg, "_post", side_effect=lambda *a: posted.append(a)), \
                mock.patch("sys.argv", argv), mock.patch("builtins.print"):
            self.assertEqual(sg.main(), sg.RATE_LIMIT_EXIT)
        self.assertEqual(sg.RATE_LIMIT_EXIT, 75)
        self.assertEqual(posted, [])

    def run_main_with_rate_limited_sibling(self, primary, post_error=None, revoke_error=None):
        def gate(repo, github, n, *rest):
            if n == 406:
                raise sg.RateLimited(1, ["gh"])
            return HEAD, primary, "primary verdict"
        rows = json.dumps({"number": 406, "head": HEAD})
        argv = ["x", "--repo", str(ROOT), "--github", "o/r", "--pr", "405", "--default-branch", "main", "--post"]
        with mock.patch.object(sg, "gate_pr", side_effect=gate), mock.patch.object(sg, "_gh", return_value=rows), \
                mock.patch.object(sg, "_post", side_effect=post_error) as post, \
                mock.patch.object(sg, "revoke_queue", side_effect=revoke_error) as revoke, \
                mock.patch("sys.argv", argv), mock.patch("builtins.print"):
            return sg.main(), post, revoke

    def test_a_rate_limited_sibling_does_not_hide_a_known_failure(self):
        # CHE-4242 review P1: the primary FAIL must still be published and the queued successes revoked
        rc, post, revoke = self.run_main_with_rate_limited_sibling("failure")
        self.assertEqual(rc, 0)
        self.assertEqual(post.call_args.args[2], "failure")
        revoke.assert_called_once()

    def test_a_rate_limited_sibling_of_a_pass_is_pending_never_success_or_failure(self):
        rc, post, revoke = self.run_main_with_rate_limited_sibling("success")
        self.assertEqual(rc, 0)
        self.assertEqual(post.call_args.args[2], "pending")
        self.assertIn("rate limited, retry", post.call_args.args[3])
        revoke.assert_called_once()  # a status that is not success must not ride a carried success

    def test_a_status_write_that_stays_rate_limited_still_attempts_the_revocation(self):
        rc, post, revoke = self.run_main_with_rate_limited_sibling("failure", post_error=sg.RateLimited(1, ["gh"]))
        self.assertEqual(rc, sg.RATE_LIMIT_EXIT)
        revoke.assert_called_once()

    def run_main_with_rate_limited_discovery(self, primary):
        argv = ["x", "--repo", str(ROOT), "--github", "o/r", "--pr", "405", "--default-branch", "main", "--post"]
        with mock.patch.object(sg, "gate_pr", return_value=(HEAD, primary, "primary verdict")), \
                mock.patch.object(sg, "_gh", side_effect=sg.RateLimited(1, ["gh"])), \
                mock.patch.object(sg, "_post") as post, mock.patch.object(sg, "revoke_queue") as revoke, \
                mock.patch("sys.argv", argv), mock.patch("builtins.print"):
            return sg.main(), post, revoke

    def test_a_rate_limited_pr_discovery_does_not_hide_a_known_failure(self):
        # CHE-4242 security review: the known FAIL must still be published and the queued successes revoked
        rc, post, revoke = self.run_main_with_rate_limited_discovery("failure")
        self.assertEqual(rc, 0)
        self.assertEqual(post.call_args.args[2], "failure")
        revoke.assert_called_once()

    def test_a_rate_limited_pr_discovery_of_a_pass_is_pending_never_success(self):
        rc, post, revoke = self.run_main_with_rate_limited_discovery("success")
        self.assertEqual(rc, 0)
        self.assertEqual(post.call_args.args[2], "pending")
        self.assertIn("rate limited, retry", post.call_args.args[3])
        revoke.assert_called_once()

    def test_writes_keep_a_wait_reserve_after_the_reads_spent_the_budget(self):
        sg._rate_limit_waited = sg.RATE_LIMIT_MAX_WAIT
        run = self.fake_run([(1, "", self.LIMIT), (0, "ok", "")])
        with mock.patch.object(sg.subprocess, "run", run), mock.patch.object(sg.time, "sleep"):
            sg._reserve_write_budget()
            self.assertEqual(sg._gh("x"), "ok")


class MergeGroupStepTest(unittest.TestCase):
    """The merge queue requires `security-gate` on its own commit, which no PR event ever reaches. The job runs the
    DEFAULT-branch script, which on the first queue run is still the old one (`--pr` only), so the workflow step
    re-evaluates the queued PR with that script unchanged and posts the state on the queue commit itself."""

    QUEUE = "e" * 40
    REF = f"refs/heads/gh-readonly-queue/main/pr-405-{'d' * 40}"
    RATE_LIMITED = 'echo "gh: HTTP 403: API rate limit exceeded for installation" >&2; exit 1'
    STUB_GH = ('#!/bin/bash\n'  # RL_PARENTS / RL_POST: the first N parent lookups / status writes answer with a rate limit
               'touch "$LOG"\n'
               'case "$*" in\n'  # PRIOR: the security-gate state already on the queue commit; RL_PRIOR=1 rate limits that read
               '  *commits/*/status*) [ -n "${RL_PRIOR:-}" ] && { ' + RATE_LIMITED + '; }; echo "${PRIOR:-none}" ;;\n'
               '  *commits/*) [ "${RL_PARENTS:-0}" -gt "$(grep -c "^rl-parents" "$LOG" || true)" ] && { echo rl-parents >> "$LOG"; ' + RATE_LIMITED + '; }\n'
               '              printf "%s\\n" $PARENTS ;;\n'
               '  *statuses/*) [ -n "${RL_POST_AFTER:-}" ] && [ "$(grep -c "^gh .*statuses/" "$LOG" || true)" -ge "$RL_POST_AFTER" ] && { echo rl-post >> "$LOG"; ' + RATE_LIMITED + '; }\n'
               '               [ "${RL_POST:-0}" -gt "$(grep -c "^rl-post" "$LOG" || true)" ] && { echo rl-post >> "$LOG"; ' + RATE_LIMITED + '; }\n'
               '               echo "gh $*" >> "$LOG" ;;\n'
               '  *) echo "gh $*" >> "$LOG" ;;\n'
               'esac\n')
    STUB_GATE = ('#!/bin/bash\n'  # the old script: --pr N required (an empty N is an argparse error), verdict read live
                 'echo "gate $*" >> "$LOG"\n'
                 'case " $* " in *" --pr "[0-9]*) ;; *) exit 2 ;; esac\n'
                 'n=$(grep -c "^gate " "$LOG")\n'  # this is the Nth read: STATES answers per read, FAIL_AT makes one read error out
                 '[ "$n" = "${FAIL_AT:-}" ] && exit 1\n'
                 '[ "$n" = "${RL_AT:-}" ] && { echo "rate limited" >&2; exit 75; }\n'
                 'if [ -n "${STATES:-}" ]; then STATE=$(echo $STATES | cut -d" " -f$n); fi\n'
                 'echo \'{"context": "security-gate", "sha": "\'$HEAD\'", "state": "\'$STATE\'", "description": "PASS on x"}\'\n')

    def step_run(self):
        steps = yaml.safe_load((ROOT / ".github/workflows/security-gate.yml").read_text())["jobs"]["security-gate"]["steps"]
        gate = [s for s in steps if "security_gate.py" in s.get("run", "")]
        self.assertEqual(len(gate), 1)
        return gate[0]["run"]

    def run_step(self, ref=REF, parents=("d" * 40, HEAD), state="success", script="", states="", fail_at="", rl_at="", rl_parents=0, rl_post=0, rl_post_after="", prior="none", rl_prior="", attempt="1"):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "bin").mkdir()
            (tmp / "scripts/security-gate").mkdir(parents=True)
            (tmp / "bin/gh").write_text(self.STUB_GH)
            (tmp / "bin/sleep").write_text('#!/bin/bash\necho "sleep $*" >> "$LOG"\n')  # no real waiting in tests
            (tmp / "scripts/security-gate/gate.sh").write_text(self.STUB_GATE)
            (tmp / "scripts/security-gate/security_gate.py").write_text(script)  # the step runs it via the shadowed python3
            (tmp / "bin/python3").write_text(
                '#!/bin/bash\n[ "${1##*/}" = security_gate.py ] && exec bash "$(dirname "$1")/gate.sh" "${@:2}"\n'
                f'exec {sys.executable} "$@"\n')
            for path in (tmp / "bin/gh", tmp / "bin/sleep", tmp / "bin/python3", tmp / "scripts/security-gate/gate.sh"):
                path.chmod(0o755)
            env = {**os.environ, "PATH": f"{tmp / 'bin'}:{os.environ['PATH']}", "LOG": str(tmp / "log"), "HEAD": HEAD,
                   "STATE": state, "STATES": states, "FAIL_AT": str(fail_at), "RL_AT": str(rl_at),
                   "RL_PARENTS": str(rl_parents), "RL_POST": str(rl_post), "RL_POST_AFTER": str(rl_post_after), "PRIOR": prior, "RL_PRIOR": rl_prior, "GITHUB_RUN_ATTEMPT": str(attempt), "PARENTS": " ".join(parents), "MERGE_GROUP_REF": ref, "MERGE_GROUP_SHA": self.QUEUE,
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

    def test_a_rate_limited_read_posts_pending_never_failure(self):
        # CHE-4242: a 403 rate limit on the installation turned a green PR red at the last queue step
        for rl_at in (1, 2, 3):
            done, log = self.run_step(rl_at=rl_at, states="success success success")
            self.assertEqual(done.returncode, 0, (rl_at, done.stderr))
            self.assertNotIn("state=failure", " ".join(self.posted(log)), (rl_at, log))
            self.assertIn("state=pending", self.posted(log)[-1], (rl_at, log))
            self.assertIn("rate limited, retry", self.posted(log)[-1], (rl_at, log))

    def test_a_rate_limited_parent_lookup_is_retried_until_it_answers(self):
        done, log = self.run_step(rl_parents=2)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(len([l for l in log if l.startswith("sleep")]), 2, log)
        self.assertIn("state=success", self.posted(log)[0], log)

    def test_a_parent_lookup_still_rate_limited_after_the_retries_posts_pending(self):
        done, log = self.run_step(rl_parents=999)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertNotIn("state=failure", " ".join(self.posted(log)), log)
        self.assertIn("state=pending", self.posted(log)[-1], log)

    def test_a_rate_limited_status_write_is_retried_until_it_lands(self):
        done, log = self.run_step(rl_post=2)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn("state=success", self.posted(log)[0], log)

    def test_a_status_write_that_stays_rate_limited_is_not_a_failed_job(self):
        # CHE-4242 review P1: no status is written (the queue entry stays pending), the job is not red
        done, log = self.run_step(rl_post=999, states="success success success")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.posted(log), [], log)
        self.assertIn("::warning::", done.stderr)

    def test_a_rerun_that_cannot_replace_an_earlier_success_keeps_the_job_red(self):
        # CHE-4242 review round 2 P1: a success from an earlier run stays on the queue commit when the write is throttled
        done, log = self.run_step(rl_post=999, states="failure failure failure", prior="success")
        self.assertNotEqual(done.returncode, 0, (done.stderr, log))
        self.assertEqual(self.posted(log), [], log)

    def test_a_rerun_whose_earlier_status_cannot_be_read_keeps_the_job_red(self):
        # unknown earlier status on a re-run fails closed: attempt 1 left a success only if it got that far
        done, log = self.run_step(rl_post=999, rl_prior="1", states="failure failure failure", attempt=2)
        self.assertNotEqual(done.returncode, 0, (done.stderr, log))

    def test_a_first_attempt_that_cannot_write_or_read_the_status_is_not_a_failed_job(self):
        # CHE-4242 review round 3 P1: shared quota exhaustion fails the write and the read together; on the first
        # attempt no earlier run can have left a success on the queue commit, so the job gets a warning, not a red mark
        done, log = self.run_step(rl_post=999, rl_prior="1", state="pending", states="pending pending pending")
        self.assertEqual(done.returncode, 0, (done.stderr, log))
        self.assertEqual(self.posted(log), [], log)
        self.assertIn("::warning::", done.stderr)

    def test_a_rerun_over_an_earlier_non_success_stays_green_when_the_write_is_throttled(self):
        for prior in ("pending", "failure", "none"):
            done, log = self.run_step(rl_post=999, states="failure failure failure", prior=prior)
            self.assertEqual(done.returncode, 0, (prior, done.stderr, log))
            self.assertIn("::warning::", done.stderr)

    def test_a_failure_that_cannot_replace_a_published_success_keeps_the_job_red(self):
        # a success is already on the queue commit: a job that stays green would hide a stale approval
        done, log = self.run_step(rl_post_after=1, states="success success failure")
        self.assertIn("state=success", self.posted(log)[0], log)
        self.assertNotEqual(done.returncode, 0, (done.stderr, log))

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
        fake_run = lambda cmd, **kw: subprocess.CompletedProcess(cmd, 0, stdout=CLEAN_DIFF)
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
        self.assertIn("pull_request_target", self.doc[True])
        self.assertIn("issue_comment", self.doc[True])
        self.assertIn("ref: ${{ github.event.repository.default_branch }}", self.text)
        self.assertNotIn("pull_request.head.ref", self.text)

    def test_privileged_job_runs_the_default_branch_not_the_pr_base(self):
        # finding 1: base.sha of a PR into an unprotected branch is attacker-influenced code
        self.assertIn("github.event.repository.default_branch", self.text)
        self.assertNotIn(".base.sha", self.text)
        self.assertIn("--default-branch", self.text)

    def test_retarget_and_comment_deletion_retrigger_the_gate(self):
        # findings 3 and 4
        self.assertIn("edited", self.doc[True]["pull_request_target"]["types"])
        self.assertIn("deleted", self.doc[True]["issue_comment"]["types"])

    # Mirrors the X99 runner hook /home/congvc/cheese-work-runners/hooks/job-started.sh: a comment runs only for
    # OWNER, MEMBER or COLLABORATOR, or for the trusted bot's `Security-Gate-Verdict:` comment (which is NONE).
    ALLOWED = ("OWNER", "MEMBER", "COLLABORATOR", "282619718", "'Bot'", "'Security-Gate-Verdict:'")
    HOOK = Path("/home/congvc/cheese-work-runners/hooks/job-started.sh")

    def test_refused_comment_authors_schedule_no_job(self):
        # CHE-3035: skip at the job `if:` what the runner hook refuses, so no job is scheduled and no red check appears
        cond = self.doc["jobs"]["security-gate"]["if"]
        self.assertIn("github.event_name == 'pull_request_target'", cond)  # PR events are never filtered by author
        for part in self.ALLOWED:
            self.assertIn(part, cond)

    def test_trusted_bot_verdict_comment_still_runs(self):
        # The verdict bot's association is NONE (observed on PR #184). Filtering NONE alone would drop every
        # verdict create/edit/delete and leave a revoked PASS in place.
        cond = self.doc["jobs"]["security-gate"]["if"]
        self.assertNotRegex(cond, r"author_association\s*!=\s*'NONE'")
        self.assertIn("startsWith(github.event.comment.body, 'Security-Gate-Verdict:')", cond)

    VERDICT = "'Security-Gate-Verdict:'"

    def test_plain_comment_schedules_no_job(self):
        # CHE-3354: a comment without the verdict header queues nothing on the single gate runner, even from a trusted author
        cond = self.doc["jobs"]["security-gate"]["if"]
        self.assertIn(f"contains(github.event.comment.body, {self.VERDICT})", cond)

    def test_removing_the_header_by_edit_still_runs_the_gate(self):
        # CHE-3354: revocation by edit. On an edit the new body lacks the header; the old body is `changes.body.from`.
        cond = self.doc["jobs"]["security-gate"]["if"]
        self.assertIn(f"github.event.action == 'edited' && contains(github.event.changes.body.from, {self.VERDICT})", cond)

    def test_plain_comment_gets_its_own_concurrency_group(self):
        # CHE-3354: keep the ignored-group test in step with the `if:` so a plain comment cannot cancel a real run
        group = self.doc["concurrency"]["group"]
        self.assertIn(f"contains(github.event.comment.body, {self.VERDICT})", group)
        self.assertIn("github.event.changes.body.from", group)

    def test_trusted_bot_edit_that_drops_the_header_still_runs(self):
        # CHE-3354 review P1: the bot revokes a PASS by editing it. The new body may lack the header; the old body had it.
        old = f"startsWith(github.event.changes.body.from, {self.VERDICT})"
        self.assertIn(old, self.doc["jobs"]["security-gate"]["if"])
        self.assertIn(old, self.doc["concurrency"]["group"])

    def _hook_rc(self, **ev):
        event = {"action": ev.get("action", "created"), "comment": {"author_association": "NONE", "body": ev["body"],
                 "user": {"id": ev.get("uid", BOT), "type": "Bot"}}}
        if "old" in ev:
            event["changes"] = {"body": {"from": ev["old"]}}
        with tempfile.TemporaryDirectory() as d:
            f = Path(d) / "event.json"
            f.write_text(json.dumps(event))
            env = {"PATH": os.environ["PATH"], "GITHUB_EVENT_PATH": str(f), "GITHUB_EVENT_NAME": "issue_comment",
                   "GITHUB_REPOSITORY": "cheese-work/multica"}
            return subprocess.run(["sh", str(self.HOOK)], env=env, capture_output=True).returncode

    def test_hook_admits_the_bot_edit_that_drops_the_header_and_nothing_wider(self):
        if not self.HOOK.exists():
            self.skipTest("runner hook not on this host")
        v = "Security-Gate-Verdict: PASS"
        self.assertEqual(self._hook_rc(body=v), 0)
        self.assertEqual(self._hook_rc(body="FAIL now", action="edited", old=v), 0)
        self.assertNotEqual(self._hook_rc(body="hello"), 0)
        self.assertNotEqual(self._hook_rc(body="hello", action="edited", old="hello"), 0)
        self.assertNotEqual(self._hook_rc(body="x", action="edited", old=v, uid=1), 0)
        self.assertNotEqual(self._hook_rc(body="x", action="created", old=v), 0)

    def test_job_filter_matches_the_runner_hook_allow_list(self):
        if not self.HOOK.exists():
            self.skipTest("runner hook not on this host")
        hook = self.HOOK.read_text()
        cond = self.doc["jobs"]["security-gate"]["if"]
        for part in ("OWNER", "MEMBER", "COLLABORATOR", "282619718", "Security-Gate-Verdict:"):
            self.assertIn(part, hook)
            self.assertIn(part, cond)

    def test_skipped_comment_cannot_cancel_a_real_evaluation(self):
        # CHE-3035: a run cancels the in-flight run of its concurrency group before its `if:` is read. An ignored
        # comment must land in its own group, or it cancels the real run and leaves no completed run behind it.
        group = self.doc["concurrency"]["group"]
        self.assertIn("github.run_id", group)
        for part in self.ALLOWED:
            self.assertIn(part, group)

    def test_merge_queue_commit_gets_the_status(self):
        self.assertIn("checks_requested", self.doc[True]["merge_group"]["types"])

    def test_comment_filter_and_group_keep_the_merge_queue_path(self):
        # CHE-3035 x CHE-3317: the author filter and the ignored-comment group must not drop or collide merge_group runs
        self.assertIn("github.event_name == 'merge_group'", self.doc["jobs"]["security-gate"]["if"])
        self.assertIn("github.event.merge_group.head_sha", self.doc["concurrency"]["group"])

    def test_only_status_write_is_added(self):
        self.assertEqual(self.doc["permissions"], {"contents": "read", "pull-requests": "read", "statuses": "write"})


if __name__ == "__main__":
    unittest.main()
