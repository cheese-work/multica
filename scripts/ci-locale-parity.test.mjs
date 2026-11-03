import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-2418 B3: the merge queue kept failing on locale skew (two PRs each green,
// together missing zh-Hant keys). The PR runs parity against the newest main.
const ci = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");
const job = ci.match(/^  locale-parity:\n([\s\S]*?)(?=^  \S)/m)?.[1] ?? "";

test("locale-parity job merges the newest main before running the parity test", () => {
  assert.ok(job, "locale-parity job exists");
  assert.match(job, /needs\.changes\.outputs\.frontend == 'true'/);
  assert.match(job, /if: github\.event_name == 'pull_request'/);
  const merge = job.search(/git .*merge --no-edit origin\/main/);
  assert.ok(merge > -1, "merges main");
  assert.ok(job.indexOf("parity.test.ts") > merge, "tests after the merge");
});

test("frontend aggregate requires locale-parity", () => {
  const aggregate = ci.match(/^  frontend:\n([\s\S]*?)(?=^  \S)/m)?.[1] ?? "";
  assert.match(aggregate, /needs: \[[^\]]*locale-parity/);
  assert.match(aggregate, /"locale-parity":"frontend"/);
});
