"""CHE-3603: no one-off smoke workflow, one queue-alarm workflow, mechanical-tier on GitHub-hosted runners."""

import unittest
from pathlib import Path

import yaml

WORKFLOWS = Path(__file__).resolve().parents[1] / ".github/workflows"


def load(name):
    return yaml.safe_load((WORKFLOWS / name).read_text())


class CiRunners(unittest.TestCase):
    def test_che701_smoke_is_deleted(self):
        self.assertFalse((WORKFLOWS / "che701-services-smoke.yml").exists())

    def test_queue_alarms_share_one_workflow_file(self):
        self.assertFalse((WORKFLOWS / "cd-queue-watch.yml").exists())
        self.assertFalse((WORKFLOWS / "merge-queue-watch.yml").exists())
        wf = load("queue-watch.yml")
        self.assertEqual(set(wf["jobs"]), {"cd-queue-alarm", "merge-queue-alarm"})
        self.assertEqual(len(wf[True]["schedule"]), 1)  # PyYAML reads the `on` key as True
        for job in wf["jobs"].values():
            self.assertEqual(job["runs-on"], "ubuntu-24.04")
        paths = wf[True]["pull_request"]["paths"]
        for path in (".github/workflows/queue-watch.yml", "deploy/cd/check-deploy-queue.py",
                     "deploy/cd/test-check-deploy-queue.py", "scripts/merge-queue-watch.py",
                     "scripts/merge-queue-watch.test.py", "scripts/fixtures/merge-queue-watch/**"):
            self.assertIn(path, paths)

    def test_queue_alarms_keep_the_self_test_and_skip_the_live_check_on_pull_requests(self):
        jobs = load("queue-watch.yml")["jobs"]
        for name, test, live in (("cd-queue-alarm", "test-check-deploy-queue.py", "check-deploy-queue.py"),
                                 ("merge-queue-alarm", "merge-queue-watch.test.py", "merge-queue-watch.py")):
            steps = jobs[name]["steps"]
            self.assertTrue(any(test in s.get("run", "") and "if" not in s for s in steps), name)
            self.assertTrue(any(live in s.get("run", "") and test not in s["run"]
                                and s.get("if") == "github.event_name != 'pull_request'" for s in steps), name)

    def test_mechanical_tier_runs_on_github_hosted_runners(self):
        for job in load("mechanical-tier.yml")["jobs"].values():
            self.assertEqual(job["runs-on"], "ubuntu-latest")


if __name__ == "__main__":
    unittest.main()
