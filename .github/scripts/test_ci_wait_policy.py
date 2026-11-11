import importlib.util
from datetime import date
from pathlib import Path
import unittest

import yaml

spec = importlib.util.spec_from_file_location("ci_wait_policy", Path(__file__).with_name("ci-wait-policy.py"))
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)

TODAY = date(2026, 11, 11)

# The readback job as it ran before CHE-5175: one step holds the runner for 900 s.
BAD = """
jobs:
  readback:
    runs-on: [self-hosted, cheese-c00-deploy]
    steps:
      - name: Watch C00 for 15 minutes
        run: bash deploy/cd/post-deploy-readback.sh "$SOURCE_SHA" 900 manifest.json
"""
# The fixed shape: a bounded wait for health, then an environment wait timer that holds no runner.
FIXED = """
jobs:
  readback:
    runs-on: [self-hosted, cheese-c00-deploy]
    steps:
      - run: bash deploy/cd/post-deploy-readback.sh "$SOURCE_SHA" 120 manifest.json
  readback-delay:
    needs: readback
    runs-on: ubuntu-24.04
    environment: c00-readback-delay
    steps:
      - run: echo waited
"""


def scan(text, exceptions=()):
    return policy.violations(yaml.safe_load(text), "cd-deploy.yml", list(exceptions), TODAY)


def exception(**override):
    base = {"repo": "cheese-work/multica", "workflow": "cd-deploy.yml", "rule": "inline-wait", "owner": "x99-claude-sonnet-200k",
            "root_fix": "CHE-5175", "added": "2026-11-11", "expires": "2026-11-18", "reason": "r"}
    return {**base, **override}


class WaitPolicyTests(unittest.TestCase):
    def test_a_long_watch_script_argument_is_red(self):
        found = scan(BAD)
        self.assertEqual(len(found), 1)
        self.assertIn("readback", found[0])
        self.assertIn("900", found[0])

    def test_the_fixed_workflow_is_green(self):
        self.assertEqual(scan(FIXED), [])

    def test_sleep_over_120_seconds_is_red_and_up_to_120_is_green(self):
        def job(command):
            return f"jobs:\n  j:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: {command}\n"
        for command in ["sleep 121", "sleep 3m", "sleep 1h"]:
            with self.subTest(command=command):
                self.assertEqual(len(scan(job(command))), 1)
        for command in ["sleep 120", "sleep 30", "sleep 2m", "echo sleep 900"]:
            with self.subTest(command=command):
                self.assertEqual(scan(job(command)), [])

    def test_watch_flag_is_red(self):
        text = "jobs:\n  j:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: gh run watch 1 --exit-status\n"
        self.assertEqual(len(scan(text)), 1)
        text = "jobs:\n  j:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: gh pr checks 5 --watch\n"
        self.assertEqual(len(scan(text)), 1)

    def test_a_live_exception_with_owner_root_fix_and_a_7_day_expiry_makes_it_green(self):
        self.assertEqual(scan(BAD, [exception()]), [])

    def test_an_exception_for_another_workflow_does_not_apply(self):
        self.assertEqual(len(scan(BAD, [exception(workflow="ci.yml")])), 1)

    def test_a_bad_exception_is_itself_a_violation(self):
        cases = {
            "expired": exception(expires="2026-11-10"),
            "longer than 7 days": exception(expires="2026-11-19"),
            "added in the future": exception(added="2099-01-01", expires="2099-01-08"),
            "added tomorrow": exception(added="2026-11-12", expires="2026-11-15"),
            "no owner": exception(owner=""),
            "no root fix": exception(root_fix=""),
            "root fix not an issue": exception(root_fix="later"),
        }
        for name, bad in cases.items():
            with self.subTest(name=name):
                found = scan(BAD, [bad])
                self.assertTrue(any("exception" in line for line in found), found)
                self.assertTrue(any("900" in line for line in found), found)

    def test_a_workflow_free_of_waits_has_no_violations_for_a_repo_without_jobs(self):
        self.assertEqual(scan("on: push\n"), [])


class RepoTests(unittest.TestCase):
    def test_this_repository_has_no_inline_waits(self):
        root = Path(__file__).resolve().parents[2]
        self.assertEqual(policy.check_repo(root, TODAY), [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
