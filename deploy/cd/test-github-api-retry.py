#!/usr/bin/env python3
"""CHE-4875: github-api-retry.py must not sleep through an hour-long rate-limit window.

A stub `gh` answers per token (GH_TOKEN) so each case replays which token is limited.
"""
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest

SCRIPT = Path(__file__).resolve().parent / "github-api-retry.py"

STUB = r'''#!/usr/bin/env python3
import os, sys, time
token = os.environ.get("GH_TOKEN", "")
if "rate_limit" in sys.argv:
    print(os.environ.get("STUB_APP_BUDGET", "5000"))
    sys.exit(0)
limited = os.environ.get("STUB_LIMITED", "").split(",")
once = os.environ.get("STUB_ONCE")
if once and os.path.exists(once):
    limited = []
elif once:
    open(once, "w").close()
if token in limited:
    reset = int(time.time()) + int(os.environ.get("STUB_RESET_IN", "1800"))
    status = os.environ.get("STUB_STATUS", "403")
    sys.stdout.write(f"HTTP/2.0 {status} Forbidden\r\nX-RateLimit-Remaining: {os.environ.get('STUB_REMAINING', '0')}\r\n"
                     f"X-RateLimit-Resource: core\r\nX-RateLimit-Reset: {reset}\r\n\r\n")
    body = os.environ.get("STUB_BODY", '{"message":"API rate limit exceeded for installation"}')
    sys.stdout.write(body)
    sys.stderr.write(f"gh: {body} (HTTP {status})\n")
    sys.exit(1)
sys.stdout.write("HTTP/2.0 200 OK\r\n\r\n" + f"ok-{token}")
'''


class GithubApiRetryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        bin_dir = Path(self.temp.name)
        stub = bin_dir / "gh"
        stub.write_text(STUB)
        stub.chmod(0o755)
        self.bin = bin_dir

    def run_retry(self, limited, reset_in=1800, extra=None, tokens=("APP", "FALLBACK")):
        env = {
            **os.environ,
            "PATH": f"{self.bin}:{os.environ['PATH']}",
            "GH_TOKEN": tokens[0],
            "APP_TOKEN": "APP",
            "FALLBACK_TOKEN": "FALLBACK",
            "STUB_LIMITED": limited,
            "STUB_RESET_IN": str(reset_in),
            **(extra or {}),
        }
        started = time.time()
        result = subprocess.run(
            ["python3", str(SCRIPT), str(int(time.time()) + 3600), "repos/x/y"],
            capture_output=True, text=True, env=env, timeout=20)
        return result, time.time() - started

    def test_limited_app_token_fails_over_to_the_fallback_without_sleeping(self):
        result, elapsed = self.run_retry(limited="APP")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "ok-FALLBACK")
        self.assertLess(elapsed, 10)
        # the log names the limited token and its bucket so the cause is visible next time
        self.assertIn("app token", result.stderr)
        self.assertIn("resource=core", result.stderr)

    def test_all_tokens_limited_past_the_sleep_cap_fails_fast(self):
        result, elapsed = self.run_retry(limited="APP,FALLBACK", reset_in=1800)
        self.assertNotEqual(result.returncode, 0)
        self.assertLess(elapsed, 10)
        self.assertIn("exceeds the 60s sleep cap", result.stderr)

    def test_short_reset_on_the_only_token_sleeps_then_succeeds(self):
        result, elapsed = self.run_retry(
            limited="APP", reset_in=2, tokens=("APP",),
            extra={"APP_TOKEN": "APP", "FALLBACK_TOKEN": "", "STUB_ONCE": str(self.bin / "once")})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("retrying in", result.stderr)
        self.assertGreaterEqual(elapsed, 1)

    def test_plain_403_is_not_retried_or_failed_over(self):
        result, elapsed = self.run_retry(
            limited="APP", extra={"STUB_REMAINING": "4000", "STUB_BODY": '{"message":"Resource not accessible by integration"}'})
        self.assertNotEqual(result.returncode, 0)
        self.assertLess(elapsed, 10)
        self.assertNotIn("retrying", result.stderr)

    def test_throttled_fallback_does_not_spend_the_app_reserve(self):
        # pick-api-token.sh chose the fallback because the App budget is below floor + headroom (1600).
        result, elapsed = self.run_retry(limited="FALLBACK", tokens=("FALLBACK",), extra={"STUB_APP_BUDGET": "1000"})
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("ok-APP", result.stdout)
        self.assertLess(elapsed, 10)
        self.assertIn("below", result.stderr)

    def test_throttled_fallback_fails_over_to_the_app_token_while_its_budget_is_above_the_floor(self):
        result, _ = self.run_retry(limited="FALLBACK", tokens=("FALLBACK",), extra={"STUB_APP_BUDGET": "1600"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "ok-APP")


if __name__ == "__main__":
    unittest.main()
