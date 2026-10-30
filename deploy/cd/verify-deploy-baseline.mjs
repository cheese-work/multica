#!/usr/bin/env node

import { execFileSync, spawnSync } from "node:child_process";
import { appendFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { isDeepStrictEqual } from "node:util";

function loadSnapshot(path) {
  return JSON.parse(execFileSync(process.execPath, [
    fileURLToPath(new URL("./tuple-snapshot.mjs", import.meta.url)),
    "verify", "--snapshot", path,
  ], { encoding: "utf8" }));
}

function isAncestor(ancestor, descendant) {
  const result = spawnSync("git", ["merge-base", "--is-ancestor", ancestor, descendant], { encoding: "utf8" });
  if (result.error || ![0, 1].includes(result.status)) {
    throw new Error(`cannot verify deployed commit ancestry: ${result.error?.message || result.stderr}`);
  }
  return result.status === 0;
}

try {
  const [candidate, baselinePath, currentPath] = process.argv.slice(2);
  if (!/^[a-f0-9]{40}$/.test(candidate ?? "")) throw new Error("candidate must be a full Git SHA");
  const baseline = loadSnapshot(baselinePath);
  const current = loadSnapshot(currentPath);
  if (candidate === current.application_sha || isAncestor(candidate, current.application_sha)) {
    if (process.env.GITHUB_STEP_SUMMARY) {
      appendFileSync(process.env.GITHUB_STEP_SUMMARY,
        `Skipped before C00 mutation: running commit \`${current.application_sha}\` already contains candidate \`${candidate}\`.\n`);
    }
    process.stdout.write("skip\n");
  } else {
    if (!isAncestor(current.application_sha, candidate)) throw new Error("candidate diverges from C00's running commit");
    delete baseline.captured_at;
    delete current.captured_at;
    if (!isDeepStrictEqual(baseline, current)) throw new Error("C00 baseline changed after preparation; prepare and admit a fresh release candidate");
    process.stdout.write("deploy\n");
  }
} catch (error) {
  process.stderr.write(`deploy baseline: ${error.message}\n`);
  process.exitCode = 1;
}
