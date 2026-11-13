import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-5208 (T5b of CHE-5194): the blocking backstop for the merge-method tag and the review policy.
// The in-run shim fixes a PR before it is published; this job fails a PR that bypassed the shim
// (absolute path to gh, the web UI, a runtime with no shim). It runs `multica guard check-pr`,
// built from the trusted workflow commit, so it applies the same rules as the shim.
const raw = readFileSync(new URL("../.github/workflows/pr-guards.yml", import.meta.url), "utf8");
const wf = raw.replace(/^\s*#.*$/gm, "");

function job(name) {
  const start = wf.indexOf(`\n  ${name}:`);
  assert.notEqual(start, -1, `job ${name} exists`);
  const rest = wf.slice(start + 1);
  const next = rest.slice(1).search(/\n {2}[\w-]+:/);
  return next === -1 ? rest : rest.slice(0, next + 1);
}

const guard = () => job("guard-check");

test("guard-check job exists with a stable, required-check-friendly name", () => {
  assert.match(guard(), /^ {4}name: PR guard rules$/m);
});

test("guard-check runs check-pr against the PR's own URL and repository", () => {
  const g = guard();
  assert.match(g, /guard check-pr "\$PR_URL" -R "\$REPO"/);
  assert.match(g, /PR_URL: \$\{\{ github\.event\.pull_request\.html_url \}\}/);
  assert.match(g, /REPO: \$\{\{ github\.repository \}\}/);
});

test("guard-check builds the CLI from the trusted workflow commit and never runs the PR head", () => {
  const g = guard();
  assert.match(g, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(g, /persist-credentials: false/);
  assert.match(g, /go build -o "\$RUNNER_TEMP\/multica" \.\/cmd\/multica/);
  assert.doesNotMatch(g, /pull_request\.head\.(sha|ref)/);
  assert.doesNotMatch(g, /refs\/pull/);
});

test("guard-check holds read-only permissions and no secret beyond the workflow token", () => {
  const g = guard();
  assert.match(g, /permissions:\n {6}contents: read\n {6}pull-requests: read/);
  assert.doesNotMatch(g, /: write/);
  const secrets = [...g.matchAll(/secrets\.(\w+)/g)].map((m) => m[1]);
  assert.deepEqual([...new Set(secrets)], ["GITHUB_TOKEN"]);
});

test("guard-check blocks: not continue-on-error, no job-level skip but the closed PR", () => {
  const g = guard();
  assert.doesNotMatch(g, /continue-on-error/);
  assert.match(g, /^ {4}if: github\.event\.pull_request\.state != 'closed'$/m);
  assert.doesNotMatch(g, /upstream-main/);
});

test("guard-check never puts the PR title or body into a shell line", () => {
  const g = guard();
  assert.doesNotMatch(g, /pull_request\.(title|body)/);
});

test("guard-check takes the optional review policy from a repository variable, never from the tree", () => {
  const g = guard();
  assert.match(g, /GUARD_POLICY_JSON: \$\{\{ vars\.GUARD_POLICY_JSON \}\}/);
  assert.match(g, /MULTICA_GUARD_POLICY="\$RUNNER_TEMP\/guard-policy\.json"/);
  assert.match(g, /MULTICA_GUARD_POLICY_SHA256=/);
});

test("guard-check keeps the one-run-per-event contract (CHE-4385): no new trigger", () => {
  assert.match(wf, /types: \[opened, synchronize, reopened, ready_for_review\]/);
});

test("guard-check runs on a GitHub-hosted runner", () => {
  assert.match(guard(), /runs-on: (ubuntu-24\.04|\$\{\{ vars\.RUNNER_[A-Z_]+ \|\| 'ubuntu-24\.04' \}\})\s*$/m);
});
