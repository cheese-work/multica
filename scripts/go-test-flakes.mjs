#!/usr/bin/env node
// CHE-4270: collect the flakes a merge_group run absorbed. Reads every go-test-*.json (gotestsum
// --jsonfile, which includes the rerun) and handler-flakes.txt (<package>\t<test> lines from
// go-test-handler-shards.sh) in the given directory. A test with a fail event and a later pass event
// in the same package is a flake. Writes <dir>/flakes.json for the flaky-quarantine job (CHE-3625)
// and prints a markdown summary for $GITHUB_STEP_SUMMARY.
// Budget: reruns absorb at most 3 flaky tests per job. The shards and the other packages rerun
// independently, so only this job-wide count sees them together; more than 3 exits 1 (a run with
// that many failures is a real break, not a flake). flakes.json is written either way.
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const MAX_FLAKES = 3;
const dir = process.argv[2];
if (!dir) {
  console.error("usage: go-test-flakes.mjs <flake-dir>");
  process.exit(2);
}

const found = new Map(); // "package\ttest" -> { package, test }
const add = (pkg, test) => found.set(`${pkg}\t${test}`, { package: pkg, test });

for (const file of readdirSync(dir)) {
  const text = readFileSync(join(dir, file), "utf8");
  if (/^go-test-.*\.json$/.test(file)) {
    const failed = new Set();
    for (const line of text.split("\n")) {
      if (!line.startsWith("{")) continue;
      const { Action, Package, Test } = JSON.parse(line);
      if (!Test) continue;
      const key = `${Package}\t${Test}`;
      if (Action === "fail") failed.add(key);
      else if (Action === "pass" && failed.has(key)) add(Package, Test);
    }
  } else if (file === "handler-flakes.txt") {
    for (const line of text.split("\n")) {
      const [pkg, test] = line.split("\t");
      if (pkg && test) add(pkg, test);
    }
  }
}

// A failing subtest fails its parents too; list the leaf only.
const all = [...found.values()];
const flakes = all
  .filter((f) => !all.some((o) => o.package === f.package && o.test.startsWith(`${f.test}/`)))
  .sort((a, b) => (a.package + a.test).localeCompare(b.package + b.test));

writeFileSync(join(dir, "flakes.json"), `${JSON.stringify({ flakes }, null, 2)}\n`);

if (flakes.length === 0) {
  console.log("### Go test reruns\n\nNo flaky tests: nothing failed and then passed.");
} else {
  const n = flakes.length;
  const over = n > MAX_FLAKES;
  const verdict = over
    ? `The job failed: more than ${MAX_FLAKES} tests failed in this job, which is a real break and not a flake.`
    : "The job passed.";
  console.log(`### Go test reruns\n\n${n} flaky test${n === 1 ? "" : "s"} failed, then passed on the one rerun. ${verdict}\n`);
  if (over) process.exitCode = 1;
  console.log("| Package | Test |\n| --- | --- |");
  for (const f of flakes) console.log(`| \`${f.package}\` | \`${f.test}\` |`);
}
