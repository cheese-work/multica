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

    def test_non_default_target_branch_fails(self):
        # finding 1: only the protected default branch is supported
        pr = {**PR, "base_ref": "evil", "default_branch": "main"}
        state, why = self.run_gate([comment(verdict(base="evil"))], pr=pr)
        self.assertEqual(state, "failure")
        self.assertIn("main", why)

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


class MergeGroupStepTest(unittest.TestCase):
    """The queue commit needs its own `security-gate` status, and the job runs the DEFAULT-branch script, which on the
    first queue run is still the old one (`--pr` only). The workflow step therefore runs that script unchanged."""

    QUEUE = "e" * 40
    REF = f"refs/heads/gh-readonly-queue/main/pr-405-{'d' * 40}"
    STUB_GH = ('#!/bin/bash\n'
               'case "$*" in\n'
               '  *commits/*) printf "%s\\n" $PARENTS ;;\n'
               '  *matching-refs/*) printf "%s\\n" $QUEUE_SHAS ;;\n'
               '  *) echo "gh $*" >> "$LOG" ;;\n'
               'esac\n')
    STUB_GATE = ('#!/bin/bash\n'  # the old script: --pr required, anything else is an argparse error
                 'echo "gate $*" >> "$LOG"\n'
                 'n=$(cat "$LOG.n" 2>/dev/null || echo 0); echo $((n + 1)) > "$LOG.n"\n'
                 '[ "$n" -ge 1 ] && STATE="${STATE2:-$STATE}"\n'
                 'case " $* " in *" --pr "*) ;; *) exit 2 ;; esac\n'
                 'echo \'{"context": "security-gate", "sha": "\'$HEAD\'", "state": "\'$STATE\'", "description": "PASS on x"}\'\n')

    def step_run(self, pr_event):
        steps = yaml.safe_load((ROOT / ".github/workflows/security-gate.yml").read_text())["jobs"]["security-gate"]["steps"]
        only = "!= 'merge_group'" if pr_event else "== 'merge_group'"
        applicable = [s for s in steps if "security_gate.py" in s.get("run", "") and only in s.get("if", only)]
        self.assertEqual(len(applicable), 1)
        return applicable[0]["run"]

    def run_step(self, ref=REF, parents=("d" * 40, HEAD), state="success", queue_shas=(), pr_event=False, state2=""):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "bin").mkdir()
            (tmp / "scripts/security-gate").mkdir(parents=True)
            (tmp / "bin/gh").write_text(self.STUB_GH)
            (tmp / "scripts/security-gate/gate.sh").write_text(self.STUB_GATE)
            (tmp / "scripts/security-gate/security_gate.py").write_text("")  # the step runs it via the shadowed python3
            (tmp / "bin/python3").write_text(
                '#!/bin/bash\n[ "${1##*/}" = security_gate.py ] && exec bash "$(dirname "$1")/gate.sh" "${@:2}"\n'
                f'exec {sys.executable} "$@"\n')
            for path in (tmp / "bin/gh", tmp / "bin/python3", tmp / "scripts/security-gate/gate.sh"):
                path.chmod(0o755)
            env = {**os.environ, "PATH": f"{tmp / 'bin'}:{os.environ['PATH']}", "LOG": str(tmp / "log"), "HEAD": HEAD,
                   "STATE": state, "STATE2": state2, "PARENTS": " ".join(parents), "QUEUE_SHAS": " ".join(queue_shas),
                   "MERGE_GROUP_REF": "" if pr_event else ref, "MERGE_GROUP_SHA": "" if pr_event else self.QUEUE,
                   "PR_NUMBER": "405", "GH_TOKEN": "t",
                   "DEFAULT_BRANCH": "main", "GITHUB_REPOSITORY": "o/r", "GITHUB_SERVER_URL": "https://github.com",
                   "GITHUB_RUN_ID": "7"}
            done = subprocess.run(["bash", "-c", self.step_run(pr_event)], cwd=tmp, env=env, capture_output=True, text=True)
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

    def test_pr_head_that_is_not_a_parent_fails(self):
        done, log = self.run_step(parents=("d" * 40, OLD))
        self.assertIn("state=failure", self.posted(log)[0])

    def test_pending_pr_verdict_stays_pending(self):
        done, log = self.run_step(state="pending")
        self.assertIn("state=pending", self.posted(log)[0])

    def test_queue_for_another_branch_fails_without_evaluating(self):
        done, log = self.run_step(ref=f"refs/heads/gh-readonly-queue/dev/pr-405-{'d' * 40}")
        self.assertIn("state=failure", self.posted(log)[0])
        self.assertFalse([l for l in log if l.startswith("gate ")])

    # A verdict revoked, edited or replaced by FAIL after the queue commit went green must not leave that commit green.
    def test_revoked_verdict_updates_the_open_queue_commit(self):
        done, log = self.run_step(pr_event=True, queue_shas=(self.QUEUE,), state="pending")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn("state=pending", self.posted(log)[0])
        self.assertIn("--post", " ".join(l for l in log if l.startswith("gate ")))  # the PR head still gets its status

    def test_pr_event_without_a_queue_entry_posts_nothing_extra(self):
        done, log = self.run_step(pr_event=True)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual([l for l in log if l.startswith("gh ")], [])

    def test_pr_event_does_not_update_a_queue_commit_that_lacks_the_head(self):
        done, log = self.run_step(pr_event=True, queue_shas=(self.QUEUE,), parents=("d" * 40, OLD))
        self.assertIn("state=failure", self.posted(log)[0])

    # Run A reads PASS, a revocation lands, run A attempts to post success: the queue commit must end non-success.
    def test_verdict_revoked_while_the_run_is_in_flight_is_not_posted_as_success(self):
        done, log = self.run_step(state="success", state2="pending")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertNotIn("state=success", " ".join(self.posted(log)))
        self.assertIn("state=pending", self.posted(log)[-1])


class ConcurrencyTest(unittest.TestCase):
    """One serial lane per PR: a revocation run queues behind a running evaluation instead of racing it."""

    def setUp(self):
        self.jobs = yaml.safe_load((ROOT / ".github/workflows/security-gate.yml").read_text())["jobs"]

    def resolve_pr(self, **env):
        run = self.jobs["resolve"]["steps"][0]["run"]
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "out"
            subprocess.run(["bash", "-c", run], env={**os.environ, "MERGE_GROUP_REF": "", "PR_NUMBER": "",
                                                     "GITHUB_OUTPUT": str(out), **env}, check=True)
            return out.read_text().strip()

    def test_merge_group_and_comment_runs_for_one_pr_share_a_group_that_never_cancels(self):
        conc = self.jobs["security-gate"]["concurrency"]
        self.assertEqual(conc["group"], "security-gate-${{ needs.resolve.outputs.pr }}")
        self.assertIs(conc["cancel-in-progress"], False)

    def test_queue_ref_and_pr_event_resolve_to_the_same_number(self):
        ref = f"refs/heads/gh-readonly-queue/main/pr-405-{'d' * 40}"
        self.assertEqual(self.resolve_pr(MERGE_GROUP_REF=ref), "pr=405")
        self.assertEqual(self.resolve_pr(PR_NUMBER="405"), "pr=405")


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

    def test_merge_queue_commit_gets_the_status(self):
        self.assertIn("checks_requested", self.doc[True]["merge_group"]["types"])

    def test_only_status_write_is_added(self):
        self.assertEqual(self.doc["permissions"], {"contents": "read", "pull-requests": "read", "statuses": "write"})


if __name__ == "__main__":
    unittest.main()
