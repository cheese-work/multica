import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

// CHE-3279 (CI economy S5c): the required PR checks stay a small fast lane. Static checks only;
// the 15 min p90 itself is measured from run data (see the PR body), not asserted here.
const file = (path) => new URL(`../${path}`, import.meta.url);
// A missing file reads as empty so the failing test names the file in an assertion, not a load error.
const read = (path) => (existsSync(file(path)) ? readFileSync(file(path), "utf8") : "");
const ci = read(".github/workflows/ci.yml");
const policy = read(".github/ci-policy.yml");

// Jobs a required check may wait on. Anything heavier would put its runtime on the PR critical path.
const FAST_LANE_DEPS = new Set(["changes", "migration-prefix-check", "backend-test-shards"]); // CHE-4268: shard matrix behind backend-tests

const section = (text, start, end) => {
  const from = text.indexOf(start);
  assert.notEqual(from, -1, `missing ${start.trim()}`);
  const to = end ? text.indexOf(end, from + start.length) : text.length;
  return text.slice(from, to === -1 ? text.length : to);
};
const job = (id) => {
  const header = `\n  ${id}:\n`;
  const body = ci.slice(ci.indexOf(header) + header.length);
  return body.split(/\n  [a-z][\w-]*:\n/)[0];
};
// Value of a job-level key (4-space indent), joined across continuation and block-list lines.
const jobKey = (id, key) => {
  const lines = job(id).split("\n");
  const at = lines.findIndex((line) => line.startsWith(`    ${key}:`));
  if (at === -1) return "";
  const rest = [];
  for (const line of lines.slice(at + 1)) {
    if (/^    \S/.test(line)) break;
    rest.push(line);
  }
  return [lines[at].slice(`    ${key}:`.length), ...rest].join(" ");
};
const needs = (id) => jobKey(id, "needs").replace(/#[^\n]*/g, "").split(/[\s,\[\]]+/).filter((name) => name && name !== "-");
const requiredChecks = () => section(policy, "required_checks:", "\nstatus_contexts:")
  .split("\n").map((l) => l.match(/^\s+- (\S+)/)?.[1]).filter(Boolean);

test("ci.yml and ci-policy.yml exist", () => {
  assert.ok(ci.length > 0, ".github/workflows/ci.yml is missing");
  assert.ok(policy.length > 0, ".github/ci-policy.yml is missing");
});

test("ci.yml runs on pull_request and merge_group without trigger path filters", () => {
  const on = section(ci, "\non:\n", "\nconcurrency:");
  assert.match(on, /^  pull_request:/m);
  assert.match(on, /^  merge_group:/m);
  assert.doesNotMatch(on, /paths(-ignore)?:/);
});

test("ci-policy.yml declares the required checks the rulesets name", () => {
  assert.deepEqual(requiredChecks().sort(), ["backend-tests", "security-gate"]);
  assert.match(policy, /^merge_queue: true$/m);
});

test("the ci.yml required check waits only on fast-lane jobs and is not push-only", () => {
  const checks = requiredChecks().filter((name) => ci.includes(`\n  ${name}:\n`));
  assert.deepEqual(checks, ["backend-tests"]);
  for (const name of checks) {
    const deps = needs(name);
    assert.ok(deps.length > 0, `${name} has no needs`);
    for (const dep of deps) assert.ok(FAST_LANE_DEPS.has(dep), `${name} waits on ${dep}`);
    assert.doesNotMatch(jobKey(name, "if"), /github\.event_name/, `${name} restricts its event`);
  }
});

test("each exception names an owner and an expiry date", () => {
  const exceptions = section(policy, "\nexceptions:\n").split(/\n  - /).slice(1);
  assert.ok(exceptions.length > 0);
  for (const entry of exceptions) {
    assert.match(entry, /^\s*repo: cheese-work\/multica$/m);
    assert.match(entry, /^\s*rule: \S+$/m);
    assert.match(entry, /^\s*reason: \S/m);
    assert.match(entry, /^\s*owner: \S+$/m);
    assert.match(entry, /^\s*expires: \d{4}-\d{2}-\d{2}$/m);
  }
});

// D-7: an exception lasts at most 60 days. `added` is the date the entry was written.
test("each exception expires within 60 days of the day it was added", () => {
  const exceptions = section(policy, "\nexceptions:\n").split(/\n  - /).slice(1);
  for (const entry of exceptions) {
    const added = entry.match(/^\s*added: (\d{4}-\d{2}-\d{2})$/m)?.[1];
    const expires = entry.match(/^\s*expires: (\d{4}-\d{2}-\d{2})$/m)?.[1];
    assert.ok(added, "exception has no added date");
    const days = (Date.parse(expires) - Date.parse(added)) / 86_400_000;
    assert.ok(days > 0 && days <= 60, `exception lasts ${days} days`);
  }
});
