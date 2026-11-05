import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

// CHE-3989 (CHE-3987 S2): ci.yml runs on pull_request for base main and agent/**, so every run belongs to the PR and
// checks out the PR merge commit. A draft stacked PR starts no runner jobs unless it carries the `run-ci` label.
// Events that change nothing (title edit, unrelated label, ready on main) must neither cancel a real run (own
// concurrency group) nor hide one (skipped jobs carry a distinct raw name, see NAME_SUFFIX).
const ci = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");
const ciOn = ci.slice(ci.indexOf("\non:\n"), ci.indexOf("\nconcurrency:"));
const job = (id) => {
  const header = `\n  ${id}:\n`;
  assert.ok(ci.includes(header), `missing job ${id}`);
  return ci.slice(ci.indexOf(header) + header.length).split(/\n  [a-z][\w-]*:\n/)[0];
};
const jobIf = (id) => job(id).match(/^    if: \$\{\{ (.+) \}\}$/m)?.[1] ?? "";
const jobIds = [...ci.slice(ci.indexOf("\njobs:\n")).matchAll(/^  ([a-z][\w-]*):$/gm)].map((m) => m[1]);

const RUN = jobIf("changes");
const evaluate = (expr, { event = "pull_request", action, base = "main", draft = false, labels = [], label, changes = {} }) => {
  const js = expr.replace(/contains\(github\.event\.pull_request\.labels\.\*\.name, '([^']+)'\)/g, "labels.includes('$1')");
  const github = { event_name: event, event: { action, label: { name: label }, changes, pull_request: { draft, base: { ref: base } } } };
  return Boolean(new Function("github", "labels", `"use strict"; return (${js});`)(github, labels));
};
const run = (ctx) => evaluate(RUN, ctx);

const STACKED = "agent/x/che-1";

test("ci.yml triggers on main and agent/** with the PR events the opt-in needs", () => {
  assert.match(ciOn, /pull_request:\n    branches: \[main, 'agent\/\*\*'\]\n    types: \[opened, synchronize, reopened, ready_for_review, labeled, edited\]\n/);
  assert.match(ciOn, /^  merge_group:/m);
  assert.match(ciOn, /^  workflow_dispatch:/m);
});

test("no second workflow dispatches ci.yml", () => {
  assert.equal(existsSync(new URL("../.github/workflows/ci-dispatch.yml", import.meta.url)), false);
  assert.doesNotMatch(ci, /gh workflow run/);
});

test("main PR: opened, synchronize and reopened run, draft or not", () => {
  for (const action of ["opened", "synchronize", "reopened"]) {
    for (const draft of [false, true]) assert.equal(run({ action, draft }), true, `${action} draft=${draft}`);
  }
});

test("main PR: title edit, unrelated label, run-ci label and ready_for_review are no-ops", () => {
  assert.equal(run({ action: "edited" }), false, "title edit");
  assert.equal(run({ action: "edited", changes: { title: { from: "a" } } }), false, "title edit with changes");
  assert.equal(run({ action: "labeled", label: "bug" }), false, "unrelated label");
  assert.equal(run({ action: "labeled", label: "run-ci" }), false, "run-ci on main");
  assert.equal(run({ action: "ready_for_review" }), false, "ready on main");
});

test("main PR: retarget to main runs", () => {
  assert.equal(run({ action: "edited", changes: { base: { ref: { from: STACKED } } } }), true);
  assert.equal(run({ action: "edited", draft: true, changes: { base: { ref: { from: STACKED } } } }), true);
});

test("non-draft stacked PR runs on every run-worthy event", () => {
  for (const action of ["opened", "synchronize", "reopened", "ready_for_review"]) assert.equal(run({ action, base: STACKED }), true, action);
  assert.equal(run({ action: "edited", base: STACKED, changes: { base: { ref: { from: "main" } } } }), true, "retarget");
});

test("draft stacked PR without run-ci runs nothing", () => {
  for (const action of ["opened", "synchronize", "reopened"]) assert.equal(run({ action, base: STACKED, draft: true }), false, action);
  assert.equal(run({ action: "labeled", label: "bug", base: STACKED, draft: true }), false, "unrelated label");
  assert.equal(run({ action: "edited", base: STACKED, draft: true }), false, "title edit");
  assert.equal(run({ action: "edited", base: STACKED, draft: true, changes: { base: { ref: { from: "main" } } } }), false, "retarget stays draft");
});

test("draft stacked PR with run-ci runs on the label and on later pushes", () => {
  assert.equal(run({ action: "labeled", label: "run-ci", base: STACKED, draft: true, labels: ["run-ci"] }), true, "label added");
  for (const action of ["opened", "synchronize", "reopened"]) assert.equal(run({ action, base: STACKED, draft: true, labels: ["run-ci"] }), true, action);
});

test("a stacked PR already running does not run again for an unrelated label or the run-ci label", () => {
  assert.equal(run({ action: "labeled", label: "run-ci", base: STACKED, labels: ["run-ci"] }), false, "run-ci on non-draft");
  assert.equal(run({ action: "labeled", label: "bug", base: STACKED, labels: ["run-ci"] }), false, "unrelated label");
});

test("merge_group, schedule and workflow_dispatch always run", () => {
  for (const event of ["merge_group", "schedule", "workflow_dispatch"]) assert.equal(run({ event }), true, event);
});

test("the run condition is the same text on changes, migration-prefix-check, upstream-update-source-guard and the concurrency group", () => {
  assert.ok(RUN.length > 100, "changes has no run condition");
  assert.equal(jobIf("upstream-update-source-guard"), RUN);
  assert.equal(jobIf("migration-prefix-check"), `github.event_name == 'pull_request' && (${RUN})`);
  assert.ok(ci.includes(`\${{ (${RUN}) && 'run' || 'noop' }}`), "concurrency group lacks the run/noop split");
});

test("concurrency keeps per-PR cancel-in-progress", () => {
  assert.match(ci, /concurrency:\n  group: \$\{\{ github\.workflow \}\}-\$\{\{ github\.event_name \}\}-\$\{\{ github\.event\.pull_request\.number \|\| github\.ref \}\}-/);
  assert.match(ci, /cancel-in-progress: true/);
});

test("always() jobs skip when changes skipped", () => {
  for (const id of ["frontend", "backend", "warning-budget"]) assert.match(jobIf(id), /needs\.changes\.result != 'skipped'/, id);
});

// A skipped job still creates a check run with its job name. A static name (or a constant expression) equals the real
// run's name, so a skipped no-op run would hide a red or pending real check of the same head. A name that depends on
// the context stays raw while skipped, so it is distinct and evaluates to the plain id in a real run.
test("every job name is the id plus a context-dependent suffix that is empty in a real run", () => {
  const NAME_SUFFIX = "${{ github.run_id == 0 && 'x' || '' }}";
  for (const id of jobIds) {
    const name = job(id).match(/^    name: (.+)$/m)?.[1];
    assert.ok(name, `${id} has no name`);
    if (name.includes("matrix.")) continue;
    assert.equal(name, `${id}${NAME_SUFFIX}`, id);
  }
});

test("ci.yml still runs this test", () => {
  assert.match(ci, /scripts\/ci-stacked-pr\.test\.mjs/);
});
