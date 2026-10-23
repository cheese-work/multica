"""Regression check for runner billing and fork-safe fallbacks."""
from pathlib import Path
import unittest


class RunnerRoutingTest(unittest.TestCase):
    def test_no_paid_runner_selectors(self):
        workflows = list(Path(".github/workflows").glob("*.y*ml"))
        self.assertTrue(workflows, "No workflows found")
        for workflow in workflows:
            with self.subTest(workflow=workflow.name):
                for line in workflow.read_text().splitlines():
                    if not line.lstrip().startswith("#"):
                        self.assertNotIn("blacksmith-", line)


if __name__ == "__main__":
    unittest.main()
