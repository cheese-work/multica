import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

// CHE-3989 (CHE-3987 S2): CI runs for PRs stacked on an `agent/**` base, but a draft stacked PR runs
// nothing until it is non-draft or carries the `run-ci` label. Static checks plus an evaluation of the
// one run-condition expression that gates every job. Main-based PR behavior is unchanged.
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

const evaluate = ({ action, base = "main", draft = false, labels = [], label, changes, event = "pull_request" }) => {
  const js = RUN.replace(/contains\(github\.event\.pull_request\.labels\.\*\.name, '([^']+)'\)/g, "labels.includes('$1')");
  const github = {
    event_name: event,
    base_ref: base,
    event: { action, label: label && { name: label }, changes, pull_request: { draft, labels } },
  };
  return new Function("github", "labels", `"use strict"; return (${js});`)(github, labels);
};

test("pull_request also runs for agent/** bases and the stacked-PR event types", () => {
  assert.match(on, /branches: \[main, "agent\/\*\*"\]/);
  for (const type of ["opened", "synchronize", "reopened", "ready_for_review", "labeled", "edited"]) {
    assert.match(on, new RegExp(`types: \\[[^\\]]*\\b${type}\\b`), `missing pull_request type ${type}`);
  }
  assert.match(on, /^  merge_group:/m);
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
  for (const action of ["opened", "synchronize", "reopened", "ready_for_review"]) {
    assert.equal(evaluate({ action, base: "agent/x/che-1" }), true, action);
  }
});

test("a draft stacked PR runs nothing without the run-ci label", () => {
  for (const action of ["opened", "synchronize", "reopened", "edited", "labeled"]) {
    assert.equal(evaluate({ action, base: "agent/x/che-1", draft: true, labels: ["other"], label: "other", changes: {} }), false, action);
  }
});

test("the run-ci label opts a draft stacked PR in, on the label event and on later pushes", () => {
  assert.equal(evaluate({ action: "labeled", base: "agent/x/che-1", draft: true, labels: ["run-ci"], label: "run-ci" }), true);
  assert.equal(evaluate({ action: "synchronize", base: "agent/x/che-1", draft: true, labels: ["run-ci"] }), true);
});

test("a retarget to main runs CI again; other edits and labels do not", () => {
  assert.equal(evaluate({ action: "edited", base: "main", draft: true, changes: { base: { ref: { from: "agent/x/che-1" } } } }), true);
  assert.equal(evaluate({ action: "edited", base: "main", changes: { title: { from: "old" } } }), false);
  assert.equal(evaluate({ action: "edited", base: "main", changes: { body: { from: "old" } } }), false);
  assert.equal(evaluate({ action: "labeled", base: "main", labels: ["run-ci"], label: "run-ci" }), false);
  assert.equal(evaluate({ action: "labeled", base: "agent/x/che-1", labels: ["bug"], label: "bug" }), false);
});

test("main-based PR behavior is unchanged", () => {
  for (const draft of [true, false]) {
    for (const action of ["opened", "synchronize", "reopened"]) assert.equal(evaluate({ action, draft }), true, `${action} draft=${draft}`);
    assert.equal(evaluate({ action: "ready_for_review", draft }), false, "ready_for_review on main stays quiet");
  }
});

test("non-PR events are never gated", () => {
  for (const event of ["merge_group", "schedule", "workflow_dispatch", "push"]) assert.equal(evaluate({ event }), true, event);
});
