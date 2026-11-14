"""CHE-4760 A3: a waiver re-ask lists the PR files changed since the waived head and marks the waived cases they hit."""

from __future__ import annotations

import importlib.util
import io
import json
import subprocess
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("reask", ROOT / "scripts/security-gate/waiver_reask_delta.py")
rd = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rd)

# CHE-3458 / multica#1016, waived head 41543b913 -> new head 0effbd706 (data read 9 Nov 2026 ICT).
SCHEMAS = "packages/core/api/schemas.ts"
MENU = "packages/views/issues/actions/issue-actions-menu-items.tsx"
CASES = {"lazy schema gate": SCHEMAS, "handoff guard": MENU}
PR_FILES = [MENU, SCHEMAS, "packages/core/api/schemas.test.ts", "packages/core/api/client.ts", "packages/core/package.json", "README.md"]
COMPARE = ["AGENTS.md", "server/cmd/server/router.go", "packages/core/api/client.ts", SCHEMAS,
           "packages/core/api/schemas.test.ts", "packages/core/package.json"]  # main's changes: only the PR-file ones count


def fake_gh(compare, pr_files, blob_changed=()):
    def gh(*args):
        path = args[0]
        if path == "graphql":  # one aliased `object(expression: "<sha>:<path>") { oid }` pair per PR file: a<i> old, b<i> new
            repo = {}
            for i, f in enumerate(pr_files):
                repo[f"a{i}"] = {"oid": f"old{i}"}
                repo[f"b{i}"] = {"oid": f"new{i}" if f in blob_changed else f"old{i}"}
            return json.dumps({"data": {"repository": repo}})
        if "/compare/" in path:
            return "\n".join(compare) + "\n"
        if path.endswith("/files"):
            return "\n".join(pr_files) + "\n"
        raise AssertionError(path)
    return gh


class Delta(unittest.TestCase):
    def test_changed_pr_files_only_and_marks_hit_case(self):
        changed, marks = rd.delta(rd.parse_compare(COMPARE), PR_FILES, CASES)
        self.assertEqual(changed, ["packages/core/api/client.ts", SCHEMAS, "packages/core/api/schemas.test.ts", "packages/core/package.json"])
        self.assertTrue(marks["lazy schema gate"])
        self.assertFalse(marks["handoff guard"])

    def test_case_file_not_in_pr_never_marked(self):
        _, marks = rd.delta(rd.parse_compare([MENU]), [SCHEMAS], CASES)
        self.assertEqual(marks, {"lazy schema gate": False, "handoff guard": False})


class Cli(unittest.TestCase):
    def run_cli(self, compare, extra=(), blob_changed=()):
        argv = ["--github", "cheese-work/multica", "--pr", "1016", "--old", "41543b913", "--new", "0effbd706",
                "--case", f"lazy schema gate={SCHEMAS}", "--case", f"handoff guard={MENU}", *extra]
        out = io.StringIO()
        with mock.patch.object(rd, "_gh", fake_gh(compare, PR_FILES, blob_changed)), redirect_stdout(out):
            code = rd.main(argv)
        return code, out.getvalue()

    def test_che3458_lists_schemas_ts_and_marks_lazy_schema_gate(self):
        code, out = self.run_cli(COMPARE)
        self.assertEqual(code, 0)
        self.assertIn(f"- {SCHEMAS}", out)
        self.assertIn(f"- lazy schema gate: CHANGED ({SCHEMAS})", out)
        self.assertIn(f"- handoff guard: not in the list ({MENU})", out)
        self.assertNotIn("AGENTS.md", out)

    def test_compare_at_github_file_cap_falls_back_to_blob_ids_and_still_lists_schemas_ts(self):
        capped = [f"f{i}.txt" for i in range(300)]  # GitHub cuts the compare at 300 files, so main's merge hides PR files
        code, out = self.run_cli(capped, blob_changed={SCHEMAS, "packages/core/package.json"})
        self.assertEqual(code, 0)
        self.assertIn(f"- {SCHEMAS}", out)
        self.assertNotIn("- packages/core/api/client.ts", out)
        self.assertIn(f"- lazy schema gate: CHANGED ({SCHEMAS})", out)
        self.assertIn(f"- handoff guard: not in the list ({MENU})", out)
        self.assertIn("blob ids", out)

    def test_capped_compare_and_failed_blob_read_is_incomplete_not_clean(self):
        def broken(*args):
            if args[0] == "graphql":
                raise subprocess.CalledProcessError(1, "gh")
            return fake_gh([f"f{i}.txt" for i in range(300)], PR_FILES)(*args)
        out = io.StringIO()
        with mock.patch.object(rd, "_gh", broken), redirect_stdout(out):
            code = rd.main(["--github", "o/r", "--pr", "1", "--old", "a", "--new", "b", "--case", f"x={SCHEMAS}"])
        self.assertEqual(code, 2)
        self.assertIn("INCOMPLETE", out.getvalue())
        self.assertNotIn("not in the list", out.getvalue())

    def test_renamed_waived_file_is_marked_changed_not_clean(self):
        # Waived case names old.py (its path at the waived head). Main renamed it to new.py; the PR file list shows new.py only.
        pr_files = ["pkg/new.py", MENU]
        compare = ["pkg/new.py\tpkg/old.py", "AGENTS.md"]  # compare line: filename TAB previous_filename
        out = io.StringIO()
        with mock.patch.object(rd, "_gh", fake_gh(compare, pr_files)), redirect_stdout(out):
            code = rd.main(["--github", "o/r", "--pr", "1", "--old", "a", "--new", "b", "--case", "renamed=pkg/old.py", "--case", f"guard={MENU}"])
        self.assertEqual(code, 0)
        self.assertIn("- renamed: CHANGED (pkg/old.py)", out.getvalue())
        self.assertIn(f"- guard: not in the list ({MENU})", out.getvalue())

    def test_case_path_missing_from_pr_files_and_compare_is_unknown(self):
        # Capped compare + blob read: a waived path that is no longer a PR file (renamed or removed) cannot be resolved.
        capped = [f"f{i}.txt" for i in range(300)]
        out = io.StringIO()
        with mock.patch.object(rd, "_gh", fake_gh(capped, PR_FILES)), redirect_stdout(out):
            rd.main(["--github", "o/r", "--pr", "1", "--old", "a", "--new", "b", "--case", "gone=pkg/old.py"])
        self.assertIn("- gone: UNKNOWN (pkg/old.py)", out.getvalue())
        self.assertNotIn("not in the list", out.getvalue())

    def test_case_without_path_is_a_usage_error(self):
        with self.assertRaises(SystemExit):
            rd.main(["--github", "o/r", "--pr", "1", "--old", "a", "--new", "b", "--case", "nopath"])


if __name__ == "__main__":
    unittest.main()
