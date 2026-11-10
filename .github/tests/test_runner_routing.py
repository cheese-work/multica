"""Regression check for runner billing and fork-safe fallbacks."""
from pathlib import Path
import re
import unittest

DOC = Path(".github/RUNNER-ROUTING.md")


def routing_rows():
    """Rows of the routing table: (workflow, job, variable, default, target)."""
    rows = []
    for line in DOC.read_text().splitlines():
        cells = [c.strip().strip("`") for c in line.strip().strip("|").split("|")]
        if line.startswith("|") and len(cells) == 5 and cells[2].startswith("RUNNER_"):
            rows.append(tuple(cells))
    return rows


def job_block(text, job):
    m = re.search(rf"^  {re.escape(job)}:\n(.*?)(?=^  \S|\Z)", text, re.M | re.S)
    return m.group(1) if m else None


class RunnerRoutingTest(unittest.TestCase):
    def test_no_paid_runner_selectors(self):
        workflows = list(Path(".github/workflows").glob("*.y*ml"))
        self.assertTrue(workflows, "No workflows found")
        for workflow in workflows:
            with self.subTest(workflow=workflow.name):
                for line in workflow.read_text().splitlines():
                    if not line.lstrip().startswith("#"):
                        self.assertNotIn("blacksmith-", line)

    def test_routing_table_jobs_use_runner_variables(self):
        rows = routing_rows()
        self.assertGreaterEqual(len(rows), 20, "routing table missing or short")
        for workflow, job, var, default, target in rows:
            with self.subTest(job=f"{workflow}:{job}"):
                block = job_block(Path(f".github/workflows/{workflow}").read_text(), job)
                self.assertIsNotNone(block, "job not found")
                self.assertIn(f"${{{{ vars.{var} || '{default}' }}}}", block)
                self.assertTrue(target, "private target missing")

    def test_variables_are_unique(self):
        names = [r[2] for r in routing_rows()]
        self.assertEqual(len(names), len(set(names)))


if __name__ == "__main__":
    unittest.main()
