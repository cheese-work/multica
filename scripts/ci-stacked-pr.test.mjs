import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

// CHE-3989 (CHE-3987 S2): a PR whose base is `agent/**` gets CI through .github/workflows/ci-dispatch.yml, which
// starts ci.yml with workflow_dispatch on the head branch. ci.yml itself stays exactly as it was for main PRs:
// a ci.yml run for a stacked draft would be skipped and shadow the dispatched results in the PR check list, and a
// ci.yml run for `labeled` or `edited` would cancel or skip the CI of every main PR.
const read = (file) => {
  const path = new URL(`../.github/workflows/${file}`, import.meta.url);
  return existsSync(path) ? readFileSync(path, "utf8") : "";
};
const ci = read("ci.yml");
const dispatch = read("ci-dispatch.yml");
const ciOn = ci.slice(ci.indexOf("\non:\n"), ci.indexOf("\nconcurrency:"));
const RUN = dispatch.match(/^    if: \$\{\{ (.+) \}\}$/m)?.[1] ?? "";

const run = ({ action, base = "main", draft = false, labels = [], label, changes = {}, head = "cheese-work/multica" }) => {
  const js = RUN.replace(/contains\(github\.event\.pull_request\.labels\.\*\.name, '([^']+)'\)/g, "labels.includes('$1')");
  const github = {
    repository: "cheese-work/multica",
    event: { action, label: { name: label }, changes, pull_request: { draft, base: { ref: base }, head: { repo: { full_name: head } } } },
  };
  return new Function("github", "labels", `"use strict"; return (${js});`)(github, labels);
};

const STACKED = "agent/x/che-1";
const PUSH_TYPES = ["opened", "synchronize", "reopened", "ready_for_review"];

test("ci.yml keeps its main-only pull_request trigger and gains no skipped or cancelling run", () => {
  assert.match(ciOn, /pull_request:\n    branches: \[main\]\n/);
  assert.doesNotMatch(ciOn, /agent\/\*\*|labeled|edited|ready_for_review/);
  assert.match(ciOn, /^  workflow_dispatch:/m);
  assert.match(ci, /cancel-in-progress: true/);
  assert.match(ci, /scripts\/ci-stacked-pr\.test\.mjs/);
});

test("ci-dispatch.yml fires on the PR events, needs no branch filter, and starts ci.yml on the head branch", () => {
  assert.ok(RUN, "ci-dispatch.yml has no job `if: ${{ ... }}`");
  assert.match(dispatch, /types: \[opened, synchronize, reopened, ready_for_review, labeled, edited\]/);
  assert.doesNotMatch(dispatch, /^    branches:/m);
  assert.match(dispatch, /actions: write/);
  assert.match(dispatch, /gh workflow run ci\.yml .*--ref "\$HEAD_REF"/);
});

// The dispatch job is the PR-visible result of the dispatched ci.yml run (workflow_dispatch results never reach the
// PR check list), so it must wait for that run and fail when the run fails. These tests run the job's own script
// against a stub `gh`.
const SCRIPT = (() => {
  const block = dispatch.match(/^ {8}run: \|\n((?: {10}.*\n|\n)+)/m)?.[1] ?? "";
  return block.replace(/^ {10}/gm, "");
})();

const runDispatch = ({ watchExit = 0, dispatchCreatesRun = true } = {}) => {
  const dir = mkdtempSync(join(tmpdir(), "ci-dispatch-"));
  const stub = (name, body) => {
    writeFileSync(join(dir, name), `#!/usr/bin/env bash\n${body}\n`);
    chmodSync(join(dir, name), 0o755);
  };
  writeFileSync(join(dir, "runs"), "11\n");
  stub("sleep", "exit 0");
  stub(
    "gh",
    `echo "$*" >> "${dir}/calls"
case "$1 $2" in
  "run list") cat "${dir}/runs" ;;
  "workflow run") ${dispatchCreatesRun ? `echo 12 >> "${dir}/runs"` : "true"} ;;
  "run watch") exit ${watchExit} ;;
esac`,
  );
  const result = spawnSync("bash", ["-c", SCRIPT], {
    env: { ...process.env, PATH: `${dir}:${process.env.PATH}`, GITHUB_REPOSITORY: "cheese-work/multica", HEAD_REF: "agent/x/che-1", HEAD_SHA: "abc", GH_TOKEN: "t" },
    encoding: "utf8",
  });
  const calls = existsSync(join(dir, "calls")) ? readFileSync(join(dir, "calls"), "utf8") : "";
  return { status: result.status, calls, stderr: result.stderr };
};

test("the dispatch job waits for the dispatched run (not an older one) and passes when it passes", () => {
  assert.ok(SCRIPT, "ci-dispatch.yml has no `run: |` script");
  const { status, calls } = runDispatch();
  assert.equal(status, 0);
  assert.match(calls, /workflow run ci\.yml/);
  assert.match(calls, /run watch 12 .*--exit-status/);
  assert.doesNotMatch(calls, /run watch 11/);
});

test("the dispatch job fails when the dispatched CI run fails or is cancelled", () => {
  assert.notEqual(runDispatch({ watchExit: 1 }).status, 0);
});

test("the dispatch job fails when no dispatched run shows up", () => {
  assert.notEqual(runDispatch({ dispatchCreatesRun: false }).status, 0);
});

test("the dispatch job has a per-PR cancelling group and enough time to watch a full CI run", () => {
  assert.match(dispatch, /concurrency:\n {6}group: ci-dispatch-\$\{\{ github\.event\.pull_request\.number \}\}\n {6}cancel-in-progress: true/);
  assert.ok(Number(dispatch.match(/timeout-minutes: (\d+)/)?.[1]) >= 60);
  assert.doesNotMatch(dispatch, /continue-on-error|\|\| true/);
});

test("a non-draft stacked PR starts CI on every push-type event", () => {
  for (const action of PUSH_TYPES) assert.equal(run({ action, base: STACKED }), true, action);
});

test("a draft stacked PR starts nothing without the run-ci label, on any event", () => {
  for (const action of [...PUSH_TYPES, "edited"]) {
    assert.equal(run({ action, base: STACKED, draft: true, labels: ["other"] }), false, action);
  }
  assert.equal(run({ action: "labeled", base: STACKED, draft: true, labels: ["other"], label: "other" }), false);
});

test("the run-ci label starts CI for a draft stacked PR, now and on later pushes", () => {
  assert.equal(run({ action: "labeled", base: STACKED, draft: true, labels: ["run-ci"], label: "run-ci" }), true);
  assert.equal(run({ action: "synchronize", base: STACKED, draft: true, labels: ["run-ci"] }), true);
});

test("a main PR starts nothing here: ci.yml owns it, and title, body and label events change nothing", () => {
  for (const draft of [true, false]) {
    for (const action of PUSH_TYPES) assert.equal(run({ action, draft }), false, `${action} draft=${draft}`);
    assert.equal(run({ action: "edited", draft, changes: { title: { from: "old" } } }), false);
    assert.equal(run({ action: "edited", draft, changes: { body: { from: "old" } } }), false);
    assert.equal(run({ action: "labeled", draft, label: "run-ci", labels: ["run-ci"] }), false);
    assert.equal(run({ action: "labeled", draft, label: "bug", labels: ["bug"] }), false);
  }
});

test("a retarget to main starts CI once; title or body edits and a retarget between agent/** bases start nothing", () => {
  assert.equal(run({ action: "edited", draft: true, changes: { base: { ref: { from: STACKED } } } }), true);
  assert.equal(run({ action: "edited", changes: { base: { ref: { from: STACKED } } } }), true);
  for (const changes of [{ title: { from: "old" } }, { body: { from: "old" } }]) {
    assert.equal(run({ action: "edited", base: STACKED, changes }), false);
  }
  assert.equal(run({ action: "edited", base: "agent/x/che-2", changes: { base: { ref: { from: STACKED } } } }), false);
});

test("a PR from a fork never starts a dispatch", () => {
  assert.equal(run({ action: "synchronize", base: STACKED, head: "someone/multica" }), false);
  assert.equal(run({ action: "labeled", base: STACKED, label: "run-ci", head: "someone/multica" }), false);
});
