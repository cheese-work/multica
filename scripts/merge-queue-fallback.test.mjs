import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { decideScopes, filters, hasGreenMergeGroup } from "./ci-scope.mjs";

// CHE-4498 (CHE-2425): fallback that guards `main` without `merge_group`. Nothing here is applied;
// the patch and the ruleset payload live in .github/fallback/ until Cheese orders the switch.
const root = (path) => fileURLToPath(new URL(`../${path}`, import.meta.url));
const fallback = (name) => readFileSync(root(`.github/fallback/${name}`), "utf8");
const ciPath = ".github/workflows/ci.yml";

function patchedCi(context) {
  const directory = mkdtempSync(join(tmpdir(), "mq-fallback-"));
  context.after(() => rmSync(directory, { recursive: true, force: true }));
  mkdirSync(join(directory, ".github/workflows"), { recursive: true });
  writeFileSync(join(directory, ciPath), readFileSync(root(ciPath)));
  const result = spawnSync("git", ["apply", root(".github/fallback/ci-push-trigger.patch")], { cwd: directory, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  return readFileSync(join(directory, ciPath), "utf8");
}

test("fallback patch applies to ci.yml: push to main triggers CI and a later push never cancels an earlier one", (context) => {
  const patched = patchedCi(context);
  const triggers = patched.slice(0, patched.indexOf("\njobs:"));
  assert.match(triggers, /^  push:\n    branches: \[main\]$/m);
  for (const trigger of ["pull_request", "schedule", "workflow_dispatch"]) assert.match(triggers, new RegExp(`^  ${trigger}:`, "m"));
  // A cancelled push run leaves its SHA without backend/frontend checks, which deploy admission needs.
  assert.match(triggers, /cancel-in-progress: \$\{\{ github\.event_name != 'push' \}\}/);
});

test("ruleset payload keeps main guarded by required non-strict checks and drops only merge_queue", () => {
  const payload = JSON.parse(fallback("ruleset-22854843-no-merge-queue.json"));
  const types = payload.rules.map((rule) => rule.type);
  assert.ok(!types.includes("merge_queue"));
  for (const kept of ["deletion", "non_fast_forward", "pull_request", "required_status_checks"]) assert.ok(types.includes(kept), kept);
  const checks = payload.rules.find((rule) => rule.type === "required_status_checks").parameters;
  assert.equal(checks.strict_required_status_checks_policy, false);
  assert.deepEqual(checks.required_status_checks.map((check) => check.context).sort(), ["backend", "backend-tests", "frontend"]);
  assert.equal(payload.enforcement, "active");
  assert.deepEqual(payload.conditions.ref_name.include, ["~DEFAULT_BRANCH"]);
  // PUT body only: read-only fields would be rejected or ignored by the API.
  for (const field of ["id", "created_at", "updated_at", "_links", "node_id", "source", "source_type"]) assert.ok(!(field in payload), field);
});

test("without a merge_group run the A2 skip logic selects full push CI and never skips a scope", () => {
  const env = { EVENT_NAME: "push", GITHUB_REF: "refs/heads/main", GITHUB_SHA: "a".repeat(40), GITHUB_REPOSITORY: "cheese-work/multica" };
  // No merge_group run listed (queue switched off): bounded wait, then not green.
  let time = 0;
  assert.equal(hasGreenMergeGroup(env, () => ({ workflow_runs: [] }), (ms) => { time += ms; }, () => time), false);
  // Fallback runbook sets CI_PUSH_FULL=true: no lookup, so no 180 s wait on every push.
  assert.equal(hasGreenMergeGroup({ ...env, CI_PUSH_FULL: "true" }, () => assert.fail("must not query Actions")), false);
  const outputs = decideScopes("push", Object.fromEntries(Object.keys(filters).map((scope) => [scope, "true"])), "main", false);
  assert.equal(outputs.push_full, "true");
  assert.equal(outputs.merge_group_green, "false");
  for (const scope of Object.keys(filters)) assert.equal(outputs[scope], "true");
});

test("runbook lists the three switch steps in order and the rollback", () => {
  const doc = fallback("README.md");
  const order = ["CI_PUSH_FULL", "ci-push-trigger.patch", "ruleset-22854843-no-merge-queue.json"].map((needle) => doc.indexOf(needle));
  assert.ok(order.every((index) => index >= 0) && order[0] < order[1] && order[1] < order[2], order.join());
  assert.match(doc, /Rollback/);
  assert.match(doc, /locale/i);
});
