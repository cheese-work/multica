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
if os.environ.get("STUB_API_ERROR"):
    sys.exit("simulated GitHub API failure")
if endpoint.endswith("git/ref/heads/main"):
    counter = state / "head-reads"
    index = int(counter.read_text()) if counter.exists() else 0
    counter.write_text(str(index + 1))
    heads = json.loads(os.environ["STUB_HEADS"])
    print(heads[min(index, len(heads) - 1)])
elif "/check-runs?" in endpoint:
    counter = state / "check-reads"
    index = int(counter.read_text()) if counter.exists() else 0
    counter.write_text(str(index + 1))
    polls = json.loads(os.environ["STUB_CHECKS"])
    for name, conclusion in polls[min(index, len(polls) - 1)].items():
        print(json.dumps({"name": name, "conclusion": conclusion}))
elif endpoint.endswith("commits/" + os.environ["SOURCE_SHA"]):
    print(json.dumps({"files": json.loads(os.environ["STUB_FILES"]), "truncated": False}))
else:
    sys.exit("unexpected API request: " + endpoint)
''')
        stub.chmod(0o755)

    def approval_state(self, heads, checks=None, files=None, wait_seconds="60"):
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
            "STUB_STATE": str(self.directory),
            "STUB_HEADS": json.dumps(heads),
            "STUB_CHECKS": json.dumps(checks if checks is not None else [SUCCESS]),
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

    def test_superseded_candidate_skips_before_production_approval(self):
        state, reason = self.approval_state([NEW_HEAD], [{**SUCCESS, "backend": "cancelled"}])
        self.assertEqual(state, "skipped", reason)
        self.assertFalse((self.directory / "check-reads").exists())

    def test_candidate_superseded_while_checks_settle_skips(self):
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

    def test_missing_required_mobile_check_is_not_excused(self):
        state, reason = self.approval_state([SOURCE_SHA], files=["apps/mobile/app.tsx"], wait_seconds="0")
        self.assertEqual(state, "skipped", reason)

    def test_api_failure_fails_closed(self):
        original = os.environ.get("STUB_API_ERROR")
        os.environ["STUB_API_ERROR"] = "1"
        try:
            with self.assertRaisesRegex(AssertionError, "simulated GitHub API failure"):
                self.approval_state([SOURCE_SHA])
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
