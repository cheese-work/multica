#!/usr/bin/env node
// Path filtering and aggregate gates share this fail-closed decision contract.
import { appendFileSync, readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { pathToFileURL } from "node:url";

export const filters = JSON.parse(
  readFileSync(new URL("../.github/ci-paths.json", import.meta.url), "utf8"),
);

// Scopes whose jobs run candidate code on the shared self-hosted X99 runner
// (script-checks, frontend-quality). Untrusted upstream-sync/* heads (CHE-1381)
// never select them; the aggregate gates then expect "skipped" for both.
const X99_SCOPES = ["scripts", "quality_only"];

export function hasGreenMergeGroup(env, query = (endpoint) => JSON.parse(execFileSync("gh", ["api", endpoint], { encoding: "utf8", timeout: 30000 }))) {
  if (env.EVENT_NAME !== "push" || env.GITHUB_REF !== "refs/heads/main" || env.CI_PUSH_FULL === "true") return false;
  if (!/^[a-f0-9]{40}$/.test(env.GITHUB_SHA ?? "") || !env.GITHUB_REPOSITORY) return false;
  try {
    const response = query(`repos/${env.GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?event=merge_group&status=success&head_sha=${env.GITHUB_SHA}&per_page=1`);
    return response.workflow_runs.some((run) => run.event === "merge_group"
      && run.head_sha === env.GITHUB_SHA && run.status === "completed" && run.conclusion === "success"
      && run.head_repository?.full_name === env.GITHUB_REPOSITORY);
  } catch {
    console.warn("Merge-group lookup failed; running full push CI.");
    return false;
  }
}

export function decideScopes(event, filtered, headRef = "", mergeGroupGreen = false) {
  const full = event === "schedule" || event === "workflow_dispatch";
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

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    switch (process.argv[2]) {
      case "decide": {
        const outputs = decideScopes(process.env.EVENT_NAME, JSON.parse(process.env.FILTER_RESULTS), process.env.HEAD_REF, hasGreenMergeGroup(process.env));
        appendFileSync(process.env.GITHUB_OUTPUT,
          Object.entries(outputs).map(([key, value]) => `${key}=${value}\n`).join(""));
        break;
      }
      case "gate":
        checkGate(JSON.parse(process.env.NEEDS_JSON), JSON.parse(process.env.JOB_SCOPES));
        console.log("All selected CI jobs succeeded; only unselected jobs were skipped.");
        break;
      default:
        throw new Error("Usage: node scripts/ci-scope.mjs decide|gate");
    }
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
