#!/usr/bin/env node
// CHE-4268: turn a verbose `go test` log (stdin) into a timing report, so the
// backend shard jobs can upload per-shard timings and shard balance can be checked.
// Usage: node scripts/go-test-timing.mjs <partition> < test.log > timing.json
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const TIMESTAMP = /^\d{4}-\d\d-\d\dT[\d:.]+Z /;
const round = (n) => Math.round(n * 1000) / 1000;

export function timingReport(log, partition) {
  const tests = [];
  const packages = [];
  for (const raw of log.split("\n")) {
    const line = raw.replace(TIMESTAMP, "");
    // Top-level tests start at column 0; subtests are indented and already inside their parent's time.
    const test = line.match(/^--- (PASS|FAIL|SKIP): (\S+) \(([\d.]+)s\)/);
    if (test) tests.push({ name: test[2], status: test[1], seconds: Number(test[3]) });
    const pkg = line.match(/^(ok|FAIL)\s+(\S+)\s+([\d.]+)s/);
    if (pkg) packages.push({ package: pkg[2], status: pkg[1], seconds: Number(pkg[3]) });
  }
  return { partition, test_seconds: round(tests.reduce((sum, t) => sum + t.seconds, 0)), tests, packages };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.stdout.write(`${JSON.stringify(timingReport(readFileSync(0, "utf8"), process.argv[2] ?? ""), null, 2)}\n`);
}
