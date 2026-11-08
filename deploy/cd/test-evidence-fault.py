#!/usr/bin/env python3
"""Evidence-only candidate workflow and the readiness fault overlay (CHE-4664)."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/cd-evidence.yml"
OVERLAY = ROOT / "deploy/cd/docker-compose.fault-readyz.yml"
C00_RUNNER = ["self-hosted", "linux", "x64", "cheese-c00-deploy"]
STAGES_AFTER_EVIDENCE = ("deploy", "admission", "cutover", "rollback")


def needs(job):
    value = job.get("needs", [])
    return [value] if isinstance(value, str) else value


class EvidenceWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load(WORKFLOW.read_text())
        self.text = WORKFLOW.read_text()
        self.jobs = self.workflow["jobs"]
        # PyYAML parses the key `on` as True.
        self.triggers = self.workflow.get("on", self.workflow.get(True))

    def test_only_a_dispatch_starts_it(self):
        self.assertEqual(list(self.triggers), ["workflow_dispatch"])
        self.assertTrue(self.triggers["workflow_dispatch"]["inputs"]["candidate_sha"]["required"])

    def test_owner_gate_precedes_every_job(self):
        gate = self.jobs["authorize-c00"]
        self.assertNotIn("environment", gate)
        self.assertEqual(gate["steps"][0]["env"]["ACTOR"], "${{ github.triggering_actor }}")
        self.assertIn("congvc-dev", gate["steps"][0]["run"])
        for name, job in self.jobs.items():
            if name != "authorize-c00":
                self.assertIn("authorize-c00", needs(job), name)

    def test_it_stops_before_deploy(self):
        self.assertEqual(sorted(self.jobs), ["authorize-c00", "prepare-release-candidate"])
        steps = json.dumps([s for j in self.jobs.values() for s in j["steps"]])
        for word in ["cutover.sh", "deploy.sh", "docker", "c00-production", "rollback"]:
            self.assertNotIn(word, steps)

    def test_the_one_c00_read_is_the_shared_baseline_capture(self):
        job = self.jobs["prepare-release-candidate"]
        self.assertEqual(job["runs-on"], C00_RUNNER)
        self.assertEqual(job["environment"], "c00-runtime")
        self.assertEqual(job["concurrency"]["group"], "cd-deploy-c00")
        self.assertEqual(job["concurrency"]["queue"], "max")
        captures = [s for s in job["steps"] if "capture-remote-baseline.sh" in s.get("run", "")]
        self.assertEqual(len(captures), 1)

    def test_it_uploads_the_release_candidate_set_under_the_name_admission_reads(self):
        upload = self.jobs["prepare-release-candidate"]["steps"][-1]
        self.assertTrue(upload["uses"].startswith("actions/upload-artifact@"))
        self.assertEqual(upload["with"]["name"], "cd-release-candidate-${{ steps.q.outputs.sha }}")
        self.assertEqual(upload["with"]["if-no-files-found"], "error")
        for name in ["cd-release-candidate.json", "baseline-tuple.json", "cd-event.json",
                     "cd-checks.json", "cd-provenance.json"]:
            self.assertIn(name, upload["with"]["path"])

    def test_it_issues_the_same_manifest_as_the_deploy_workflow(self):
        # Same evidence as the deploy path: only the steps that issue it are compared.
        deploy = yaml.safe_load((ROOT / ".github/workflows/cd-deploy.yml").read_text())
        want = deploy["jobs"]["prepare-release-candidate"]["steps"]
        got = self.jobs["prepare-release-candidate"]["steps"]
        self.assertEqual(got, want)


@unittest.skipUnless(shutil.which("docker"), "docker compose needed to render the overlay")
class ReadyzFaultOverlayTests(unittest.TestCase):
    def render(self, **env):
        files = ["-f", "docker-compose.selfhost.yml", "-f", "deploy/cd/docker-compose.ab.yml",
                 "-f", "deploy/cd/docker-compose.fault-readyz.yml"]
        out = subprocess.run(
            ["docker", "compose", *files, "config", "--format", "json"], cwd=ROOT, text=True,
            capture_output=True, check=True,
            env={"HOME": os.environ["HOME"], "PATH": os.environ["PATH"],
                 "JWT_SECRET": "synthetic", **env}).stdout
        return json.loads(out)["services"]

    def test_candidate_start_cannot_reach_the_database(self):
        # cutover.sh starts the candidate with MULTICA_SKIP_MIGRATIONS=1.
        for colour in ["backend-blue", "backend-green"]:
            url = self.render(MULTICA_SKIP_MIGRATIONS="1")[colour]["environment"]["DATABASE_URL"]
            self.assertIn("@fault-db.invalid", url, colour)
            self.assertNotIn("@postgres:", url, colour)

    def test_migration_step_keeps_the_real_database(self):
        # The migrator runs in the same service without that variable and must still succeed.
        for colour in ["backend-blue", "backend-green"]:
            url = self.render()[colour]["environment"]["DATABASE_URL"]
            self.assertIn("@postgres:5432/", url, colour)

    def test_only_the_database_url_changes(self):
        services = yaml.safe_load(OVERLAY.read_text())["services"]
        self.assertEqual(sorted(services), ["backend-blue", "backend-green"])
        for service in services.values():
            self.assertEqual(list(service), ["environment"])
            self.assertEqual(list(service["environment"]), ["DATABASE_URL"])


if __name__ == "__main__":
    unittest.main()
