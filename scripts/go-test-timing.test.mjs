import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { timingReport } from "./go-test-timing.mjs";

// CHE-4268: every backend shard job uploads this report so shard balance can be checked from run data.
const log = [
  "=== RUN   TestSlow",
  "    --- PASS: TestSlow/sub (9.00s)",
  "--- PASS: TestSlow (12.50s)",
  "--- FAIL: TestBroken (0.25s)",
  "--- SKIP: TestSkipped (0.00s)",
  "ok  \tgithub.com/multica-ai/multica/server/internal/service\t263.207s",
  "FAIL\tgithub.com/multica-ai/multica/server/internal/util\t1.500s",
  "?   \tgithub.com/multica-ai/multica/server/pkg/none\t[no test files]",
].join("\n");

test("top-level tests and packages are summed; subtests are not double counted", () => {
  const report = timingReport(log, "handler-0");
  assert.equal(report.partition, "handler-0");
  assert.deepEqual(report.tests.map((t) => [t.name, t.status, t.seconds]), [["TestSlow", "PASS", 12.5], ["TestBroken", "FAIL", 0.25], ["TestSkipped", "SKIP", 0]]);
  assert.equal(report.test_seconds, 12.75);
  assert.deepEqual(report.packages.map((p) => [p.package.split("/").at(-1), p.status, p.seconds]), [["service", "ok", 263.207], ["util", "FAIL", 1.5]]);
});

test("a log with timestamps and an empty log both parse", () => {
  assert.equal(timingReport("2026-11-06T05:07:45.3Z --- PASS: TestA (1.00s)", "rest").test_seconds, 1);
  assert.deepEqual(timingReport("", "rest").tests, []);
});

test("the CLI reads stdin and prints JSON", () => {
  const script = fileURLToPath(new URL("./go-test-timing.mjs", import.meta.url));
  const run = spawnSync(process.execPath, [script, "service"], { input: log, encoding: "utf8" });
  assert.equal(run.status, 0, run.stderr);
  assert.equal(JSON.parse(run.stdout).partition, "service");
});
