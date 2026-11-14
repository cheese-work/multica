#!/usr/bin/env node
// Path filtering and aggregate gates share this fail-closed decision contract.
import { appendFileSync, readFileSync, realpathSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

export const filters = JSON.parse(
  readFileSync(new URL("../.github/ci-paths.json", import.meta.url), "utf8"),
);

// Scopes whose jobs run candidate code on the shared self-hosted X99 runner
// (script-checks, frontend-quality). Untrusted upstream-sync/* heads (CHE-1381)
// never select them; the aggregate gates then expect "skipped" for both.
const X99_SCOPES = ["scripts", "quality_only"];

const MERGE_GROUP_WAIT_MS = 180000;
const MERGE_GROUP_POLL_MS = 10000;
const blockingSleep = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

// The push to main lands seconds before the merge-group CI run finishes, so a run that is
// still queued or in progress, or not listed yet, is awaited rather than treated as not green.
// The bound covers total elapsed time, lookups included.
export function hasGreenMergeGroup(env, query = (endpoint) => JSON.parse(execFileSync("gh", ["api", endpoint], { encoding: "utf8", timeout: 30000 })), sleep = blockingSleep, now = Date.now) {
  if (env.EVENT_NAME !== "push" || env.GITHUB_REF !== "refs/heads/main" || env.CI_PUSH_FULL === "true") return false;
  if (!/^[a-f0-9]{40}$/.test(env.GITHUB_SHA ?? "") || !env.GITHUB_REPOSITORY) return false;
  const override = Number.parseInt(env.MERGE_GROUP_WAIT_MS ?? "", 10);
  const waitMs = Number.isNaN(override) ? MERGE_GROUP_WAIT_MS : override;
  const deadline = now() + waitMs;
  try {
    for (;;) {
      const response = query(`repos/${env.GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?event=merge_group&head_sha=${env.GITHUB_SHA}&per_page=10`);
      const runs = response.workflow_runs.filter((run) => run.event === "merge_group"
        && run.head_sha === env.GITHUB_SHA && run.head_repository?.full_name === env.GITHUB_REPOSITORY);
      if (runs.some((run) => run.status === "completed" && run.conclusion === "success")) return true;
      const remaining = deadline - now();
      if (remaining <= 0 || runs.length > 0 && runs.every((run) => run.status === "completed")) return false;
      sleep(Math.min(MERGE_GROUP_POLL_MS, remaining));
    }
  } catch {
    console.warn("Merge-group lookup failed; running full push CI.");
    return false;
  }
}

// forcePushFull (CI_PUSH_FULL=true, the merge-queue fallback, CHE-4498): a push to main runs every scope,
// not only the paths changed since the previous push.
export function decideScopes(event, filtered, headRef = "", mergeGroupGreen = false, forcePushFull = false) {
  const full = event === "schedule" || event === "workflow_dispatch" || (event === "push" && headRef === "main" && forcePushFull);
  if (!full && event !== "push" && event !== "pull_request" && event !== "merge_group") {
    throw new Error(`Unsupported CI event: ${event}`);
  }
  const reuse = event === "push" && headRef === "main" && mergeGroupGreen === true;
  const outputs = { full: String(full), merge_group_green: String(reuse), push_full: String(!reuse) };
  for (const scope of Object.keys(filters)) {
    if (!full && filtered[scope] !== "true" && filtered[scope] !== "false") {
      throw new Error(`Missing or invalid path-filter result: ${scope}`);
    }
    outputs[scope] = full ? "true" : reuse ? "false" : filtered[scope];
  }
  // Product builds already install dependencies and run the shared checks.
  // Only allocate a separate runner when quality is the sole frontend work.
  outputs.quality_only = String(outputs.quality === "true" && outputs.frontend === "false");
  // Full-suite jobs (CHE-4874): a PR runs one only when its own scope changed; merge_group,
  // main pushes, schedule and dispatch keep the whole suite. A reused merge group skips all.
  for (const scope of ["backend", "frontend", "sqlc"]) {
    outputs[`push_${scope}`] = String(!reuse && (event !== "pull_request" || outputs[scope] === "true"));
  }
  if (headRef.startsWith("upstream-sync/")) {
    for (const scope of X99_SCOPES) outputs[scope] = "false";
  }
  return outputs;
}

export function checkGate(needs, jobScopes) {
  if (needs.changes?.result !== "success") {
    throw new Error("Path filtering did not succeed");
  }
  for (const [job, scope] of Object.entries(jobScopes)) {
    // "always" marks a job with no job-level `if:` scoping at all (CHE-748
    // moved several jobs off Blacksmith onto free GitHub-hosted runners and
    // dropped their path-filter guard at the same time) -- it always runs
    // and must always succeed, independent of any path-filter output.
    if (scope === "always") {
      if (needs[job]?.result !== "success") {
        throw new Error(`${job}: expected success, got ${needs[job]?.result ?? "missing"}`);
      }
      continue;
    }
    const selected = needs.changes.outputs?.[scope];
    if (selected !== "true" && selected !== "false") {
      throw new Error(`Missing or invalid scope: ${scope}`);
    }
    const expected = selected === "true" ? "success" : "skipped";
    if (needs[job]?.result !== expected) {
      throw new Error(`${job}: expected ${expected}, got ${needs[job]?.result ?? "missing"}`);
    }
  }
  // A newly added dependency must have a declared scope before the gate can pass.
  for (const job of Object.keys(needs)) {
    if (job !== "changes" && !Object.hasOwn(jobScopes, job)) {
      throw new Error(`Unchecked dependency: ${job}`);
    }
  }
}

if (process.argv[1] && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url))) {
  try {
    switch (process.argv[2]) {
      case "decide": {
        const outputs = decideScopes(process.env.EVENT_NAME, JSON.parse(process.env.FILTER_RESULTS), process.env.HEAD_REF, hasGreenMergeGroup(process.env), process.env.CI_PUSH_FULL === "true");
        appendFileSync(process.env.GITHUB_OUTPUT,
          Object.entries(outputs).map(([key, value]) => `${key}=${value}\n`).join(""));
        break;
      }
      case "merge-group-green":
        appendFileSync(process.env.GITHUB_OUTPUT, `merge_group_green=${hasGreenMergeGroup(process.env)}\n`);
        break;
      case "gate":
        checkGate(JSON.parse(process.env.NEEDS_JSON), JSON.parse(process.env.JOB_SCOPES));
        console.log("All selected CI jobs succeeded; only unselected jobs were skipped.");
        break;
      default:
        throw new Error("Usage: node scripts/ci-scope.mjs decide|merge-group-green|gate");
    }
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
