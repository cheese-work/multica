"""Opt-in binary qualification: MULTICA_CLI_PATH=/path/to/candidate python3 this_file.

Stops at a missing footer before API initialization; never sends a decision ask.
Use the injected MULTICA_CLI_PATH to reproduce an installed decoder mismatch.
"""

import datetime
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class StateClaimsDecoderTest(unittest.TestCase):
    def test_request_decodes_state_claims_before_reading_footer(self):
        ask = {
            "context_eli10": "The agent CLI must accept the server's required state claims.",
            "agent_did": "Read the current CLI version and the daemon status.",
            "decision": "Use the qualified CLI in the existing cutover window?",
            "stakes_if_wrong": "An incompatible CLI prevents agents from sending asks.",
            "options": [
                {"label": "Use qualified build", "what_happens": "Use the tested build in the existing window.",
                 "pros": ["Restores compatible asks"], "cons": ["Requires qualification"],
                 "cost_or_time": "Existing cutover window"},
                {"label": "Keep current build", "what_happens": "Keep the installed binary.",
                 "pros": ["No binary change"], "cons": ["Asks remain unavailable"],
                 "cost_or_time": "No build time"},
            ],
            "recommendation": "Use the qualified build in the existing window.",
            "recommended_option": "Use qualified build",
            "default_if_no_reply": "Keep the installed binary until qualification passes.",
            "source_issue_id": "11111111-1111-4111-8111-111111111111",
            "source_comment_id": "22222222-2222-4222-8222-222222222222",
            "state_claims": [{
                "claim": "This fixture supplies state claims to the decoder.",
                "readback_command": '"$MULTICA_CLI_PATH" version',
                "observed_at": datetime.datetime.now(datetime.timezone(datetime.timedelta(hours=7))).isoformat(timespec="seconds"),
            }],
        }
        with tempfile.TemporaryDirectory(prefix="che6161-decoder-") as directory:
            fixture = Path(directory) / "ask.json"
            fixture.write_text(json.dumps(ask), encoding="utf-8")
            # The missing footer stops a compatible CLI before API initialization.
            # A closed loopback endpoint and dummy token are a second backstop.
            env = dict(os.environ, MULTICA_SERVER_URL="http://127.0.0.1:1",
                       MULTICA_TOKEN="mat_decoder-test-not-a-real-token")
            result = subprocess.run(
                [os.environ["MULTICA_CLI_PATH"], "decision", "request",
                 "--issue", ask["source_issue_id"], "--class", "reversible",
                 "--for", "33333333-3333-4333-8333-333333333333",
                 "--ask-file", str(fixture),
                 "--footer-file", str(Path(directory) / "missing-footer.md")],
                capture_output=True, text=True, timeout=30, env=env,
            )
            self.assertIn("read footer file:", result.stderr,
                          f"CLI rejected the ask before decoding state_claims: exit={result.returncode}; {result.stderr.strip()}")
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
