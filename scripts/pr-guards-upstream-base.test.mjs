import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-5014: an upstream candidate PR (base `upstream-main`) carries no fork-only content, so the
// `Changelog line` and `mechanical-tier` guards skip it. Every other base keeps both guards.
// A job-level `if` that is false reports `skipped`, which is neutral: no red or cancelled check.
const raw = readFileSync(new URL("../.github/workflows/pr-guards.yml", import.meta.url), "utf8");
const wf = raw.replace(/^\s*#.*$/gm, "");

function jobIf(name) {
  const start = wf.indexOf(`\n  ${name}:`);
  assert.notEqual(start, -1, `job ${name} exists`);
  const rest = wf.slice(start + 1);
  const next = rest.slice(1).search(/\n {2}[\w-]+:/);
  const block = next === -1 ? rest : rest.slice(0, next + 1);
  return block.match(/^ {4}if: (.+)$/m)?.[1] ?? "true";
}

// The `if` expressions here use only `&&`, `!=`, `==`, single-quoted strings and dotted names,
// which are valid JavaScript, so evaluate them against a minimal `github` context.
const runs = (name, { base, state = "open" }) =>
  new Function("github", `return Boolean(${jobIf(name)});`)({
    base_ref: base,
    event: { pull_request: { state, base: { ref: base } } },
  });

for (const job of ["changelog-line", "mechanical-tier"]) {
  test(`${job} skips a PR based on upstream-main`, () => {
    assert.equal(runs(job, { base: "upstream-main" }), false);
  });
  test(`${job} still runs on a main-based PR`, () => {
    assert.equal(runs(job, { base: "main" }), true);
  });
  test(`${job} still runs on a stacked PR base`, () => {
    assert.equal(runs(job, { base: "agent/x/che-1-stack" }), true);
  });
}

test("changelog-line keeps its closed-PR skip", () => {
  assert.equal(runs("changelog-line", { base: "main", state: "closed" }), false);
});

test("the other guards are not gated on the base branch", () => {
  for (const job of ["close-target", "fork-sync-guard", "security-gate"]) {
    assert.doesNotMatch(jobIf(job), /upstream-main/, job);
  }
});
