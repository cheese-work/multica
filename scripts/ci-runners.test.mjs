import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8");
const runsOn = (text) => text.split("\n").filter((line) => /^\s+runs-on:/.test(line));
const WORKFLOWS = ["ci.yml", "pr-changelog-check.yml"];

// Evaluates a `runs-on: ${{ ... }}` expression for one event, as GitHub would.
function labels(line, ctx) {
  const expr = line.match(/\$\{\{(.*)\}\}/)[1]
    .replaceAll("github.event.pull_request.head.repo.full_name", "ctx.prHead")
    .replace(/github\.(repository_owner|repository|event_name|head_ref|ref_name)/g, (_, k) => `ctx.${k}`)
    .replaceAll("fromJSON(", "JSON.parse(")
    .replaceAll("startsWith(", "sw(");
  const sw = (s, p) => String(s).startsWith(p);
  return new Function("ctx", "sw", `return ${expr}`)(ctx, sw);
}

const REPO = "cheese-work/multica";
const base = { repository_owner: "cheese-work", repository: REPO, event_name: "pull_request", head_ref: "agent/x", ref_name: "1/merge", prHead: REPO };
const SCENARIOS = [
  ["same-repository pull request", {}, true],
  ["fork pull request", { prHead: "someone/multica" }, false],
  ["pull request whose head repository is gone", { prHead: null }, false],
  ["upstream-sync pull request", { head_ref: "upstream-sync/1" }, false],
  ["push to main", { event_name: "push", head_ref: "", ref_name: "main", prHead: undefined }, true],
  ["scheduled run", { event_name: "schedule", head_ref: "", ref_name: "main", prHead: undefined }, true],
  ["dispatch on an upstream-sync ref", { event_name: "workflow_dispatch", head_ref: "", ref_name: "upstream-sync/2", prHead: undefined }, false],
  ["another owner's repository", { repository_owner: "congvc-dev", repository: "congvc-dev/multica" }, false],
];

for (const name of WORKFLOWS) {
  test(`${name} jobs run on the X99 runners`, () => {
    const lines = runsOn(read(name));
    assert.ok(lines.length > 0);
    for (const line of lines) assert.match(line, /cheese-x99/, line.trim());
  });

  test(`${name} sends only trusted same-repository events to X99`, () => {
    for (const line of runsOn(read(name))) {
      for (const [what, patch, onX99] of SCENARIOS) {
        const got = labels(line, { ...base, ...patch });
        assert.equal(got.includes("cheese-x99"), onX99, `${what}: ${line.trim()}`);
        if (!onX99) assert.ok(got.length === 1, `${what} falls back to a hosted label`);
      }
    }
  });
}
