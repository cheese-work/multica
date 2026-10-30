#!/usr/bin/env python3
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
SOURCE_SHA = "c689a72ae" + "1" * 31
NEW_HEAD = "93ac2140b" + "2" * 31
LIVE_SHA = "59819ba" + "0" * 33
SUCCESS = {"backend": "success", "frontend": "success", "cd-qualification": "success"}


class CandidatePreflightTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load((ROOT / ".github/workflows/cd-deploy.yml").read_text())
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        (self.directory / "deploy").symlink_to(ROOT / "deploy", target_is_directory=True)
        (self.directory / ".github").symlink_to(ROOT / ".github", target_is_directory=True)
        self.bin = self.directory / "bin"
        self.bin.mkdir()
        stub = self.bin / "gh"
        stub.write_text('''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

state = Path(os.environ["STUB_STATE"])
endpoint = sys.argv[2]
api_error = os.environ.get("STUB_API_ERROR")
if api_error and (api_error == "1" or api_error in endpoint):
    sys.exit("simulated GitHub API failure")
if endpoint.endswith("git/ref/heads/main"):
    counter = state / "head-reads"
    index = int(counter.read_text()) if counter.exists() else 0
    counter.write_text(str(index + 1))
    heads = json.loads(os.environ["STUB_HEADS"])
    print(heads[min(index, len(heads) - 1)])
elif "/compare/" in endpoint:
    if f"/compare/{os.environ['LIVE_SHA']}..." in endpoint:
        print(os.environ["STUB_LIVE_STATUS"])
    else:
        print(os.environ["STUB_MAIN_STATUS"])
elif "/check-runs?" in endpoint:
    counter = state / "check-reads"
    index = int(counter.read_text()) if counter.exists() else 0
    counter.write_text(str(index + 1))
    polls = json.loads(os.environ["STUB_CHECKS"])
    poll = polls[min(index, len(polls) - 1)]
    runs = poll if isinstance(poll, list) else [{"name": n, "conclusion": c, "id": 1} for n, c in poll.items()]
    for run in runs:
        print(json.dumps(run))
elif endpoint.endswith("/commits/" + os.environ["LIVE_SHA"][:7]):
    if os.environ.get("STUB_AMBIGUOUS_SHORT"):
        sys.exit("gh: No commit found for SHA: " + os.environ["LIVE_SHA"][:7] + " (HTTP 422)")
    print(os.environ["LIVE_SHA"])
elif "/commits/" in endpoint:
    print(json.dumps({"files": json.loads(os.environ["STUB_FILES"]), "truncated": False}))
else:
    sys.exit("unexpected API request: " + endpoint)
''')
        stub.chmod(0o755)
        curl = self.bin / "curl"
        curl.write_text('''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

if os.environ.get("STUB_LIVE_ERROR"):
    sys.exit("simulated C00 version read failure")
assert sys.argv[-1] == os.environ["C00_CONFIG_URL"]
assert "--max-time" in sys.argv
assert "--fail" in sys.argv
counter = Path(os.environ["STUB_STATE"]) / "live-reads"
index = int(counter.read_text()) if counter.exists() else 0
counter.write_text(str(index + 1))
versions = json.loads(os.environ["STUB_LIVE_VERSIONS"])
print(json.dumps({"server_commit": versions[min(index, len(versions) - 1)]}))
''')
        curl.chmod(0o755)

    def approval_state(self, heads, checks=None, files=None, wait_seconds="60",
                       live_status="ahead", main_status="ahead", live_versions=None,
                       live_sha=LIVE_SHA, ambiguous_short=False):
        protected = self.workflow["jobs"]["approve-c00"]
        self.assertEqual(protected["environment"], "c00-production")
        dependencies = protected.get("needs", [])
        if isinstance(dependencies, str):
            dependencies = [dependencies]
        if "candidate-preflight" not in dependencies:
            return "waiting", "No eligibility job runs before c00-production approval."

        preflight = self.workflow["jobs"]["candidate-preflight"]
        self.assertNotIn("environment", preflight)
        self.assertIn("needs.candidate-preflight.outputs.eligible == 'true'", protected["if"])
        self.assertIn("needs.candidate-preflight.outputs.eligible == 'true'", self.workflow["jobs"]["prepare-release-candidate"]["if"])
        selection = next(step for step in preflight["steps"] if step.get("id") == "candidate")
        output = self.directory / "output"
        summary = self.directory / "summary"
        environment = {
            **os.environ,
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "GITHUB_REPOSITORY": "cheese-work/multica",
            "GITHUB_OUTPUT": str(output),
            "GITHUB_STEP_SUMMARY": str(summary),
            "SOURCE_SHA": SOURCE_SHA,
            "LIVE_SHA": live_sha,
            "STUB_AMBIGUOUS_SHORT": "1" if ambiguous_short else "",
            "C00_CONFIG_URL": "https://c00.example.test/api/config",
            "STUB_STATE": str(self.directory),
            "STUB_HEADS": json.dumps(heads),
            "STUB_CHECKS": json.dumps(checks if checks is not None else [SUCCESS]),
            "STUB_LIVE_STATUS": live_status,
            "STUB_MAIN_STATUS": main_status,
            "STUB_LIVE_VERSIONS": json.dumps(live_versions or [live_sha[:7]]),
            "STUB_FILES": json.dumps(files if files is not None else ["server/only.go"]),
            "CHECKS_WAIT_SECONDS": wait_seconds,
            "CHECKS_POLL_SECONDS": "0",
        }
        result = subprocess.run(
            ["bash", "-c", selection["run"]], cwd=self.directory, env=environment,
            text=True, capture_output=True,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        outputs = dict(line.split("=", 1) for line in output.read_text().splitlines())
        self.assertIn(SOURCE_SHA, summary.read_text())
        return ("waiting" if outputs["eligible"] == "true" else "skipped"), summary.read_text()

    def test_failed_candidate_skips_before_production_approval(self):
        state, reason = self.approval_state([NEW_HEAD], [{**SUCCESS, "backend": "cancelled"}])
        self.assertEqual(state, "skipped", reason)
        self.assertTrue((self.directory / "check-reads").exists())

    def test_older_but_forward_candidate_reaches_approval(self):
        state, reason = self.approval_state([NEW_HEAD])
        self.assertEqual(state, "waiting", reason)

    def test_downgrade_same_version_and_divergent_candidates_skip(self):
        for status in ["behind", "identical", "diverged"]:
            with self.subTest(status=status):
                state, reason = self.approval_state([SOURCE_SHA], live_status=status)
                self.assertEqual(state, "skipped", reason)
                self.assertIn(LIVE_SHA, reason)
        self.assertFalse((self.directory / "check-reads").exists())

    def test_non_ancestor_of_main_skips(self):
        for status in ["behind", "diverged"]:
            with self.subTest(status=status):
                state, reason = self.approval_state([NEW_HEAD], main_status=status)
                self.assertEqual(state, "skipped", reason)
        self.assertFalse((self.directory / "check-reads").exists())

    def test_ambiguous_short_live_version_resolves_through_local_git(self):
        git = ["git", "-c", "user.name=t", "-c", "user.email=t@example.test"]
        subprocess.run(git + ["init", "--quiet"], cwd=self.directory, check=True)
        subprocess.run(git + ["commit", "--quiet", "--allow-empty", "-m", "live"], cwd=self.directory, check=True)
        live_sha = subprocess.run(["git", "rev-parse", "HEAD"], cwd=self.directory, check=True,
                                  text=True, capture_output=True).stdout.strip()
        state, reason = self.approval_state([SOURCE_SHA], live_sha=live_sha, ambiguous_short=True)
        self.assertEqual(state, "waiting", reason)

    def test_live_version_is_rechecked_after_required_checks(self):
        state, reason = self.approval_state([SOURCE_SHA], live_versions=[LIVE_SHA[:7], SOURCE_SHA])
        self.assertEqual(state, "skipped", reason)
        self.assertEqual((self.directory / "live-reads").read_text(), "2")

    def test_new_main_head_does_not_replace_a_forward_candidate_while_checks_settle(self):
        state, reason = self.approval_state(
            [SOURCE_SHA, NEW_HEAD], [{**SUCCESS, "backend": None}, SUCCESS],
        )
        self.assertEqual(state, "waiting", reason)
        self.assertEqual((self.directory / "check-reads").read_text(), "2")

    def test_unknown_live_version_fails_closed(self):
        for version in [None, "", "unknown", "main", "not-a-sha"]:
            with self.subTest(version=version):
                with self.assertRaises(AssertionError):
                    self.approval_state([SOURCE_SHA], live_versions=[version])

    def test_live_version_read_failure_fails_closed(self):
        original = os.environ.get("STUB_LIVE_ERROR")
        os.environ["STUB_LIVE_ERROR"] = "1"
        try:
            with self.assertRaisesRegex(AssertionError, "simulated C00 version read failure"):
                self.approval_state([SOURCE_SHA])
        finally:
            if original is None:
                os.environ.pop("STUB_LIVE_ERROR")
            else:
                os.environ["STUB_LIVE_ERROR"] = original

    def test_new_main_push_keeps_running_qualification_and_replaces_only_pending(self):
        qualification = yaml.safe_load((ROOT / ".github/workflows/cd-qualification.yml").read_text())
        triggers = qualification.get("on", qualification.get(True))
        self.assertEqual(triggers["push"]["branches"], ["main"])
        lock = qualification["concurrency"]
        self.assertEqual(lock["group"], "cd-qualification-build-${{ github.ref }}")
        # false: one running + one pending run per group; a newer push replaces
        # only the pending run, never the build already in progress.
        self.assertIs(lock["cancel-in-progress"], False)

    def test_candidate_checks_that_fail_while_settling_skip(self):
        state, reason = self.approval_state(
            [SOURCE_SHA, NEW_HEAD], [{**SUCCESS, "backend": None}, {**SUCCESS, "backend": "cancelled"}],
        )
        self.assertEqual(state, "skipped", reason)
        self.assertEqual((self.directory / "check-reads").read_text(), "2")

    def test_current_successful_candidate_reaches_approval(self):
        state, reason = self.approval_state([SOURCE_SHA])
        self.assertEqual(state, "waiting", reason)
        self.assertTrue((self.directory / "check-reads").exists())

    def test_current_non_successful_checks_do_not_request_approval(self):
        for conclusion in ["cancelled", "failure", "skipped", None]:
            with self.subTest(conclusion=conclusion):
                state, reason = self.approval_state(
                    [SOURCE_SHA], [{**SUCCESS, "backend": conclusion}], wait_seconds="0",
                )
                self.assertEqual(state, "skipped", reason)

    def test_rerun_that_passed_replaces_the_earlier_failure(self):
        # GitHub lists check runs newest first; the earlier failed run of a
        # flaky job must not hide the green re-run (CHE-2027).
        rerun = [
            {"name": "backend", "conclusion": "success", "id": 20},
            {"name": "backend", "conclusion": "failure", "id": 10},
            {"name": "frontend", "conclusion": "success", "id": 11},
            {"name": "cd-qualification", "conclusion": "success", "id": 12},
        ]
        state, reason = self.approval_state([SOURCE_SHA], [rerun], wait_seconds="0")
        self.assertEqual(state, "waiting", reason)

    def test_later_failure_still_blocks_after_an_earlier_pass(self):
        rerun = [
            {"name": "backend", "conclusion": "failure", "id": 20},
            {"name": "backend", "conclusion": "success", "id": 10},
            {"name": "frontend", "conclusion": "success", "id": 11},
            {"name": "cd-qualification", "conclusion": "success", "id": 12},
        ]
        state, reason = self.approval_state([SOURCE_SHA], [rerun], wait_seconds="0")
        self.assertEqual(state, "skipped", reason)

    def test_missing_required_mobile_check_is_not_excused(self):
        state, reason = self.approval_state([SOURCE_SHA], files=["apps/mobile/app.tsx"], wait_seconds="0")
        self.assertEqual(state, "skipped", reason)

    def test_api_failure_fails_closed(self):
        original = os.environ.get("STUB_API_ERROR")
        try:
            for failing in ["1", "/compare/", "/commits/" + LIVE_SHA[:7]]:
                for head in [SOURCE_SHA, NEW_HEAD]:
                    with self.subTest(failing=failing, head=head):
                        os.environ["STUB_API_ERROR"] = failing
                        with self.assertRaisesRegex(AssertionError, "simulated GitHub API failure"):
                            self.approval_state([head])
        finally:
            if original is None:
                os.environ.pop("STUB_API_ERROR")
            else:
                os.environ["STUB_API_ERROR"] = original

    def test_workflow_preserves_fresh_post_approval_admission(self):
        preflight = self.workflow["jobs"]["candidate-preflight"]
        preparation = self.workflow["jobs"]["prepare-release-candidate"]
        self.assertEqual(preflight["outputs"]["eligible"], "${{ steps.candidate.outputs.eligible }}")
        self.assertIn("workflow_run", preflight["if"])
        self.assertIn("conclusion == 'success'", preflight["if"])
        checkout = next(step for step in preflight["steps"] if step.get("uses", "").startswith("actions/checkout@"))
        self.assertEqual(checkout["with"]["ref"], "${{ github.sha }}")
        self.assertEqual(checkout["with"]["fetch-depth"], 0)
        self.assertNotIn("secrets.C00", json.dumps(preflight))
        selection = next(step for step in preflight["steps"] if step.get("id") == "candidate")
        self.assertEqual(selection["env"]["C00_CONFIG_URL"],
                         "${{ vars.C00_CONFIG_URL }}")
        self.assertIn("record-checks.sh", json.dumps(preparation["steps"]))
        self.assertIn("github.event.workflow_run.head_sha", self.workflow["run-name"])
        self.assertIn("inputs.ref", self.workflow["run-name"])
        self.assertIn("github.event_name == 'workflow_dispatch'", self.workflow["jobs"]["admission"]["if"])

    def test_check_validation_rejects_another_candidates_sha(self):
        checks = self.directory / "other-checks.json"
        checks.write_text(json.dumps({"sha": NEW_HEAD, "contexts": {**SUCCESS, "mobile": "success"}}))
        result = subprocess.run(
            ["node", "deploy/cd/admission.mjs", "checks", "--checks", str(checks), "--source-sha", SOURCE_SHA],
            cwd=ROOT, text=True, capture_output=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checks were recorded for a different source SHA", result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
