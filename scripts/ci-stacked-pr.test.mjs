import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

// CHE-3989 (CHE-3987 S2): CI runs for PRs stacked on an `agent/**` base, but a draft stacked PR runs
// nothing until it is non-draft or carries the `run-ci` label. Static checks plus an evaluation of the
// one run-condition expression that gates every job. ci.yml adds only `ready_for_review` to the PR types:
// `labeled` and `edited` never start ci.yml, because a skipped ci.yml run replaces the earlier check results
// of the same head and a full one cancels the running CI. The `run-ci` label and a retarget to main go
// through .github/workflows/ci-dispatch.yml, which starts ci.yml with workflow_dispatch.
const path = new URL("../.github/workflows/ci.yml", import.meta.url);
const ci = existsSync(path) ? readFileSync(path, "utf8") : "";
const on = ci.slice(ci.indexOf("\non:\n"), ci.indexOf("\nconcurrency:"));
const job = (id) => {
  const header = `\n  ${id}:\n`;
  const body = ci.slice(ci.indexOf(header) + header.length);
  return body.split(/\n  [a-z][\w-]*:\n/)[0];
};

// The condition lives in `changes.if`; the other jobs and the concurrency group must carry the same text.
const RUN = job("changes").match(/^    if: \$\{\{ (.+) \}\}$/m)?.[1] ?? "";

const evalExpr = (expr, github, labels = []) => {
  const js = expr.replace(/contains\(github\.event\.pull_request\.labels\.\*\.name, '([^']+)'\)/g, "labels.includes('$1')");
  return new Function("github", "labels", `"use strict"; return (${js});`)(github, labels);
};
const evaluate = ({ base = "main", draft = false, labels = [], event = "pull_request" }) =>
  evalExpr(RUN, { event_name: event, base_ref: base, event: { pull_request: { draft, labels } } }, labels);

test("pull_request also runs for agent/** bases and ready_for_review, never for labeled or edited", () => {
  assert.match(on, /branches: \[main, "agent\/\*\*"\]/);
  const types = on.match(/types: \[([^\]]*)\]\n  merge_group/)?.[1].split(",").map((t) => t.trim());
  assert.deepEqual(types, ["opened", "synchronize", "reopened", "ready_for_review"]);
  assert.match(on, /^  merge_group:/m);
  assert.match(on, /^  workflow_dispatch:/m);
});

test("the run condition guards the changes job, the unconditional jobs and the concurrency group", () => {
  assert.ok(RUN, "changes job has no `if: ${{ ... }}` run condition");
  assert.ok(job("migration-prefix-check").includes(RUN), "migration-prefix-check must carry the run condition");
  assert.ok(job("upstream-update-source-guard").includes(RUN), "upstream-update-source-guard must carry the run condition");
  assert.ok(ci.slice(0, ci.indexOf("\njobs:")).includes(RUN), "concurrency group must carry the run condition");
  assert.match(ci, /cancel-in-progress: true/);
  for (const id of ["frontend", "backend", "warning-budget"]) {
    assert.match(job(id), /needs\.changes\.result != 'skipped'/, `${id} must not run when changes is skipped`);
  }
});

test("a non-draft stacked PR runs CI", () => {
  assert.equal(evaluate({ base: "agent/x/che-1" }), true);
});

test("a draft stacked PR runs nothing without the run-ci label", () => {
  assert.equal(evaluate({ base: "agent/x/che-1", draft: true, labels: ["other"] }), false);
});

test("the run-ci label opts a draft stacked PR in on later pushes", () => {
  assert.equal(evaluate({ base: "agent/x/che-1", draft: true, labels: ["run-ci"] }), true);
});

test("a main-based PR runs as before, draft or not", () => {
  for (const draft of [true, false]) assert.equal(evaluate({ draft }), true, `draft=${draft}`);
});

test("non-PR events are never gated", () => {
  for (const event of ["merge_group", "schedule", "workflow_dispatch", "push"]) assert.equal(evaluate({ event }), true, event);
});

// ci-dispatch.yml: the label opt-in and the retarget start ci.yml by workflow_dispatch on the head branch.
const dpath = new URL("../.github/workflows/ci-dispatch.yml", import.meta.url);
const dispatch = existsSync(dpath) ? readFileSync(dpath, "utf8") : "";
const DISPATCH_IF = dispatch.match(/^    if: \$\{\{ (.+) \}\}$/m)?.[1] ?? "";
const dispatchRuns = ({ action, base, label, changes, head = "cheese-work/multica" }) =>
  evalExpr(DISPATCH_IF, {
    repository: "cheese-work/multica",
    event: { action, label: label && { name: label }, changes, pull_request: { base: { ref: base }, head: { repo: { full_name: head } } } },
  });

test("ci-dispatch.yml fires only on labeled and edited and starts ci.yml with workflow_dispatch", () => {
  assert.match(dispatch, /types: \[labeled, edited\]/);
  assert.doesNotMatch(dispatch, /^    branches:/m);
  assert.match(dispatch, /actions: write/);
  assert.match(dispatch, /gh workflow run ci\.yml .*--ref "\$HEAD_REF"/);
});

test("the run-ci label on a stacked PR starts CI; other labels and main-based PRs start nothing", () => {
  assert.equal(dispatchRuns({ action: "labeled", base: "agent/x/che-1", label: "run-ci" }), true);
  assert.equal(dispatchRuns({ action: "labeled", base: "agent/x/che-1", label: "bug" }), false);
  assert.equal(dispatchRuns({ action: "labeled", base: "main", label: "run-ci" }), false);
  assert.equal(dispatchRuns({ action: "labeled", base: "main", label: "bug" }), false);
});

test("a retarget to main starts CI; repeated title or body edits start nothing", () => {
  assert.equal(dispatchRuns({ action: "edited", base: "main", changes: { base: { ref: { from: "agent/x/che-1" } } } }), true);
  for (const base of ["main", "agent/x/che-1"]) {
    assert.equal(dispatchRuns({ action: "edited", base, changes: { title: { from: "old" } } }), false, base);
    assert.equal(dispatchRuns({ action: "edited", base, changes: { body: { from: "old" } } }), false, base);
  }
  assert.equal(dispatchRuns({ action: "edited", base: "agent/x/che-2", changes: { base: { ref: { from: "agent/x/che-1" } } } }), false);
});

test("a PR from a fork never starts a dispatch", () => {
  assert.equal(dispatchRuns({ action: "labeled", base: "agent/x/che-1", label: "run-ci", head: "someone/multica" }), false);
});
