"""CHE-4596 (CHE-4592 step 3, D8 = A): Opus signs security; Sol co-signs sensitive paths; Opus never signs its own PR."""

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
HUMAN = 129714106
SONNET_PR = {"head_sha": HEAD, "head_ref": "agent/x99-claude-sonnet-200k/che-4596-x", "author_id": BOT,
             "repo": "cheese-work/multica", "number": 7, "base_ref": "main", "default_branch": "main"}
OPUS_PR = {**SONNET_PR, "head_ref": "agent/x99-claude-opus/che-4596-x"}
LUNA_PR = {**SONNET_PR, "head_ref": "agent/x99-codex-luna/che-4596-x"}
SOL_PR = {**SONNET_PR, "head_ref": "agent/x99-codex-sol/che-4596-x"}
HUMAN_PR = {**SONNET_PR, "head_ref": "feature/x", "author_id": HUMAN}
OPUS = "x99-claude-opus (claude-opus-5-5)"
SOL = "x99-codex-sol (gpt-6.1-sol)"
LUNA = "x99-codex-luna (gpt-6-luna)"
GEMINI = "x99-agy-gemini (gemini-3)"
PLAIN = "app/feature.py"
SENSITIVE = "server/internal/auth/jwt.go"


def diff_of(*paths):
    return "".join(f"diff --git a/{p} b/{p}\n--- a/{p}\n+++ b/{p}\n@@ -1 +1,2 @@\n+x = 1\n" for p in paths)


def verdict(reviewer, result="PASS"):
    findings = "none" if result == "PASS" else "\n- app/feature.py:1 input reaches sink; exploit: curl"
    return "\n".join([f"Security-Gate-Verdict: {result}", f"Reviewer: {reviewer}", "Signed: r, 2026-11-08 12:00 ICT",
                      "Repo: cheese-work/multica", "PR: 7", "Base: main", f"SHA: {HEAD}", f"Findings: {findings}"])


def run(pr, path, *signed):
    """`signed` items are a reviewer string or a (reviewer, result) pair."""
    comments = []
    for i, s in enumerate(signed):
        who, result = (s, "PASS") if isinstance(s, str) else s
        comments.append({"id": i, "author_id": BOT, "body": verdict(who, result), "updated_at": f"2026-11-08T05:00:0{i}Z"})
    return sg.evaluate(pr, comments, {BOT}, diff_of(path), {HUMAN})[0]


class PlainPathTest(unittest.TestCase):
    def test_opus_pass_is_enough_on_a_plain_path(self):
        self.assertEqual(run(SONNET_PR, PLAIN, OPUS), "success")

    def test_opus_signs_a_same_lab_sonnet_pr(self):
        # D7 = A flips usage to Anthropic: the lab-diversity rule no longer applies to the security signer
        self.assertEqual(run(SONNET_PR, PLAIN, OPUS), "success")

    def test_opus_signs_openai_and_human_prs(self):
        self.assertEqual(run(LUNA_PR, PLAIN, OPUS), "success")
        self.assertEqual(run(HUMAN_PR, PLAIN, OPUS), "success")

    def test_sol_alone_is_not_enough_on_a_plain_path(self):
        self.assertEqual(run(SONNET_PR, PLAIN, SOL), "pending")

    def test_non_signers_never_count(self):
        for who in (LUNA, "x99-claude-sonnet-200k (claude-sonnet-5-5)", "x99-agy-geminis (gemini-3)", "x99-agy-other (gemini-3)"):
            with self.subTest(who=who):
                self.assertEqual(run(HUMAN_PR, PLAIN, who), "pending")

    def test_a_sol_fail_blocks_an_opus_pass(self):
        self.assertEqual(run(SONNET_PR, PLAIN, OPUS, (SOL, "FAIL")), "failure")


class SensitivePathTest(unittest.TestCase):
    def test_opus_alone_is_pending_on_a_sensitive_path(self):
        self.assertEqual(run(SONNET_PR, SENSITIVE, OPUS), "pending")

    def test_sol_alone_is_pending_on_a_sensitive_path(self):
        self.assertEqual(run(SONNET_PR, SENSITIVE, SOL), "pending")

    def test_opus_and_sol_pass_on_a_sensitive_path(self):
        self.assertEqual(run(SONNET_PR, SENSITIVE, OPUS, SOL), "success")

    def test_sensitive_path_needs_both_for_a_human_author_too(self):
        self.assertEqual(run(HUMAN_PR, SENSITIVE, OPUS), "pending")
        self.assertEqual(run(HUMAN_PR, SENSITIVE, OPUS, SOL), "success")

    def test_pending_text_names_the_missing_signer(self):
        comments = [{"id": 1, "author_id": BOT, "body": verdict(OPUS), "updated_at": "2026-11-08T05:00:00Z"}]
        _, why = sg.evaluate(SONNET_PR, comments, {BOT}, diff_of(SENSITIVE), {HUMAN})
        self.assertIn("Sol", why)
        self.assertFalse(why.endswith(")"), "request_verdicts reads a trailing (...) as a refusal reason")

    def test_deploy_migration_and_permission_paths_are_sensitive(self):
        for p in ("deploy/docker-compose.yml", "infra/deploy/run.sh", ".github/workflows/deploy-c00.yml",
                  ".github/workflows/release.yml", "server/migrations/001_init.up.sql", "db/migrate/20260101_x.rb",
                  "server/internal/permissions/check.go", "pkg/rbac/roles.go", "internal/acl/rules.go",
                  "server/internal/trust/boundary.go", "server/internal/handler/permission_check.go",
                  "app/trust_boundary.py", "pkg/trust-boundary.go", "app/trust.py"):
            with self.subTest(path=p):
                self.assertEqual(sg.two_signer_paths(diff_of(p)), [p])

    def test_lookalikes_stay_plain(self):
        for p in ("docs/deployment-notes.md", "app/redeploy_helper.py", "app/migrations_doc.md", "app/acl_docs.md",
                  "app/trustworthy.py", "app/trusted_authors.py", "app/feature.py"):
            with self.subTest(path=p):
                self.assertEqual(sg.two_signer_paths(diff_of(p)), [])


class SignerModelTest(unittest.TestCase):
    def test_opus_name_with_a_non_opus_model_does_not_sign(self):
        for bad in ("x99-claude-opus (claude-sonnet-5-5)", "x99-claude-opus (claude-haiku-5-5)"):
            with self.subTest(reviewer=bad):
                self.assertNotEqual(run(SONNET_PR, PLAIN, bad), "success")

    def test_opus_model_signs(self):
        self.assertEqual(run(SONNET_PR, PLAIN, "mbp-claude-opus (claude-opus-4-1)"), "success")


class OpusAuthorTest(unittest.TestCase):
    def test_sol_pass_alone_signs_an_opus_pr(self):
        self.assertEqual(run(OPUS_PR, PLAIN, SOL), "success")

    def test_sol_pass_alone_signs_an_opus_pr_on_a_sensitive_path(self):
        self.assertEqual(run(OPUS_PR, SENSITIVE, SOL), "success")

    def test_opus_never_signs_its_own_pr(self):
        self.assertEqual(run(OPUS_PR, PLAIN, OPUS), "failure")
        self.assertEqual(run(OPUS_PR, PLAIN, OPUS, ("mbp-claude-opus (claude-opus-5-5)", "PASS")), "failure")

    def test_opus_pass_does_not_help_an_opus_pr(self):
        self.assertEqual(run(OPUS_PR, SENSITIVE, OPUS), "failure")

    def test_no_verdict_on_an_opus_pr_is_pending(self):
        self.assertEqual(run(OPUS_PR, PLAIN), "pending")


class SolAuthorTest(unittest.TestCase):
    def test_sol_never_signs_its_own_pr(self):
        self.assertEqual(run(SOL_PR, PLAIN, SOL), "failure")

    def test_opus_signs_a_sol_pr_even_on_a_sensitive_path(self):
        # Sol cannot co-sign its own PR; Opus is the only signer left
        self.assertEqual(run(SOL_PR, SENSITIVE, OPUS), "success")


class GeminiSolRoleTest(unittest.TestCase):
    """CHE-5097: Cheese moved Sol's review work to Gemini, so a Gemini signer holds the `sol` role."""

    def test_gemini_has_the_sol_role_by_whole_name_token(self):
        for name in ("x99-agy-gemini", "x99-agy-gemini-ux", "x99-agy-gemini-200k", "x99-codex-sol"):
            with self.subTest(name=name):
                self.assertEqual(sg.signer_role(name), "sol")
        for name in ("x99-agy-geminis", "x99-agy-gemini2", "x99-agy-other", "x99-codex-luna", None):
            with self.subTest(name=name):
                self.assertIsNone(sg.signer_role(name))

    def test_gemini_pass_is_the_sol_signature_on_a_sensitive_path(self):
        self.assertEqual(run(SONNET_PR, SENSITIVE, OPUS, GEMINI), "success")
        self.assertEqual(run(SONNET_PR, SENSITIVE, GEMINI), "pending")

    def test_gemini_alone_is_not_enough_on_a_plain_path(self):
        self.assertEqual(run(SONNET_PR, PLAIN, GEMINI), "pending")

    def test_gemini_signs_an_opus_pr(self):
        self.assertEqual(run(OPUS_PR, PLAIN, GEMINI), "success")

    def test_gemini_never_signs_its_own_pr(self):
        gemini_pr = {**SONNET_PR, "head_ref": "agent/x99-agy-gemini/che-5097-x"}
        self.assertEqual(run(gemini_pr, PLAIN, GEMINI), "failure")
        self.assertEqual(run(gemini_pr, SENSITIVE, GEMINI), "failure")
        self.assertEqual(run(gemini_pr, SENSITIVE, GEMINI, OPUS), "success")  # Opus alone, as for a Sol PR
        self.assertEqual(run(gemini_pr, SENSITIVE, OPUS), "success")

    def test_a_gemini_fail_blocks(self):
        self.assertEqual(run(SONNET_PR, PLAIN, OPUS, (GEMINI, "FAIL")), "failure")


class IndependentFailTest(unittest.TestCase):
    """A trusted reviewer from another lab blocks with a FAIL even when its role never signs a PASS (RGS-1 base behaviour)."""

    def test_non_signer_fail_from_another_lab_blocks_an_opus_pass(self):
        self.assertEqual(run(SONNET_PR, PLAIN, OPUS, (LUNA, "FAIL")), "failure")
        self.assertEqual(run(SONNET_PR, PLAIN, (LUNA, "FAIL"), OPUS), "failure")
        self.assertEqual(run(SONNET_PR, PLAIN, OPUS, (GEMINI, "FAIL")), "failure")

    def test_non_signer_fail_blocks_with_no_pass_yet(self):
        self.assertEqual(run(SONNET_PR, PLAIN, (LUNA, "FAIL")), "failure")

    def test_non_signer_fail_blocks_a_sensitive_pr(self):
        self.assertEqual(run(SONNET_PR, SENSITIVE, OPUS, SOL, (LUNA, "FAIL")), "failure")

    def test_same_lab_non_signer_fail_does_not_block(self):
        sonnet2 = "x99-claude-sonnet-200k (claude-sonnet-5-5)"
        self.assertEqual(run(SONNET_PR, PLAIN, OPUS, (sonnet2, "FAIL")), "success")

    def test_the_author_own_fail_does_not_block(self):
        self.assertEqual(run(LUNA_PR, PLAIN, OPUS, (LUNA, "FAIL")), "success")

    def test_non_signer_pass_still_never_counts(self):
        self.assertEqual(run(SONNET_PR, PLAIN, OPUS, LUNA), "success")
        self.assertEqual(run(SONNET_PR, PLAIN, LUNA), "pending")


class Issue1098Test(unittest.TestCase):
    def test_sonnet_author_opus_pass_on_a_plain_path_succeeds(self):
        # cheese-work/multica#1098 (CHE-4229): was `failure` under RGS-1 lab diversity
        pr = {**SONNET_PR, "number": 1098, "head_ref": "agent/x99-claude-sonnet-200k/che-4229-5e7836"}
        comments = [{"id": 1, "author_id": BOT, "body": verdict(OPUS).replace("PR: 7", "PR: 1098"),
                     "updated_at": "2026-11-08T19:07:00Z"}]
        self.assertEqual(sg.evaluate(pr, comments, {BOT}, diff_of(PLAIN), {HUMAN})[0], "success")

    def test_diff_digest_carry_over_still_counts_for_the_signer(self):
        d = "d" * 64
        body = verdict(OPUS).replace(f"SHA: {HEAD}", f"SHA: {'c' * 40}\nDiff-Digest: {d}")
        comments = [{"id": 1, "author_id": BOT, "body": body, "updated_at": "2026-11-08T05:00:00Z"}]
        self.assertEqual(sg.evaluate(SONNET_PR, comments, {BOT}, diff_of(PLAIN), {HUMAN}, digest=d)[0], "success")


if __name__ == "__main__":
    unittest.main()
